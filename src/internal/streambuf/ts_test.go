package streambuf

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func genStream(frames, gop int) []byte {
	g := newTSGen(gop)
	var b []byte
	for i := 0; i < frames; i++ {
		b = append(b, g.frameBytes()...)
	}
	return b
}

// pushRandom feeds data to a packetizer in random-sized reads.
func pushRandom(p *packetizer, data []byte, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	var out []byte
	for len(data) > 0 {
		n := 1 + rng.Intn(5000)
		if n > len(data) {
			n = len(data)
		}
		out = append(out, p.push(data[:n])...)
		data = data[n:]
	}
	return append(out, p.flush()...)
}

func TestPacketizerAlignsArbitraryReads(t *testing.T) {
	stream := genStream(500, 25)
	var p packetizer
	out := pushRandom(&p, stream, 1)

	if !bytes.Equal(out, stream) {
		t.Fatalf("output differs from input: got %d bytes, want %d", len(out), len(stream))
	}
	if p.mode != modeTS || p.dropped != 0 {
		t.Fatalf("mode %v, dropped %d", p.mode, p.dropped)
	}
}

func TestPacketizerEveryOutputIsWholePackets(t *testing.T) {
	stream := genStream(200, 25)
	var p packetizer
	rng := rand.New(rand.NewSource(2))
	for len(stream) > 0 {
		n := 1 + rng.Intn(1000)
		if n > len(stream) {
			n = len(stream)
		}
		out := p.push(stream[:n])
		stream = stream[n:]
		if len(out)%tsPacketSize != 0 {
			t.Fatalf("output of %d bytes is not whole packets", len(out))
		}
		for off := 0; off < len(out); off += tsPacketSize {
			if out[off] != tsSyncByte {
				t.Fatalf("packet at %d does not start with a sync byte", off)
			}
		}
	}
}

func TestPacketizerSkipsLeadingGarbageAndResyncs(t *testing.T) {
	good := genStream(100, 25)
	half := len(good) / 2 / tsPacketSize * tsPacketSize

	// Garbage before the stream, and a corrupted region (a partial packet
	// followed by noise) in the middle.
	var in []byte
	in = append(in, []byte("HTTP/1.1 200 OK\r\n\r\n")...)
	in = append(in, good[:half]...)
	in = append(in, good[half:half+100]...)
	in = append(in, bytes.Repeat([]byte{0x00, 0x13}, 300)...)
	in = append(in, good[half:]...)

	var p packetizer
	out := pushRandom(&p, in, 3)

	want := append(append([]byte(nil), good[:half]...), good[half:]...)
	if !bytes.Equal(out, want) {
		t.Fatalf("got %d bytes, want %d (every valid packet, nothing else)", len(out), len(want))
	}
	if p.dropped == 0 {
		t.Fatalf("dropped bytes not counted")
	}
}

func TestPacketizerPassesThroughNonTS(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	in := make([]byte, 3*tsDetectLimit)
	rng.Read(in)
	for i := range in {
		if in[i] == tsSyncByte {
			in[i] = 0
		}
	}

	var p packetizer
	out := pushRandom(&p, in, 5)
	if p.mode != modeOpaque {
		t.Fatalf("mode = %v, want pass-through", p.mode)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("pass-through changed the data")
	}
}

func joinFrames(t *testing.T, data []byte) (frames []int) {
	ins := newInspector()
	for off := 0; off < len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		if ins.packet(pkt) {
			f, key := firstFrame(t, pkt)
			if !key {
				t.Fatalf("join point at non-keyframe %d", f)
			}
			frames = append(frames, f)
		}
	}
	return
}

func TestInspectorJoinsOnlyOnVideoKeyframes(t *testing.T) {
	// Audio packets carry the random access flag on every frame (as from
	// ffmpeg); they must not count as join points when there is video.
	got := joinFrames(t, genStream(100, 25))
	want := []int{0, 25, 50, 75}
	if len(got) != len(want) {
		t.Fatalf("join points at frames %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("join points at frames %v, want %v", got, want)
		}
	}
}

func TestInspectorProgramTables(t *testing.T) {
	ins := newInspector()
	data := genStream(20, 25)
	for off := 0; off < len(data); off += tsPacketSize {
		ins.packet(data[off : off+tsPacketSize])
	}

	if !ins.videoPIDs[testVideoPID] || len(ins.videoPIDs) != 1 {
		t.Fatalf("video PIDs = %v", ins.videoPIDs)
	}
	if len(ins.psi) != 2*tsPacketSize {
		t.Fatalf("program tables are %d bytes, want PAT+PMT", len(ins.psi))
	}
	if pid := uint16(ins.psi[1]&0x1f)<<8 | uint16(ins.psi[2]); pid != 0 {
		t.Fatalf("tables do not start with the PAT (PID %d)", pid)
	}

	// Repeats of the same tables (only the continuity counter differs)
	// must not replace the snapshot.
	before := ins.psi
	ins.packet(patPacket(9))
	if &before[0] != &ins.psi[0] {
		t.Fatalf("unchanged PAT replaced the table snapshot")
	}
}

func TestInspectorRadioJoinsOnAudioFrames(t *testing.T) {
	ins := newInspector()
	ins.packet(patPacket(0))
	ins.packet(pmtPacket(0, [2]uint16{0x0f, testAudioPID}))

	if !ins.packet(tsPacket(testAudioPID, true, false, 0, []byte("AUD"))) {
		t.Fatalf("audio frame start is not a join point in an audio-only program")
	}
	if ins.packet(tsPacket(testAudioPID, false, false, 1, nil)) {
		t.Fatalf("packet without a frame start is a join point")
	}
}

func TestInspectorFallsBackWhenMuxerSetsNoRAI(t *testing.T) {
	ins := newInspector()
	ins.packet(patPacket(0))
	ins.packet(pmtPacket(0, avStreams...))

	joins := 0
	for i := 0; i < noRAIFallbackFrames+10; i++ {
		if ins.packet(tsPacket(testVideoPID, true, false, byte(i), nil)) {
			joins++
		}
	}
	if joins != 11 {
		t.Fatalf("%d join points, want frame starts after %d frames without flags", joins, noRAIFallbackFrames)
	}
}

// Real ffmpeg output: check the assumptions against the actual muxer.
func TestInspectorWithRealFFmpegOutput(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	file := filepath.Join(t.TempDir(), "sample.ts")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50",
		"-c:a", "aac", "-f", "mpegts", file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not create a sample: %v %s", err, out)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	var p packetizer
	out := pushRandom(&p, data, 6)
	ins := newInspector()
	joins := 0
	for off := 0; off < len(out); off += tsPacketSize {
		if ins.packet(out[off : off+tsPacketSize]) {
			joins++
		}
	}

	// 6 s at 25 fps with a keyframe every 50 frames.
	if joins != 3 {
		t.Fatalf("%d join points in 6 s with a 2 s GOP, want 3", joins)
	}
	if len(ins.videoPIDs) != 1 || ins.psi == nil {
		t.Fatalf("video PIDs %v, tables %d bytes", ins.videoPIDs, len(ins.psi))
	}
}
