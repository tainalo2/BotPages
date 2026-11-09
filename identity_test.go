package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestServerDoesNotRetainPlaintextIdentityMaps(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin", Bots: map[string]string{"bot": "secret"}, Publishers: map[string]string{"publisher": "other-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.cfg.Bots != nil || s.cfg.Publishers != nil {
		t.Fatal("server retained plaintext identity maps after import")
	}
}

func TestAdminUIExposesIdentityManagement(t *testing.T) {
	source, err := os.ReadFile("web/src.jsx")
	if err != nil {
		t.Fatal(err)
	}
	ui := string(source)
	for _, required := range []string{"/api/admin/bots", "/api/admin/publishers", "Révoquer", "Jeton créé"} {
		if !strings.Contains(ui, required) {
			t.Fatalf("admin UI does not contain %q", required)
		}
	}
}

func TestRevokedIdentityNameCanBeReissued(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin", Bots: map[string]string{"writer": "old-token"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	listed := call(s, "GET", "/api/admin/bots", "admin", nil)
	var identities []Identity
	if err := json.Unmarshal(listed.Body.Bytes(), &identities); err != nil || len(identities) != 1 {
		t.Fatalf("list: %v %s", err, listed.Body.String())
	}
	if revoked := call(s, "DELETE", "/api/admin/bots/"+fmt.Sprint(identities[0].ID), "admin", nil); revoked.Code != 204 {
		t.Fatalf("revoke: %d", revoked.Code)
	}
	if recreated := call(s, "POST", "/api/admin/bots", "admin", []byte(`{"name":"writer"}`)); recreated.Code != 201 {
		t.Fatalf("recreate: %d %s", recreated.Code, recreated.Body.String())
	}
}

func TestAdminRevocationTakesEffectOnNextRequest(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	createdResponse := call(s, "POST", "/api/admin/publishers", "admin", []byte(`{"name":"shared"}`))
	if createdResponse.Code != 201 {
		t.Fatal(createdResponse.Code, createdResponse.Body.String())
	}
	var created CreatedIdentity
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if before := call(s, "GET", "/api/sites", created.Token, nil); before.Code != 200 {
		t.Fatalf("token did not work before revocation: %d", before.Code)
	}

	revoked := call(s, "DELETE", "/api/admin/publishers/"+fmt.Sprint(created.ID), "admin", nil)
	if revoked.Code != 204 {
		t.Fatalf("revoke: %d %s", revoked.Code, revoked.Body.String())
	}
	if after := call(s, "GET", "/api/sites", created.Token, nil); after.Code != 401 {
		t.Fatalf("revoked token still works: %d", after.Code)
	}
	listed := call(s, "GET", "/api/admin/publishers", "admin", nil)
	if !strings.Contains(listed.Body.String(), `"revoked_at"`) {
		t.Fatalf("revocation absent from list: %s", listed.Body.String())
	}
}

func TestAdminListsBotsAndPublishersWithoutTokenHashes(t *testing.T) {
	s, err := NewServer(Config{
		Dir: t.TempDir(), AdminToken: "admin",
		Bots: map[string]string{"writer": "bot-secret"}, Publishers: map[string]string{"shared": "pub-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bots := call(s, "GET", "/api/admin/bots", "admin", nil)
	if bots.Code != 200 || !strings.Contains(bots.Body.String(), `"name":"writer"`) || strings.Contains(bots.Body.String(), "secret") {
		t.Fatalf("bot list: %d %s", bots.Code, bots.Body.String())
	}
	publishers := call(s, "GET", "/api/admin/publishers", "admin", nil)
	if publishers.Code != 200 || !strings.Contains(publishers.Body.String(), `"name":"shared"`) || strings.Contains(publishers.Body.String(), "secret") {
		t.Fatalf("publisher list: %d %s", publishers.Code, publishers.Body.String())
	}
	if unauthorized := call(s, "GET", "/api/admin/bots", "bot-secret", nil); unauthorized.Code != 401 {
		t.Fatalf("non-admin list status = %d", unauthorized.Code)
	}
}

func TestAdminCreatesBotAndReceivesTokenOnce(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w := call(s, "POST", "/api/admin/bots", "admin", []byte(`{"name":"writer","site_scope":"news"}`))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token response is cacheable: %q", w.Header().Get("Cache-Control"))
	}
	var created struct {
		Identity
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "writer" || created.SiteScope != "news" || len(created.Token) < 32 {
		t.Fatalf("unexpected created identity: %+v", created)
	}
	if got := call(s, "GET", "/api/sites", created.Token, nil); got.Code != 200 {
		t.Fatalf("created token was not usable: %d", got.Code)
	}
}

func TestCompletedImportIgnoresLaterPublisherEnvironment(t *testing.T) {
	dir := t.TempDir()
	first, err := NewServer(Config{Dir: dir, AdminToken: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewServer(Config{Dir: dir, AdminToken: "admin", Publishers: map[string]string{"later": "admin"}})
	if err != nil {
		t.Fatalf("completed import should ignore changed environment: %v", err)
	}
	second.Close()
}

func TestEnvironmentIdentityImportRunsOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	first, err := NewServer(Config{Dir: dir, AdminToken: "admin", Bots: map[string]string{"first": "first-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := NewServer(Config{Dir: dir, AdminToken: "admin", Bots: map[string]string{"second": "second-token"}})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if w := call(second, "GET", "/api/sites", "first-token", nil); w.Code != 200 {
		t.Fatalf("original import disappeared: %d", w.Code)
	}
	if w := call(second, "GET", "/api/sites", "second-token", nil); w.Code != 401 {
		t.Fatalf("environment was imported twice: %d", w.Code)
	}
	var count int
	if err := second.identities.db.QueryRow(`SELECT count(*) FROM bots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("identity count = %d, err = %v", count, err)
	}
}

func TestIdentityDatabaseImportsTokensWithoutStoringPlaintext(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServer(Config{
		Dir:        dir,
		AdminToken: "admin-secret",
		Bots:       map[string]string{"newser": "bot-secret"},
		Publishers: map[string]string{"shared": "publisher-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if w := call(s, "GET", "/api/sites", "bot-secret", nil); w.Code != 200 {
		t.Fatalf("imported bot token was not accepted: %d %s", w.Code, w.Body.String())
	}
	if w := call(s, "GET", "/api/sites", "publisher-secret", nil); w.Code != 200 {
		t.Fatalf("imported publisher token was not accepted: %d %s", w.Code, w.Body.String())
	}

	database, err := os.ReadFile(dir + "/identities.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"bot-secret", "publisher-secret"} {
		if strings.Contains(string(database), secret) {
			t.Fatalf("database contains plaintext token %q", secret)
		}
	}
}
