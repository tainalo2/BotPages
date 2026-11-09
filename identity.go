package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const publisherScope = "*"

type Identity struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	SiteScope string     `json:"site_scope"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type identityStore struct {
	db *sql.DB
}

type CreatedIdentity struct {
	Identity
	Token string `json:"token"`
}

func openIdentityStore(dir, adminToken string, bots, publishers map[string]string) (*identityStore, error) {
	db, err := sql.Open("sqlite", filepath.Join(dir, "identities.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &identityStore{db: db}
	if err := store.initialize(adminToken, bots, publishers); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *identityStore) initialize(adminToken string, bots, publishers map[string]string) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS bots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			token_hash BLOB NOT NULL UNIQUE,
			site_scope TEXT NOT NULL,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS bots_active_name_scope ON bots(name, site_scope) WHERE revoked_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS identity_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	var imported string
	err = tx.QueryRowContext(ctx, `SELECT value FROM identity_meta WHERE key = 'environment_imported'`).Scan(&imported)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for name, token := range bots {
			if err := insertImportedIdentity(ctx, tx, name, name, token, now); err != nil {
				return fmt.Errorf("import bot %q: %w", name, err)
			}
		}
		for name, token := range publishers {
			if token == "" || equal(token, adminToken) {
				return errors.New("publisher tokens must be non-empty and differ from the admin token")
			}
			if err := insertImportedIdentity(ctx, tx, name, publisherScope, token, now); err != nil {
				return fmt.Errorf("import publisher %q: %w", name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_meta(key, value) VALUES ('environment_imported', ?)`, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertImportedIdentity(ctx context.Context, tx *sql.Tx, name, scope, token, created string) error {
	if !safeName(name) || token == "" {
		return errors.New("identity name and token must be valid")
	}
	hash := sha256.Sum256([]byte(token))
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bots(name, token_hash, site_scope, created_at) VALUES (?, ?, ?, ?)`, name, hash[:], scope, created)
	return err
}

func (s *identityStore) authenticate(token string) (Identity, bool, error) {
	if token == "" {
		return Identity{}, false, nil
	}
	hash := sha256.Sum256([]byte(token))
	var identity Identity
	var created string
	err := s.db.QueryRow(`SELECT id, name, site_scope, created_at FROM bots WHERE token_hash = ? AND revoked_at IS NULL`, hash[:]).Scan(&identity.ID, &identity.Name, &identity.SiteScope, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, err
	}
	identity.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return identity, err == nil, err
}

func (s *identityStore) list(publisher bool) ([]Identity, error) {
	operator := "<>"
	if publisher {
		operator = "="
	}
	rows, err := s.db.Query(`SELECT id, name, site_scope, created_at, revoked_at FROM bots WHERE site_scope `+operator+` ? ORDER BY id`, publisherScope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := []Identity{}
	for rows.Next() {
		var identity Identity
		var created string
		var revoked sql.NullString
		if err := rows.Scan(&identity.ID, &identity.Name, &identity.SiteScope, &created, &revoked); err != nil {
			return nil, err
		}
		identity.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		if revoked.Valid {
			value, err := time.Parse(time.RFC3339Nano, revoked.String)
			if err != nil {
				return nil, err
			}
			identity.RevokedAt = &value
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

func (s *identityStore) revoke(id int64, publisher bool) (bool, error) {
	operator := "<>"
	if publisher {
		operator = "="
	}
	result, err := s.db.Exec(`UPDATE bots SET revoked_at = ? WHERE id = ? AND site_scope `+operator+` ? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), id, publisherScope)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *identityStore) create(name, scope string) (CreatedIdentity, error) {
	if !safeName(name) || (scope != publisherScope && !safeName(scope)) {
		return CreatedIdentity{}, errors.New("invalid identity name or site scope")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return CreatedIdentity{}, err
	}
	token := hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	created := time.Now().UTC()
	result, err := s.db.Exec(`INSERT INTO bots(name, token_hash, site_scope, created_at) VALUES (?, ?, ?, ?)`, name, hash[:], scope, created.Format(time.RFC3339Nano))
	if err != nil {
		return CreatedIdentity{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return CreatedIdentity{}, err
	}
	return CreatedIdentity{Identity: Identity{ID: id, Name: name, SiteScope: scope, CreatedAt: created}, Token: token}, nil
}

func (s *identityStore) close() error { return s.db.Close() }
