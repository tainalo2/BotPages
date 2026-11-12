package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Epic 5 : comptes et sessions utilisateurs finaux pour les mini-apps goja.
//
// Les comptes sont stockés dans le SQLite PAR SITE (même fichier que
// sitedb.go), dans des tables préfixées _botpages_ qui restent inaccessibles
// au code goja publié (validateRuntimeSQL refuse toute requête contenant
// "_BOTPAGES_"). La clé de signature des sessions est elle aussi générée et
// conservée par site, jamais partagée.

const (
	memberSessionCookieName = "botpages_session"
	memberSessionTTL        = 24 * time.Hour
	argon2idTime            = 1
	argon2idMemoryKB        = 64 * 1024
	argon2idThreads         = 4
	argon2idKeyLength       = 32
	argon2idSaltLength      = 16
)

var errMemberInvalidCredentials = errors.New("invalid username or password")
var errMemberDuplicateUsername = errors.New("username already exists")
var errMemberInvalidUsername = errors.New("invalid username")
var errMemberInvalidPassword = errors.New("password must be between 8 and 200 characters")
var errMemberInvalidSession = errors.New("invalid or expired session")

type memberIdentity struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// ensureMemberTables crée les tables de comptes/session si elles n'existent
// pas encore. Idempotent : appelé avant chaque opération membre.
func ensureMemberTables(ctx context.Context, store *siteDatabase) error {
	const schema = `
CREATE TABLE IF NOT EXISTS _botpages_members (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT UNIQUE NOT NULL,
	password_hash TEXT NOT NULL,
	session_version INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS _botpages_member_session_key (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	key BLOB NOT NULL
);`
	_, err := store.db.ExecContext(ctx, schema)
	return err
}

// memberSiteSessionKey retourne la clé HMAC du site, en la générant si c'est
// la première utilisation. Chaque site a sa propre clé : un jeton émis sur un
// site ne peut jamais être validé par un autre.
func memberSiteSessionKey(ctx context.Context, store *siteDatabase) ([]byte, error) {
	var key []byte
	err := store.db.QueryRowContext(ctx, "SELECT key FROM _botpages_member_session_key WHERE id = 1").Scan(&key)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	generated := make([]byte, 32)
	if _, err := rand.Read(generated); err != nil {
		return nil, err
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO _botpages_member_session_key (id, key) VALUES (1, ?)", generated); err != nil {
		// Une course entre deux premières requêtes peut provoquer un conflit
		// de clé primaire : relire la valeur gagnante dans ce cas.
		var existing []byte
		if readErr := store.db.QueryRowContext(ctx, "SELECT key FROM _botpages_member_session_key WHERE id = 1").Scan(&existing); readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return generated, nil
}

func validMemberUsername(username string) bool {
	if len(username) < 3 || len(username) > 64 {
		return false
	}
	for _, c := range username {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func hashMemberPassword(password string) (string, error) {
	salt := make([]byte, argon2idSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argon2idTime, argon2idMemoryKB, argon2idThreads, argon2idKeyLength)
	encoded := fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2idMemoryKB, argon2idTime, argon2idThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash))
	return encoded, nil
}

func verifyMemberPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var memoryKB, timeCost, threads int
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memoryKB, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(timeCost), uint32(memoryKB), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// registerMember crée un nouveau compte sur le site courant. Le mot de passe
// en clair n'est jamais conservé : seul le hash argon2id est stocké.
func registerMember(ctx context.Context, store *siteDatabase, username, password string) error {
	if !validMemberUsername(username) {
		return errMemberInvalidUsername
	}
	if len(password) < 8 || len(password) > 200 {
		return errMemberInvalidPassword
	}
	if err := ensureMemberTables(ctx, store); err != nil {
		return err
	}
	hash, err := hashMemberPassword(password)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx,
		"INSERT INTO _botpages_members (username, password_hash, session_version, created_at) VALUES (?, ?, 0, ?)",
		username, hash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return errMemberDuplicateUsername
	}
	return nil
}

// authenticateMember vérifie les identifiants et renvoie l'identifiant et la
// version de session courante du compte (utilisée pour signer le jeton).
func authenticateMember(ctx context.Context, store *siteDatabase, username, password string) (id int64, version int64, err error) {
	if err := ensureMemberTables(ctx, store); err != nil {
		return 0, 0, err
	}
	var hash string
	err = store.db.QueryRowContext(ctx,
		"SELECT id, password_hash, session_version FROM _botpages_members WHERE username = ?", username,
	).Scan(&id, &hash, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, errMemberInvalidCredentials
	}
	if err != nil {
		return 0, 0, err
	}
	if !verifyMemberPassword(hash, password) {
		return 0, 0, errMemberInvalidCredentials
	}
	return id, version, nil
}

func memberByID(ctx context.Context, store *siteDatabase, id int64) (username string, version int64, err error) {
	err = store.db.QueryRowContext(ctx,
		"SELECT username, session_version FROM _botpages_members WHERE id = ?", id,
	).Scan(&username, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, errMemberInvalidSession
	}
	return username, version, err
}

// logoutMember invalide tous les jetons émis jusqu'ici pour ce compte, en
// incrémentant sa version de session : le cookie déjà émis ne correspondra
// plus à la version attendue et sera rejeté.
func logoutMember(ctx context.Context, store *siteDatabase, id int64) error {
	_, err := store.db.ExecContext(ctx, "UPDATE _botpages_members SET session_version = session_version + 1 WHERE id = ?", id)
	return err
}

type memberSessionPayload struct {
	UID int64 `json:"uid"`
	Ver int64 `json:"v"`
	Exp int64 `json:"exp"`
}

// issueMemberSessionToken signe un jeton opaque pour le compte et la version
// de session donnés, à poser dans le cookie HttpOnly scopé au chemin de
// l'app. Le contenu n'est jamais exposé au code goja : seules les fonctions
// currentUser/members.* du runtime le manipulent.
func issueMemberSessionToken(key []byte, id, version int64) (string, error) {
	payload := memberSessionPayload{UID: id, Ver: version, Exp: time.Now().Add(memberSessionTTL).Unix()}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encodedPayload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encodedPayload + "." + signature, nil
}

// verifyMemberSessionToken vérifie la signature HMAC (avec la clé du site
// courant) et l'expiration, puis renvoie l'identifiant et la version de
// session déclarés dans le jeton. Elle ne consulte pas la base : l'appelant
// doit comparer la version renvoyée à celle actuellement stockée pour le
// compte, afin que logout() invalide bien les anciens jetons.
func verifyMemberSessionToken(key []byte, token string) (id int64, version int64, err error) {
	segments := strings.SplitN(token, ".", 2)
	if len(segments) != 2 {
		return 0, 0, errMemberInvalidSession
	}
	encodedPayload, signature := segments[0], segments[1]
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encodedPayload))
	expected := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
		return 0, 0, errMemberInvalidSession
	}
	data, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return 0, 0, errMemberInvalidSession
	}
	var payload memberSessionPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, 0, errMemberInvalidSession
	}
	if payload.Exp < time.Now().Unix() {
		return 0, 0, errMemberInvalidSession
	}
	return payload.UID, payload.Ver, nil
}

func memberSessionSetCookie(path, token string, maxAge int) string {
	cookie := fmt.Sprintf("%s=%s; Path=%s; HttpOnly; Secure; SameSite=Strict", memberSessionCookieName, token, path)
	if maxAge <= 0 {
		cookie += "; Max-Age=0"
	} else {
		cookie += fmt.Sprintf("; Max-Age=%d", maxAge)
	}
	return cookie
}
