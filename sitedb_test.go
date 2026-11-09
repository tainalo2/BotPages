package main

import (
	"database/sql"
	"os"
	"testing"
)

func TestSiteDBCreatedWithWAL(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureSiteDB(dir, "mysite"); err != nil {
		t.Fatal(err)
	}
	dbPath := siteDBPath(dir, "mysite")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %s, want wal", mode)
	}
}
