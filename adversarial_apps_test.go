package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type adversarialAppFixture struct {
	manifest AppManifest
	entry    string
}

func loadAdversarialAppFixture(t *testing.T, name string) adversarialAppFixture {
	t.Helper()
	root := filepath.Join("testdata", "apps", name)
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatalf("read %s manifest: %v", name, err)
	}
	entryBytes, err := os.ReadFile(filepath.Join(root, "entry.js"))
	if err != nil {
		t.Fatalf("read %s entry: %v", name, err)
	}
	var manifest AppManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parse %s manifest: %v", name, err)
	}
	return adversarialAppFixture{manifest: manifest, entry: string(entryBytes)}
}

func TestAdversarialAppFixturesAreContained(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		prepare func(*testing.T, *Server)
	}{
		{name: "infinite-loop", status: http.StatusGatewayTimeout, body: "execution timed out"},
		{name: "memory-fork-bomb", status: http.StatusInsufficientStorage, body: "memory limit exceeded"},
		{name: "network-outside-allowlist", status: http.StatusInternalServerError, body: "not admin-approved"},
		{
			name:   "cross-site-db",
			status: http.StatusInternalServerError,
			body:   "cross-site database access denied",
			prepare: func(t *testing.T, s *Server) {
				t.Helper()
				if response := publishAs(s, "victim-secret", "victim", "index.html", "unlisted"); response.Code != http.StatusCreated {
					t.Fatalf("create victim site: %d %s", response.Code, response.Body.String())
				}
				store, err := s.ensureSiteDatabase("victim")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec("CREATE TABLE secrets (value TEXT NOT NULL); INSERT INTO secrets(value) VALUES ('must-not-leak')"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := NewServer(Config{
				Dir:        t.TempDir(),
				AdminToken: "admin-secret",
				Bots:       map[string]string{"newser": "bot-secret", "victim": "victim-secret"},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if test.prepare != nil {
				test.prepare(t, s)
			}

			fixture := loadAdversarialAppFixture(t, test.name)
			base := publishTestApp(t, s, fixture.manifest, fixture.entry)
			response := call(s, http.MethodGet, base+"/run", "", nil)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("sandbox response: %d %q, want %d containing %q", response.Code, response.Body.String(), test.status, test.body)
			}
		})
	}
}
