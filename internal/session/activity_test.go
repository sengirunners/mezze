package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// TestTitleParser: OSC 0/2 titles ending in BEL or ST, split anywhere
// across reads; other sequences are skipped, an unterminated title is
// dropped, the stored text is one clipped line.
func TestTitleParser(t *testing.T) {
	long := strings.Repeat("x", 3000)
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"bel", "a\x1b]0;hello\x07b", "hello", true},
		{"osc2 st", "\x1b]2;topic: fix bug\x1b\\rest", "topic: fix bug", true},
		{"last wins", "\x1b]0;one\x07\x1b]2;two\x07", "two", true},
		{"osc1 icon ignored", "\x1b]1;icon\x07", "", false},
		{"hyperlink skipped", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "", false},
		{"csi", "\x1b[1;31mred\x1b[0m", "", false},
		{"unterminated then csi", "\x1b]0;half\x1b[2Jmore", "", false},
		{"no number", "\x1b];x\x07", "", false},
		{"empty clears", "\x1b]0;\x07", "", true},
		{"controls and lines", "\x1b]0;\tfirst\u0085 line\x1b\\", "first line", true},
		{"long clipped", "\x1b]0;" + long + "\x07", strings.Repeat("x", orchapi.MaxActivityLen-1) + "…", true},
		{"utf8", "\x1b]0;✳ Résumé\x07", "✳ Résumé", true},
	}
	for _, tc := range cases {
		for _, split := range []int{0, 1, 2, 3} {
			var p titleParser
			var got string
			var ok bool
			feed := func(b []byte) {
				if g, k := p.feed(b); k {
					got, ok = g, true
				}
			}
			switch split {
			case 0: // one read
				feed([]byte(tc.in))
			case 1: // byte by byte
				for i := 0; i < len(tc.in); i++ {
					feed([]byte{tc.in[i]})
				}
			default: // two reads at every cut
				for cut := 0; cut <= len(tc.in); cut++ {
					p, got, ok = titleParser{}, "", false
					feed([]byte(tc.in[:cut]))
					feed([]byte(tc.in[cut:]))
					if got != tc.want || ok != tc.ok {
						t.Fatalf("%s cut at %d: %q %v, want %q %v", tc.name, cut, got, ok, tc.want, tc.ok)
					}
				}
				continue
			}
			if got != tc.want || ok != tc.ok {
				t.Errorf("%s (split %d): %q %v, want %q %v", tc.name, split, got, ok, tc.want, tc.ok)
			}
		}
	}
}

func TestClipLine(t *testing.T) {
	for in, want := range map[string]string{
		"  Editing a.go  ":  "Editing a.go",
		"one\ntwo":          "one",
		"cr\rsecond":        "cr",
		"bell\x07 \x1b[31m": "bell [31m",
		"c1\u009bx":         "c1x",
	} {
		if got := clipLine(in); got != want {
			t.Errorf("clipLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clipLine(strings.Repeat("é", 500)); len([]rune(got)) != orchapi.MaxActivityLen {
		t.Errorf("clipped to %d runes", len([]rune(got)))
	}
}

// sessionEvents collects session events from the bus.
func sessionEvents(t *testing.T, h *harness) func(time.Duration) []api.Event {
	t.Helper()
	ch, cancel := h.d.Events.Subscribe(h.owner(), events.Filter{Types: []string{"dev.zm.session."}}, 256) // the owner in person: owner-only keys too
	t.Cleanup(cancel)
	return func(wait time.Duration) []api.Event {
		var out []api.Event
		deadline := time.After(wait)
		for {
			select {
			case e := <-ch:
				out = append(out, e)
			case <-deadline:
				return out
			}
		}
	}
}

func types(evs []api.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, strings.TrimPrefix(e.Type, "dev.zm.session."))
	}
	return out
}

// TestHookActivity: the hook route records activity with the state in one
// write and one event; a repeat is no write and no event; stop keeps it.
// Local sessions carry who (the machine owner) and reach all.
func TestHookActivity(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	evs := sessionEvents(t, h)
	hook := func(state orchapi.SessionState, activity string) orchapi.Session {
		t.Helper()
		r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-act-1", State: state},
			Activity: activity})
		if err != nil {
			t.Fatal(err)
		}
		return r.Session
	}
	ss := hook(orchapi.StateIdle, "")
	evs(100 * time.Millisecond)
	ss = hook(orchapi.StateRunning, "Editing store.go\nsecret second line")
	if ss.Activity != "Editing store.go" || ss.ActivityAt == nil || ss.State != orchapi.StateRunning {
		t.Fatalf("after tool hook: %+v", ss)
	}
	if ss.Who == nil || ss.Who.Name != "me" || ss.Who.Kind != api.IdentityHuman || ss.Reach != orchapi.ReachAll {
		t.Fatalf("who/reach: %+v %q", ss.Who, ss.Reach)
	}
	got := evs(150 * time.Millisecond)
	if strings.Join(types(got), ",") != "state_changed" {
		t.Fatalf("state and activity in one call: events %v", types(got))
	}
	var data orchapi.Session
	_ = json.Unmarshal(got[0].Data, &data)
	if data.Activity != "Editing store.go" || data.Who == nil || data.Reach != orchapi.ReachAll {
		t.Fatalf("event data %+v", data)
	}
	// Same activity again: nothing written, no event.
	before := ss.Updated
	if ss = hook(orchapi.StateRunning, "Editing store.go"); store.Millis(ss.Updated) != store.Millis(before) {
		t.Fatalf("a repeated activity wrote the row: %v -> %v", before, ss.Updated)
	}
	// A new activity alone: one updated event.
	ss = hook(orchapi.StateRunning, "Searching")
	if got := types(evs(150 * time.Millisecond)); strings.Join(got, ",") != "updated" || ss.Activity != "Searching" {
		t.Fatalf("activity only: events %v, %+v", got, ss)
	}
	// Stop: idle, and the activity is cleared (TestStopClearsActivity).
	if ss = hook(orchapi.StateIdle, ""); ss.State != orchapi.StateIdle || ss.Activity != "" {
		t.Fatalf("stop: %+v", ss)
	}
	ss = hook(orchapi.StateRunning, "Searching")
	// A repeat older than activityRefresh bumps activity_at: a row write, no event.
	old := time.Now().Add(-time.Minute).UTC()
	if _, err := h.s.update(ctx, ss.ID, "", func(r *row) (bool, error) { r.ActivityAt = &old; return true, nil }); err != nil {
		t.Fatal(err)
	}
	evs(50 * time.Millisecond)
	ss = hook(orchapi.StateRunning, "Searching")
	if ss.ActivityAt == nil || time.Since(*ss.ActivityAt) > 5*time.Second {
		t.Fatalf("activity_at not refreshed: %+v", ss.ActivityAt)
	}
	if got := types(evs(150 * time.Millisecond)); len(got) != 0 {
		t.Fatalf("refresh: events %v", got)
	}
	// The list and the single read carry the same.
	l, err := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{})
	if err != nil || len(l) != 1 || l[0].Activity != "Searching" || l[0].Who == nil || l[0].Reach != orchapi.ReachAll {
		t.Fatalf("list %+v %v", l, err)
	}
	one, err := orchclient.Session(ctx, h.c, ss.ID)
	if err != nil || one.Activity != "Searching" || one.Who == nil || one.Who.Name != "me" {
		t.Fatalf("get %+v %v", one, err)
	}
	// /register keeps its frozen body: no activity field there.
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-act-1", State: orchapi.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if one, _ = orchclient.Session(ctx, h.c, ss.ID); one.Activity != "Searching" {
		t.Fatalf("register cleared activity: %+v", one)
	}
}

// TestHookActivityNewAndEnded: a first hook creates the session with its
// activity; an ended session takes none.
func TestHookActivityNewAndEnded(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	r, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-new", State: orchapi.StateRunning},
		Activity: "Thinking"})
	if err != nil || r.Session.Activity != "Thinking" || r.Session.ActivityAt == nil {
		t.Fatalf("new: %+v %v", r.Session, err)
	}
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-new", End: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.setActivity(ctx, r.Session.ID, "Reading x.go", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := orchclient.Session(ctx, h.c, r.Session.ID); got.State != orchapi.StateEnded || got.Activity != "Thinking" {
		t.Fatalf("ended: %+v", got)
	}
}

// TestTermTitle: a managed session's terminal title (OSC 0/2, split across
// writes) becomes term_title, written at most once per titleEvery, with an
// updated event; the last title before exit is kept.
func TestTermTitle(t *testing.T) {
	old, oldU := titleEvery, updatedEvery
	titleEvery, updatedEvery = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { titleEvery, updatedEvery = old, oldU })
	h := newHarness(t, nil)
	evs := sessionEvents(t, h)
	ss := h.spawn("claude", `printf '\033]0;first\007'; sleep 0.05; printf '\033]2;sec'; sleep 0.05; printf 'ond\033\\'; sleep 0.05;
printf '\033]0;third\007'; sleep 1; printf '\033]0;last\007'; sleep 0.05; exit 0`, nil)
	got := h.wait(ss.ID, "titled", func(s orchapi.Session) bool { return s.TermTitle == "third" })
	if got.State == orchapi.StateEnded {
		t.Fatalf("ended before the title: %+v", got)
	}
	got = h.wait(ss.ID, "ended", ended)
	if got.TermTitle != "last" {
		t.Fatalf("final title %q", got.TermTitle)
	}
	var titles []string
	for _, e := range evs(200 * time.Millisecond) {
		if e.Type == orchapi.EventSessionUpdated {
			var d orchapi.Session
			_ = json.Unmarshal(e.Data, &d)
			titles = append(titles, d.TermTitle)
		}
	}
	// The first title is written at once; the next ones within titleEvery
	// collapse into one write of the latest (so at most one per title, never
	// a half-read one).
	if len(titles) < 2 || len(titles) > 4 || titles[len(titles)-1] != "last" || contains(titles, "sec") {
		t.Fatalf("title events %q", titles)
	}
}

// TestPresenceActivityWhoReach: presence stores activity, activity_at and
// term_title; a change of those alone is an updated event; who is the
// replica's identity (kind from the registry) and reach the token's cap.
func TestPresenceActivityWhoReach(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	p := h.project("harbor")
	if _, err := h.d.Projects.AddArea(ctx, h.owner(), "harbor", api.Area{Name: "solver"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "build-box", Kind: api.IdentityMachine}); err != nil {
		t.Fatal(err)
	}
	evs := sessionEvents(t, h)
	at := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Millisecond)
	beat := []orchapi.PresenceSession{{Address: "bea-mac/4bb7545a", Area: "solver", Agent: "claude", State: orchapi.StateRunning,
		Started: at, Activity: "Editing a.go", ActivityAt: &at, TermTitle: "fix \x1b the bug"}}
	whole := core.WithSubject(ctx, &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{
		{Relation: authz.Member, Object: authz.ProjectObject(p.ID)}}, nil)})
	if err := h.s.RecordPresence(whole, p.ID, "bea-mac", "build-box", beat); err != nil {
		t.Fatal(err)
	}
	got, ok := h.s.loadPresence(ctx, "bea-mac/4bb7545a")
	got = h.s.dress(ctx, got)
	if !ok || got.Activity != "Editing a.go" || got.ActivityAt == nil || !got.ActivityAt.Equal(at) || got.TermTitle != "fix  the bug" {
		t.Fatalf("presence row %+v", got)
	}
	if got.Who == nil || got.Who.Name != "build-box" || got.Who.Kind != api.IdentityMachine || got.Reach != orchapi.ReachProject {
		t.Fatalf("who/reach %+v %q", got.Who, got.Reach)
	}
	if ts := types(evs(150 * time.Millisecond)); strings.Join(ts, ",") != "registered" {
		t.Fatalf("first beat events %v", ts)
	}
	// The same beat again: no event. A new activity alone: updated.
	if err := h.s.RecordPresence(whole, p.ID, "bea-mac", "build-box", beat); err != nil {
		t.Fatal(err)
	}
	if ts := types(evs(100 * time.Millisecond)); len(ts) != 0 {
		t.Fatalf("unchanged beat events %v", ts)
	}
	beat[0].Activity = "Searching"
	if err := h.s.RecordPresence(whole, p.ID, "bea-mac", "build-box", beat); err != nil {
		t.Fatal(err)
	}
	got1 := evs(150 * time.Millisecond)
	if ts := types(got1); strings.Join(ts, ",") != "updated" {
		t.Fatalf("activity beat events %v", ts)
	}
	var data orchapi.Session
	_ = json.Unmarshal(got1[0].Data, &data)
	if data.Activity != "Searching" || data.Who == nil || data.Who.Name != "build-box" || data.Reach != orchapi.ReachProject {
		t.Fatalf("event data %+v", data)
	}
	// An area-capped token: reach area. The list carries it.
	area := core.WithSubject(ctx, &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{
		{Relation: authz.Member, Object: authz.ProjectObject(p.ID)}}, &authz.Cap{Project: p.ID, Area: "solver"})})
	if err := h.s.RecordPresence(area, p.ID, "bea-mac", "build-box", beat); err != nil {
		t.Fatal(err)
	}
	l, err := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if r := find(l, "bea-mac/4bb7545a"); r == nil || r.Reach != orchapi.ReachArea || r.Activity != "Searching" || r.Who == nil || r.Who.Kind != api.IdentityMachine {
		t.Fatalf("list %+v", l)
	}

	// An older replica sending a shell's title: the authority does not keep it.
	shellBeat := append(beat, orchapi.PresenceSession{Address: "bea-mac/5bb7545a", Area: "solver", Agent: "shell",
		State: orchapi.StateRunning, Started: at, TermTitle: "ssh prod-db-01"})
	if err := h.s.RecordPresence(whole, p.ID, "bea-mac", "build-box", shellBeat); err != nil {
		t.Fatal(err)
	}
	if sh, ok := h.s.loadPresence(ctx, "bea-mac/5bb7545a"); !ok || sh.TermTitle != "" {
		t.Fatalf("a shell's title kept: %+v", sh)
	}
}

func find(ss []orchapi.Session, addr string) *orchapi.Session {
	for i := range ss {
		if ss[i].Address == addr {
			return &ss[i]
		}
	}
	return nil
}

// TestUpdatedCoalesced: at most one stored updated event per session per
// updatedEvery; changes inside the gap go out once, trailing, as the session
// is then; another event of the session cancels the trailing one.
func TestUpdatedCoalesced(t *testing.T) {
	old := updatedEvery
	updatedEvery = 400 * time.Millisecond
	t.Cleanup(func() { updatedEvery = old })
	h := newHarness(t, nil)
	ctx := context.Background()
	evs := sessionEvents(t, h)
	hook := func(state orchapi.SessionState, activity string) {
		t.Helper()
		if _, err := orchclient.Hook(ctx, h.c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-coal", State: state},
			Activity: activity}); err != nil {
			t.Fatal(err)
		}
	}
	hook(orchapi.StateRunning, "Thinking")
	evs(100 * time.Millisecond)
	for _, a := range []string{"Reading a.go", "Editing a.go", "Searching", "Running the tests"} {
		hook(orchapi.StateRunning, a)
	}
	got := evs(700 * time.Millisecond)
	var acts []string
	for _, e := range got {
		var d orchapi.Session
		_ = json.Unmarshal(e.Data, &d)
		acts = append(acts, strings.TrimPrefix(e.Type, "dev.zm.session.")+":"+d.Activity)
	}
	if strings.Join(acts, ",") != "updated:Reading a.go,updated:Running the tests" {
		t.Fatalf("events %v", acts)
	}
	// Inside the gap a state change carries the activity; no trailing updated.
	hook(orchapi.StateRunning, "Editing b.go")
	hook(orchapi.StateIdle, "")
	if ts := types(evs(700 * time.Millisecond)); strings.Join(ts, ",") != "updated,state_changed" &&
		strings.Join(ts, ",") != "state_changed" {
		t.Fatalf("events %v", ts)
	}
	stored, err := h.d.Events.Since(ctx, h.owner(), "", events.Filter{Types: []string{orchapi.EventSessionUpdated}}, 0)
	if err != nil || len(stored) > 3 {
		t.Fatalf("stored updated events %d %v", len(stored), err)
	}
}

// TestPresenceMovedProject: a replica's session that shows up in another
// project's heartbeat leaves the old project (dev.zm.session.left there,
// nothing about the new one) and registers in the new one; the old
// project's next heartbeat does not end it.
func TestPresenceMovedProject(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	p1, p2 := h.project("one"), h.project("two")
	evs := sessionEvents(t, h)
	at := time.Now().UTC()
	beat := []orchapi.PresenceSession{{Address: "bea-mac/4bb7545a", Agent: "claude", State: orchapi.StateRunning, Started: at}}
	if err := h.s.RecordPresence(ctx, p1.ID, "bea-mac", "bea", beat); err != nil {
		t.Fatal(err)
	}
	evs(100 * time.Millisecond)
	if err := h.s.RecordPresence(ctx, p2.ID, "bea-mac", "bea", beat); err != nil {
		t.Fatal(err)
	}
	got := evs(150 * time.Millisecond)
	if ts := types(got); strings.Join(ts, ",") != "left,registered" {
		t.Fatalf("events %v", ts)
	}
	if got[0].Project != p1.ID || strings.Contains(string(got[0].Data), p2.ID) || got[1].Project != p2.ID {
		t.Fatalf("left %s %s; registered in %s", got[0].Project, got[0].Data, got[1].Project)
	}
	// p1's own heartbeat, now without it: nothing to end there.
	if err := h.s.RecordPresence(ctx, p1.ID, "bea-mac", "bea", nil); err != nil {
		t.Fatal(err)
	}
	if ts := types(evs(100 * time.Millisecond)); len(ts) != 0 {
		t.Fatalf("old project's beat: events %v", ts)
	}
	if ss, ok := h.s.loadPresence(ctx, "bea-mac/4bb7545a"); !ok || ss.Project != p2.ID || ss.State != orchapi.StateRunning {
		t.Fatalf("row %+v", ss)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{Project: "one"}); len(l) != 0 {
		t.Fatalf("still in one: %+v", l)
	}
}

// TestTermTitleOwnerOnly: a shell's terminal title reaches only the
// machine's owner: a token (or tailnet) reader of the project gets the
// session without it, in GET /v0/sessions, GET /v0/sessions/{id} and the
// session's events; an agent runtime's title reaches every reader.
func TestTermTitleOwnerOnly(t *testing.T) {
	old := titleEvery
	titleEvery = 50 * time.Millisecond
	t.Cleanup(func() { titleEvery = old })
	h := newHarness(t, nil)
	ctx := context.Background()
	repo := t.TempDir()
	p := h.project("acme", repo)
	bin := filepath.Join(repo, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in --version) echo 1.0; exit 0;; esac\nprintf '\\033]0;Fix the bug\\007'\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh := h.spawn("shell", `printf '\033]0;ssh prod-db-01\007'; exec sleep 30`, func(r *orchapi.SpawnRequest) { r.Cwd = repo })
	cl, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "claude", Command: []string{bin}, Cwd: repo})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(sh.ID, "titled", func(s orchapi.Session) bool { return s.TermTitle == "ssh prod-db-01" })
	h.wait(cl.ID, "titled", func(s orchapi.Session) bool { return s.TermTitle == "Fix the bug" })

	m, err := h.d.Projects.Mint(ctx, h.owner(), api.MintTokenRequest{As: "vida", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	tc := *h.c
	tc.Token = m.Secret
	l, err := orchclient.Sessions(ctx, &tc, orchapi.SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if r := find(l, sh.Address); r == nil || r.TermTitle != "" {
		t.Fatalf("token list: a shell's title: %+v", r)
	}
	if r := find(l, cl.Address); r == nil || r.TermTitle != "Fix the bug" {
		t.Fatalf("token list: an agent's title: %+v", r)
	}
	if one, err := orchclient.Session(ctx, &tc, sh.ID); err != nil || one.TermTitle != "" {
		t.Fatalf("token get: %+v %v", one, err)
	}
	if one, _ := orchclient.Session(ctx, h.c, sh.ID); one.TermTitle != "ssh prod-db-01" {
		t.Fatalf("owner get lost the title: %+v", one)
	}
	member := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(p.ID)}}, nil)}
	evs, err := h.d.Events.Since(ctx, member, "", events.Filter{Subject: sh.Address}, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	for _, e := range evs {
		if strings.Contains(string(e.Data), `"term_title"`) {
			t.Fatalf("a member's event carries the shell's title: %s %s", e.Type, e.Data)
		}
	}
	own, _ := h.d.Events.Since(ctx, h.owner(), "", events.Filter{Subject: sh.Address, Types: []string{orchapi.EventSessionUpdated}}, 0)
	if len(own) == 0 || !strings.Contains(string(own[len(own)-1].Data), `"term_title":"ssh prod-db-01"`) {
		t.Fatalf("the owner's events lost the title: %v", own)
	}
}

// TestUpdatedWaitsForStarted: an updated event of a session spawn
// holds (live, not yet announced) waits for its started event and then goes
// out after it, as the session is then; a client upserting by event never
// sees an update of a session it was not told started.
func TestUpdatedWaitsForStarted(t *testing.T) {
	old := updatedEvery
	updatedEvery = 10 * time.Millisecond
	t.Cleanup(func() { updatedEvery = old })
	h := newHarness(t, nil)
	evs := sessionEvents(t, h)
	ss := h.spawn("shell", `exec sleep 30`, nil)
	evs(100 * time.Millisecond)
	h.s.updates.hold(ss.Address)
	h.s.publish(orchapi.EventSessionUpdated, ss)
	h.s.publish(activityOnly, ss)
	if ts := types(evs(100 * time.Millisecond)); len(ts) != 0 {
		t.Fatalf("held updated went out before started: %v", ts)
	}
	h.s.publish(orchapi.EventSessionStarted, ss)
	// (The sampler may add an updated of its own; it never goes first.)
	updatedOnly := func(ts []string) bool {
		for _, ty := range ts {
			if ty != "updated" {
				return false
			}
		}
		return len(ts) > 0
	}
	if ts := types(evs(200 * time.Millisecond)); len(ts) < 2 || ts[0] != "started" || !updatedOnly(ts[1:]) {
		t.Fatalf("events %v, want started then updated", ts)
	}
	h.s.publish(orchapi.EventSessionUpdated, ss) // released: no longer held
	if ts := types(evs(200 * time.Millisecond)); !updatedOnly(ts) {
		t.Fatalf("after release: events %v", ts)
	}
}

// TestUpdatesForgetEnded: an ended session's coalescing slot goes, also
// when its trailing timer is firing at that moment, and a timer of a
// forgotten slot publishes nothing.
func TestUpdatesForgetEnded(t *testing.T) {
	old := updatedEvery
	updatedEvery = 30 * time.Millisecond
	t.Cleanup(func() { updatedEvery = old })
	h := newHarness(t, nil)
	evs := sessionEvents(t, h)
	ss := orchapi.Session{Address: "mini/0badc0de", ID: "0badc0de", Machine: "mini", Project: api.ProjectNone, State: orchapi.StateRunning}
	for i := 0; i < 50; i++ {
		h.s.publishUpdated(ss, false) // the first publishes, the rest schedule a trailing one
		h.s.publishUpdated(ss, false)
		time.Sleep(time.Duration(i%7) * 5 * time.Millisecond) // sometimes while the timer fires
		h.s.updates.settle(ss.Address, true)
	}
	time.Sleep(100 * time.Millisecond)
	h.s.updates.mu.Lock()
	n := len(h.s.updates.m)
	h.s.updates.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d slots left after the session ended", n)
	}
	for _, e := range evs(50 * time.Millisecond) {
		if e.Type != orchapi.EventSessionUpdated {
			t.Fatalf("unexpected %s", e.Type)
		}
	}
}

// This machine's numbers come from the probe, sampled at most every
// machineEvery. The disk's come from one statfs: a free figure from the
// probe (the harness's fake says 100000 MB) exceeded a small VM's total.
func TestMachineStatsSampled(t *testing.T) {
	h := newHarness(t, nil)
	h.s.Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 1 << 40} // more than any disk
	}
	ctx := context.Background()
	a := h.s.MachineStats(ctx)
	if a.MemoryTotalMB <= 0 || a.MemoryUsedMB < 0 || a.MemoryUsedMB > a.MemoryTotalMB || a.DiskTotalMB <= 0 ||
		a.DiskFreeMB > a.DiskTotalMB || a.MaxSessions != 0 || a.Sessions != 0 {
		t.Fatalf("stats %+v", a)
	}
	h.spawn("shell", `exec sleep 30`, nil)
	if b := h.s.MachineStats(ctx); !b.At.Equal(a.At) || b.Sessions != 0 {
		t.Fatalf("resampled within machineEvery: %+v", b)
	}
	old := machineEvery
	machineEvery = 0
	t.Cleanup(func() { machineEvery = old })
	if b := h.s.MachineStats(ctx); b.Sessions != 1 {
		t.Fatalf("not resampled: %+v", b)
	}
}
