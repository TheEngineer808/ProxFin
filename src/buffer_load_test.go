//go:build !windows

package src

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestBufferLoad measures the buffer under load: CHANNELS channels of real
// 8 Mbps MPEG-TS (ffmpeg replaying a sample in real time), each watched by
// VIEWERS clients (curl processes, so only Threadfin's own CPU is measured).
//
//	THREADFIN_LOAD_TEST=1 CHANNELS=20 VIEWERS=5 SECONDS=20 WARMUP=40 go test -mod=mod ./src/ -run TestBufferLoad -v -timeout 10m
func TestBufferLoad(t *testing.T) {
	if os.Getenv("THREADFIN_LOAD_TEST") == "" {
		t.Skip("set THREADFIN_LOAD_TEST=1 to run the load test")
	}
	channels, viewers := envInt("CHANNELS", 20), envInt("VIEWERS", 5)
	seconds := envInt("SECONDS", 20)

	useRealFFmpeg(t)
	for _, tool := range []string{"curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}

	// An 8 Mbps sample, replayed in real time with stream copy.
	dir := t.TempDir()
	sample := filepath.Join(dir, "sample.ts")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=1000", "-t", "10", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50",
		"-b:v", "8M", "-maxrate", "8M", "-bufsize", "8M", "-c:a", "aac", "-f", "mpegts", sample).CombinedOutput()
	if err != nil {
		t.Fatalf("creating the sample: %v %s", err, out)
	}
	Settings.FFmpegOptions = "-hide_banner -loglevel error -re -stream_loop -1 -i [URL] -c copy -f mpegts pipe:1"

	systemMutex.Lock()
	Settings.Files.M3U["Mload"] = map[string]interface{}{"buffer": "ffmpeg", "tuner": "1000", "name": "load"}
	systemMutex.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch := strings.TrimPrefix(r.URL.Path, "/")
		bufferingStream("Mload", filepath.Join(dir, ch+".ts"), nil, nil, nil, ch, w, r)
	}))
	defer srv.Close()

	for c := 0; c < channels; c++ {
		os.Symlink(sample, filepath.Join(dir, fmt.Sprintf("ch%d.ts", c)))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var received []float64
	for c := 0; c < channels; c++ {
		for v := 0; v < viewers; v++ {
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				out, _ := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{size_download}",
					"--max-time", strconv.Itoa(seconds+envInt("WARMUP", 5)), url).Output()
				n, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
				mu.Lock()
				received = append(received, n)
				mu.Unlock()
			}(fmt.Sprintf("%s/ch%d", srv.URL, c))
		}
	}

	// Let everything start (and, with a longer WARMUP, the buffers fill up
	// to their retention, the steady state of a long-running server).
	time.Sleep(time.Duration(envInt("WARMUP", 5)) * time.Second)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gcBefore, pauseBefore := ms.NumGC, ms.PauseTotalNs
	cpuBefore, wallBefore := processCPU(), time.Now()
	var peakHeap uint64
	for i := 0; i < seconds; i++ {
		time.Sleep(time.Second)
		runtime.ReadMemStats(&ms)
		if ms.HeapInuse > peakHeap {
			peakHeap = ms.HeapInuse
		}
	}
	cpu := (processCPU() - cpuBefore).Seconds() / time.Since(wallBefore).Seconds()
	runtime.ReadMemStats(&ms)

	wg.Wait()
	sort.Float64s(received)

	t.Logf("%d channels x %d viewers, measured over %d s", channels, viewers, seconds)
	t.Logf("Threadfin CPU: %.1f%% of one core (%.2f%% per channel)", cpu*100, cpu*100/float64(channels))
	t.Logf("GC: %d cycles, %.1f ms total pause; peak heap in use %.1f MB",
		ms.NumGC-gcBefore, float64(ms.PauseTotalNs-pauseBefore)/1e6, float64(peakHeap)/1e6)
	t.Logf("per viewer received: min %.1f MB, median %.1f MB, max %.1f MB",
		received[0]/1e6, received[len(received)/2]/1e6, received[len(received)-1]/1e6)

	// 8 Mbps for the whole run, less ffmpeg's start.
	if want := float64(seconds) * 1e6 * 0.9; received[0] < want {
		t.Errorf("a viewer received only %.1f MB, want at least %.1f MB", received[0]/1e6, want/1e6)
	}
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
