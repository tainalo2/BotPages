package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Dir         string
	AdminToken  string
	Bots        map[string]string
	Publishers  map[string]string // jetons partagés : publient sur tout site non répertorié
	Readers     map[string]map[string]string
	AdminDir    string
	AdminHost   string
	ContentHost string
}
type Site struct {
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
}
type Server struct {
	cfg        Config
	identities *identityStore
	mu         sync.Mutex
	limits     map[string]rateWindow
}
type rateWindow struct {
	start time.Time
	count int
}

// version est injectée à la compilation : -ldflags "-X main.version=1.2.3".
var version = "dev"

const maxSiteBytes int64 = 512 << 20
const maxAssetBytes int64 = 32 << 20

func NewServer(c Config) (*Server, error) {
	if c.Dir == "" || c.AdminToken == "" {
		return nil, errors.New("data directory and admin token required")
	}

	if (c.AdminHost == "") != (c.ContentHost == "") || (c.AdminHost != "" && strings.ContainsAny(c.AdminHost+c.ContentHost, "/:*")) {
		return nil, errors.New("distinct admin and content hosts must be configured together")
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return nil, err
	}
	identities, err := openIdentityStore(c.Dir, c.AdminToken, c.Bots, c.Publishers)
	if err != nil {
		return nil, err
	}
	c.Bots = nil
	c.Publishers = nil
	return &Server{cfg: c, identities: identities, limits: make(map[string]rateWindow)}, nil
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *Server) actor(r *http.Request) (string, bool) {
	h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h != "" && equal(h, s.cfg.AdminToken) {
		return "admin", true
	}
	identity, ok, err := s.identities.authenticate(h)
	if err != nil || !ok {
		return "", false
	}
	if identity.SiteScope == publisherScope {
		return "publisher/" + identity.Name, false // « / » ne peut jamais égaler un nom de site
	}
	return identity.SiteScope, false
}

func (s *Server) Close() error { return s.identities.close() }

// isPublisher : jeton partagé des bots, limité aux sites non répertoriés.
func isPublisher(actor string) bool { return strings.HasPrefix(actor, "publisher/") }

// mayWrite : l'admin écrit partout, un bot sur son site, un publisher sur tout site
// (la restriction aux sites non répertoriés est vérifiée une fois le site chargé).
func mayWrite(actor string, admin bool, site string) bool {
	return admin || (actor != "" && actor == site) || isPublisher(actor)
}

// botIdentity : nom déclaratif du bot appelant (en-tête X-BotPages-Bot), pour les journaux
// uniquement ; ce n'est jamais une autorisation.
func botIdentity(r *http.Request) string {
	v := r.Header.Get("X-BotPages-Bot")
	if len(v) > 40 || !safeName(v) {
		return "-"
	}
	return v
}

func audit(r *http.Request, action, actor, site, file string) {
	log.Printf("audit action=%s actor=%s bot=%s site=%s path=%q", action, actor, botIdentity(r), site, file)
}
func (s *Server) authorizedReader(r *http.Request, site string) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	wanted, ok := s.cfg.Readers[site][u]
	return ok && equal(p, wanted)
}
func (s *Server) allowWrite(r *http.Request) bool {
	actor, _ := s.actor(r)
	if actor == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	v := s.limits[actor]
	if now.Sub(v.start) >= time.Minute {
		v = rateWindow{start: now}
	}
	if v.count >= 60 {
		return false
	}
	v.count++
	s.limits[actor] = v
	return true
}
func (s *Server) withinQuota(name, target string, incoming int64) bool {
	var size int64
	err := filepath.Walk(filepath.Join(s.cfg.Dir, name), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && !strings.HasPrefix(info.Name(), ".upload-") && !strings.HasPrefix(info.Name(), ".asset-") {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return false
	}
	if st, err := os.Stat(target); err == nil {
		size -= st.Size()
	}
	if st, err := os.Stat(target + ".previous"); err == nil {
		size -= st.Size()
	}
	if st, err := os.Stat(target); err == nil {
		size += st.Size()
	} // previous copy retained for rollback
	return size+incoming <= maxSiteBytes
}
func safeName(v string) bool {
	if v == "" || len(v) > 100 || v == "." || v == ".." {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Active document formats besides HTML may execute script outside the HTML sandbox
// (for example PDF plugins or SVG navigation), so never publish them as assets.
func unsafeDocumentPath(v string) bool {
	switch strings.ToLower(path.Ext(v)) {
	case ".svg", ".svgz", ".xml", ".xhtml", ".xht", ".pdf", ".mhtml", ".mht", ".shtml":
		return true
	}
	return false
}
func safePath(v string) bool {
	return v != "" && len(v) < 300 && !strings.HasPrefix(v, "/") && !strings.Contains(v, "\\") && !strings.Contains(v, "//") && path.Clean(v) == v && !strings.HasPrefix(v, "../") && v != ".." && !strings.HasPrefix(v, ".") && !strings.Contains(v, "/.")
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) revokeIdentity(w http.ResponseWriter, r *http.Request, publisher bool) {
	_, admin := s.actor(r)
	if !admin {
		http.Error(w, "admin required", http.StatusUnauthorized)
		return
	}
	prefix := "/api/admin/bots/"
	if publisher {
		prefix = "/api/admin/publishers/"
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, prefix), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	updated, err := s.identities.revoke(id, publisher)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !updated {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIdentities(w http.ResponseWriter, r *http.Request, publisher bool) {
	_, admin := s.actor(r)
	if !admin {
		http.Error(w, "admin required", http.StatusUnauthorized)
		return
	}
	identities, err := s.identities.list(publisher)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, identities)
}

func (s *Server) createIdentity(w http.ResponseWriter, r *http.Request, publisher bool) {
	_, admin := s.actor(r)
	if !admin {
		http.Error(w, "admin required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Name      string `json:"name"`
		SiteScope string `json:"site_scope"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if publisher {
		input.SiteScope = publisherScope
	} else if input.SiteScope == "" {
		input.SiteScope = input.Name
	}
	created, err := s.identities.create(input.Name, input.SiteScope)
	if err != nil {
		http.Error(w, "invalid or duplicate identity", http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusCreated, created)
}
func newSlug() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (s *Server) site(name string) (Site, error) {
	b, err := os.ReadFile(filepath.Join(s.cfg.Dir, name, "site.json"))
	if err != nil {
		return Site{}, err
	}
	var site Site
	err = json.Unmarshal(b, &site)
	return site, err
}
func (s *Server) saveSite(site Site) error {
	dir := filepath.Join(s.cfg.Dir, site.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, _ := json.Marshal(site)
	tmp, err := os.CreateTemp(dir, ".site-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "site.json"))
}

type publication struct {
	Site       string `json:"site"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	Visibility string `json:"visibility"`
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request, actor string, admin bool) {
	if actor == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var in publication
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		http.Error(w, "invalid JSON or exceeds 8 MiB", 400)
		return
	}
	if !safeName(in.Site) || !safePath(in.Path) || strings.HasSuffix(in.Path, ".previous") || unsafeDocumentPath(in.Path) || in.Path == "site.json" || !mayWrite(actor, admin, in.Site) {
		http.Error(w, "invalid site or path", 403)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.site(in.Site)
	if os.IsNotExist(err) {
		if in.Visibility == "" {
			in.Visibility = "unlisted"
		}
		if in.Visibility != "unlisted" && in.Visibility != "protected" || (!admin && in.Visibility != "unlisted") {
			http.Error(w, "visibility requires admin", 403)
			return
		}
		slug, e := newSlug()
		if e != nil {
			http.Error(w, "server error", 500)
			return
		}
		site = Site{Name: in.Site, Slug: slug, Visibility: in.Visibility}
		if e = s.saveSite(site); e != nil {
			http.Error(w, "server error", 500)
			return
		}
	} else if err != nil {
		http.Error(w, "server error", 500)
		return
	} else if isPublisher(actor) && site.Visibility != "unlisted" {
		http.Error(w, "protected site requires admin", 403)
		return
	} else if in.Visibility != "" && in.Visibility != site.Visibility {
		http.Error(w, "visibility cannot change with publication", 403)
		return
	}
	if err := EnsureSiteDB(s.cfg.Dir, in.Site); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	target := filepath.Join(s.cfg.Dir, in.Site, site.Slug, filepath.FromSlash(in.Path))
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".upload-")
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err = io.WriteString(tmp, in.Content); err != nil {
		tmp.Close()
		http.Error(w, "server error", 500)
		return
	}
	if err = tmp.Close(); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	if !s.withinQuota(in.Site, target, int64(len(in.Content))) {
		http.Error(w, "site quota exceeded", 413)
		return
	}
	if err = backup(target); err != nil {
		http.Error(w, "backup failed", 500)
		return
	}
	if err = os.Rename(tmp.Name(), target); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	audit(r, "publish", actor, site.Name, in.Path)
	respond(w, 201, map[string]string{"site": site.Name, "slug": site.Slug, "url": s.pageURL(site.Slug, in.Path)})
}
func (s *Server) pageURL(slug, file string) string {
	if s.cfg.ContentHost != "" {
		if s.cfg.AdminHost == s.cfg.ContentHost {
			return "https://" + s.cfg.ContentHost + "/p/" + slug + "/" + file
		}
		return "https://" + slug + "." + s.cfg.ContentHost + "/p/" + slug + "/" + file
	}
	return "/p/" + slug + "/" + file
}
func backup(target string) error {
	b, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".previous-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target+".previous")
}
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	a, admin := s.actor(r)
	var in struct {
		Site string `json:"site"`
		Path string `json:"path"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if a == "" || json.NewDecoder(r.Body).Decode(&in) != nil || !safeName(in.Site) || !safePath(in.Path) || strings.HasSuffix(in.Path, ".previous") || !mayWrite(a, admin, in.Site) {
		http.Error(w, "invalid rollback request", 403)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.site(in.Site)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if isPublisher(a) && site.Visibility != "unlisted" {
		http.Error(w, "protected site requires admin", 403)
		return
	}
	target := filepath.Join(s.cfg.Dir, in.Site, site.Slug, filepath.FromSlash(in.Path))
	if _, err := os.Stat(target + ".previous"); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := os.Rename(target+".previous", target); err != nil {
		http.Error(w, "rollback failed", 500)
		return
	}
	audit(r, "rollback", a, site.Name, in.Path)
	respond(w, 200, map[string]string{"url": s.pageURL(site.Slug, in.Path)})
}
func (s *Server) uploadAsset(w http.ResponseWriter, r *http.Request) {
	actor, admin := s.actor(r)
	if actor == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/assets/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || !safeName(parts[0]) || !safePath(parts[1]) || strings.HasSuffix(parts[1], ".previous") || unsafeDocumentPath(parts[1]) || !mayWrite(actor, admin, parts[0]) {
		http.Error(w, "invalid site or path", 403)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.site(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if isPublisher(actor) && site.Visibility != "unlisted" {
		http.Error(w, "protected site requires admin", 403)
		return
	}
	target := filepath.Join(s.cfg.Dir, site.Name, site.Slug, filepath.FromSlash(parts[1]))
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".asset-")
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	defer os.Remove(tmp.Name())
	r.Body = http.MaxBytesReader(w, r.Body, maxAssetBytes)
	_, err = io.Copy(tmp, r.Body)
	if err != nil {
		tmp.Close()
		http.Error(w, "invalid upload or exceeds 32 MiB", 413)
		return
	}
	if err = tmp.Close(); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	st, err := os.Stat(tmp.Name())
	if err != nil || !s.withinQuota(site.Name, target, st.Size()) {
		http.Error(w, "site quota exceeded", 413)
		return
	}
	if err = backup(target); err != nil {
		http.Error(w, "backup failed", 500)
		return
	}
	if err = os.Rename(tmp.Name(), target); err != nil {
		http.Error(w, "server error", 500)
		return
	}
	audit(r, "asset", actor, site.Name, parts[1])
	respond(w, 201, map[string]string{"url": s.pageURL(site.Slug, parts[1])})
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	actor, admin := s.actor(r)
	if actor == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	out := []Site{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		site, err := s.site(e.Name())
		if err != nil {
			continue
		}
		if admin || e.Name() == actor || (isPublisher(actor) && site.Visibility == "unlisted") {
			out = append(out, site)
		}
	}
	respond(w, 200, out)
}
func (s *Server) servePage(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/p/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || len(parts[0]) != 36 || !safePath(parts[1]) || strings.HasSuffix(parts[1], ".previous") || unsafeDocumentPath(parts[1]) {
		http.NotFound(w, r)
		return
	}
	slug, file := parts[0], parts[1]
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		site, err := s.site(e.Name())
		if err != nil || site.Slug != slug {
			continue
		}
		if site.Visibility == "protected" && !s.authorizedReader(r, site.Name) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BotPages-`+site.Slug+`"`)
			http.Error(w, "authentication required", 401)
			return
		}
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts; frame-ancestors 'none'")
		w.Header().Set("Origin-Agent-Cluster", "?1")
		if typ := mime.TypeByExtension(path.Ext(file)); typ != "" {
			w.Header().Set("Content-Type", typ)
		}
		f, openErr := os.Open(filepath.Join(s.cfg.Dir, e.Name(), slug, filepath.FromSlash(file)))
		if openErr != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, statErr := f.Stat()
		if statErr != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, file, st.ModTime(), f)
		return
	}
	http.NotFound(w, r)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == "GET" {
		respond(w, 200, map[string]string{"status": "ok", "version": version})
		return
	}
	if s.cfg.AdminHost != "" && s.cfg.ContentHost != "" {
		h := strings.ToLower(strings.Split(r.Host, ":")[0])
		if strings.HasPrefix(r.URL.Path, "/p/") {
			slug := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/p/"), "/", 2)[0]
			if len(slug) != 36 || (s.cfg.AdminHost != s.cfg.ContentHost && h != slug+"."+s.cfg.ContentHost) || (s.cfg.AdminHost == s.cfg.ContentHost && h != s.cfg.AdminHost) {
				http.NotFound(w, r)
				return
			}
		} else if h != s.cfg.AdminHost {
			http.NotFound(w, r)
			return
		}
	}
	if (r.Method == "POST" || r.Method == "PUT") && r.URL.Path != "/mcp" && !s.allowWrite(r) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", 429)
		return
	}
	switch {
	case r.URL.Path == "/health" && r.Method == "GET":
		respond(w, 200, map[string]string{"status": "ok", "version": version})
	case r.URL.Path == "/api/publish" && r.Method == "POST":
		a, admin := s.actor(r)
		s.publish(w, r, a, admin)
	case r.URL.Path == "/api/sites" && r.Method == "GET":
		s.list(w, r)
	case r.URL.Path == "/api/admin/bots" && r.Method == "GET":
		s.listIdentities(w, r, false)
	case r.URL.Path == "/api/admin/bots" && r.Method == "POST":
		s.createIdentity(w, r, false)
	case strings.HasPrefix(r.URL.Path, "/api/admin/bots/") && r.Method == "DELETE":
		s.revokeIdentity(w, r, false)
	case r.URL.Path == "/api/admin/publishers" && r.Method == "GET":
		s.listIdentities(w, r, true)
	case r.URL.Path == "/api/admin/publishers" && r.Method == "POST":
		s.createIdentity(w, r, true)
	case strings.HasPrefix(r.URL.Path, "/api/admin/publishers/") && r.Method == "DELETE":
		s.revokeIdentity(w, r, true)
	case r.URL.Path == "/api/rollback" && r.Method == "POST":
		s.rollback(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/assets/") && r.Method == "PUT":
		s.uploadAsset(w, r)
	case r.URL.Path == "/mcp" && r.Method == "POST":
		s.mcp(w, r)
	case strings.HasPrefix(r.URL.Path, "/p/") && (r.Method == "GET" || r.Method == "HEAD"):
		s.servePage(w, r)
	case strings.HasPrefix(r.URL.Path, "/admin/") && r.Method == "GET" && s.cfg.AdminDir != "":
		http.StripPrefix("/admin/", http.FileServer(http.Dir(s.cfg.AdminDir))).ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) actorAdmin(r *http.Request) (string, bool) { a, admin := s.actor(r); return a, admin }
func loadConfig() Config {
	bots := map[string]string{}
	readers := map[string]map[string]string{}
	publishers := map[string]string{}
	_ = json.Unmarshal([]byte(os.Getenv("BOTPAGES_BOT_TOKENS")), &bots)
	_ = json.Unmarshal([]byte(os.Getenv("BOTPAGES_PUBLISHER_TOKENS")), &publishers)
	_ = json.Unmarshal([]byte(os.Getenv("BOTPAGES_READERS")), &readers)
	return Config{Dir: os.Getenv("BOTPAGES_DATA"), AdminToken: os.Getenv("BOTPAGES_ADMIN_TOKEN"), Bots: bots, Publishers: publishers, Readers: readers, AdminDir: os.Getenv("BOTPAGES_ADMIN_DIR"), AdminHost: os.Getenv("BOTPAGES_ADMIN_HOST"), ContentHost: os.Getenv("BOTPAGES_CONTENT_HOST")}
}
func main() {
	c := loadConfig()
	s, err := NewServer(c)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("BOTPAGES_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	fmt.Println("BotPages", version, "listening on", addr)
	log.Fatal(http.ListenAndServe(addr, s))
}
