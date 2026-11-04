package src

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

var filterTestRules = []Filter{
	{Type: "group-title", Rule: "News"},
	{Type: "group-title", Rule: "news {HD}"},
	{Type: "group-title", Rule: "Sports !{SD}"},
	{Type: "group-title", Rule: "Sports {HD, FHD} !{SD}"},
	{Type: "group-title", Rule: "Movies !{SD} {HD}"},
	{Type: "group-title", Rule: "Kids {}"},
	{Type: "group-title", Rule: "Kids !{x,}"},
	{Type: "group-title", Rule: "Movies", CaseSensitive: true},
	{Type: "group-title", Rule: "MOVIES {Hd}", CaseSensitive: true},
	{Type: "group-title", Rule: "News", LiveEvent: true},
	{Type: "group-title", Rule: ""},
	{Type: "group-title", Rule: "", LiveEvent: true},
	{Type: "custom-filter", Rule: "hd"},
	{Type: "custom-filter", Rule: "Sport", CaseSensitive: true},
	{Type: "custom-filter", Rule: "uk: {News}"},
	{Type: "custom-filter", Rule: "ppv !{replay}", LiveEvent: true},
	{Type: "custom-filter", Rule: "a.b {c}"},
	{Type: "unknown", Rule: "News"},
}

func filterTestStreams() (streams []map[string]string) {
	groups := []string{"News", "news", "NEWS", "Sports", "Movies", "MOVIES", "Kids", "UK: News", "PPV", ""}
	names := []string{"BBC One HD", "CNN SD", "Sky Sports FHD", "ESPN", "Movie hd", "PPV Event 1 replay", "UK: News 24", "x", "a.b c", ""}
	for i := 0; i < 400; i++ {
		g, n := groups[i%len(groups)], names[(i/len(groups))%len(names)]
		s := map[string]string{"name": n, "_values": fmt.Sprintf("tvg%d %s %s", i, g, n)}
		if i%7 != 0 {
			s["group-title"] = g
		}
		if i%5 == 0 {
			s["_values"] += "\r\n"
		}
		if i%11 == 0 {
			delete(s, "name")
		}
		streams = append(streams, s)
	}
	return
}

// The prepared filters must give the same result as the filter that parsed
// every rule for every stream.
func TestMatchStreamMatchesLegacy(t *testing.T) {
	saved := Data.Filter
	t.Cleanup(func() { Data.Filter = saved })

	streams := filterTestStreams()
	rnd := rand.New(rand.NewSource(1))
	checks, passed, live := 0, 0, 0

	for round := 0; round < 300; round++ {
		var filters []Filter
		if round < len(filterTestRules) {
			filters = []Filter{filterTestRules[round]}
		} else {
			for n := rnd.Intn(7); n > 0; n-- {
				filters = append(filters, filterTestRules[rnd.Intn(len(filterTestRules))])
			}
		}
		Data.Filter = filters
		prepared := prepareFilters(filters)

		for _, s := range streams {
			wantStatus, wantLive := legacyFilterThisStream(s)
			gotStatus, gotLive := matchStream(s, prepared)
			if gotStatus != wantStatus || gotLive != wantLive {
				t.Fatalf("filters %+v, stream %v: got (%v, %v), want (%v, %v)", filters, s, gotStatus, gotLive, wantStatus, wantLive)
			}
			if st, lv := filterThisStream(s); st != wantStatus || lv != wantLive {
				t.Fatalf("filterThisStream differs for %v", s)
			}
			checks++
			if wantStatus {
				passed++
			}
			if wantLive {
				live++
			}
		}
	}
	t.Logf("%d filter/stream combinations match (%d pass, %d live event)", checks, passed, live)
	if passed == 0 || passed == checks || live == 0 {
		t.Fatal("the test data does not exercise both results")
	}
}

func TestMatchStreamSpeed(t *testing.T) {
	saved := Data.Filter
	t.Cleanup(func() { Data.Filter = saved })

	var filters []Filter
	for i := 0; i < 20; i++ { // no match: every filter is evaluated
		filters = append(filters, Filter{Type: "group-title", Rule: fmt.Sprintf("Group %d {HD} !{SD}", 1000+i)})
	}
	Data.Filter = filters
	var streams []map[string]string
	for i := 0; i < 5000; i++ {
		streams = append(streams, map[string]string{"group-title": fmt.Sprintf("Group %d", i%500), "name": fmt.Sprintf("Channel %d HD", i), "_values": fmt.Sprintf("tvg%d Channel %d", i, i)})
	}

	start := time.Now()
	for _, s := range streams {
		legacyFilterThisStream(s)
	}
	legacy := time.Since(start)

	start = time.Now()
	prepared := prepareFilters(filters)
	for _, s := range streams {
		matchStream(s, prepared)
	}
	current := time.Since(start)

	t.Logf("5000 streams x 20 filters: before %v, now %v", legacy, current)
	if current > legacy {
		t.Errorf("prepared filters are slower (%v) than before (%v)", current, legacy)
	}
}
