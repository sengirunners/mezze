package session

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

func seedPresenceInventory(t *testing.T, h *harness, first, count int) {
	t.Helper()
	tx, err := h.d.Store.Registry().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO sessions(id,address,machine,project,agent,state,managed,started,updated)
 VALUES(?,?,'mini',?,'codex','running',0,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i := first; i < first+count; i++ {
		id := fmt.Sprintf("%08x", i)
		project := []string{"one", "two", api.ProjectNone}[i%3]
		if _, err := stmt.Exec(id, "mini/"+id, project, store.Millis(time.Now()), store.Millis(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestPresenceInventoryCompleteBeyondUIPage(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	seedPresenceInventory(t, h, 0, 501)
	for _, count := range []int{501, 1001} {
		if count == 1001 {
			seedPresenceInventory(t, h, 501, 500)
		}
		ss, err := h.s.PresenceSessions(ctx, count, 0)
		if err != nil || len(ss) != count {
			t.Fatalf("inventory at exact cap %d: count=%d err=%v", count, len(ss), err)
		}
		projects := map[string]bool{}
		ids := map[string]bool{}
		for _, s := range ss {
			projects[s.Project], ids[s.ID] = true, true
		}
		if len(projects) != 3 || len(ids) != count {
			t.Fatalf("inventory lost projects or duplicated rows: projects=%v ids=%d", projects, len(ids))
		}
		if ss, err := h.s.PresenceSessions(ctx, count-1, 0); err == nil || ss != nil {
			t.Fatalf("overflow returned a partial inventory: count=%d err=%v", len(ss), err)
		}
	}
	ui, err := h.s.List(ctx, authz.All(), orchapi.SessionQuery{})
	if err != nil || len(ui) != 500 {
		t.Fatalf("UI page limit changed: count=%d err=%v", len(ui), err)
	}
}

func TestPresenceInventoryRecentEndedAndLocalOnly(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	now := storedTime(time.Now())
	seedPresenceInventory(t, h, 0, 7)
	for i, ended := range []time.Time{now.Add(-time.Minute), now.Add(-time.Hour), now.Add(-2 * time.Hour), now.Add(-24 * time.Hour), now.Add(-25 * time.Hour)} {
		if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='ended',ended=?,outcome='{"kind":"failed","evidence":"private"}' WHERE id=?`, store.Millis(ended), fmt.Sprintf("%08x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='ended',ended=? WHERE id='00000006'`, store.Millis(now)); err != nil {
		t.Fatal(err)
	}
	// Neither a foreign session table row nor remote presence belongs here.
	if _, err := h.d.Store.Registry().Exec(`INSERT INTO sessions(id,address,machine,project,agent,state,managed,started,updated)
 VALUES('eeeeeeee','remote/eeeeeeee','remote','one','codex','running',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := h.s.RecordPresence(ctx, "one", "remote", "other", []orchapi.PresenceSession{{Address: "remote/ffffffff", Agent: "codex", State: orchapi.StateRunning, Started: now}}); err != nil {
		t.Fatal(err)
	}
	ss, err := h.s.presenceSessionsAt(ctx, 1, 2, now)
	if err != nil || len(ss) != 3 {
		t.Fatalf("separate live/ended limits: count=%d err=%v", len(ss), err)
	}
	seen := map[string]bool{}
	for _, s := range ss {
		seen[s.ID] = true
		if s.State == orchapi.StateEnded && (s.Outcome == nil || s.Outcome.Kind != orchapi.OutcomeFailed || s.Outcome.Evidence != "") {
			t.Fatalf("wrong or overbroad outcome projection: %+v", s.Outcome)
		}
	}
	if !seen["00000000"] || !seen["00000001"] || !seen["00000002"] || len(seen) != 3 {
		t.Fatalf("ended cap did not select newest outcomes: %v", seen)
	}
	ss, err = h.s.presenceSessionsAt(ctx, 1, 500, now)
	if err != nil || len(ss) != 4 { // live plus three strictly younger than 24h
		t.Fatalf("24h boundary or missing-outcome filter: count=%d err=%v", len(ss), err)
	}
}

func TestPresenceInventoryHeartbeatFieldsAndPrivacy(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	now := storedTime(time.Now())
	seedPresenceInventory(t, h, 0, 4)
	for i, command := range []string{`["/bin/codex","private argv"]`, `["/bin/sh","private argv"]`, `[""]`, `[]`} {
		if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET command=?,title='topic',note='busy',area='solver',activity='Thinking',activity_at=?,term_title='terminal topic',managed=1,shared=1 WHERE id=?`, command, store.Millis(now), fmt.Sprintf("%08x", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.d.Store.Registry().Exec(`INSERT INTO session_lineage(id,parent,depth) VALUES('00000000','mini/parent01',1)`); err != nil {
		t.Fatal(err)
	}
	// Local process overlays do not need actual child processes or samples.
	p := &proc{id8: "00000000", typedAt: now, conns: map[io.Closer]struct{}{io.NopCloser(strings.NewReader("")): {}}}
	h.s.mu.Lock()
	h.s.procs["00000000"] = p
	h.s.procs["00000001"] = &proc{id8: "00000001", typedAt: now}
	h.s.procs["00000002"] = &proc{id8: "00000002", typedAt: now.Add(-10 * time.Minute)}
	h.s.mu.Unlock()
	t.Cleanup(func() {
		h.s.mu.Lock()
		delete(h.s.procs, "00000000")
		delete(h.s.procs, "00000001")
		delete(h.s.procs, "00000002")
		h.s.mu.Unlock()
	})
	ss, err := h.s.presenceSessionsAt(ctx, 4, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Title != "topic" || s.Note != "busy" || s.Area != "solver" || s.Activity != "Thinking" || s.ActivityAt == nil || !s.ActivityAt.Equal(now) || !s.Shared || !s.Managed {
			t.Fatalf("heartbeat fields lost: %+v", s)
		}
		if s.LastPrompt != nil || s.Fg != nil || len(s.Ports) != 0 || s.RuntimeID != "" || s.Cwd != "" || len(s.Command) > 1 {
			t.Fatalf("inventory copied unrelated private/sample fields: %+v", s)
		}
		if s.ID == "00000001" && (s.InUse == nil || s.InUse.Attached != 0 || s.InUse.TypedAt == nil) {
			t.Fatalf("recent typing alone did not mark in use: %+v", s)
		}
		if s.ID == "00000002" && s.InUse != nil {
			t.Fatalf("stale typing still marked in use: %+v", s)
		}
		if s.ID == "00000000" {
			if s.Parent != "mini/parent01" || s.TermTitle != "terminal topic" || s.InUse == nil || s.InUse.Attached != 1 || s.InUse.TypedAt == nil || s.InUse.By != h.d.Projects.LocalIdentity().Name {
				t.Fatalf("lineage or shared input state lost: %+v", s)
			}
		} else if s.ID == "00000003" {
			if s.TermTitle != "terminal topic" { // a registered runtime with no known command
				t.Fatalf("runtime title lost: %+v", s)
			}
		} else if s.TermTitle != "" {
			t.Fatalf("wrapper/empty executable leaked terminal title: %+v", s)
		}
	}
}

func TestPresenceInventoryErrorsAreNotPartial(t *testing.T) {
	h := newHarness(t, nil)
	seedPresenceInventory(t, h, 0, 1)
	for _, limits := range [][2]int{{0, 0}, {10001, 0}, {1, -1}, {1, 501}} {
		if ss, err := h.s.PresenceSessions(context.Background(), limits[0], limits[1]); err == nil || ss != nil {
			t.Fatalf("invalid limits %v: %+v err=%v", limits, ss, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ss, err := h.s.PresenceSessions(ctx, 1, 0); err == nil || ss != nil {
		t.Fatalf("cancelled inventory returned rows: %+v err=%v", ss, err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET managed='invalid' WHERE id='00000000'`); err != nil {
		t.Fatal(err)
	}
	if ss, err := h.s.PresenceSessions(context.Background(), 1, 0); err == nil || ss != nil {
		t.Fatalf("scan failure returned inventory: %+v err=%v", ss, err)
	}
	if err := h.d.Store.Registry().Close(); err != nil {
		t.Fatal(err)
	}
	if ss, err := h.s.PresenceSessions(context.Background(), 1, 0); err == nil || ss != nil {
		t.Fatalf("closed database returned inventory: %+v err=%v", ss, err)
	}
}

func TestPresenceInventoryAfterStoreClose(t *testing.T) {
	h := newHarness(t, nil)
	seedPresenceInventory(t, h, 0, 2)
	// Stop background services, then exercise Store.Close itself, which clears
	// Registry rather than merely leaving a pointer to a closed sql.DB.
	h.close()
	if err := h.d.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if h.d.Store.Registry() != nil {
		t.Fatal("Store.Close did not clear the registry")
	}
	if ss, err := h.s.PresenceSessions(context.Background(), 2, 0); err == nil || ss != nil {
		t.Fatalf("closed store returned inventory: %+v err=%v", ss, err)
	}
	for _, s := range []*Service{nil, {}, {d: &core.Deps{}}} {
		if ss, err := s.PresenceSessions(context.Background(), 2, 0); err == nil || ss != nil {
			t.Fatalf("missing store returned inventory: %+v err=%v", ss, err)
		}
	}
}
