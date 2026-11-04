package authentication

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setupAuth(t *testing.T) {
	t.Helper()
	mu.Lock()
	data = make(map[string]interface{})
	tokens = make(map[string]interface{})
	lastTokenCleanup = time.Time{}
	initAuthentication = false
	mu.Unlock()

	if err := Init(filepath.Join(t.TempDir(), "x"), 60); err != nil {
		t.Fatal(err)
	}
	if err := CreateDefaultUser("admin", "secret"); err != nil {
		t.Fatal(err)
	}
}

// Plex fetching /lineup.json while a client fetches /xmltv/ and the web UI
// is open: logins, token refreshes and user lookups run in parallel. With
// unsynchronised maps this was a fatal "concurrent map writes" (run with
// -race to see the data race).
func TestConcurrentAuthentication(t *testing.T) {
	setupAuth(t)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				token, err := UserAuthentication("admin", "secret")
				if err != nil {
					t.Error(err)
					return
				}
				token, err = CheckTheValidityOfTheToken(token)
				if err != nil {
					t.Error(err)
					return
				}
				id, err := GetUserID(token)
				if err != nil {
					t.Error(err)
					return
				}
				users, _ := GetAllUserData()
				for _, u := range users {
					_ = u.(map[string]interface{})["data"]
				}
				userData, _ := ReadUserData(id)
				userData["authentication.xml"] = true
				WriteUserData(id, userData)
			}
		}()
	}
	wg.Wait()
}

// Every login used to add a token that was never removed once expired.
func TestExpiredTokensAreRemoved(t *testing.T) {
	setupAuth(t)

	for i := 0; i < 1000; i++ {
		if _, err := UserAuthentication("admin", "secret"); err != nil {
			t.Fatal(err)
		}
	}

	// Let them all expire.
	mu.Lock()
	for _, v := range tokens {
		v.(map[string]interface{})["expires"] = time.Now().Add(-time.Second)
	}
	lastTokenCleanup = time.Time{}
	mu.Unlock()

	token, err := UserAuthentication("admin", "secret")
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	n := len(tokens)
	mu.Unlock()
	if n != 1 {
		t.Errorf("expected only the new token to remain, found %d tokens", n)
	}
	if _, err := CheckTheValidityOfTheToken(token); err != nil {
		t.Errorf("the new token is not valid: %v", err)
	}
}

// Callers get copies they can use after the lock is released; changing them
// doesn't change the stored users until written back.
func TestUserDataIsCopied(t *testing.T) {
	setupAuth(t)

	users, _ := GetAllUserData()
	for id, u := range users {
		u.(map[string]interface{})["_username"] = "changed"
		d, _ := ReadUserData(id)
		d["authentication.web"] = "changed"

		again, _ := GetAllUserData()
		if again[id].(map[string]interface{})["_username"] == "changed" {
			t.Error("GetAllUserData returned the stored map, not a copy")
		}
		if d2, _ := ReadUserData(id); d2["authentication.web"] == "changed" {
			t.Error("ReadUserData returned the stored map, not a copy")
		}
	}
}
