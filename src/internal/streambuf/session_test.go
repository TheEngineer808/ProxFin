package streambuf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestViewersOfAChannelShareOneProcess(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {}})
	m := NewManager(Logger{})
	req := request(l, testConfig(), src("P", "a"))

	var wg sync.WaitGroup
	got := make([]int, 20)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := m.Join(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			defer v.Close()
			data, _ := readFor(t, v, 500*time.Millisecond)
			got[i] = len(data)
		}(i)
	}
	wg.Wait()

	if n := len(l.launched()); n != 1 {
		t.Fatalf("%d processes started for one channel, want 1", n)
	}
	for i, n := range got {
		if n == 0 {
			t.Fatalf("viewer %d received nothing", i)
		}
	}
}

// Many simultaneous first tunes on one playlist: each channel gets exactly
// one process and the tuner limit holds exactly.
func TestSimultaneousTunesRespectTunerLimit(t *testing.T) {
	b := map[string]behaviour{}
	for i := 0; i < 6; i++ {
		b[fmt.Sprint("ch", i)] = behaviour{}
	}
	l := newFakeLauncher(b)
	m := NewManager(Logger{})
	cfg := testConfig()

	var mu sync.Mutex
	var viewers []*Viewer
	noTuner := 0

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		for j := 0; j < 5; j++ { // 5 viewers per channel, all at once
			wg.Add(1)
			go func(ch string) {
				defer wg.Done()
				s := src("P", ch)
				s.TunerLimit = 3
				v, err := m.Join(context.Background(), request(l, cfg, s))
				mu.Lock()
				defer mu.Unlock()
				if errors.Is(err, ErrNoTuner) {
					noTuner++
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				viewers = append(viewers, v)
			}(fmt.Sprint("ch", i))
		}
	}
	wg.Wait()

	st := m.Stats()
	if st.Streams != 3 || st.Viewers != 15 || noTuner != 15 {
		t.Fatalf("streams %d, viewers %d, refused %d; want 3, 15, 15", st.Streams, st.Viewers, noTuner)
	}
	eventually(t, time.Second, "3 processes", func() bool { return len(l.launched()) == 3 })

	for _, v := range viewers {
		v.Close()
	}
	eventually(t, 2*time.Second, "all streams stopped", func() bool {
		return m.Stats().Streams == 0 && l.running.Load() == 0
	})
	if st := m.Stats(); st.Playlists != 0 {
		t.Fatalf("tuners still counted after every stream stopped: %+v", st)
	}
}

func TestJoiningViewerStartsOnKeyframeWithTablesAndBurst(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {gop: 25}})
	m := NewManager(Logger{})
	cfg := testConfig()
	req := request(l, cfg, src("P", "a"))

	first, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	go readFor(t, first, 3*time.Second)
	time.Sleep(time.Second) // let the stream run past several keyframes

	v, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	start := time.Now()
	bufs, err := v.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if wait := time.Since(start); wait > 50*time.Millisecond {
		t.Fatalf("joining a running channel took %v", wait)
	}

	var data []byte
	for _, b := range bufs {
		data = append(data, b...)
	}

	// PAT, then PMT, then the first video packet is a keyframe.
	if pid := uint16(data[1]&0x1f)<<8 | uint16(data[2]); pid != 0 {
		t.Fatalf("first packet is PID %#x, want the PAT", pid)
	}
	if pid := uint16(data[189]&0x1f)<<8 | uint16(data[190]); pid != testPMTPID {
		t.Fatalf("second packet is PID %#x, want the PMT", pid)
	}
	if _, key := firstFrame(t, data); !key {
		t.Fatalf("viewer's video does not start on a keyframe")
	}
	if len(data) < cfg.Prebuffer {
		t.Fatalf("joining viewer got %d bytes at once, want the %d-byte cushion", len(data), cfg.Prebuffer)
	}
}

func TestFirstViewerWaitsForPrebuffer(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.Prebuffer = 64 << 10
	v, err := m.Join(context.Background(), request(l, cfg, src("P", "a")))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	bufs, err := v.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range bufs {
		n += len(b)
	}
	if n < cfg.Prebuffer {
		t.Fatalf("first delivery %d bytes, want at least the %d-byte prebuffer", n, cfg.Prebuffer)
	}
}

// A low-bitrate stream must not wait for a prebuffer it will take ages to
// fill.
func TestPrebufferWaitIsCapped(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"radio": {rate: 50 * time.Millisecond}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.Prebuffer = 10 << 20
	cfg.MaxBufferBytes = 64 << 20

	v, err := m.Join(context.Background(), request(l, cfg, src("P", "radio")))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	start := time.Now()
	if _, err := v.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wait := time.Since(start); wait > cfg.PrebufferMaxWait+300*time.Millisecond {
		t.Fatalf("waited %v, want at most about %v", wait, cfg.PrebufferMaxWait)
	}
}

func TestFailoverKeepsViewerConnected(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{
		"primary": {failNow: true, exitErr: errors.New("HTTP 404")},
		"backup":  {},
	})
	m := NewManager(Logger{})
	req := request(l, testConfig(), src("P", "primary"), src("P", "backup"))

	v, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	data, err := readFor(t, v, time.Second)
	if err != nil {
		t.Fatalf("viewer ended: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("no data from the backup")
	}
	if got := l.launched(); len(got) < 2 || got[0] != "primary" || got[1] != "backup" {
		t.Fatalf("launch order %v", got)
	}
}

func TestMidStreamFailoverGapIsShort(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{
		"primary": {runFor: 300 * time.Millisecond}, // drops before it counts as healthy
		"backup":  {},
	})
	m := NewManager(Logger{})
	req := request(l, testConfig(), src("P", "primary"), src("P", "backup"))

	v, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	last := time.Now()
	var maxGap time.Duration
	for {
		_, err := v.Next(ctx)
		if err != nil {
			break
		}
		if gap := time.Since(last); gap > maxGap {
			maxGap = gap
		}
		last = time.Now()
	}
	if maxGap > 150*time.Millisecond {
		t.Fatalf("longest pause during failover %v", maxGap)
	}
}

func TestHealthySourceIsReconnectedBeforeFailover(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{
		"primary": {runFor: 600 * time.Millisecond}, // > HealthyAfter, then drops
		"backup":  {},
	})
	m := NewManager(Logger{})
	req := request(l, testConfig(), src("P", "primary"), src("P", "backup"))

	v, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	readFor(t, v, 900*time.Millisecond)

	if got := l.launched(); len(got) < 2 || got[1] != "primary" {
		t.Fatalf("launch order %v, want the primary reconnected first", got)
	}
}

func TestEveryBackupIsTriedThenViewersAreReleased(t *testing.T) {
	fail := behaviour{failNow: true, exitErr: errors.New("connection refused")}
	l := newFakeLauncher(map[string]behaviour{"p": fail, "b1": fail, "b3": fail})
	m := NewManager(Logger{})
	req := request(l, testConfig(), src("P", "p"), src("P", "b1"), src("P", "b3"))

	v, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := v.WaitReady(ctx); !errors.Is(err, ErrStreamFailed) {
		t.Fatalf("WaitReady = %v, want ErrStreamFailed", err)
	}

	got := l.launched()
	want := []string{"p", "b1", "b3", "p"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("attempts %v, want %v", got, want)
	}
	if st := m.Stats(); st.Streams != 0 || st.Playlists != 0 {
		t.Fatalf("failed stream still holds a tuner: %+v", st)
	}
}

func TestStalledSourceIsReplaced(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{
		"primary": {runFor: 100 * time.Millisecond, hang: true},
		"backup":  {},
	})
	m := NewManager(Logger{})
	cfg := testConfig()
	req := request(l, cfg, src("P", "primary"), src("P", "backup"))

	v, err := m.Join(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	readFor(t, v, cfg.StallTimeout+600*time.Millisecond)
	if got := l.launched(); len(got) < 2 || got[1] != "backup" {
		t.Fatalf("launch order %v, want the backup after the stall", got)
	}
}

func TestSourceWithoutOutputTimesOut(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"silent": {runFor: time.Nanosecond, hang: true}})
	m := NewManager(Logger{})
	cfg := testConfig()
	v, err := m.Join(context.Background(), request(l, cfg, src("P", "silent")))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*cfg.StartTimeout)
	defer cancel()
	if err := v.WaitReady(ctx); !errors.Is(err, ErrStreamFailed) {
		t.Fatalf("WaitReady = %v, want ErrStreamFailed", err)
	}
}

func TestLingerReusesRunningStream(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {}})
	m := NewManager(Logger{})
	cfg := testConfig()
	req := request(l, cfg, src("P", "a"))

	// Probe and reconnect, as Plex does.
	v, _ := m.Join(context.Background(), req)
	readFor(t, v, 200*time.Millisecond)
	v.Close()

	time.Sleep(cfg.Linger / 2)
	v, _ = m.Join(context.Background(), req)
	start := time.Now()
	if _, err := v.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wait := time.Since(start); wait > 50*time.Millisecond {
		t.Fatalf("reconnect took %v", wait)
	}
	v.Close()

	if n := len(l.launched()); n != 1 {
		t.Fatalf("%d processes started, want the running one reused", n)
	}
	eventually(t, cfg.Linger+time.Second, "process stopped after linger", func() bool {
		return l.running.Load() == 0 && m.Stats().Streams == 0
	})
}

func TestLingeringStreamGivesUpItsTuner(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {}, "b": {}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.Linger = time.Hour
	a, b := src("P", "a"), src("P", "b")
	a.TunerLimit, b.TunerLimit = 1, 1

	v, _ := m.Join(context.Background(), request(l, cfg, a))
	readFor(t, v, 100*time.Millisecond)
	v.Close() // channel a now lingers, holding the only tuner

	start := time.Now()
	v, err := m.Join(context.Background(), request(l, cfg, b))
	if err != nil {
		t.Fatalf("switching channel with one tuner: %v", err)
	}
	defer v.Close()
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Fatalf("channel switch waited %v", d)
	}
	if st := m.Stats(); st.Streams != 1 {
		t.Fatalf("%d streams running, want only the new channel", st.Streams)
	}
}

func TestTunerWaitCoversChannelSwitchOverlap(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {}, "b": {}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.Linger = 0
	cfg.TunerWait = time.Second
	a, b := src("P", "a"), src("P", "b")
	a.TunerLimit, b.TunerLimit = 1, 1

	old, _ := m.Join(context.Background(), request(l, cfg, a))

	// The client opens the new channel before closing the old one.
	go func() {
		time.Sleep(200 * time.Millisecond)
		old.Close()
	}()

	v, err := m.Join(context.Background(), request(l, cfg, b))
	if err != nil {
		t.Fatalf("new channel refused: %v", err)
	}
	v.Close()

	cfg.TunerWait = 100 * time.Millisecond
	busy, _ := m.Join(context.Background(), request(l, cfg, a))
	defer busy.Close()
	if _, err := m.Join(context.Background(), request(l, cfg, b)); !errors.Is(err, ErrNoTuner) {
		t.Fatalf("Join = %v, want ErrNoTuner while the tuner stays busy", err)
	}
}

func TestBackupOnAnotherPlaylistUsesThatPlaylistsTuner(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{
		"p": {failNow: true, exitErr: errors.New("HTTP 503")},
		"b": {},
	})
	m := NewManager(Logger{})
	primary, backup := src("P1", "p"), src("P2", "b")
	backup.TunerLimit = 1

	v, _ := m.Join(context.Background(), request(l, testConfig(), primary, backup))
	defer v.Close()
	readFor(t, v, 300*time.Millisecond)

	m.mu.Lock()
	p1, p2 := m.tuners["P1"], m.tuners["P2"]
	m.mu.Unlock()
	if p1 != 0 || p2 != 1 {
		t.Fatalf("tuners in use: P1=%d P2=%d, want the backup's playlist to hold it", p1, p2)
	}
}

func TestSlowViewerIsMovedToLiveWithoutAffectingOthers(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {gop: 10}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.Retention = 300 * time.Millisecond
	req := request(l, cfg, src("P", "a"))

	fast, _ := m.Join(context.Background(), req)
	defer fast.Close()
	slow, _ := m.Join(context.Background(), req)
	defer slow.Close()

	var fastData []byte
	done := make(chan struct{})
	go func() {
		fastData, _ = readFor(t, fast, 1500*time.Millisecond)
		close(done)
	}()

	if _, err := slow.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(800 * time.Millisecond) // the slow viewer stops reading

	bufs, err := slow.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var resumed []byte
	for _, b := range bufs {
		resumed = append(resumed, b...)
	}
	if slow.skips != 1 {
		t.Fatalf("skips = %d, want 1", slow.skips)
	}
	if pid := uint16(resumed[1]&0x1f)<<8 | uint16(resumed[2]); pid != 0 {
		t.Fatalf("resumed data does not start with the program tables")
	}
	if _, key := firstFrame(t, resumed); !key {
		t.Fatalf("resumed data does not start on a keyframe")
	}

	<-done
	// The fast viewer got one contiguous stream: consecutive frame numbers.
	prev := -1
	for off := 0; off+tsPacketSize <= len(fastData); off += tsPacketSize {
		p := fastData[off : off+tsPacketSize]
		if !bytes.HasPrefix(p[4:], frameMagic) && !(p[3]&0x20 != 0 && bytes.HasPrefix(p[6:], frameMagic)) {
			continue
		}
		f, _ := firstFrame(t, p)
		if prev >= 0 && f != prev+1 {
			t.Fatalf("fast viewer skipped from frame %d to %d", prev, f)
		}
		prev = f
	}
}

func TestViewerContextCancelReturnsPromptly(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {runFor: time.Nanosecond, hang: true}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.StartTimeout = time.Hour
	v, _ := m.Join(context.Background(), request(l, cfg, src("P", "a")))
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := v.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next = %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Next returned %v after its context ended", d)
	}
}

// Pooled chunk memory must never be reused while a viewer still holds it:
// heavy recycling (tiny retention), viewers at random speeds, poisoned
// releases. Every viewer must receive intact packets with frame numbers
// that only move forward.
func TestPooledChunksAreNeverReusedEarly(t *testing.T) {
	l := newFakeLauncher(map[string]behaviour{"a": {rate: time.Millisecond, gop: 5}})
	m := NewManager(Logger{})
	cfg := testConfig()
	cfg.Retention = 50 * time.Millisecond
	cfg.Prebuffer = 4 << 10
	req := request(l, cfg, src("P", "a"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			v, err := m.Join(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			defer v.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			prev := -1
			for {
				bufs, err := v.Next(ctx)
				if err != nil {
					return
				}
				// A slow write, often outlasting the retention: the ring has
				// dropped these chunks by then, only the viewer holds them.
				time.Sleep(time.Duration(rng.Intn(80)) * time.Millisecond)
				for _, b := range bufs {
					if len(b)%tsPacketSize != 0 {
						t.Errorf("piece of %d bytes is not whole packets", len(b))
						return
					}
					for off := 0; off < len(b); off += tsPacketSize {
						p := b[off : off+tsPacketSize]
						if p[0] != tsSyncByte {
							t.Errorf("corrupted packet (byte %#x): chunk memory reused while held", p[0])
							return
						}
						pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
						if pid != testVideoPID || p[1]&0x40 == 0 {
							continue
						}
						f, _ := firstFrame(t, p)
						if f <= prev {
							t.Errorf("frame %d after %d", f, prev)
							return
						}
						prev = f
					}
				}
			}
		}(int64(i))
	}
	wg.Wait()
}
