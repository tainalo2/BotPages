package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityBootstrapPersistsWithoutDuplicatePlaintext(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Dir:        dir,
		AdminToken: "admin-secret",
		Bots:       map[string]string{"newser": "bot-secret"},
		Publishers: map[string]string{"publisher": "pub-secret"},
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, admin := actorForToken(s, "bot-secret"); got != "newser" || admin {
		t.Fatalf("bootstrapped bot actor = %q, admin=%v", got, admin)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.Bots["late-bot"] = "late-secret"
	s, err = NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, admin := actorForToken(s, "bot-secret"); got != "newser" || admin {
		t.Fatalf("persisted bot actor = %q, admin=%v", got, admin)
	}
	if got, _ := actorForToken(s, "late-secret"); got != "" {
		t.Fatalf("environment was imported again as %q", got)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM bots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("bootstrap created %d identities, want 2", count)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "bot-secret") || strings.Contains(string(contents), "pub-secret") {
		t.Fatal("identity database contains a plaintext token")
	}
}

func TestAdminTokenCRUDAndImmediateRevocation(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	create := call(s, http.MethodPost, "/api/admin/tokens", "admin-secret", []byte(`{"name":"mailer","kind":"bot","site_scope":"mailer"}`))
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || len(created.Token) < 32 {
		t.Fatalf("invalid created identity: %+v", created)
	}
	if got, admin := actorForToken(s, created.Token); got != "mailer" || admin {
		t.Fatalf("created actor = %q, admin=%v", got, admin)
	}
	if authenticated := call(s, http.MethodGet, "/api/sites", created.Token, nil); authenticated.Code != http.StatusOK {
		t.Fatalf("created token request: %d %s", authenticated.Code, authenticated.Body.String())
	}

	listed := call(s, http.MethodGet, "/api/admin/tokens", "admin-secret", nil)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), created.Token) || !strings.Contains(listed.Body.String(), `"name":"mailer"`) {
		t.Fatalf("list leaked or omitted identity: %d %s", listed.Code, listed.Body.String())
	}

	revoked := call(s, http.MethodDelete, "/api/admin/tokens/"+jsonNumber(created.ID), "admin-secret", nil)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", revoked.Code, revoked.Body.String())
	}
	if got, _ := actorForToken(s, created.Token); got != "" {
		t.Fatalf("revoked token still authenticates as %q", got)
	}
	if rejected := call(s, http.MethodGet, "/api/sites", created.Token, nil); rejected.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token HTTP request: %d %s", rejected.Code, rejected.Body.String())
	}
}

func TestRateLimitIsPerSQLiteToken(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	createToken := func(name string) string {
		t.Helper()
		body := []byte(`{"name":"` + name + `","kind":"bot","site_scope":"shared-site"}`)
		w := call(s, http.MethodPost, "/api/admin/tokens", "admin-secret", body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, w.Code, w.Body.String())
		}
		var created struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		return created.Token
	}
	first := createToken("first")
	second := createToken("second")

	for i := 0; i < 60; i++ {
		w := call(s, http.MethodPost, "/api/publish", first, []byte(`{}`))
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("first token limited early at request %d", i+1)
		}
	}
	if w := call(s, http.MethodPost, "/api/publish", first, []byte(`{}`)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("first token exceeded limit: %d %s", w.Code, w.Body.String())
	}
	if w := publishAs(s, second, "shared-site", "index.html", "unlisted"); w.Code != http.StatusCreated {
		t.Fatalf("second token shared the first token's rate limit: %d %s", w.Code, w.Body.String())
	}
}

func TestPublisherCreatedByAdminKeepsUnlistedScope(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := call(s, http.MethodPost, "/api/admin/tokens", "admin-secret", []byte(`{"name":"shared","kind":"publisher"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create publisher: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if w = publishAs(s, created.Token, "public-site", "index.html", "unlisted"); w.Code != http.StatusCreated {
		t.Fatalf("publisher unlisted: %d %s", w.Code, w.Body.String())
	}
	if w = publishAs(s, "admin-secret", "private-site", "index.html", "protected"); w.Code != http.StatusCreated {
		t.Fatalf("admin protected: %d %s", w.Code, w.Body.String())
	}
	if w = publishAs(s, created.Token, "private-site", "other.html", ""); w.Code != http.StatusForbidden {
		t.Fatalf("publisher modified protected site: %d %s", w.Code, w.Body.String())
	}
}

func TestTokenAdminAPIRequiresAdmin(t *testing.T) {
	s := setup(t)
	defer s.Close()
	for _, token := range []string{"", "bot-secret"} {
		if w := call(s, http.MethodGet, "/api/admin/tokens", token, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("list with %q: %d", token, w.Code)
		}
	}
	if w := call(s, http.MethodPost, "/api/admin/tokens", "admin-secret", []byte(`{"name":"bad name","kind":"bot","site_scope":"x"}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid create: %d %s", w.Code, w.Body.String())
	}
}

func actorForToken(s *Server, token string) (string, bool) {
	r := httptestRequestWithToken(token)
	return s.actor(r)
}

func httptestRequestWithToken(token string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func jsonNumber(v int64) string {
	return strings.TrimSpace(string(mustJSON(v)))
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
