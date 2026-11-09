package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const identityDatabaseName = "identity.db"

type tokenRecord struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	SiteScope *string `json:"site_scope"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

func openIdentityDB(dataDir string, bots, publishers map[string]string, adminToken string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.Join(dataDir, identityDatabaseName))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS bots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    site_scope TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS identity_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`); err != nil {
		db.Close()
		return nil, err
	}
	if err = importLegacyIdentities(db, bots, publishers, adminToken); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func importLegacyIdentities(db *sql.DB, bots, publishers map[string]string, adminToken string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM identity_metadata WHERE key = 'environment_imported'`).Scan(&exists)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	insert := func(name, token string, scope any) error {
		if !safeName(name) || token == "" || equal(token, adminToken) {
			return fmt.Errorf("invalid legacy identity %q", name)
		}
		_, err := tx.Exec(`INSERT OR IGNORE INTO bots(name, token_hash, site_scope) VALUES (?, ?, ?)`, name, hashToken(token), scope)
		return err
	}
	for name, token := range bots {
		if err := insert(name, token, name); err != nil {
			return err
		}
	}
	for name, token := range publishers {
		if err := insert(name, token, nil); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO identity_metadata(key, value) VALUES ('environment_imported', CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	return tx.Commit()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bp_" + hex.EncodeToString(b), nil
}

func (s *Server) identityForToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var name string
	var scope sql.NullString
	err := s.db.QueryRow(`SELECT name, site_scope FROM bots WHERE token_hash = ? AND revoked_at IS NULL`, hashToken(token)).Scan(&name, &scope)
	if err != nil {
		return "", false
	}
	if scope.Valid {
		return scope.String, false
	}
	return "publisher/" + name, false
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	if _, admin := s.actor(r); !admin {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rows, err := s.db.Query(`SELECT id, name, site_scope, created_at, revoked_at FROM bots ORDER BY id`)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []tokenRecord{}
	for rows.Next() {
		var rec tokenRecord
		var scope, revoked sql.NullString
		if err := rows.Scan(&rec.ID, &rec.Name, &scope, &rec.CreatedAt, &revoked); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		if scope.Valid {
			rec.Kind = "bot"
			rec.SiteScope = &scope.String
		} else {
			rec.Kind = "publisher"
		}
		if revoked.Valid {
			rec.RevokedAt = &revoked.String
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if _, admin := s.actor(r); !admin {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		SiteScope string `json:"site_scope"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || !safeName(in.Name) {
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	var scope any
	switch in.Kind {
	case "bot":
		if !safeName(in.SiteScope) {
			http.Error(w, "bot site_scope is required", http.StatusBadRequest)
			return
		}
		scope = in.SiteScope
	case "publisher":
		if in.SiteScope != "" {
			http.Error(w, "publisher cannot have site_scope", http.StatusBadRequest)
			return
		}
		scope = nil
	default:
		http.Error(w, "kind must be bot or publisher", http.StatusBadRequest)
		return
	}
	token, err := newToken()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	result, err := s.db.Exec(`INSERT INTO bots(name, token_hash, site_scope) VALUES (?, ?, ?)`, in.Name, hashToken(token), scope)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusCreated, map[string]any{"id": id, "name": in.Name, "kind": in.Kind, "site_scope": scope, "token": token})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if _, admin := s.actor(r); !admin {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rawID := strings.TrimPrefix(r.URL.Path, "/api/admin/tokens/")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "invalid token id", http.StatusBadRequest)
		return
	}
	result, err := s.db.Exec(`UPDATE bots SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND revoked_at IS NULL`, id)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
