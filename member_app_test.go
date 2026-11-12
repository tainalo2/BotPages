package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setupMembersTwoSites publie la même mini-app "espace membre" sur deux
// sites distincts, chacun avec son propre jeton bot et donc sa propre base
// SQLite / clé de session.
func setupMembersTwoSites(t *testing.T) (s *Server, baseA, baseB string) {
	t.Helper()
	s, err := NewServer(Config{
		Dir:        t.TempDir(),
		AdminToken: "admin-secret",
		Bots: map[string]string{
			"sitea": "token-a",
			"siteb": "token-b",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	manifest := AppManifest{
		Name:     "members",
		MemoryMB: 48,
		Routes: []AppRoute{
			{Method: "POST", Path: "/register"},
			{Method: "POST", Path: "/login"},
			{Method: "POST", Path: "/logout"},
			{Method: "GET", Path: "/whoami"},
		},
	}
	entry := memberSpaceEntryJS
	baseA = publishTestAppForSite(t, s, "sitea", "token-a", manifest, entry)
	baseB = publishTestAppForSite(t, s, "siteb", "token-b", manifest, entry)
	return s, baseA, baseB
}

func callWithCookie(s *Server, method, path, cookie string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func setCookieFrom(w *httptest.ResponseRecorder) string {
	raw := w.Result().Header.Get("Set-Cookie")
	return strings.SplitN(raw, ";", 2)[0]
}

func TestMemberCanRegisterLoginAndSeeCurrentIdentity(t *testing.T) {
	s, baseA, _ := setupMembersTwoSites(t)

	reg := callWithCookie(s, http.MethodPost, baseA+"/register", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	if reg.Code != http.StatusOK {
		t.Fatalf("register: %d %s", reg.Code, reg.Body.String())
	}

	login := callWithCookie(s, http.MethodPost, baseA+"/login", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookie := setCookieFrom(login)
	if cookie == "" {
		t.Fatal("login did not set a session cookie")
	}

	who := callWithCookie(s, http.MethodGet, baseA+"/whoami", cookie, nil)
	if who.Code != http.StatusOK || !strings.Contains(who.Body.String(), "alice") {
		t.Fatalf("whoami: %d %s", who.Code, who.Body.String())
	}
}

// TestMemberSessionCookieRejectedCrossSite : le cookie de session émis sur
// le site A, réutilisé tel quel sur le site B, doit être rejeté par le
// runtime (la clé de signature est propre à chaque site).
func TestMemberSessionCookieRejectedCrossSite(t *testing.T) {
	s, baseA, baseB := setupMembersTwoSites(t)

	callWithCookie(s, http.MethodPost, baseA+"/register", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	login := callWithCookie(s, http.MethodPost, baseA+"/login", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	cookie := setCookieFrom(login)
	if cookie == "" {
		t.Fatal("login did not set a session cookie on site A")
	}

	// Le même cookie, réutilisé sur le site B, ne doit jamais révéler d'identité.
	who := callWithCookie(s, http.MethodGet, baseB+"/whoami", cookie, nil)
	if strings.Contains(who.Body.String(), "alice") {
		t.Fatalf("cross-site session cookie accepted on site B: %d %s", who.Code, who.Body.String())
	}
	if who.Code != http.StatusOK && who.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status for cross-site cookie: %d %s", who.Code, who.Body.String())
	}
	if !strings.Contains(who.Body.String(), `"user":null`) && who.Code != http.StatusUnauthorized {
		t.Fatalf("site B must report no current user for site A's cookie, got: %s", who.Body.String())
	}
}

func TestMemberLogoutInvalidatesExistingCookie(t *testing.T) {
	s, baseA, _ := setupMembersTwoSites(t)

	callWithCookie(s, http.MethodPost, baseA+"/register", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	login := callWithCookie(s, http.MethodPost, baseA+"/login", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	cookie := setCookieFrom(login)

	before := callWithCookie(s, http.MethodGet, baseA+"/whoami", cookie, nil)
	if !strings.Contains(before.Body.String(), "alice") {
		t.Fatalf("expected identity before logout, got %s", before.Body.String())
	}

	logout := callWithCookie(s, http.MethodPost, baseA+"/logout", cookie, nil)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", logout.Code, logout.Body.String())
	}

	after := callWithCookie(s, http.MethodGet, baseA+"/whoami", cookie, nil)
	if strings.Contains(after.Body.String(), "alice") {
		t.Fatalf("same cookie still authenticated after logout: %s", after.Body.String())
	}
}
