package main

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
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
