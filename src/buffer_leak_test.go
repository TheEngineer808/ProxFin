//go:build !windows

package src

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Viewers tuning in and out over and over, against healthy, dropping,
// stalling and failing sources, must leave nothing behind: once they have
// all gone the goroutine count, the heap, the stream manager and the child
// processes return to where they started.
func TestBufferChurnDoesNotLeak(t *testing.T) {
	setupBufferTest(t)

	srv := newBufferServer(t, "stream", nil)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{}}

	// watch requests one channel and reads it for d, or until the server
	// ends the response, then hangs up.
	watch := func(path string, d time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	settle := func() (goroutines int, heap uint64) {
		deadline := time.Now().Add(10 * time.Second)
		for (streamManager.Stats().Streams > 0 || countProcesses(bufferTestScript) > 0) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		client.CloseIdleConnections()
		srv.CloseClientConnections()
		// Let finished goroutines unwind before counting.
		var prev int
		for i := 0; i < 40; i++ {
			runtime.GC()
			n := runtime.NumGoroutine()
			if n == prev {
				break
			}
			prev = n
			time.Sleep(50 * time.Millisecond)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return runtime.NumGoroutine(), ms.HeapInuse
	}

	// Warm up once so lazily created state (pools, embedded files, HTTP
	// transports) is part of the baseline.
	watch("/stream-warmup", 500*time.Millisecond)
	baseGoroutines, baseHeap := settle()

	modes := []string{"stream", "stream", "short", "hang", "fail"}
	const rounds = 6
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for ch, mode := range modes {
			for viewer := 0; viewer < 3; viewer++ {
				wg.Add(1)
				go func(path string, d time.Duration) {
					defer wg.Done()
					watch(path, d)
				}(fmt.Sprintf("/%s-churn%d", mode, ch), time.Duration(200+150*viewer+50*round)*time.Millisecond)
			}
		}
		wg.Wait()
	}

	goroutines, heap := settle()
	t.Logf("goroutines %d -> %d, heap in use %d KB -> %d KB",
		baseGoroutines, goroutines, baseHeap/1024, heap/1024)

	if s := streamManager.Stats(); s.Streams > 0 {
		t.Errorf("streams still registered after every viewer left: %+v", s)
	}
	if n := countProcesses(bufferTestScript); n > 0 {
		t.Errorf("%d source processes still running after every viewer left", n)
	}
	if goroutines > baseGoroutines+5 {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines grew from %d to %d after churn:\n%s",
			baseGoroutines, goroutines, buf[:runtime.Stack(buf, true)])
	}
	// The buffer holds a few MB per live stream; with none live, the heap
	// must come back close to the baseline.
	if heap > baseHeap+16<<20 {
		t.Errorf("heap in use grew from %d KB to %d KB after churn", baseHeap/1024, heap/1024)
	}
}
