package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateAppManifestDefaultsAndRejectsUnsafeValues(t *testing.T) {
	m := AppManifest{Name: "hello", Routes: []AppRoute{{Method: "POST", Path: "/echo"}}}
	if err := validateAppManifest(&m); err != nil {
		t.Fatal(err)
	}
	if m.TimeoutMS != 5000 || m.MemoryMB != 32 {
		t.Fatalf("defaults timeout=%d memory=%d", m.TimeoutMS, m.MemoryMB)
	}
	if got := m.timeout(); got != 5*time.Second {
		t.Fatalf("timeout = %s", got)
	}

	bad := []AppManifest{
		{Name: "../escape", Routes: []AppRoute{{Method: "GET", Path: "/"}}},
		{Name: "ok", Routes: []AppRoute{{Method: "TRACE", Path: "/"}}},
		{Name: "ok", Routes: []AppRoute{{Method: "GET", Path: "/../secret"}}},
		{Name: "ok", Routes: []AppRoute{{Method: "GET", Path: "/"}}, TimeoutMS: 10001},
		{Name: "ok", Routes: []AppRoute{{Method: "GET", Path: "/"}}, NetworkAllowlist: []string{"https://example.com/path"}},
	}
	for _, candidate := range bad {
		if err := validateAppManifest(&candidate); err == nil {
			t.Fatalf("accepted invalid manifest: %+v", candidate)
		}
	}
}

func appPayload(site, name, entry string, domains []string) []byte {
	b, _ := json.Marshal(map[string]any{
		"site": site,
		"manifest": AppManifest{
			Name:             name,
			Routes:           []AppRoute{{Method: "POST", Path: "/echo"}},
			NetworkAllowlist: domains,
		},
		"entry": entry,
	})
	return b
}

func TestPublishAppPersistsValidatedPackageAndEnforcesSiteAuthorization(t *testing.T) {
	s := setupPublisher(t)
	defer s.Close()

	entry := `function handle(request) { return {status: 200, body: request.body}; }`
	w := call(s, http.MethodPost, "/api/apps", "bot-secret", appPayload("newser", "echo", entry, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("publish app: %d %s", w.Code, w.Body.String())
	}
	var published map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	if published["url"] == "" || published["slug"] == "" {
		t.Fatalf("missing publication URL: %v", published)
	}
	root := filepath.Join(s.cfg.Dir, "newser", ".apps", "echo")
	if info, err := os.Lstat(root); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("active app must be an atomic version pointer: mode=%v err=%v", func() os.FileMode {
			if info == nil {
				return 0
			}
			return info.Mode()
		}(), err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestBytes), `"timeout_ms":5000`) {
		t.Fatalf("manifest defaults not persisted: %s", manifestBytes)
	}
	if got, err := os.ReadFile(filepath.Join(root, "entry.js")); err != nil || string(got) != entry {
		t.Fatalf("entry persisted = %q, %v", got, err)
	}

	if denied := call(s, http.MethodPost, "/api/apps", "bot-secret", appPayload("other", "echo", entry, nil)); denied.Code != http.StatusForbidden {
		t.Fatalf("cross-site bot publication = %d %s", denied.Code, denied.Body.String())
	}
	if publisher := call(s, http.MethodPost, "/api/apps", "pub-secret", appPayload("publisher-site", "echo", entry, nil)); publisher.Code != http.StatusCreated {
		t.Fatalf("publisher app: %d %s", publisher.Code, publisher.Body.String())
	}
}

func TestAppExecutionConcurrencyIsBounded(t *testing.T) {
	s, err := NewServer(Config{
		Dir:               t.TempDir(),
		AdminToken:        "admin-secret",
		Bots:              map[string]string{"newser": "bot-secret"},
		MaxConcurrentApps: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loop := publishTestApp(t, s, AppManifest{Name: "bounded", TimeoutMS: 200, Routes: []AppRoute{{Method: "GET", Path: "/run"}}}, `function handle(){ while (true) {} }`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(s, http.MethodGet, loop+"/run", "", nil)
	}()
	time.Sleep(20 * time.Millisecond)
	w := call(s, http.MethodGet, loop+"/run", "", nil)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "capacity") {
		t.Fatalf("saturated runtime: %d %q", w.Code, w.Body.String())
	}
	<-done
}

func TestAppExecutionConcurrencyReservesCapacityAcrossSites(t *testing.T) {
	s, err := NewServer(Config{
		Dir:               t.TempDir(),
		AdminToken:        "admin-secret",
		Bots:              map[string]string{"alpha": "alpha-secret", "beta": "beta-secret"},
		MaxConcurrentApps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	alpha := publishTestAppForSite(t, s, "alpha", "alpha-secret", AppManifest{Name: "slow", TimeoutMS: 250, Routes: []AppRoute{{Method: "GET", Path: "/run"}}}, `function handle(){ while (true) {} }`)
	beta := publishTestAppForSite(t, s, "beta", "beta-secret", AppManifest{Name: "fast", TimeoutMS: 250, Routes: []AppRoute{{Method: "GET", Path: "/run"}}}, `function handle(){ return {body: "beta-ok"}; }`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		call(s, http.MethodGet, alpha+"/run", "", nil)
	}()
	time.Sleep(20 * time.Millisecond)
	secondAlpha := call(s, http.MethodGet, alpha+"/run", "", nil)
	if secondAlpha.Code != http.StatusServiceUnavailable || !strings.Contains(secondAlpha.Body.String(), "capacity") {
		t.Fatalf("site monopolized global slots: %d %q", secondAlpha.Code, secondAlpha.Body.String())
	}
	betaResponse := call(s, http.MethodGet, beta+"/run", "", nil)
	if betaResponse.Code != http.StatusOK || betaResponse.Body.String() != "beta-ok" {
		t.Fatalf("other site could not use reserved capacity: %d %q", betaResponse.Code, betaResponse.Body.String())
	}
	<-done
}

func TestAppExecutionRejectsPerSiteLimitThatCanMonopolizeGlobalSlots(t *testing.T) {
	s, err := NewServer(Config{
		Dir:                      t.TempDir(),
		AdminToken:               "admin-secret",
		MaxConcurrentApps:        4,
		MaxConcurrentAppsPerSite: 4,
	})
	if err == nil {
		_ = s.Close()
		t.Fatal("accepted a per-site limit that can consume every global slot")
	}
}

func TestAppMemoryWatchdogUsesFrequentSampling(t *testing.T) {
	if appMemorySampleInterval != time.Millisecond {
		t.Fatalf("memory sample interval = %s, want %s", appMemorySampleInterval, time.Millisecond)
	}
}

func publishTestApp(t *testing.T, s *Server, manifest AppManifest, entry string) string {
	t.Helper()
	return publishTestAppForSite(t, s, "newser", "bot-secret", manifest, entry)
}

func publishTestAppForSite(t *testing.T, s *Server, site, token string, manifest AppManifest, entry string) string {
	t.Helper()
	body, _ := json.Marshal(appPublication{Site: site, Manifest: manifest, Entry: entry})
	w := call(s, http.MethodPost, "/api/apps", token, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return "/a/" + out["slug"] + "/" + manifest.Name
}

func TestAppExecutionAndFilesystemIsolation(t *testing.T) {
	s := setup(t)
	defer s.Close()
	if w := publishAs(s, "bot-secret", "newser", "data.txt", "unlisted"); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}

	manifest := AppManifest{
		Name:        "runtime",
		Permissions: []string{"fs:read"},
		Routes:      []AppRoute{{Method: "POST", Path: "/echo"}, {Method: "GET", Path: "/file"}},
	}
	entry := `function handle(request) {
		if (request.path === "/file") return {status: 200, body: readFile("data.txt")};
		return {status: 201, headers: {"Content-Type": "text/plain"}, body: request.method + ":" + request.body};
	}`
	base := publishTestApp(t, s, manifest, entry)
	w := call(s, http.MethodPost, base+"/echo?x=1", "", []byte("hello"))
	if w.Code != http.StatusCreated || w.Body.String() != "POST:hello" || w.Header().Get("Content-Type") != "text/plain" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("execute: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	w = call(s, http.MethodGet, base+"/file", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "newser") {
		t.Fatalf("site file: %d %q", w.Code, w.Body.String())
	}

	site, err := s.site("newser")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.cfg.Dir, "identity.db"), filepath.Join(s.cfg.Dir, site.Name, site.Slug, "escape")); err != nil {
		t.Fatal(err)
	}
	denied := AppManifest{Name: "denied", Permissions: []string{"fs:read"}, Routes: []AppRoute{{Method: "GET", Path: "/file"}}}
	deniedBase := publishTestApp(t, s, denied, `function handle() { return {body: readFile("escape")}; }`)
	w = call(s, http.MethodGet, deniedBase+"/file", "", nil)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "filesystem access denied") {
		t.Fatalf("filesystem denial: %d %q", w.Code, w.Body.String())
	}
}

func TestInfiniteLoopTimesOutWithoutBlockingAnotherApp(t *testing.T) {
	s := setup(t)
	defer s.Close()
	loopManifest := AppManifest{Name: "loop", TimeoutMS: 50, Routes: []AppRoute{{Method: "GET", Path: "/run"}}}
	loopBase := publishTestApp(t, s, loopManifest, `function handle() { while (true) {} }`)
	fastManifest := AppManifest{Name: "fast", TimeoutMS: 500, Routes: []AppRoute{{Method: "GET", Path: "/run"}}}
	fastBase := publishTestApp(t, s, fastManifest, `function handle() { return {body: "ok"}; }`)

	var wg sync.WaitGroup
	wg.Add(1)
	loopResult := make(chan int, 1)
	go func() {
		defer wg.Done()
		loopResult <- call(s, http.MethodGet, loopBase+"/run", "", nil).Code
	}()
	time.Sleep(10 * time.Millisecond)
	start := time.Now()
	fast := call(s, http.MethodGet, fastBase+"/run", "", nil)
	if fast.Code != http.StatusOK || fast.Body.String() != "ok" || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("independent app blocked: %d %q after %s", fast.Code, fast.Body.String(), time.Since(start))
	}
	wg.Wait()
	if code := <-loopResult; code != http.StatusGatewayTimeout {
		t.Fatalf("loop status = %d", code)
	}
}

func TestMemoryWatchdogInterruptsAllocatingApp(t *testing.T) {
	s := setup(t)
	defer s.Close()
	manifest := AppManifest{Name: "memory", TimeoutMS: 1000, MemoryMB: 1, Routes: []AppRoute{{Method: "GET", Path: "/run"}}}
	base := publishTestApp(t, s, manifest, `function handle() { var values = []; while (true) { values.push("x".repeat(1024 * 1024)); } }`)
	start := time.Now()
	w := call(s, http.MethodGet, base+"/run", "", nil)
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), "memory limit") || time.Since(start) >= time.Second {
		t.Fatalf("memory watchdog: %d %q after %s", w.Code, w.Body.String(), time.Since(start))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMCPPublishesApp(t *testing.T) {
	s := setup(t)
	defer s.Close()
	listed := call(s, http.MethodPost, "/mcp", "bot-secret", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"publish_app"`) {
		t.Fatalf("tools/list: %d %s", listed.Code, listed.Body.String())
	}
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"publish_app","arguments":{"site":"newser","manifest":{"name":"mcp-app","routes":[{"method":"GET","path":"/run"}]},"entry":"function handle(){return {body:'mcp'}}"}}}`
	published := call(s, http.MethodPost, "/mcp", "bot-secret", []byte(body))
	if published.Code != http.StatusOK || strings.Contains(published.Body.String(), `"isError":true`) || !strings.Contains(published.Body.String(), "mcp-app") {
		t.Fatalf("publish_app: %d %s", published.Code, published.Body.String())
	}
}

func TestFetchRequiresAdminApprovedDeclaredDomain(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "api.example.test" {
			t.Fatalf("unexpected outbound host %q", r.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    r,
		}, nil
	})}
	s, err := NewServer(Config{
		Dir:           t.TempDir(),
		AdminToken:    "admin-secret",
		Bots:          map[string]string{"newser": "bot-secret"},
		AppHTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manifest := AppManifest{
		Name:             "network",
		NetworkAllowlist: []string{"api.example.test"},
		Routes:           []AppRoute{{Method: "GET", Path: "/run"}},
	}
	base := publishTestApp(t, s, manifest, `function handle() { var r = fetch("https://api.example.test/data"); return {body: r.body}; }`)
	w := call(s, http.MethodGet, base+"/run", "", nil)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "not admin-approved") {
		t.Fatalf("unapproved fetch: %d %q", w.Code, w.Body.String())
	}
	approval, _ := json.Marshal(map[string]any{"site": "newser", "app": "network", "domains": []string{"api.example.test"}})
	if denied := call(s, http.MethodPost, "/api/admin/apps/network", "bot-secret", approval); denied.Code != http.StatusUnauthorized {
		t.Fatalf("bot approved network: %d", denied.Code)
	}
	if approved := call(s, http.MethodPost, "/api/admin/apps/network", "admin-secret", approval); approved.Code != http.StatusOK {
		t.Fatalf("admin approval: %d %s", approved.Code, approved.Body.String())
	}
	w = call(s, http.MethodGet, base+"/run", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Fatalf("approved fetch: %d %q", w.Code, w.Body.String())
	}
	base = publishTestApp(t, s, manifest, `function handle() { var r = fetch("https://api.example.test/data"); return {body: r.body}; }`)
	w = call(s, http.MethodGet, base+"/run", "", nil)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "not admin-approved") {
		t.Fatalf("replacement retained approval: %d %q", w.Code, w.Body.String())
	}
}

func TestOutboundIPPolicyAndAppQuota(t *testing.T) {
	for _, raw := range []string{"0.0.0.1", "127.0.0.1", "10.0.0.1", "100.100.100.200", "169.254.169.254", "192.0.2.1", "192.88.99.1", "198.18.0.1", "203.0.113.1", "::1", "64:ff9b::a00:1", "64:ff9b:1::a00:1", "100::1", "2001::a00:1", "2001:10::1", "2001:20::1", "2001:db8::1", "2002:0a00:1::", "3fff::1", "5f00::1"} {
		if publicOutboundIP(net.ParseIP(raw)) {
			t.Errorf("special address allowed: %s", raw)
		}
	}
	if !publicOutboundIP(net.ParseIP("8.8.8.8")) || !publicOutboundIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("public address denied")
	}

	s := setup(t)
	defer s.Close()
	if w := publishAs(s, "bot-secret", "newser", "index.html", "unlisted"); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	filler := filepath.Join(s.cfg.Dir, "newser", "filler")
	file, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSiteBytes); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	w := call(s, http.MethodPost, "/api/apps", "bot-secret", appPayload("newser", "quota", `function handle(){return {body:"x"}}`, nil))
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "quota") {
		t.Fatalf("app bypassed site quota: %d %q", w.Code, w.Body.String())
	}
}
