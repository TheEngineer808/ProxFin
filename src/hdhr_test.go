package src

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// oldGetNewPMSID is the previous algorithm, kept to prove the new one
// assigns the same guide numbers.
func oldGetNewPMSID(cache map[string]string) (id string) {
	var i int
newID:
	var ids []string
	id = fmt.Sprintf("id-%d", i)
	for _, v := range cache {
		ids = append(ids, v)
	}
	if indexOfString(id, ids) != -1 {
		i++
		goto newID
	}
	return
}

func TestGuideNumberPMSMatchesThePreviousAlgorithm(t *testing.T) {
	saved := System.File.PMS
	System.File.PMS = filepath.Join(t.TempDir(), "pms.json")
	t.Cleanup(func() { System.File.PMS = saved; pmsGuideNumbers.used = nil; Data.Cache.PMS = nil })

	// Existing guide numbers with gaps (id-1, id-3, id-4 are free).
	existing := map[string]interface{}{"A": "id-0", "B": "id-2", "C": "id-5", "D": "id-6"}
	if err := saveMapToJSONFile(System.File.PMS, existing); err != nil {
		t.Fatal(err)
	}

	old := map[string]string{}
	for k, v := range existing {
		old[k] = v.(string)
	}

	pmsGuideNumbers.used = nil
	names := []string{"A", "E", "F", "B", "G", "H", "E", "I", "J", "C", "K"}
	for _, name := range names {
		got, err := getGuideNumberPMS(name)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := old[name]
		if !ok {
			want = oldGetNewPMSID(old)
			old[name] = want
		}
		if got != want {
			t.Fatalf("%s: got %s, the previous algorithm gives %s", name, got, want)
		}
	}
	if !pmsGuideNumbers.dirty {
		t.Error("new guide numbers must be marked for saving")
	}
}

// Thousands of new channels used to take minutes per /lineup.json request.
func TestGuideNumberPMSScales(t *testing.T) {
	saved := System.File.PMS
	System.File.PMS = filepath.Join(t.TempDir(), "pms.json")
	t.Cleanup(func() { System.File.PMS = saved; pmsGuideNumbers.used = nil; Data.Cache.PMS = nil })
	if err := saveMapToJSONFile(System.File.PMS, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}

	pmsGuideNumbers.used = nil
	start := time.Now()
	for i := 0; i < 20000; i++ {
		id, err := getGuideNumberPMS(fmt.Sprintf("channel %d", i))
		if err != nil {
			t.Fatal(err)
		}
		if id != fmt.Sprintf("id-%d", i) {
			t.Fatalf("channel %d got %s", i, id)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("20000 new channels took %v", d)
	}
}
