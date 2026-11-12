package main

import (
	"context"
	"strings"
	"testing"
)

func newTestSiteDatabase(t *testing.T) *siteDatabase {
	t.Helper()
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	store, err := s.ensureSiteDatabase("members-site")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestHashMemberPasswordNeverStoresPlaintextAndRejectsWrongPassword(t *testing.T) {
	store := newTestSiteDatabase(t)
	ctx := context.Background()

	if err := registerMember(ctx, store, "alice", "correct-horse"); err != nil {
		t.Fatal(err)
	}

	var storedHash string
	if err := store.db.QueryRowContext(ctx, "SELECT password_hash FROM _botpages_members WHERE username = ?", "alice").Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedHash, "correct-horse") {
		t.Fatalf("plaintext password leaked into stored hash: %q", storedHash)
	}
	if !strings.HasPrefix(storedHash, "argon2id$") {
		t.Fatalf("password not hashed with argon2id: %q", storedHash)
	}

	if _, _, err := authenticateMember(ctx, store, "alice", "correct-horse"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if _, _, err := authenticateMember(ctx, store, "alice", "wrong-password"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, _, err := authenticateMember(ctx, store, "nobody", "whatever1"); err == nil {
		t.Fatal("unknown username accepted")
	}
}

func TestRegisterMemberRejectsDuplicateUsername(t *testing.T) {
	store := newTestSiteDatabase(t)
	ctx := context.Background()
	if err := registerMember(ctx, store, "bob", "password1"); err != nil {
		t.Fatal(err)
	}
	if err := registerMember(ctx, store, "bob", "another-pass"); err == nil {
		t.Fatal("duplicate username accepted")
	}
}

func TestMemberSessionTokenRoundTripAndLogoutInvalidation(t *testing.T) {
	store := newTestSiteDatabase(t)
	ctx := context.Background()
	if err := registerMember(ctx, store, "carol", "password1"); err != nil {
		t.Fatal(err)
	}
	id, version, err := authenticateMember(ctx, store, "carol", "password1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := memberSiteSessionKey(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issueMemberSessionToken(key, id, version)
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotVersion, err := verifyMemberSessionToken(key, token)
	if err != nil || gotID != id || gotVersion != version {
		t.Fatalf("verify = %d %d %v, want %d %d", gotID, gotVersion, err, id, version)
	}

	if err := logoutMember(ctx, store, id); err != nil {
		t.Fatal(err)
	}
	_, currentVersion, err := memberByID(ctx, store, id)
	if err != nil {
		t.Fatal(err)
	}
	if currentVersion == gotVersion {
		t.Fatal("logout did not bump session version")
	}
}

func TestVerifyMemberSessionTokenRejectsForeignKey(t *testing.T) {
	storeA := newTestSiteDatabase(t)
	ctx := context.Background()
	if err := registerMember(ctx, storeA, "dave", "password1"); err != nil {
		t.Fatal(err)
	}
	id, version, err := authenticateMember(ctx, storeA, "dave", "password1")
	if err != nil {
		t.Fatal(err)
	}
	keyA, err := memberSiteSessionKey(ctx, storeA)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issueMemberSessionToken(keyA, id, version)
	if err != nil {
		t.Fatal(err)
	}

	keyB := make([]byte, len(keyA))
	copy(keyB, keyA)
	keyB[0] ^= 0xFF // simule la clé indépendante d'un autre site
	if _, _, err := verifyMemberSessionToken(keyB, token); err == nil {
		t.Fatal("token verified against a foreign site key")
	}
}
