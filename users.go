package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/crypto/argon2"
)

type siteUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (s *Server) ensureUserTables(ctx context.Context, siteName string) error {
	store, err := s.ensureSiteDatabase(siteName)
	if err != nil {
		return err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS _botpages_users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS _botpages_site_keys (id INTEGER PRIMARY KEY, key TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS _botpages_sessions (token TEXT PRIMARY KEY, user_id INTEGER NOT NULL, expires_at TEXT NOT NULL, FOREIGN KEY(user_id) REFERENCES _botpages_users(id))`,
	}
	for _, stmt := range stmts {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	// ensure a per-site signing key exists
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM _botpages_site_keys").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		if _, err := store.db.ExecContext(ctx, "INSERT INTO _botpages_site_keys (id, key) VALUES (1, ?)", hex.EncodeToString(b)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) siteSigningKey(ctx context.Context, siteName string) ([]byte, error) {
	if err := s.ensureUserTables(ctx, siteName); err != nil {
		return nil, err
	}
	store, err := s.ensureSiteDatabase(siteName)
	if err != nil {
		return nil, err
	}
	var keyHex string
	if err := store.readDB.QueryRowContext(ctx, "SELECT key FROM _botpages_site_keys WHERE id=1").Scan(&keyHex); err != nil {
		return nil, err
	}
	return hex.DecodeString(keyHex)
}

func hashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	hash := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=1,p=4$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash))
}

func verifyPassword(password, encoded string) bool {
	// parse
	parts := split(encoded, "$")
	if len(parts) < 6 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	computed := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	return hmac.Equal(computed, hash)
}

func split(s, sep string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			out = append(out, cur)
			cur = ""
			i += len(sep) - 1
		} else {
			cur += string(s[i])
		}
	}
	out = append(out, cur)
	return out
}

func (s *Server) createSiteUser(ctx context.Context, siteName, username, password string) (siteUser, error) {
	if err := s.ensureUserTables(ctx, siteName); err != nil {
		return siteUser{}, err
	}
	store, err := s.ensureSiteDatabase(siteName)
	if err != nil {
		return siteUser{}, err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	res, err := store.db.ExecContext(ctx, "INSERT INTO _botpages_users (username, password_hash, created_at) VALUES (?, ?, ?)", username, hashPassword(password), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return siteUser{}, err
	}
	id, _ := res.LastInsertId()
	return siteUser{ID: id, Username: username}, nil
}

func (s *Server) authenticateSiteUser(ctx context.Context, siteName, username, password string) (siteUser, bool, error) {
	if err := s.ensureUserTables(ctx, siteName); err != nil {
		return siteUser{}, false, err
	}
	store, err := s.ensureSiteDatabase(siteName)
	if err != nil {
		return siteUser{}, false, err
	}
	var id int64
	var hash string
	err = store.readDB.QueryRowContext(ctx, "SELECT id, password_hash FROM _botpages_users WHERE username=?", username).Scan(&id, &hash)
	if err == sql.ErrNoRows {
		return siteUser{}, false, nil
	}
	if err != nil {
		return siteUser{}, false, err
	}
	if !verifyPassword(password, hash) {
		return siteUser{}, false, nil
	}
	return siteUser{ID: id, Username: username}, true, nil
}

// session cookie helpers
func (s *Server) issueSession(ctx context.Context, siteName string, userID int64) (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	tokenStr := hex.EncodeToString(token)
	store, err := s.ensureSiteDatabase(siteName)
	if err != nil {
		return "", err
	}
	if err := s.ensureUserTables(ctx, siteName); err != nil {
		return "", err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	expires := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := store.db.ExecContext(ctx, "INSERT INTO _botpages_sessions (token, user_id, expires_at) VALUES (?, ?, ?)", tokenStr, userID, expires); err != nil {
		return "", err
	}
	key, err := s.siteSigningKey(ctx, siteName)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(siteName + ":" + tokenStr))
	sig := base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawStdEncoding.EncodeToString([]byte(siteName)) + "." + tokenStr + "." + sig, nil
}

func (s *Server) validateSession(ctx context.Context, siteName, cookieValue string) (siteUser, bool, error) {
	parts := split(cookieValue, ".")
	if len(parts) != 3 {
		return siteUser{}, false, nil
	}
	siteB64 := parts[0]
	token := parts[1]
	sig := parts[2]
	siteBytes, err := base64.RawStdEncoding.DecodeString(siteB64)
	if err != nil {
		return siteUser{}, false, nil
	}
	if string(siteBytes) != siteName {
		// cross-site mismatch
		return siteUser{}, false, nil
	}
	key, err := s.siteSigningKey(ctx, siteName)
	if err != nil {
		return siteUser{}, false, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(siteName + ":" + token))
	expected := base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return siteUser{}, false, nil
	}
	store, err := s.ensureSiteDatabase(siteName)
	if err != nil {
		return siteUser{}, false, err
	}
	var userID int64
	var username string
	var expires string
	err = store.readDB.QueryRowContext(ctx, "SELECT s.user_id, u.username, s.expires_at FROM _botpages_sessions s JOIN _botpages_users u ON u.id=s.user_id WHERE s.token=?", token).Scan(&userID, &username, &expires)
	if err != nil {
		return siteUser{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, expires)
	if err != nil || time.Now().After(t) {
		return siteUser{}, false, nil
	}
	return siteUser{ID: userID, Username: username}, true, nil
}

func (s *Server) bindCurrentUser(ctx context.Context, vm *any, siteName string, user *siteUser) {}
