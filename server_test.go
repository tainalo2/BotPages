package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret", Bots: map[string]string{"newser": "bot-secret"}, Readers: map[string]map[string]string{"private": {"alex": "reader-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func call(s *Server, method, path, auth string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestPublishAndUnlisted(t *testing.T) {
	s := setup(t)
	body := []byte(`{"site":"newser","path":"index.html","content":"<h1>Hello</h1>","visibility":"unlisted"}`)
	w := call(s, "POST", "/api/publish", "bot-secret", body)
	if w.Code != 201 {
		t.Fatalf("publish %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	slug := out["slug"].(string)
	w = call(s, "GET", "/p/"+slug+"/index.html", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Hello") || w.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("serve: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Dir, "newser", slug, "index.html")); err != nil {
		t.Fatal(err)
	}
}
func TestProtectedRequiresReader(t *testing.T) {
	s := setup(t)
	w := call(s, "POST", "/api/publish", "admin-secret", []byte(`{"site":"private","path":"index.html","content":"secret","visibility":"protected"}`))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	path := "/p/" + out["slug"].(string) + "/index.html"
	if w = call(s, "GET", path, "", nil); w.Code != 401 {
		t.Fatalf("anonymous %d", w.Code)
	}
	r := httptest.NewRequest("GET", path, nil)
	r.SetBasicAuth("alex", "reader-secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "secret" {
		t.Fatalf("reader %d %s", w.Code, w.Body.String())
	}
}
func TestRejectTraversalAndBotVisibility(t *testing.T) {
	s := setup(t)
	for _, body := range []string{`{"site":"newser","path":"../escape","content":"x"}`, `{"site":"other","path":"index.html","content":"x"}`, `{"site":"newser","path":"index.html","content":"x","visibility":"protected"}`} {
		w := call(s, "POST", "/api/publish", "bot-secret", []byte(body))
		if w.Code < 400 {
			t.Fatalf("accepted %s", body)
		}
	}
}
func TestMCPInitializeAndTools(t *testing.T) {
	s := setup(t)
	w := call(s, "POST", "/mcp", "bot-secret", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "serverInfo") {
		t.Fatalf("init %d %s", w.Code, w.Body.String())
	}
	w = call(s, "POST", "/mcp", "bot-secret", []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "publish_page") {
		t.Fatalf("tools %d %s", w.Code, w.Body.String())
	}
}

func TestBinaryAssetAndRange(t *testing.T) {
	s := setup(t)
	w := call(s, "POST", "/api/publish", "bot-secret", []byte(`{"site":"newser","path":"index.html","content":"hi"}`))
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	var site map[string]any
	json.Unmarshal(w.Body.Bytes(), &site)
	r := httptest.NewRequest("PUT", "/api/assets/newser/video.mp4", bytes.NewReader([]byte{0, 1, 2, 3, 4, 5}))
	r.Header.Set("Authorization", "Bearer bot-secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("upload %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/p/"+site["slug"].(string)+"/video.mp4", nil)
	r.Header.Set("Range", "bytes=2-4")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), []byte{2, 3, 4}) {
		t.Fatalf("range %d %v", w.Code, w.Body.Bytes())
	}
}

func TestHostIsolation(t *testing.T) {
	s := setup(t)
	s.cfg.AdminHost = "admin.example.test"
	s.cfg.ContentHost = "pages.example.test"
	for _, tc := range []struct{ host, path string }{{"pages.example.test", "/api/sites"}, {"admin.example.test", "/p/123/index.html"}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Host = tc.host
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s %s: %d", tc.host, tc.path, w.Code)
		}
	}
}
