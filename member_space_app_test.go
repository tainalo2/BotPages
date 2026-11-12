package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestMemberSpaceAppPackageEndToEnd publie le package réel (manifest.json +
// entry.js sous member-space-app/, exactement ce que publie publish.sh) et
// exerce le parcours complet register -> login -> page d'identité -> logout
// au travers du serveur HTTP, pour prouver que l'artefact livré fonctionne
// et pas seulement une copie inline utilisée par les tests unitaires.
func TestMemberSpaceAppPackageEndToEnd(t *testing.T) {
	manifestBytes, err := os.ReadFile("member-space-app/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	entryBytes, err := os.ReadFile("member-space-app/entry.js")
	if err != nil {
		t.Fatal(err)
	}
	var manifest AppManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}

	s, err := NewServer(Config{Dir: t.TempDir(), AdminToken: "admin-secret", Bots: map[string]string{"members-site": "token-site"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := publishTestAppForSite(t, s, "members-site", "token-site", manifest, string(entryBytes))

	home := callWithCookie(s, http.MethodGet, base+"/", "", nil)
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Non connecté") {
		t.Fatalf("home page before login: %d %s", home.Code, home.Body.String())
	}

	reg := callWithCookie(s, http.MethodPost, base+"/register", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	if reg.Code != http.StatusOK {
		t.Fatalf("register: %d %s", reg.Code, reg.Body.String())
	}

	login := callWithCookie(s, http.MethodPost, base+"/login", "", []byte(`{"username":"alice","password":"correct-horse"}`))
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookie := setCookieFrom(login)
	if cookie == "" {
		t.Fatal("login did not set a session cookie")
	}
	if !strings.Contains(cookie, "HttpOnly") && !strings.Contains(login.Result().Header.Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("session cookie missing HttpOnly: %s", login.Result().Header.Get("Set-Cookie"))
	}
	rawSetCookie := login.Result().Header.Get("Set-Cookie")
	for _, attribute := range []string{"HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(rawSetCookie, attribute) {
			t.Fatalf("session cookie missing %s: %s", attribute, rawSetCookie)
		}
	}

	homeIn := callWithCookie(s, http.MethodGet, base+"/", cookie, nil)
	if homeIn.Code != http.StatusOK || !strings.Contains(homeIn.Body.String(), "Connecté en tant que alice") {
		t.Fatalf("home page after login must show identity: %d %s", homeIn.Code, homeIn.Body.String())
	}

	who := callWithCookie(s, http.MethodGet, base+"/whoami", cookie, nil)
	if who.Code != http.StatusOK || !strings.Contains(who.Body.String(), "Connecté en tant que alice") {
		t.Fatalf("whoami: %d %s", who.Code, who.Body.String())
	}

	logout := callWithCookie(s, http.MethodPost, base+"/logout", cookie, nil)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", logout.Code, logout.Body.String())
	}

	after := callWithCookie(s, http.MethodGet, base+"/whoami", cookie, nil)
	if strings.Contains(after.Body.String(), "alice") {
		t.Fatalf("session must be rejected after logout: %s", after.Body.String())
	}
}
