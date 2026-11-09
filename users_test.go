package main

import (
	"context"
	"testing"
)

func TestArgon2idHashing(t *testing.T) {
	h := hashPassword("secret")
	if !verifyPassword("secret", h) {
		t.Fatal("verifyPassword should accept correct password")
	}
	if verifyPassword("wrong", h) {
		t.Fatal("verifyPassword should reject wrong password")
	}
}

func TestCrossSiteSessionCookieRejected(t *testing.T) {
	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.ensureUserTables(ctx, "sitea"); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureUserTables(ctx, "siteb"); err != nil {
		t.Fatal(err)
	}
	u, err := s.createSiteUser(ctx, "sitea", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := s.issueSession(ctx, "sitea", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.validateSession(ctx, "sitea", cookie); !ok {
		t.Fatal("session should be valid on origin site")
	}
	if _, ok, _ := s.validateSession(ctx, "siteb", cookie); ok {
		t.Fatal("session from site A must be rejected on site B")
	}
}
