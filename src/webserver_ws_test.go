package src

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/data/", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// expectClosed fails unless the server closes the connection within d.
func expectClosed(t *testing.T, c *websocket.Conn, d time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	_, _, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected the server to close the connection, got another message")
	}
	if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatalf("the server left the connection open for %v", d)
	}
}

// The web UI opens a websocket per request (every 10 seconds for the log).
// The server must close each one once it has answered; it used to leave
// them open until a GC finalizer happened to run.
func TestWebSocketIsClosedAfterTheAnswer(t *testing.T) {
	System.Flag.Info = true
	srv := httptest.NewServer(http.HandlerFunc(WS))
	defer srv.Close()

	c := dialWS(t, srv)
	if err := c.WriteJSON(map[string]string{"cmd": "updateLog"}); err != nil {
		t.Fatal(err)
	}
	var resp map[string]interface{}
	if err := c.ReadJSON(&resp); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c, 2*time.Second)
}

// A client that connects but never sends, or sends an oversized message,
// must not hold a socket and a goroutine forever.
func TestWebSocketDropsIdleAndOversizedClients(t *testing.T) {
	System.Flag.Info = true
	savedTimeout, savedSize := wsReadTimeout, wsMaxMessageSize
	wsReadTimeout, wsMaxMessageSize = 300*time.Millisecond, 1024
	t.Cleanup(func() { wsReadTimeout, wsMaxMessageSize = savedTimeout, savedSize })

	srv := httptest.NewServer(http.HandlerFunc(WS))
	defer srv.Close()

	t.Run("idle", func(t *testing.T) {
		expectClosed(t, dialWS(t, srv), 2*time.Second)
	})

	t.Run("oversized", func(t *testing.T) {
		c := dialWS(t, srv)
		big := `{"cmd":"updateLog","base64":"` + strings.Repeat("A", 4096) + `"}`
		if err := c.WriteMessage(websocket.TextMessage, []byte(big)); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, c, 2*time.Second)
	})
}
