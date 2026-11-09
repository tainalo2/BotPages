package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

const siteDatabaseFilename = "data.sqlite"

type siteDatabase struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func (s *Server) ensureSiteDatabase(siteName string) (*siteDatabase, error) {
	if !safeName(siteName) {
		return nil, errors.New("invalid site database scope")
	}

	s.siteDatabasesMu.Lock()
	defer s.siteDatabasesMu.Unlock()
	if store := s.siteDatabases[siteName]; store != nil {
		return store, nil
	}

	databasePath := filepath.Join(s.cfg.Dir, siteName, siteDatabaseFilename)
	if err := os.MkdirAll(filepath.Dir(databasePath), 0700); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath}).String() + "?_pragma=busy_timeout%285000%29&_pragma=foreign_keys%281%29"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if journalMode != "wal" {
		_ = db.Close()
		return nil, fmt.Errorf("enable WAL: unexpected journal mode %q", journalMode)
	}
	if err := os.Chmod(databasePath, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}

	store := &siteDatabase{db: db}
	s.siteDatabases[siteName] = store
	return store, nil
}

func (s *Server) closeSiteDatabases() error {
	s.siteDatabasesMu.Lock()
	defer s.siteDatabasesMu.Unlock()
	var closeErr error
	for siteName, store := range s.siteDatabases {
		closeErr = errors.Join(closeErr, store.db.Close())
		delete(s.siteDatabases, siteName)
	}
	return closeErr
}
