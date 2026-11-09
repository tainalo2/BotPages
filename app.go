package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const (
	defaultAppTimeoutMS = 5000
	maxAppTimeoutMS     = 10000
	defaultAppMemoryMB  = 32
	maxAppMemoryMB      = 64
)

type AppRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type AppManifest struct {
	Name             string     `json:"name"`
	Permissions      []string   `json:"permissions,omitempty"`
	NetworkAllowlist []string   `json:"network_allowlist,omitempty"`
	Routes           []AppRoute `json:"routes"`
	TimeoutMS        int        `json:"timeout_ms,omitempty"`
	MemoryMB         int        `json:"memory_mb,omitempty"`
}

var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

var deniedOutboundPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

func (m AppManifest) timeout() time.Duration { return time.Duration(m.TimeoutMS) * time.Millisecond }

func validateAppManifest(m *AppManifest) error {
	if m == nil || !safeName(m.Name) {
		return errors.New("invalid app name")
	}
	if m.TimeoutMS == 0 {
		m.TimeoutMS = defaultAppTimeoutMS
	}
	if m.TimeoutMS < 1 || m.TimeoutMS > maxAppTimeoutMS {
		return errors.New("timeout_ms must be between 1 and 10000")
	}
	if m.MemoryMB == 0 {
		m.MemoryMB = defaultAppMemoryMB
	}
	if m.MemoryMB < 1 || m.MemoryMB > maxAppMemoryMB {
		return errors.New("memory_mb must be between 1 and 64")
	}
	if len(m.Routes) == 0 || len(m.Routes) > 32 {
		return errors.New("between 1 and 32 routes required")
	}
	seenRoutes := make(map[string]struct{}, len(m.Routes))
	for i := range m.Routes {
		m.Routes[i].Method = strings.ToUpper(m.Routes[i].Method)
		switch m.Routes[i].Method {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			return errors.New("unsupported route method")
		}
		p := m.Routes[i].Path
		if p == "" || len(p) > 200 || !strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "//") || path.Clean(p) != p {
			return errors.New("invalid route path")
		}
		key := m.Routes[i].Method + " " + p
		if _, ok := seenRoutes[key]; ok {
			return errors.New("duplicate route")
		}
		seenRoutes[key] = struct{}{}
	}
	seenPermissions := map[string]struct{}{}
	for _, permission := range m.Permissions {
		if permission != "fs:read" {
			return errors.New("unsupported permission")
		}
		if _, ok := seenPermissions[permission]; ok {
			return errors.New("duplicate permission")
		}
		seenPermissions[permission] = struct{}{}
	}
	seenDomains := map[string]struct{}{}
	for i, domain := range m.NetworkAllowlist {
		domain = strings.ToLower(strings.TrimSuffix(domain, "."))
		if len(domain) > 253 || !domainPattern.MatchString(domain) || strings.Contains(domain, "..") || net.ParseIP(domain) != nil || domain == "localhost" {
			return errors.New("network_allowlist entries must be DNS hostnames")
		}
		if _, ok := seenDomains[domain]; ok {
			return errors.New("duplicate network domain")
		}
		seenDomains[domain] = struct{}{}
		m.NetworkAllowlist[i] = domain
	}
	return nil
}

type appPublication struct {
	Site     string      `json:"site"`
	Manifest AppManifest `json:"manifest"`
	Entry    string      `json:"entry"`
}

func (s *Server) publishApp(w http.ResponseWriter, r *http.Request, actor string, admin bool) {
	if actor == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	var in appPublication
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid app package or exceeds 4 MiB", http.StatusBadRequest)
		return
	}
	if !safeName(in.Site) || !mayWrite(actor, admin, in.Site) {
		http.Error(w, "invalid site", http.StatusForbidden)
		return
	}
	if len(in.Entry) == 0 || len(in.Entry) > 2<<20 {
		http.Error(w, "entry.js is required and limited to 2 MiB", http.StatusBadRequest)
		return
	}
	if err := validateAppManifest(&in.Manifest); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := goja.Compile("entry.js", in.Entry, true); err != nil {
		http.Error(w, "invalid entry.js: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.site(in.Site)
	if os.IsNotExist(err) {
		if !admin && isPublisher(actor) || !admin && actor == in.Site {
			site, err = s.createAppSite(in.Site)
		} else if admin {
			site, err = s.createAppSite(in.Site)
		}
	} else if err == nil && isPublisher(actor) && site.Visibility != "unlisted" {
		http.Error(w, "protected site requires admin", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	appsDir := filepath.Join(s.cfg.Dir, site.Name, ".apps")
	versionsDir := filepath.Join(s.cfg.Dir, site.Name, ".app-versions", in.Manifest.Name)
	if err := os.MkdirAll(appsDir, 0700); err != nil || os.MkdirAll(versionsDir, 0700) != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	tmp, err := os.MkdirTemp(versionsDir, ".publish-")
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	manifestJSON, err := json.Marshal(in.Manifest)
	if err != nil || writeSyncedFile(filepath.Join(tmp, "manifest.json"), manifestJSON) != nil || writeSyncedFile(filepath.Join(tmp, "entry.js"), []byte(in.Entry)) != nil || syncDir(tmp) != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	target := filepath.Join(appsDir, in.Manifest.Name)
	if !s.withinAppQuota(site.Name, target, tmp, int64(len(manifestJSON)+len(in.Entry))) {
		http.Error(w, "site quota exceeded", http.StatusRequestEntityTooLarge)
		return
	}
	approvalPath := filepath.Join(s.cfg.Dir, site.Name, ".app-approvals", in.Manifest.Name+".json")
	if err := os.Remove(approvalPath); err != nil {
		if !os.IsNotExist(err) {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	} else if syncDir(filepath.Dir(approvalPath)) != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if err := activateAppVersion(appsDir, versionsDir, target, tmp); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	audit(r, "publish_app", actor, site.Name, in.Manifest.Name)
	respond(w, http.StatusCreated, map[string]string{
		"site": site.Name,
		"slug": site.Slug,
		"app":  in.Manifest.Name,
		"url":  "/a/" + site.Slug + "/" + in.Manifest.Name,
	})
}

func writeSyncedFile(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncDir(name string) error {
	dir, err := os.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func activateAppVersion(appsDir, versionsDir, target, staged string) error {
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("existing app is not a version pointer")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	oldVersion, _ := filepath.EvalSymlinks(target)
	ready := staged + ".ready"
	if err := os.Rename(staged, ready); err != nil {
		return err
	}
	if err := syncDir(versionsDir); err != nil {
		return err
	}
	pointer, err := os.CreateTemp(appsDir, ".activate-")
	if err != nil {
		return err
	}
	pointerName := pointer.Name()
	if err := pointer.Close(); err != nil {
		return err
	}
	if err := os.Remove(pointerName); err != nil {
		return err
	}
	relative, err := filepath.Rel(appsDir, ready)
	if err != nil {
		return err
	}
	if err := os.Symlink(relative, pointerName); err != nil {
		return err
	}
	if err := os.Rename(pointerName, target); err != nil {
		_ = os.Remove(pointerName)
		return err
	}
	if err := syncDir(appsDir); err != nil {
		return err
	}
	if oldVersion != "" && pathWithin(versionsDir, oldVersion) && oldVersion != ready {
		_ = os.RemoveAll(oldVersion)
		_ = syncDir(versionsDir)
	}
	return nil
}

func (s *Server) withinAppQuota(siteName, target, staged string, incoming int64) bool {
	root := filepath.Join(s.cfg.Dir, siteName)
	resolvedTarget, _ := filepath.EvalSymlinks(target)
	var total int64
	err := filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if current != root && ((resolvedTarget != "" && pathWithin(resolvedTarget, current)) || pathWithin(staged, current) || strings.HasPrefix(info.Name(), ".publish-")) {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return err == nil && total+incoming <= maxSiteBytes
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Server) createAppSite(name string) (Site, error) {
	slug, err := newSlug()
	if err != nil {
		return Site{}, err
	}
	site := Site{Name: name, Slug: slug, Visibility: "unlisted"}
	if err := s.saveSite(site); err != nil {
		return Site{}, err
	}
	return site, nil
}

type networkApproval struct {
	Site    string   `json:"site"`
	App     string   `json:"app"`
	Domains []string `json:"domains"`
}

func (s *Server) approveAppNetwork(w http.ResponseWriter, r *http.Request) {
	_, admin := s.actor(r)
	if !admin {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var approval networkApproval
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&approval) != nil || decoder.Decode(&struct{}{}) != io.EOF || !safeName(approval.Site) || !safeName(approval.App) {
		http.Error(w, "invalid network approval", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.site(approval.Site)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	manifestBytes, err := os.ReadFile(filepath.Join(s.cfg.Dir, site.Name, ".apps", approval.App, "manifest.json"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var manifest AppManifest
	if json.Unmarshal(manifestBytes, &manifest) != nil || validateAppManifest(&manifest) != nil {
		http.Error(w, "invalid stored manifest", http.StatusInternalServerError)
		return
	}
	declared := make(map[string]bool, len(manifest.NetworkAllowlist))
	for _, domain := range manifest.NetworkAllowlist {
		declared[domain] = true
	}
	seen := map[string]bool{}
	for i, domain := range approval.Domains {
		domain = strings.ToLower(strings.TrimSuffix(domain, "."))
		if !declared[domain] || seen[domain] {
			http.Error(w, "approval contains undeclared or duplicate domain", http.StatusBadRequest)
			return
		}
		seen[domain] = true
		approval.Domains[i] = domain
	}
	dir := filepath.Join(s.cfg.Dir, site.Name, ".app-approvals")
	if os.MkdirAll(dir, 0700) != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	data, _ := json.Marshal(approval.Domains)
	tmp, err := os.CreateTemp(dir, ".approval-")
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	writeErr := tmp.Chmod(0600)
	if writeErr == nil {
		_, writeErr = tmp.Write(data)
	}
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(tmp.Name(), filepath.Join(dir, approval.App+".json"))
	}
	if writeErr == nil {
		writeErr = syncDir(dir)
	}
	if writeErr != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	audit(r, "approve_app_network", "admin", site.Name, approval.App)
	respond(w, http.StatusOK, approval)
}

func (s *Server) approvedDomains(site Site, manifest AppManifest) map[string]bool {
	data, err := os.ReadFile(filepath.Join(s.cfg.Dir, site.Name, ".app-approvals", manifest.Name+".json"))
	if err != nil {
		return map[string]bool{}
	}
	var approved []string
	if json.Unmarshal(data, &approved) != nil {
		return map[string]bool{}
	}
	declared := make(map[string]bool, len(manifest.NetworkAllowlist))
	for _, domain := range manifest.NetworkAllowlist {
		declared[domain] = true
	}
	result := make(map[string]bool)
	for _, domain := range approved {
		if declared[domain] {
			result[domain] = true
		}
	}
	return result
}

type loadedApp struct {
	site     Site
	manifest AppManifest
	entry    string
}

type appHalt struct{ reason string }

func (h appHalt) Error() string { return h.reason }

type appResponse struct {
	status      int
	contentType string
	body        string
}

func (s *Server) serveApp(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/a/"), "/", 3)
	if len(parts) != 3 || len(parts[0]) != 36 || !safeName(parts[1]) {
		http.NotFound(w, r)
		return
	}
	routePath := "/" + parts[2]
	app, err := s.loadApp(parts[0], parts[1])
	if err != nil || !appHasRoute(app.manifest, r.Method, routePath) {
		http.NotFound(w, r)
		return
	}
	if app.site.Visibility == "protected" && !s.authorizedReader(r, app.site.Name) {
		w.Header().Set("WWW-Authenticate", `Basic realm="BotPages-`+app.site.Slug+`"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	body, err := readLimitedBody(r.Body, 1<<20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case s.appSlots <- struct{}{}:
		defer func() { <-s.appSlots }()
	default:
		http.Error(w, "app runtime capacity exhausted", http.StatusServiceUnavailable)
		return
	}
	response, halt, err := s.executeApp(app, r, routePath, string(body))
	if halt == "timeout" {
		http.Error(w, "app execution timed out", http.StatusGatewayTimeout)
		return
	}
	if halt == "memory" {
		http.Error(w, "app memory limit exceeded", http.StatusInsufficientStorage)
		return
	}
	if err != nil {
		http.Error(w, "app execution failed: "+boundedError(err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "sandbox; frame-ancestors 'none'")
	w.Header().Set("Origin-Agent-Cluster", "?1")
	w.Header().Set("Cache-Control", "no-store")
	if response.contentType != "" {
		w.Header().Set("Content-Type", response.contentType)
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(response.status)
	_, _ = io.WriteString(w, response.body)
}

func (s *Server) loadApp(slug, name string) (loadedApp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return loadedApp{}, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		site, err := s.site(entry.Name())
		if err != nil || site.Slug != slug {
			continue
		}
		root := filepath.Join(s.cfg.Dir, site.Name, ".apps", name)
		manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
		if err != nil {
			return loadedApp{}, err
		}
		entryBytes, err := os.ReadFile(filepath.Join(root, "entry.js"))
		if err != nil {
			return loadedApp{}, err
		}
		var manifest AppManifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return loadedApp{}, err
		}
		if err := validateAppManifest(&manifest); err != nil {
			return loadedApp{}, err
		}
		return loadedApp{site: site, manifest: manifest, entry: string(entryBytes)}, nil
	}
	return loadedApp{}, os.ErrNotExist
}

func appHasRoute(manifest AppManifest, method, routePath string) bool {
	for _, route := range manifest.Routes {
		if route.Method == method && route.Path == routePath {
			return true
		}
	}
	return false
}

func (s *Server) executeApp(app loadedApp, r *http.Request, routePath, body string) (response appResponse, halt string, err error) {
	vm := goja.New()
	vm.SetMaxCallStackSize(1024)
	executionContext, cancelExecution := context.WithTimeout(r.Context(), app.manifest.timeout())
	defer cancelExecution()
	permissions := make(map[string]bool, len(app.manifest.Permissions))
	for _, permission := range app.manifest.Permissions {
		permissions[permission] = true
	}
	if err := vm.Set("readFile", func(call goja.FunctionCall) goja.Value {
		if !permissions["fs:read"] {
			panic(vm.NewGoError(errors.New("filesystem access denied")))
		}
		contents, err := s.readSiteFile(app.site, call.Argument(0).String())
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(string(contents))
	}); err != nil {
		return response, "", err
	}
	approved := s.approvedDomains(app.site, app.manifest)
	if err := vm.Set("fetch", func(call goja.FunctionCall) goja.Value {
		value, err := s.fetchForApp(executionContext, vm, approved, call)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return value
	}); err != nil {
		return response, "", err
	}

	var haltMu sync.Mutex
	setHalt := func(reason string) {
		haltMu.Lock()
		if halt == "" {
			halt = reason
			vm.Interrupt(appHalt{reason: reason})
		}
		haltMu.Unlock()
	}
	stop := make(chan struct{})
	var watchers sync.WaitGroup
	watchers.Add(1)
	go func() {
		defer watchers.Done()
		timer := time.NewTimer(app.manifest.timeout())
		defer timer.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		baseline := stats.HeapAlloc
		limit := uint64(app.manifest.MemoryMB) << 20
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				setHalt("timeout")
				return
			case <-ticker.C:
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > baseline+limit {
					setHalt("memory")
					return
				}
			}
		}
	}()
	cleanup := func() {
		close(stop)
		watchers.Wait()
	}

	var value goja.Value
	var runErr error
	exception := vm.Try(func() {
		_, runErr = vm.RunString(app.entry)
		if runErr != nil {
			return
		}
		handler, ok := goja.AssertFunction(vm.Get("handle"))
		if !ok {
			runErr = errors.New("entry.js must define handle(request)")
			return
		}
		headers := make(map[string]string)
		for key, values := range r.Header {
			if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Cookie") {
				continue
			}
			headers[key] = strings.Join(values, ", ")
		}
		value, runErr = handler(goja.Undefined(), vm.ToValue(map[string]any{
			"method":  r.Method,
			"path":    routePath,
			"query":   r.URL.Query(),
			"headers": headers,
			"body":    body,
		}))
		if runErr != nil {
			return
		}
		if _, ok := value.(*goja.Object); !ok || value.ExportType() == reflect.TypeOf((*goja.Promise)(nil)) {
			runErr = errors.New("handle(request) must return a synchronous response object")
			return
		}
		object := value.ToObject(vm)
		statusValue := object.Get("status")
		response.status = http.StatusOK
		if statusValue != nil && !goja.IsUndefined(statusValue) {
			response.status = int(statusValue.ToInteger())
		}
		if response.status < 200 || response.status > 599 {
			runErr = errors.New("response status must be between 200 and 599")
			return
		}
		bodyValue := object.Get("body")
		if bodyValue != nil && !goja.IsUndefined(bodyValue) && !goja.IsNull(bodyValue) {
			response.body = bodyValue.String()
		}
		if len(response.body) > 1<<20 {
			runErr = errors.New("response body exceeds 1 MiB")
			return
		}
		headersValue := object.Get("headers")
		if headersValue != nil && !goja.IsUndefined(headersValue) && !goja.IsNull(headersValue) {
			headersObject := headersValue.ToObject(vm)
			for _, key := range headersObject.Keys() {
				if !strings.EqualFold(key, "Content-Type") {
					continue
				}
				contentType := headersObject.Get(key)
				if contentType != nil && !goja.IsUndefined(contentType) && !goja.IsNull(contentType) {
					response.contentType = contentType.String()
					if len(response.contentType) > 200 || strings.ContainsAny(response.contentType, "\r\n") {
						runErr = errors.New("invalid Content-Type response header")
					}
				}
				break
			}
		}
	})
	if exception != nil {
		runErr = exception
	}
	cleanup()
	haltMu.Lock()
	defer haltMu.Unlock()
	if halt == "" && errors.Is(executionContext.Err(), context.DeadlineExceeded) {
		halt = "timeout"
	}
	if halt != "" {
		return response, halt, runErr
	}
	return response, "", runErr
}

func (s *Server) fetchForApp(ctx context.Context, vm *goja.Runtime, approved map[string]bool, call goja.FunctionCall) (goja.Value, error) {
	rawURL := call.Argument(0).String()
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return nil, errors.New("network access denied: fetch requires an https URL without credentials")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if !approved[host] {
		return nil, fmt.Errorf("network access denied: domain %q is not admin-approved", host)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	client := *s.appClient
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirectHost := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
		if len(via) >= 5 || req.URL.Scheme != "https" || !approved[redirectHost] || (req.URL.Port() != "" && req.URL.Port() != "443") {
			return errors.New("network redirect denied")
		}
		return nil
	}
	out, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer out.Body.Close()
	finalHost := strings.ToLower(strings.TrimSuffix(out.Request.URL.Hostname(), "."))
	if !approved[finalHost] {
		return nil, errors.New("network redirect denied")
	}
	body, err := readLimitedBody(out.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("fetch response: %w", err)
	}
	return vm.ToValue(map[string]any{
		"status":  out.StatusCode,
		"body":    string(body),
		"headers": map[string]string{"Content-Type": out.Header.Get("Content-Type")},
	}), nil
}

func secureAppHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, candidate := range addresses {
			if !publicOutboundIP(candidate.IP) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		}
		return nil, errors.New("network access denied: domain has no public address")
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func publicOutboundIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range deniedOutboundPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func (s *Server) readSiteFile(site Site, relative string) ([]byte, error) {
	if !safePath(relative) {
		return nil, errors.New("filesystem access denied: invalid path")
	}
	root, err := os.OpenRoot(filepath.Join(s.cfg.Dir, site.Name, site.Slug))
	if err != nil {
		return nil, errors.New("filesystem access denied")
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(relative))
	if err != nil {
		return nil, errors.New("filesystem access denied: file cannot be opened safely")
	}
	defer file.Close()
	return readLimitedBody(file, 1<<20)
}

func boundedError(err error) string {
	message := err.Error()
	if len(message) > 500 {
		return message[:500]
	}
	return message
}

func readLimitedBody(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}
