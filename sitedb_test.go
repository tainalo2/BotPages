package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestSiteDatabasePageLimitReservesSiteQuotaAndWALSpace(t *testing.T) {
	s := setup(t)
	if response := publishAs(s, "bot-secret", "newser", "index.html", "unlisted"); response.Code != http.StatusCreated {
		t.Fatalf("create site: %d %s", response.Code, response.Body.String())
	}
	const otherSiteBytes = int64(64 << 20)
	filler, err := os.Create(filepath.Join(s.cfg.Dir, "newser", "quota-filler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Truncate(otherSiteBytes); err != nil {
		_ = filler.Close()
		t.Fatal(err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	entry := `function handle() { return {body: "ok"}; }`
	if response := call(s, http.MethodPost, "/api/apps", "bot-secret", appPayload("newser", "quota-db", entry, nil)); response.Code != http.StatusCreated {
		t.Fatalf("publish app: %d %s", response.Code, response.Body.String())
	}
	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	var pageSize, maxPages int64
	if err := store.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA max_page_count").Scan(&maxPages); err != nil {
		t.Fatal(err)
	}
	maximumDatabaseBytes := maxPages * pageSize
	fullWALBytes := maxPages*(pageSize+sqliteWALFrameHeaderBytes) + 32
	availableDatabaseBytes := maxSiteBytes - otherSiteBytes - siteDatabaseWALReserve - siteDatabaseMetadataReserve
	if maximumDatabaseBytes+fullWALBytes > availableDatabaseBytes {
		t.Fatalf("database and worst-case WAL cap %d ignores site files or sidecar reserve", maximumDatabaseBytes+fullWALBytes)
	}
	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer connection pool = %d, want 1 so max_page_count applies to every write", got)
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
		Dir:                      t.TempDir(),
		AdminToken:               "admin-secret",
		Bots:                     map[string]string{"newser": "bot-secret"},
		MaxConcurrentApps:        32,
		MaxConcurrentAppsPerSite: 24,
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

func TestGojaDatabaseWriteWaitHonorsExecutionTimeout(t *testing.T) {
	s := setup(t)
	entry := `function handle() { db.exec("INSERT INTO waiting_writes(id) VALUES (1)"); return {body: "unexpected"}; }`
	base := publishTestApp(t, s, AppManifest{Name: "write-timeout", TimeoutMS: 25, Routes: []AppRoute{{Method: "POST", Path: "/run"}}}, entry)
	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("CREATE TABLE waiting_writes (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	store.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			store.writeMu.Unlock()
		}
	}()

	started := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call(s, http.MethodPost, base+"/run", "", nil) }()
	select {
	case response := <-done:
		store.writeMu.Unlock()
		locked = false
		if response.Code != http.StatusGatewayTimeout || time.Since(started) > 100*time.Millisecond {
			t.Fatalf("blocked write ignored timeout: %d %q after %s", response.Code, response.Body.String(), time.Since(started))
		}
	case <-time.After(150 * time.Millisecond):
		store.writeMu.Unlock()
		locked = false
		<-done
		t.Fatal("blocked write did not return when its execution context expired")
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

func TestPublishAppRejectsMigrationTransactionControlWithoutSideEffects(t *testing.T) {
	s := setup(t)
	migrations := []AppMigration{{
		Name: "001_escape_transaction.sql",
		SQL:  "CREATE TABLE escaped (id INTEGER); COMMIT; INSERT INTO escaped(id) VALUES (1);",
	}}
	response := call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "escape", `function handle() { return {body: "no"}; }`, migrations))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("transaction control migration status = %d, want 400: %s", response.Code, response.Body.String())
	}
	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'escaped'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("transaction-control migration escaped the runner transaction")
	}
}

func TestPublishAppDoesNotMigrateWhenActivationCannotProceed(t *testing.T) {
	s := setup(t)
	first := []AppMigration{{Name: "001_create_activation_items.sql", SQL: "CREATE TABLE activation_items (id INTEGER PRIMARY KEY);"}}
	response := call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "activation", `function handle() { return {body: "old"}; }`, first))
	if response.Code != http.StatusCreated {
		t.Fatalf("initial publication: %d %s", response.Code, response.Body.String())
	}
	activePath := filepath.Join(s.cfg.Dir, "newser", ".apps", "activation")
	if err := os.Remove(activePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, []byte("blocks atomic activation"), 0600); err != nil {
		t.Fatal(err)
	}

	second := append(append([]AppMigration{}, first...), AppMigration{Name: "002_insert_activation_item.sql", SQL: "INSERT INTO activation_items(id) VALUES (1);"})
	response = call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "activation", `function handle() { return {body: "new"}; }`, second))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("blocked activation status = %d, want 500: %s", response.Code, response.Body.String())
	}
	store, err := s.ensureSiteDatabase("newser")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM activation_items").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("migration committed before failed activation: item count = %d", count)
	}
}

func TestAppActivationJournalRecoversCodeSchemaAndApprovalTogether(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "uncommitted"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			s := setup(t)
			entry := `function handle() { return {body: "old"}; }`
			if response := call(s, http.MethodPost, "/api/apps", "bot-secret", migrationAppPayload("newser", "recover", entry, nil)); response.Code != http.StatusCreated {
				t.Fatalf("initial publication: %d %s", response.Code, response.Body.String())
			}
			site, err := s.site("newser")
			if err != nil {
				t.Fatal(err)
			}
			appsDir := filepath.Join(s.cfg.Dir, site.Name, ".apps")
			versionsDir := filepath.Join(s.cfg.Dir, site.Name, ".app-versions", "recover")
			target := filepath.Join(appsDir, "recover")
			oldVersion, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			approvalPath := filepath.Join(s.cfg.Dir, site.Name, ".app-approvals", "recover.json")
			if err := writeAtomicFile(approvalPath, []byte(`{"approved":true}`)); err != nil {
				t.Fatal(err)
			}
			staged, err := os.MkdirTemp(versionsDir, ".publish-")
			if err != nil {
				t.Fatal(err)
			}
			manifestJSON, _ := json.Marshal(AppManifest{Name: "recover", Routes: []AppRoute{{Method: "GET", Path: "/run"}}})
			if err := os.WriteFile(filepath.Join(staged, "manifest.json"), manifestJSON, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(staged, "entry.js"), []byte(`function handle() { return {body: "new"}; }`), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := s.ensureSiteDatabase(site.Name)
			if err != nil {
				t.Fatal(err)
			}
			versionID := filepath.Base(staged + ".ready")
			run, err := prepareMigrations(context.Background(), s, store, site.Name, "recover", versionID, []AppMigration{{Name: "001_recovery.sql", SQL: "CREATE TABLE recovery_marker (id INTEGER);"}})
			if err != nil {
				t.Fatal(err)
			}
			approval, err := snapshotFile(approvalPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := activateAppVersion(appsDir, versionsDir, target, staged, "recover", approval); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := run.Commit(); err != nil {
					t.Fatal(err)
				}
			} else if err := run.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := s.reconcileAppActivation(context.Background(), site, "recover", store); err != nil {
				t.Fatal(err)
			}
			resolved, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				if filepath.Base(resolved) != versionID {
					t.Fatalf("committed recovery activated %q, want %q", resolved, versionID)
				}
				if _, err := os.Stat(approvalPath); !os.IsNotExist(err) {
					t.Fatalf("committed recovery retained approval: %v", err)
				}
				if _, err := store.db.Exec("INSERT INTO recovery_marker(id) VALUES (1)"); err != nil {
					t.Fatalf("committed recovery lost schema: %v", err)
				}
			} else {
				if resolved != oldVersion {
					t.Fatalf("uncommitted recovery activated %q, want %q", resolved, oldVersion)
				}
				contents, err := os.ReadFile(approvalPath)
				if err != nil || string(contents) != `{"approved":true}` {
					t.Fatalf("uncommitted recovery did not restore approval: %q, %v", contents, err)
				}
				if _, err := store.db.Exec("INSERT INTO recovery_marker(id) VALUES (1)"); err == nil {
					t.Fatal("uncommitted recovery retained migrated schema")
				}
			}
			journalPath := filepath.Join(s.cfg.Dir, site.Name, ".app-activations", "recover.json")
			if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
				t.Fatalf("activation journal was not cleaned up: %v", err)
			}
		})
	}
}
