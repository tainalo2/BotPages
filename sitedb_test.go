package main

import (
	"database/sql"
	"encoding/json"
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

func migrationAppPayload(site, name, entry string, migrations []AppMigration) []byte {
	body, _ := json.Marshal(appPublication{
		Site: site,
		Manifest: AppManifest{
			Name:       name,
			Routes:     []AppRoute{{Method: "GET", Path: "/run"}},
			Migrations: migrations,
		},
		Entry: entry,
	})
	return body
}

func TestPublishAppAppliesNumberedMigrationsOnce(t *testing.T) {
	s := setup(t)
	entry := `function handle() { return {body: String(db.query("SELECT COUNT(*) AS count FROM notes")[0].count)}; }`
	migrations := []AppMigration{{Name: "001_create_notes.sql", SQL: "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL); INSERT INTO notes(body) VALUES ('seed');"}}

	for publication := 0; publication < 2; publication++ {
		response := call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "migrated", entry, migrations))
		if response.Code != http.StatusCreated {
			t.Fatalf("publication %d: %d %s", publication+1, response.Code, response.Body.String())
		}
	}
	site, err := s.site("newser")
	if err != nil {
		t.Fatal(err)
	}
	response := call(s, http.MethodGet, "/a/"+site.Slug+"/migrated/run", "", nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "1" {
		t.Fatalf("migration was not applied exactly once: %d %q", response.Code, response.Body.String())
	}
	if contents, err := os.ReadFile(filepath.Join(s.cfg.Dir, "newser", ".apps", "migrated", "migrations", "001_create_notes.sql")); err != nil || string(contents) != migrations[0].SQL {
		t.Fatalf("migration file was not persisted: %q, %v", contents, err)
	}
}

func TestPublishAppRejectsMigrationGapsAndRollsBackFailures(t *testing.T) {
	s := setup(t)
	oldEntry := `function handle() { return {body: "old"}; }`
	first := []AppMigration{{Name: "001_create_items.sql", SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY);"}}
	response := call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "transactional", oldEntry, first))
	if response.Code != http.StatusCreated {
		t.Fatalf("initial publication: %d %s", response.Code, response.Body.String())
	}

	gap := []AppMigration{{Name: "002_skipped.sql", SQL: "SELECT 1"}}
	response = call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "invalid-gap", oldEntry, gap))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "sequential") {
		t.Fatalf("migration gap accepted: %d %q", response.Code, response.Body.String())
	}

	broken := append(append([]AppMigration{}, first...), AppMigration{
		Name: "002_broken.sql",
		SQL:  "INSERT INTO items(id) VALUES (1); THIS IS NOT SQL;",
	})
	response = call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "transactional", `function handle() { return {body: "new"}; }`, broken))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "migration") {
		t.Fatalf("broken migration accepted: %d %q", response.Code, response.Body.String())
	}

	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM items").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration was not rolled back: item count = %d", count)
	}
	site, err := s.site("newser")
	if err != nil {
		t.Fatal(err)
	}
	response = call(s, http.MethodGet, "/a/"+site.Slug+"/transactional/run", "", nil)
	if response.Code != http.StatusOK || response.Body.String() != "old" {
		t.Fatalf("failed migration activated new code: %d %q", response.Code, response.Body.String())
	}
}
