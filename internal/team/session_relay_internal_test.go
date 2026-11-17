package team

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func relayEvs(from, to int64, text string) []json.RawMessage {
	var out []json.RawMessage
	for i := from; i <= to; i++ {
		out = append(out, json.RawMessage(fmt.Sprintf(`{"seq":%d,"type":"message","text":%q}`, i, text)))
	}
	return out
}

// The authority's ring: a read leaves a want, an upload fills
// the ring, a contiguous one appends, the oldest go past the byte cap, a
// read older than the ring asks again from there, a page no one waits for
// is dropped, and after a redaction a page holding the text never lands.
func TestRelayRing(t *testing.T) {
	old := relayRingMax
	t.Cleanup(func() { relayRingMax = old })
	var st relayState
	ss := orchapi.Session{Address: "bea-mac/aaaa1111", Project: "P"}
	if _, ok := st.wanted(ss.Address, "P"); ok {
		t.Fatal("a want with no reader")
	}
	st.take(ss.Address, "P", 0, orchapi.SessionEvents{Events: relayEvs(1, 3, "x"), Next: 3})
	if _, _, ok, _ := st.read(ss, 0, 10); ok {
		t.Fatal("a page no reader waited for was kept")
	}
	if w, ok := st.wanted(ss.Address, "P"); !ok || w != 0 {
		t.Fatalf("want after a read: %d %v", w, ok)
	}
	st.take(ss.Address, "P", 0, orchapi.SessionEvents{Events: relayEvs(1, 3, "x"), Next: 3, State: orchapi.StateRunning})
	page, _, ok, _ := st.read(ss, 1, 10)
	if !ok || len(page.Events) != 2 || page.Next != 3 {
		t.Fatalf("read after 1: %+v %v", page, ok)
	}
	if w, _ := st.wanted(ss.Address, "P"); w != 3 {
		t.Fatalf("want once held: %d", w)
	}
	st.take(ss.Address, "P", 3, orchapi.SessionEvents{Events: relayEvs(4, 5, "y"), Next: 5})
	if page, _, _, _ = st.read(ss, 0, 10); len(page.Events) != 5 || page.Next != 5 {
		t.Fatalf("after an append: %+v", page)
	}
	// Past the cap the oldest go; a read before them asks for them again.
	relayRingMax = 3 * len(relayEvs(1, 1, "x")[0])
	st.take(ss.Address, "P", 5, orchapi.SessionEvents{Events: relayEvs(6, 6, "x"), Next: 6})
	if page, _, ok, _ = st.read(ss, 0, 10); ok {
		t.Fatalf("a read older than the ring was answered: %+v", page)
	}
	if w, _ := st.wanted(ss.Address, "P"); w != 0 {
		t.Fatalf("want for an evicted seq: %d", w)
	}
	relayRingMax = old
	// A redaction drops the ring and refuses pages with the text.
	secret := "sk-live-0123456789abcdef"
	st.redact("P", []string{secret})
	st.read(ss, 0, 10)
	st.take(ss.Address, "P", 0, orchapi.SessionEvents{Events: relayEvs(1, 2, "the key is "+secret), Next: 2})
	if _, _, ok, err := st.read(ss, 0, 10); ok || !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a page holding a redacted text: served %v, %v", ok, err)
	}
	if _, ok := st.wanted(ss.Address, "P"); ok {
		t.Fatal("a refused page is asked for again")
	}
	st.prune() // every heartbeat prunes: a withheld ring stays
	if _, _, _, err := st.read(ss, 0, 10); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("withheld after a prune: %v", err)
	}
	st.mu.Lock()
	st.rings[ss.Address].withheld = time.Now().Add(-time.Second) // the hour is over
	st.mu.Unlock()
	st.read(ss, 0, 10)
	st.take(ss.Address, "P", 0, orchapi.SessionEvents{Events: relayEvs(1, 2, "the key is "+"[redacted]"), Next: 2})
	if page, _, ok, _ := st.read(ss, 0, 10); !ok || len(page.Events) != 2 {
		t.Fatalf("a scrubbed page: %+v %v", page, ok)
	}
	// Rings no one read lately go.
	st.mu.Lock()
	st.rings[ss.Address].until = time.Now().Add(-3 * relayWantFor)
	st.mu.Unlock()
	st.prune()
	if len(st.rings) != 0 {
		t.Fatal("a stale ring was kept")
	}
}

// An upload stays under half of relayUploadMax; an event too big alone is
// skipped by seq rather than refused every beat.
func TestRelayCapPage(t *testing.T) {
	old := relayUploadMax
	t.Cleanup(func() { relayUploadMax = old })
	one := len(relayEvs(1, 1, "x")[0])
	relayUploadMax = 2 * (2*one + 1)
	p := capPage(orchapi.SessionEvents{Events: relayEvs(1, 5, "x"), Next: 5, Ended: true})
	if len(p.Events) != 2 || p.Next != 2 || p.Ended {
		t.Fatalf("capped: %d events, next %d, ended %v", len(p.Events), p.Next, p.Ended)
	}
	big := json.RawMessage(fmt.Sprintf(`{"seq":7,"type":"message","text":%q}`, strings.Repeat("y", 4*one)))
	p = capPage(orchapi.SessionEvents{Events: append([]json.RawMessage{big}, relayEvs(8, 8, "x")...), Next: 8})
	if len(p.Events) != 0 || p.Next != 7 {
		t.Fatalf("an oversize first event: %d events, next %d", len(p.Events), p.Next)
	}
}
