package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func setupPublisher(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret",
		Bots:       map[string]string{"newser": "bot-secret"},
		Publishers: map[string]string{"publisher": "pub-secret"},
		Readers:    map[string]map[string]string{"private": {"alex": "reader-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func publishAs(s *Server, token, site, path, visibility string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"site": site, "path": path, "content": "<h1>" + site + "</h1>", "visibility": visibility})
	return call(s, "POST", "/api/publish", token, b)
}

func TestPublisherPublishesAnyUnlistedSite(t *testing.T) {
	s := setupPublisher(t)
	for _, site := range []string{"newser", "mailer", "infraer"} {
		if w := publishAs(s, "pub-secret", site, "index.html", ""); w.Code != 201 {
			t.Fatalf("publisher %s: %d %s", site, w.Code, w.Body.String())
		}
	}
	// Le site créé par un bot reste modifiable par le publisher, et inversement.
	if w := publishAs(s, "bot-secret", "newser", "b.html", ""); w.Code != 201 {
		t.Fatalf("bot on its site: %d", w.Code)
	}
	if w := publishAs(s, "bot-secret", "mailer", "x.html", ""); w.Code != 403 {
		t.Fatalf("bot token must stay scoped: %d", w.Code)
	}
	// Chaque site garde son propre slug.
	w := call(s, "GET", "/api/sites", "pub-secret", nil)
	var sites []Site
	json.Unmarshal(w.Body.Bytes(), &sites)
	slugs := map[string]bool{}
	for _, x := range sites {
		slugs[x.Slug] = true
	}
	if w.Code != 200 || len(sites) != 3 || len(slugs) != 3 {
		t.Fatalf("list %d %v", w.Code, sites)
	}
}

func TestPublisherCannotTouchProtectedSites(t *testing.T) {
	s := setupPublisher(t)
	if w := publishAs(s, "pub-secret", "secret-site", "index.html", "protected"); w.Code != 403 {
		t.Fatalf("publisher created protected site: %d", w.Code)
	}
	if w := publishAs(s, "admin-secret", "private", "index.html", "protected"); w.Code != 201 {
		t.Fatalf("admin protected: %d", w.Code)
	}
	if w := publishAs(s, "pub-secret", "private", "index.html", ""); w.Code != 403 {
		t.Fatalf("publisher overwrote protected page: %d", w.Code)
	}
	r := httptest.NewRequest("PUT", "/api/assets/private/x.png", bytes.NewReader([]byte{1, 2, 3}))
	r.Header.Set("Authorization", "Bearer pub-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("publisher uploaded to protected site: %d", w.Code)
	}
	if w := call(s, "POST", "/api/rollback", "pub-secret", []byte(`{"site":"private","path":"index.html"}`)); w.Code != 403 {
		t.Fatalf("publisher rolled back protected site: %d", w.Code)
	}
	w = call(s, "GET", "/api/sites", "pub-secret", nil)
	if strings.Contains(w.Body.String(), `"private"`) {
		t.Fatalf("protected site listed to publisher: %s", w.Body.String())
	}
}

func TestPublisherAssetsRollbackAndMCP(t *testing.T) {
	s := setupPublisher(t)
	if w := publishAs(s, "pub-secret", "mailer", "index.html", ""); w.Code != 201 {
		t.Fatal(w.Code)
	}
	if w := publishAs(s, "pub-secret", "mailer", "index.html", ""); w.Code != 201 {
		t.Fatal(w.Code)
	}
	if w := call(s, "POST", "/api/rollback", "pub-secret", []byte(`{"site":"mailer","path":"index.html"}`)); w.Code != 200 {
		t.Fatalf("rollback %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("PUT", "/api/assets/mailer/a.png", bytes.NewReader([]byte{1, 2, 3}))
	r.Header.Set("Authorization", "Bearer pub-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("asset %d", w.Code)
	}
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"publish_page","arguments":{"site":"retrobater","path":"index.html","content":"<p>ok</p>"}}}`
	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer pub-secret")
	r.Header.Set("X-BotPages-Bot", "retrobater")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "retrobater") {
		t.Fatalf("mcp publish %d %s", w.Code, w.Body.String())
	}
}

func TestPublisherIsNotAdmin(t *testing.T) {
	s := setupPublisher(t)
	if a, admin := s.actor(httptest.NewRequest("GET", "/", nil)); a != "" || admin {
		t.Fatal("anonymous actor")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer pub-secret")
	if a, admin := s.actor(r); admin || a != "publisher/publisher" {
		t.Fatalf("actor %q admin=%v", a, admin)
	}
	if _, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "same", Publishers: map[string]string{"p": "same"}}); err == nil {
		t.Fatal("publisher token equal to admin token accepted")
	}
}

func TestBotIdentityHeaderSanitized(t *testing.T) {
	for in, want := range map[string]string{"newser": "newser", "": "-", "Bad Name\n": "-", strings.Repeat("a", 80): "-"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-BotPages-Bot", in)
		if got := botIdentity(r); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestHealthReportsVersion(t *testing.T) {
	s := setupPublisher(t)
	old := version
	version = "9.8.7"
	defer func() { version = old }()
	w := call(s, "GET", "/health", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"9.8.7"`) {
		t.Fatalf("health %d %s", w.Code, w.Body.String())
	}
	w = call(s, "POST", "/mcp", "pub-secret", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if !strings.Contains(w.Body.String(), `"version":"9.8.7"`) {
		t.Fatalf("mcp serverInfo %s", w.Body.String())
	}
}
