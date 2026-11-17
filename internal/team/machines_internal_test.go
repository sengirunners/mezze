package team

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// A machine joined to projects at two authorities is one row in a
// replica's merged list: its latest numbers, all its projects.
func TestDedupMachines(t *testing.T) {
	t1, t2 := time.Unix(1000, 0).UTC(), time.Unix(2000, 0).UTC()
	rows := []api.Machine{
		{Machine: "bea-mac"}, // this machine's own (no seen)
		{Machine: "vela-mac", Projects: []string{"p1"}, Seen: &t1, Stats: api.MachineStats{Sessions: 1}},
		{Machine: "mini", Projects: []string{"p1"}, Seen: &t1},
		{Machine: "vela-mac", Projects: []string{"p2"}, Seen: &t2, Stats: api.MachineStats{Sessions: 2}},
		{Machine: "bea-mac", Projects: []string{"p2"}, Seen: &t2}, // an authority's copy of this machine
	}
	raw, _ := json.Marshal(rows)
	var got []api.Machine
	if err := json.Unmarshal(dedupMachines(raw), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Machine)
	}
	if strings.Join(names, ",") != "bea-mac,vela-mac,mini" {
		t.Fatalf("rows %v", names)
	}
	if v := got[1]; v.Stats.Sessions != 2 || !v.Seen.Equal(t2) || strings.Join(v.Projects, ",") != "p1,p2" {
		t.Fatalf("vela-mac %+v", v)
	}
	if c := got[0]; c.Seen != nil {
		t.Fatalf("this machine's own row lost to a copy: %+v", c)
	}
}

// The leases a session ended itself, by key or by native id, either way
// round: a late answer cannot bring one back, and a new claim clears it.
func TestEndedLeases(t *testing.T) {
	var c replicaCache
	c.init()
	c.claimedIn("P", "s", "issue:abc")
	c.claimedIn("P", "s", "issue:xabc")
	c.endLease("s", "abc")
	if c.holds("s", "abc") || !c.holds("s", "xabc") {
		t.Fatalf("held %v", c.held["P"]["s"])
	}
	late := map[string]bool{"issue:abc": true, "issue:xabc": true}
	c.dropEnded("P", "s", late)
	if late["issue:abc"] || !late["issue:xabc"] || !c.ended["P"]["s"]["abc"] {
		t.Fatalf("a late answer: %v, ended %v", late, c.ended["P"]["s"])
	}
	c.dropEnded("P", "s", map[string]bool{})
	if len(c.ended["P"]["s"]) != 0 {
		t.Fatalf("ended after the authority stopped listing it: %v", c.ended["P"]["s"])
	}
	for _, id := range []struct{ ended, claimed string }{{"abc", "issue:abc"}, {"issue:abc", "abc"}, {"abc", "abc"}} {
		c.endLease("s", id.ended)
		c.claimed("s", id.claimed)
		if len(c.ended["P"]["s"]) != 0 {
			t.Fatalf("ended %q, claimed %q: %v", id.ended, id.claimed, c.ended["P"]["s"])
		}
	}
}

// A session holding keys in two projects of one authority is in one of
// their heartbeats, stably (its own project when that is one, else the first
// by id it holds keys in); another authority's project gets it too. One
// project's answer never touches another's keys.
func TestPresentIn(t *testing.T) {
	var c replicaCache
	c.init()
	x := map[string]bool{"A": true, "B": true} // one authority's joined projects
	y := map[string]bool{"C": true}            // another authority's
	own := orchapi.Session{Address: "m/1", Project: "B"}
	if got := c.presentIn(own, x); got != "B" {
		t.Fatalf("own project: %q", got)
	}
	elsewhere := orchapi.Session{Address: "m/2", Project: "local"}
	if got := c.presentIn(elsewhere, x); got != "" {
		t.Fatalf("holding nothing: %q", got)
	}
	c.claimedIn("B", "m/2", "issue:b1")
	c.claimedIn("A", "m/2", "issue:a1")
	c.claimedIn("C", "m/2", "other:c1")
	for i := 0; i < 5; i++ {
		if got := c.presentIn(elsewhere, x); got != "A" {
			t.Fatalf("beat %d: %q", i, got)
		}
	}
	if got := c.presentIn(elsewhere, y); got != "C" {
		t.Fatalf("the other authority: %q", got)
	}
	// Its own project is joined at another authority: still present here.
	ownElsewhere := orchapi.Session{Address: "m/2", Project: "C"}
	if got := c.presentIn(ownElsewhere, x); got != "A" {
		t.Fatalf("own project at another authority: %q", got)
	}
	c.dropEnded("A", "m/2", map[string]bool{})
	if !c.held["B"]["m/2"]["issue:b1"] {
		t.Fatal("A's answer touched B's keys")
	}
}

// A release through the proxy, then a heartbeat answer that left before it
// (still listing the key): the key does not come back as held, and the
// answer after (not listing it) reports nothing lost.
func TestReleasedThenStaleAnswer(t *testing.T) {
	var c replicaCache
	c.init()
	c.claimedIn("P", "s", "local:abc")
	c.released("s", "local:abc")
	if c.holds("s", "local:abc") {
		t.Fatal("still held after the release")
	}
	stale := map[string]bool{"local:abc": true}
	c.dropEnded("P", "s", stale)
	if stale["local:abc"] {
		t.Fatal("a stale answer brings the released key back")
	}
	c.dropEnded("P", "s", map[string]bool{})
	if len(c.ended["P"]["s"]) != 0 {
		t.Fatalf("ended after the authority stopped listing it: %v", c.ended["P"]["s"])
	}
	// Claimed again: held, and a later answer without it is a real loss.
	c.claimedIn("P", "s", "local:abc")
	c.claimed("s", "local:abc")
	if !c.holds("s", "local:abc") || len(c.ended["P"]["s"]) != 0 {
		t.Fatalf("claimed again: held %v ended %v", c.held["P"]["s"], c.ended["P"]["s"])
	}
}
