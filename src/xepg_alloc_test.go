package src

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"testing"
)

// legacyChannelNumbers and legacyNewID are the closures createXEPGDatabase
// used before (logic copied verbatim).
type legacyChannelNumbers struct{ allChannelNumbers []float64 }

func (l *legacyChannelNumbers) getFreeChannelNumber(startingNumber float64) (xChannelID string) {

	sort.Float64s(l.allChannelNumbers)

	for {

		if indexOfFloat64(startingNumber, l.allChannelNumbers) == -1 {
			xChannelID = fmt.Sprintf("%g", startingNumber)
			l.allChannelNumbers = append(l.allChannelNumbers, startingNumber)
			return
		}

		startingNumber++

	}
}

func legacyNewID(channels map[string]interface{}) (xepg string) {

	var firstID = 0 //len(Data.XEPG.Channels)

newXEPGID:

	if _, ok := channels["x-ID."+strconv.FormatInt(int64(firstID), 10)]; ok {
		firstID++
		goto newXEPGID
	}

	xepg = "x-ID." + strconv.FormatInt(int64(firstID), 10)
	return
}

func TestChannelNumberAllocatorMatchesLegacy(t *testing.T) {
	starts := []float64{1000, 1000, 1000, 2000, 1500, 100.5, 0.1, 0, math.Copysign(0, -1), 1e6}
	rnd := rand.New(rand.NewSource(7))

	for round := 0; round < 40; round++ {
		legacy := &legacyChannelNumbers{}
		current := newChannelNumberAllocator()

		// numbers of the channels already in the database
		for i := rnd.Intn(400); i > 0; i-- {
			var n float64
			switch rnd.Intn(4) {
			case 0:
				n = float64(1000 + rnd.Intn(300))
			case 1:
				n = float64(2000 + rnd.Intn(50))
			case 2:
				n = 100.5 + float64(rnd.Intn(20))
			default:
				n = float64(rnd.Intn(1200))
			}
			legacy.allChannelNumbers = append(legacy.allChannelNumbers, n)
			current.add(n)
		}

		for i := 0; i < 300; i++ {
			start := starts[rnd.Intn(len(starts))]
			want := legacy.getFreeChannelNumber(start)
			got := current.take(start)
			if got != want {
				t.Fatalf("round %d, call %d (start %v): got %s, want %s", round, i, start, got, want)
			}
		}
	}
}

func TestChannelNumberAllocatorEdgeCases(t *testing.T) {
	c := newChannelNumberAllocator()
	if got := c.take(math.NaN()); got != "NaN" {
		t.Fatalf("NaN start: %s", got)
	}
	if got := c.take(math.NaN()); got != "NaN" {
		t.Fatalf("second NaN start: %s", got)
	}
	if len(c.used) != 0 || len(c.next) != 0 {
		t.Fatal("NaN entries were stored")
	}
	c.add(math.Inf(1))
	if got := c.take(math.Inf(1)); got != "+Inf" { // the old search never returned
		t.Fatalf("+Inf start: %s", got)
	}
}

func TestXEPGIDAllocatorMatchesLegacy(t *testing.T) {
	rnd := rand.New(rand.NewSource(3))

	for round := 0; round < 30; round++ {
		legacyChannels := map[string]interface{}{}
		channels := map[string]interface{}{}
		for i := 0; i < 300; i++ {
			if rnd.Intn(3) > 0 {
				id := "x-ID." + strconv.Itoa(i)
				legacyChannels[id], channels[id] = true, true
			}
		}
		legacyChannels["other"], channels["other"] = true, true

		var a xepgIDAllocator
		for i := 0; i < 400; i++ {
			want := legacyNewID(legacyChannels)
			got := a.newID(channels)
			if got != want {
				t.Fatalf("round %d, call %d: got %s, want %s", round, i, got, want)
			}
			legacyChannels[want], channels[got] = true, true
		}
	}
}

func BenchmarkChannelNumbers4000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		c := newChannelNumberAllocator()
		for n := 0; n < 4000; n++ {
			c.take(1000)
		}
	}
}

func BenchmarkChannelNumbers4000Legacy(b *testing.B) {
	for i := 0; i < b.N; i++ {
		l := &legacyChannelNumbers{}
		for n := 0; n < 4000; n++ {
			l.getFreeChannelNumber(1000)
		}
	}
}

func BenchmarkNewID20000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		channels := map[string]interface{}{}
		var a xepgIDAllocator
		for n := 0; n < 20000; n++ {
			channels[a.newID(channels)] = true
		}
	}
}

func BenchmarkNewID20000Legacy(b *testing.B) {
	for i := 0; i < b.N; i++ {
		channels := map[string]interface{}{}
		for n := 0; n < 20000; n++ {
			channels[legacyNewID(channels)] = true
		}
	}
}
