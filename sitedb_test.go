package main

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestPublishAppCreatesSiteSQLiteDatabaseInWALMode(t *testing.T) {
	s := setup(t)
	entry := `function handle() { return {body: "ok"}; }`
	published := call(s, http.MethodPost, "/api/apps", "bot-secret", appPayload("newser", "stateful", entry, nil))
	if published.Code != http.StatusCreated {
		t.Fatalf("publish app: %d %s", published.Code, published.Body.String())
	}

	databasePath := filepath.Join(s.cfg.Dir, "newser", "data.sqlite")
	if _, err := os.Stat(databasePath); err != nil {
		t.Fatalf("site database was not created at publication: %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
}

func TestGojaDatabaseAPIIsPersistentAndRejectsCrossSiteAccess(t *testing.T) {
	s := setup(t)
	entry := `function handle() {
		var scoped = openDatabase("newser");
		scoped.exec("UPDATE counters SET value = value + 1");
		var rows = db.query("SELECT value FROM counters");
		return {body: String(rows[0].value)};
	}`
	base := publishTestApp(t, s, AppManifest{Name: "state", Routes: []AppRoute{{Method: "GET", Path: "/run"}}}, entry)
	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("CREATE TABLE counters (value INTEGER NOT NULL); INSERT INTO counters(value) VALUES (0)"); err != nil {
		t.Fatal(err)
	}

	for want := 1; want <= 2; want++ {
		response := call(s, http.MethodGet, base+"/run", "", nil)
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != strconv.Itoa(want) {
			t.Fatalf("persistent database call %d: %d %q", want, response.Code, response.Body.String())
		}
	}

	deniedEntry := `function handle() {
		openDatabase("other-site").query("SELECT 1");
		return {body: "unexpected"};
	}`
	deniedBase := publishTestApp(t, s, AppManifest{Name: "cross-site", Routes: []AppRoute{{Method: "GET", Path: "/run"}}}, deniedEntry)
	response := call(s, http.MethodGet, deniedBase+"/run", "", nil)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "cross-site database access denied") {
		t.Fatalf("cross-site database access: %d %q", response.Code, response.Body.String())
	}
}

func TestGojaDatabaseWritesAreSafeUnderConcurrency(t *testing.T) {
	s, err := NewServer(Config{
		Dir:               t.TempDir(),
		AdminToken:        "admin-secret",
		Bots:              map[string]string{"newser": "bot-secret"},
		MaxConcurrentApps: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	entry := `function handle() {
		db.exec("UPDATE counters SET value = value + 1");
		return {body: "ok"};
	}`
	base := publishTestApp(t, s, AppManifest{Name: "writes", Routes: []AppRoute{{Method: "POST", Path: "/run"}}}, entry)
	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("CREATE TABLE counters (value INTEGER NOT NULL); INSERT INTO counters(value) VALUES (0)"); err != nil {
		t.Fatal(err)
	}

	const writes = 24
	var wait sync.WaitGroup
	errors := make(chan string, writes)
	for i := 0; i < writes; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response := call(s, http.MethodPost, base+"/run", "", nil)
			if response.Code != http.StatusOK {
				errors <- response.Body.String()
			}
		}()
	}
	wait.Wait()
	close(errors)
	for message := range errors {
		t.Errorf("concurrent write failed: %s", message)
	}
	var value int
	if err := store.db.QueryRow("SELECT value FROM counters").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != writes {
		t.Fatalf("counter = %d, want %d", value, writes)
	}
}
