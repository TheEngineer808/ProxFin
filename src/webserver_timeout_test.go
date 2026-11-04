package src

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A client that trickles its request headers, or leaves a keep-alive
// connection idle, must be disconnected instead of holding a connection
// and a goroutine forever.
func TestHTTPServerDropsSlowAndIdleClients(t *testing.T) {
	savedHeader, savedIdle := httpReadHeaderTimeout, httpIdleTimeout
	httpReadHeaderTimeout, httpIdleTimeout = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { httpReadHeaderTimeout, httpIdleTimeout = savedHeader, savedIdle })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	go srv.Serve(ln)
	defer srv.Close()

	closedWithin := func(c net.Conn, d time.Duration) bool {
		c.SetReadDeadline(time.Now().Add(d))
		_, err := io.ReadAll(c)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return false
		}
		return true
	}

	t.Run("slow headers", func(t *testing.T) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // never finishes the headers
		if !closedWithin(c, 3*time.Second) {
			t.Error("the server kept a connection with unfinished headers open")
		}
	})

	t.Run("idle keep-alive", func(t *testing.T) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		if !closedWithin(c, 3*time.Second) {
			t.Error("the server kept an idle keep-alive connection open")
		}
	})
}
