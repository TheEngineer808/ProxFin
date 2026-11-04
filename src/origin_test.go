package src

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestCrossSiteRequestDetection(t *testing.T) {
	savedHTTP, savedHTTPS := Settings.HttpThreadfinDomain, Settings.HttpsThreadfinDomain
	t.Cleanup(func() { Settings.HttpThreadfinDomain, Settings.HttpsThreadfinDomain = savedHTTP, savedHTTPS })
	Settings.HttpThreadfinDomain, Settings.HttpsThreadfinDomain = "", ""

	cases := []struct {
		name      string
		host      string
		headers   map[string]string
		domain    string
		crossSite bool
	}{
		{name: "Plex, curl, scripts (no browser headers)", host: "nas:34400"},
		{name: "fetch metadata: same origin", host: "nas:34400", headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://nas:34400"}},
		{name: "fetch metadata: typed in", host: "nas:34400", headers: map[string]string{"Sec-Fetch-Site": "none"}},
		{name: "fetch metadata: another website", host: "nas:34400", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, crossSite: true},
		{name: "fetch metadata: cross-site image tag (no Origin)", host: "nas:34400", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, crossSite: true},
		{name: "fetch metadata: another port of the same host", host: "nas:34400", headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://nas:8080"}, crossSite: true},
		{name: "older browser: own origin", host: "nas:34400", headers: map[string]string{"Origin": "http://nas:34400"}},
		{name: "older browser: own origin, case and default port", host: "TV.example.com", headers: map[string]string{"Origin": "http://tv.example.com:80"}},
		{name: "older browser: another website", host: "nas:34400", headers: map[string]string{"Origin": "http://evil.example"}, crossSite: true},
		{name: "older browser: null origin", host: "nas:34400", headers: map[string]string{"Origin": "null"}, crossSite: true},
		{name: "reverse proxy rewriting Host", host: "127.0.0.1:34400", headers: map[string]string{"Origin": "https://tv.example.com", "X-Forwarded-Host": "tv.example.com", "X-Forwarded-Proto": "https"}},
		{name: "reverse proxy, configured domain", host: "threadfin:34400", headers: map[string]string{"Origin": "https://tv.example.com"}, domain: "tv.example.com"},
		{name: "configured domain doesn't allow others", host: "threadfin:34400", headers: map[string]string{"Origin": "https://evil.example"}, domain: "tv.example.com", crossSite: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			Settings.HttpThreadfinDomain = c.domain
			r := httptest.NewRequest(http.MethodGet, "http://"+c.host+"/data/", nil)
			r.Host = c.host
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := crossSiteRequest(r); got != c.crossSite {
				t.Errorf("crossSiteRequest = %v, want %v", got, c.crossSite)
			}
		})
	}
}

// A page on another website must not be able to use the web interface's
// websocket: with web authentication off it could change every setting,
// including the FFmpeg path and options.
func TestWebSocketRefusesOtherWebsites(t *testing.T) {
	System.Flag.Info = true
	srv := httptest.NewServer(http.HandlerFunc(WS))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/data/"

	_, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": {"https://evil.example"}})
	if err == nil {
		t.Fatal("a websocket from another website was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %v", resp)
	}

	// The web interface itself (same origin) and non-browser clients work.
	for _, h := range []http.Header{{"Origin": {srv.URL}}, {"Origin": {srv.URL}, "Sec-Fetch-Site": {"same-origin"}}, nil} {
		c, _, err := websocket.DefaultDialer.Dial(url, h)
		if err != nil {
			t.Fatalf("headers %v: %v", h, err)
		}
		c.WriteJSON(map[string]string{"cmd": "updateLog"})
		var resp map[string]interface{}
		if err := c.ReadJSON(&resp); err != nil {
			t.Errorf("headers %v: %v", h, err)
		}
		c.Close()
	}
}

// The API, the PPV endpoints and the login form refuse requests made by
// other websites; scripts without browser headers still work.
func TestStateChangingEndpointsRefuseOtherWebsites(t *testing.T) {
	savedAPI, savedXEPG, savedAuth := Settings.API, System.File.XEPG, Settings.AuthenticationWEB
	t.Cleanup(func() { Settings.API, System.File.XEPG, Settings.AuthenticationWEB = savedAPI, savedXEPG, savedAuth })
	Settings.API = true
	System.File.XEPG = filepath.Join(t.TempDir(), "xepg.json")
	os.WriteFile(System.File.XEPG, []byte("not json"), 0644)

	evil := map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}

	do := func(h http.HandlerFunc, method, path, body string, headers map[string]string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "text/plain") // what a cross-site form or no-cors fetch can send
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Code
	}

	if code := do(API, http.MethodPost, "/api/", `{"cmd":"status"}`, evil); code != http.StatusForbidden {
		t.Errorf("API from another website: %d", code)
	}
	if code := do(API, http.MethodPost, "/api/", `{"cmd":"status"}`, nil); code != http.StatusOK {
		t.Errorf("API from a script: %d", code)
	}

	for _, h := range []http.HandlerFunc{enablePPV, disablePPV} {
		if code := do(h, http.MethodGet, "/ppv/enable", "", evil); code != http.StatusForbidden {
			t.Errorf("PPV from another website: %d", code)
		}
		// A script reaches the handler (which then fails on the bad file).
		if code := do(h, http.MethodGet, "/ppv/enable", "", nil); code != http.StatusInternalServerError {
			t.Errorf("PPV from a script: %d", code)
		}
	}
	if b, _ := os.ReadFile(System.File.XEPG); string(b) != "not json" {
		t.Error("xepg.json was changed")
	}
}

// The web interface only needs each user's data; the password hashes and
// salts must not be sent to the browser.
func TestUsersForBrowserDropsHashes(t *testing.T) {
	users := map[string]interface{}{
		"id1": map[string]interface{}{
			"_id": "id1", "_username": "hash", "_password": "hash", "_salt": "salt",
			"data": map[string]interface{}{"username": "admin", "authentication.web": true},
		},
	}
	got := usersForBrowser(users)["id1"].(map[string]interface{})
	for _, k := range []string{"_username", "_password", "_salt"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s was sent to the browser", k)
		}
	}
	if got["_id"] != "id1" || got["data"].(map[string]interface{})["username"] != "admin" {
		t.Errorf("user data missing: %v", got)
	}
	if _, ok := users["id1"].(map[string]interface{})["_password"]; !ok {
		t.Error("the stored user was modified")
	}
}
