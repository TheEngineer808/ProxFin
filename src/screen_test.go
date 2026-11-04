package src

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

func quietWebLog(t *testing.T, entries int) {
	t.Helper()
	savedMax, savedInfo := Settings.LogEntriesRAM, System.Flag.Info
	Settings.LogEntriesRAM = entries
	System.Flag.Info = false
	log.SetOutput(io.Discard)
	resetWebLog()
	t.Cleanup(func() {
		Settings.LogEntriesRAM, System.Flag.Info = savedMax, savedInfo
		log.SetOutput(os.Stderr)
		resetWebLog()
	})
}

// Once the log was full, every other new message was dropped, and more
// than twice the limit (errors and warnings didn't trim) wiped it.
func TestWebLogKeepsTheNewestEntries(t *testing.T) {
	quietWebLog(t, 50)

	for i := 0; i < 130; i++ {
		switch i % 10 {
		case 3:
			ShowError(fmt.Errorf("e%d", i), 0)
		case 7:
			showWarning(0)
		default:
			showInfo(fmt.Sprintf("Test:msg-%d", i))
		}
	}

	logs := webLogSnapshot()
	if len(logs.Log) != 50 {
		t.Fatalf("expected 50 entries, got %d", len(logs.Log))
	}

	// The newest 50 messages (80..129), in order.
	for n, entry := range logs.Log {
		i := 80 + n
		var want string
		switch i % 10 {
		case 3:
			want = fmt.Sprintf("e%d", i)
		case 7:
			want = "[WARNING]"
		default:
			want = fmt.Sprintf("msg-%d", i)
		}
		if !strings.Contains(entry, want) {
			t.Fatalf("entry %d: expected %q, got %q", n, want, entry)
		}
	}

	if logs.Errors != 5 || logs.Warnings != 5 {
		t.Errorf("expected 5 errors and 5 warnings, got %d and %d", logs.Errors, logs.Warnings)
	}

	// A flood of errors trims too, instead of growing until the next info.
	for i := 0; i < 500; i++ {
		ShowError(fmt.Errorf("flood"), 0)
	}
	if logs = webLogSnapshot(); len(logs.Log) != 50 || logs.Errors != 50 || logs.Warnings != 0 {
		t.Errorf("after an error flood: %d entries, %d errors, %d warnings", len(logs.Log), logs.Errors, logs.Warnings)
	}
}

// Streams, refreshes and HTTP handlers log at the same time while the web
// UI reads the log (run with -race).
func TestWebLogConcurrentWriters(t *testing.T) {
	quietWebLog(t, 100)

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				switch g % 3 {
				case 0:
					showInfo(fmt.Sprintf("G%d:%d", g, i))
				case 1:
					ShowError(fmt.Errorf("g%d", g), 0)
				case 2:
					_ = webLogSnapshot()
				}
			}
		}(g)
	}
	wg.Wait()

	logs := webLogSnapshot()
	if len(logs.Log) != 100 {
		t.Errorf("expected 100 entries, got %d", len(logs.Log))
	}
	errors := 0
	for _, e := range logs.Log {
		if strings.Contains(e, "ERROR") {
			errors++
		}
	}
	if logs.Errors != errors {
		t.Errorf("error counter %d doesn't match the %d errors in the log", logs.Errors, errors)
	}
}
