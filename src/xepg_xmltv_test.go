package src

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// legacyCountXMLTV is what checkXMLCompatibility did before (copied):
// unmarshal the whole guide and count.
func legacyCountXMLTV(body []byte) (channels, programs int, err error) {
	var xmltv XMLTV
	err = xml.Unmarshal(body, &xmltv)
	if err != nil {
		return
	}
	return len(xmltv.Channel), len(xmltv.Program), nil
}

func TestCountXMLTVMatchesUnmarshal(t *testing.T) {
	valid := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE tv SYSTEM "xmltv.dtd">
<!-- comment -->
<tv generator-info-name="g" source-info-name="s">
  <channel id="a"><display-name>A</display-name><icon src="http://x/a.png"/></channel>
  <channel id="b"><display-name>B</display-name><live>true</live></channel>
  <programme start="20261104000000 +0000" stop="20261104010000 +0000" channel="a">
    <title lang="en">T</title><credits><actor role="r">X</actor></credits><video><quality>HDTV</quality></video>
    <previously-shown start="2020"/><new/>
  </programme>
  <other><channel id="nested"/><programme channel="nested"/></other>
  <programme channel="b"><title>U</title></programme>
  text
</tv>
<trailing/>`
	cases := map[string]string{
		"valid":          valid,
		"empty":          "",
		"prolog only":    `<?xml version="1.0"?>`,
		"empty tv":       `<tv/>`,
		"wrong root":     `<rss><channel id="a"/></rss>`,
		"unclosed":       `<tv><channel id="a"><display-name>A</display-name></channel><programme channel="a">`,
		"bad nesting":    `<tv><channel id="a"></programme></tv>`,
		"bad bool":       `<tv><channel id="a"><active>yes</active></channel></tv>`,
		"bad bool late":  `<tv><channel id="a"/><programme channel="a"/><channel id="b"><live>maybe</live></channel></tv>`,
		"charset":        `<?xml version="1.0" encoding="ISO-8859-1"?><tv><channel id="a"/></tv>`,
		"namespaced":     `<x:tv xmlns:x="urn:x"><x:channel id="a"/><x:programme channel="a"/></x:tv>`,
		"text in root":   `<tv>hello<channel id="a"/>world</tv>`,
		"garbage":        `not xml at all`,
		"unclosed child": `<tv><channel id="a">`,
	}
	for name, content := range cases {
		wantC, wantP, wantErr := legacyCountXMLTV([]byte(content))
		gotC, gotP, gotErr := countXMLTV(strings.NewReader(content))
		if fmt.Sprint(wantErr) != fmt.Sprint(gotErr) || wantC != gotC || wantP != gotP {
			t.Errorf("%s: got (%d, %d, %v), want (%d, %d, %v)", name, gotC, gotP, gotErr, wantC, wantP, wantErr)
		}
	}
	if c, p, err := countXMLTV(strings.NewReader(valid)); c != 2 || p != 2 || err != nil {
		t.Fatalf("valid guide: %d channels, %d programmes, %v", c, p, err)
	}
}

func TestCountXMLTVMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("large guide")
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0"?><tv>`)
	desc := strings.Repeat("A fairly typical programme description. ", 5)
	for i := 0; i < 60000; i++ {
		fmt.Fprintf(&b, `<programme start="20261104000000 +0000" stop="20261104010000 +0000" channel="ch%d"><title lang="en">Show %d</title><desc lang="en">%s</desc><category>Series</category></programme>`+"\n", i%500, i, desc)
	}
	b.WriteString(`</tv>`)
	body := b.Bytes()

	peak := func(f func()) uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		base, max := m.HeapAlloc, m.HeapAlloc
		done := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			var m runtime.MemStats
			for {
				select {
				case <-done:
					return
				case <-time.After(time.Millisecond):
				}
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > max {
					max = m.HeapAlloc
				}
			}
		}()
		f()
		close(done)
		<-stopped
		return max - base
	}

	legacy := peak(func() { legacyCountXMLTV(body) })
	current := peak(func() { countXMLTV(bytes.NewReader(body)) })
	t.Logf("%.1f MB guide: peak heap growth %.1f MB before, %.1f MB now", float64(len(body))/1e6, float64(legacy)/1e6, float64(current)/1e6)
	if current*2 > legacy {
		t.Errorf("counting needs %.1f MB, unmarshalling %.1f MB", float64(current)/1e6, float64(legacy)/1e6)
	}
}
