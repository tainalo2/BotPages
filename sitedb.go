package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// siteDBPath returns the dedicated SQLite file for a site.
func siteDBPath(dir, site string) string {
	return filepath.Join(dir, site, site+".db")
}

// EnsureSiteDB creates the per-site SQLite database if needed and enables WAL mode.
func EnsureSiteDB(dir, site string) error {
	if !safeName(site) {
		return errors.New("invalid site name")
	}
	dbPath := siteDBPath(dir, site)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return err
	}
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=journal_mode(WAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			return err
		}
	}
	return nil
}
