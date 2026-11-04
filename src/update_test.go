package src

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The update check runs before the web server starts and, later, inside the
// maintenance scheduler. Whatever the update server answers, it must neither
// panic (which stopped Threadfin from starting, or crashed it at the daily
// update time) nor try to install anything.
func TestBinaryUpdateSurvivesBadServerResponses(t *testing.T) {
	saved := struct {
		update, auto  bool
		branch, url   string
		github, build string
	}{System.GitHub.Update, Settings.ThreadfinAutoUpdate, System.Branch, Settings.UpdateURL, System.Update.Github, System.Build}
	t.Cleanup(func() {
		System.GitHub.Update, Settings.ThreadfinAutoUpdate = saved.update, saved.auto
		System.Branch, Settings.UpdateURL, System.Update.Github, System.Build = saved.branch, saved.url, saved.github, saved.build
	})

	System.GitHub.Update = true
	Settings.ThreadfinAutoUpdate = true
	System.Flag.Info = true
	if System.Version == "" {
		System.Version, System.Build = "1.2", "40"
	}

	// A listener that accepts connections and drops them before answering:
	// the HTTP request fails without any response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	cases := []struct {
		name, branch string
		status       int
		body         string
		dropped      bool
	}{
		{name: "GitHub: no releases", branch: "Main", status: 200, body: `[]`},
		{name: "GitHub: only prereleases", branch: "Main", status: 200, body: `[{"tag_name":"2.0.0","prerelease":true}]`},
		{name: "GitHub: beta without prereleases", branch: "Beta", status: 200, body: `[{"tag_name":"0.0.1","prerelease":false}]`},
		{name: "GitHub: rate limited", branch: "Main", status: 403, body: `{"message":"API rate limit exceeded"}`},
		{name: "GitHub: unparseable tag", branch: "Main", status: 200, body: `[{"tag_name":"latest","prerelease":false}]`},
		{name: "update server: no version", branch: "Development", status: 200, body: `{"status":false}`},
		{name: "update server: connection dropped", branch: "Development", dropped: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()

			System.Branch = c.branch
			System.Update.Github = srv.URL
			Settings.UpdateURL = srv.URL
			if c.dropped {
				Settings.UpdateURL = "http://" + ln.Addr().String()
			}

			done := make(chan interface{})
			go func() {
				defer func() { done <- recover() }()
				BinaryUpdate()
			}()

			select {
			case p := <-done:
				if p != nil {
					t.Fatalf("BinaryUpdate panicked: %v", p)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("BinaryUpdate did not return")
			}
		})
	}
}
