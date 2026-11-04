package streambuf

import (
	"testing"
	"time"
)

// Steady state of the writer: every chunk read from the process is copied
// into pooled memory, appended to the ring and, once it ages out, released.
func BenchmarkChunkAppend(b *testing.B) {
	out := make([]byte, 7*tsPacketSize*10) // a 13 KB read
	ins := newInspector()
	r := newRing(64*len(out), time.Hour, 64<<10) // holds 64 chunks
	now := time.Now()
	poisonReleased = false // as in production
	defer func() { poisonReleased = true }()

	b.ReportAllocs()
	b.SetBytes(int64(len(out)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.append(makeChunk(out, modeOpaque, ins, now))
	}
}
