// Package storetest is the automation record store's conformance suite
// (SPEC 18.6): every store.Store driver runs Run from its own tests.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/automations/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Project is the project id List is asked about (the store is one
// project's; the id only names it to the scope).
const Project = "01JZZZZZZZZZZZZZZZZZZZZZZZ"

// Run runs the suite; open returns an empty store for each test.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	for _, c := range []struct {
		name string
		f    func(t *testing.T, s store.Store)
	}{
		{"CreateGet", createGet},
		{"OneRecordPerHomeTask", oneRecordPerHomeTask},
		{"CreateWriteIDOnce", createWriteIDOnce},
		{"UpdateRevisions", updateRevisions},
		{"UpdateWriteIDOnce", updateWriteIDOnce},
		{"Resume", resume},
		{"Tombstone", tombstone},
		{"HistoryBounds", historyBounds},
		{"HealthAndPurge", healthAndPurge},
		{"ListScope", listScope},
		{"Claims", claims},
		{"SetHolder", setHolder},
		{"Scrub", scrub},
	} {
		t.Run(c.name, func(t *testing.T) { c.f(t, open(t)) })
	}
}

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

var seq int

func rec(home, task, area string) store.Record {
	seq++
	return store.Record{ID: fmt.Sprintf("01JAUTO%019d", seq), Area: area, Kind: orchapi.AutomationKindAutomation, Home: home, TaskID: task,
		Definition: store.Definition{Name: "nightly " + task, Enabled: true, Owner: "zm-abc", Fields: json.RawMessage(`{ "on": {"schedule": "0 3 * * *"} }`),
			Body: "Run the nightly report.", Settings: map[string]orchapi.AutomationSetting{"budget": {Type: "number", Value: json.RawMessage(`5`)}}}}
}

func w(id string, at time.Time) store.Write { return store.Write{ID: id, By: "machine:home", At: at} }

func must[T any](t *testing.T, v T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ctx() context.Context { return context.Background() }

func createGet(t *testing.T, s store.Store) {
	r := rec("home", "automation:1", "solver")
	got := must2[store.Record](t)(s.Create(ctx(), r, w("", t0)))
	if got.Rev != 1 || got.ID != r.ID || got.Home != "home" || got.TaskID != "automation:1" || got.Area != "solver" || !got.Created.Equal(t0) ||
		got.UpdatedBy != "machine:home" || got.Deleted != nil || got.Health != nil {
		t.Fatalf("created %+v", got)
	}
	again := must2[store.Record](t)(s.Get(ctx(), r.ID))
	if string(again.Fields) != `{"on":{"schedule":"0 3 * * *"}}` || again.Body != r.Body || again.Settings["budget"].Type != "number" || again.Owner != "zm-abc" || !again.Enabled {
		t.Fatalf("read back %+v (%s)", again, again.Fields)
	}
	if _, err := s.Get(ctx(), "01JNOPE0000000000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	h := must2[[]store.Revision](t)(s.History(ctx(), r.ID))
	if len(h) != 1 || h[0].Op != store.OpCreate || h[0].Rev != 1 || h[0].Def.Body != r.Body {
		t.Fatalf("history %+v", h)
	}
}

// must2 adapts a (value, error) pair to must.
func must2[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T { t.Helper(); return must(t, v, err) }
}

func oneRecordPerHomeTask(t *testing.T, s store.Store) {
	a := must2[store.Record](t)(s.Create(ctx(), rec("home", "task-1", ""), w("", t0)))
	_, err := s.Create(ctx(), rec("home", "task-1", ""), w("", t0))
	var ex *store.ExistsError
	if !errors.As(err, &ex) || ex.ID != a.ID {
		t.Fatalf("same home and task id: %v", err)
	}
	must2[store.Record](t)(s.Create(ctx(), rec("other", "task-1", ""), w("", t0)))
	// A tombstone still holds its task id.
	must2[store.Record](t)(s.Delete(ctx(), a.ID, 0, w("", t0)))
	if _, err := s.Create(ctx(), rec("home", "task-1", ""), w("", t0)); !errors.As(err, &ex) || ex.ID != a.ID {
		t.Fatalf("over a tombstone: %v", err)
	}
}

func createWriteIDOnce(t *testing.T, s store.Store) {
	r := rec("home", "task-w", "")
	first := must2[store.Record](t)(s.Create(ctx(), r, w("01JWRITE00000000000000000A", t0)))
	again := rec("home", "task-w2", "") // a retry may carry other content: the first result stands
	second := must2[store.Record](t)(s.Create(ctx(), again, w("01JWRITE00000000000000000A", t0.Add(time.Minute))))
	if second.ID != first.ID || second.Rev != 1 || second.TaskID != "task-w" {
		t.Fatalf("repeat create: %+v, first %+v", second, first)
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), authz.All(), Project, store.Query{})); len(l) != 1 {
		t.Fatalf("one write id made %d records", len(l))
	}
	must2[store.Record](t)(s.Update(ctx(), first.ID, w("01JWRITE00000000000000000B", t0), rename("x")))
	if _, err := s.Create(ctx(), rec("home", "task-w3", ""), w("01JWRITE00000000000000000B", t0)); !errors.Is(err, store.ErrWriteIDUsed) {
		t.Fatalf("a create reusing an update's write id: %v", err)
	}
}

func must3[T any](t *testing.T) func(T, bool, error) (T, bool) {
	return func(v T, b bool, err error) (T, bool) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v, b
	}
}

func rename(name string) func(store.Record) (store.Definition, error) {
	return func(r store.Record) (store.Definition, error) {
		d := r.Definition
		d.Name = name
		return d, nil
	}
}

func updateRevisions(t *testing.T, s store.Store) {
	r := must2[store.Record](t)(s.Create(ctx(), rec("home", "t", ""), w("", t0)))
	u := must2[store.Record](t)(s.Update(ctx(), r.ID, store.Write{By: "machine:home", At: t0.Add(time.Hour)}, rename("renamed")))
	if u.Rev != 2 || u.Name != "renamed" || !u.Updated.Equal(t0.Add(time.Hour)) {
		t.Fatalf("update %+v", u)
	}
	same := must2[store.Record](t)(s.Update(ctx(), r.ID, w("", t0.Add(2*time.Hour)), rename("renamed")))
	if same.Rev != 2 {
		t.Fatalf("an update that changes nothing made rev %d", same.Rev)
	}
	boom := errors.New("refused")
	if _, err := s.Update(ctx(), r.ID, w("", t0), func(store.Record) (store.Definition, error) { return store.Definition{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("change error: %v", err)
	}
	if _, err := s.Update(ctx(), "01JNOPE0000000000000000000", w("", t0), rename("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	h := must2[[]store.Revision](t)(s.History(ctx(), r.ID))
	if len(h) != 2 || h[0].Rev != 2 || h[0].Op != store.OpUpdate || h[0].Def.Name != "renamed" || h[1].Def.Name != r.Name {
		t.Fatalf("history %+v", h)
	}
}

func updateWriteIDOnce(t *testing.T, s store.Store) {
	r := must2[store.Record](t)(s.Create(ctx(), rec("home", "t", ""), w("", t0)))
	runs := 0
	bump := func(cur store.Record) (store.Definition, error) {
		runs++
		d := cur.Definition
		d.Body += " again"
		return d, nil
	}
	a := must2[store.Record](t)(s.Update(ctx(), r.ID, w("01JWRITE0000000000000000UA", t0), bump))
	b := must2[store.Record](t)(s.Update(ctx(), r.ID, w("01JWRITE0000000000000000UA", t0.Add(time.Minute)), bump))
	if runs != 1 || a.Rev != 2 || b.Rev != 2 || b.Body != a.Body {
		t.Fatalf("repeat update ran %d times: %+v then %+v", runs, a, b)
	}
	// A later write moves the record on; the repeat still answers its own revision.
	must2[store.Record](t)(s.Update(ctx(), r.ID, w("", t0), rename("later")))
	c := must2[store.Record](t)(s.Update(ctx(), r.ID, w("01JWRITE0000000000000000UA", t0), bump))
	if runs != 1 || c.Rev != 2 || c.Body != a.Body || c.Name != a.Name {
		t.Fatalf("late repeat: %+v", c)
	}
	if h := must2[[]store.Revision](t)(s.History(ctx(), r.ID)); len(h) != 3 || h[1].WriteID != "01JWRITE0000000000000000UA" {
		t.Fatalf("history %+v", h)
	}
	other := must2[store.Record](t)(s.Create(ctx(), rec("home", "t2", ""), w("", t0)))
	if _, err := s.Update(ctx(), other.ID, w("01JWRITE0000000000000000UA", t0), bump); !errors.Is(err, store.ErrWriteIDUsed) {
		t.Fatalf("another record's write id: %v", err)
	}
	if _, err := s.Delete(ctx(), other.ID, 0, w("01JWRITE0000000000000000UA", t0)); !errors.Is(err, store.ErrWriteIDUsed) {
		t.Fatalf("delete with another record's write id: %v", err)
	}
}

func resume(t *testing.T, s store.Store) {
	r := must2[store.Record](t)(s.Create(ctx(), rec("home", "t", ""), w("", t0)))
	if r.ResumeRev != nil {
		t.Fatalf("resume_rev at create: %v", *r.ResumeRev)
	}
	u := must2[store.Record](t)(s.Update(ctx(), r.ID, store.Write{By: "human:ivo", At: t0, Resume: true}, func(c store.Record) (store.Definition, error) { return c.Definition, nil }))
	if u.Rev != 2 || u.ResumeRev == nil || *u.ResumeRev != 2 {
		t.Fatalf("resume: %+v", u)
	}
	v := must2[store.Record](t)(s.Update(ctx(), r.ID, w("", t0), rename("x")))
	if v.ResumeRev == nil || *v.ResumeRev != 2 {
		t.Fatalf("another write moved resume_rev: %+v", v.ResumeRev)
	}
}

func tombstone(t *testing.T, s store.Store) {
	r := must2[store.Record](t)(s.Create(ctx(), rec("home", "t", ""), w("", t0)))
	if _, err := s.Delete(ctx(), r.ID, 2, w("", t0)); !errors.Is(err, store.ErrChanged) {
		t.Fatalf("delete made from another rev: %v", err)
	}
	d := must2[store.Record](t)(s.Delete(ctx(), r.ID, 1, w("01JWRITE0000000000000000DA", t0.Add(time.Hour))))
	if d.Deleted == nil || !d.Deleted.Equal(t0.Add(time.Hour)) || d.Rev != 2 {
		t.Fatalf("delete %+v", d)
	}
	if again := must2[store.Record](t)(s.Delete(ctx(), r.ID, 0, w("", t0.Add(2*time.Hour)))); again.Rev != 2 || !again.Deleted.Equal(*d.Deleted) {
		t.Fatalf("second delete %+v", again)
	}
	if again := must2[store.Record](t)(s.Delete(ctx(), r.ID, 0, w("01JWRITE0000000000000000DA", t0.Add(3*time.Hour)))); again.Rev != 2 {
		t.Fatalf("repeated delete %+v", again)
	}
	if _, err := s.Update(ctx(), r.ID, w("", t0), rename("x")); !errors.Is(err, store.ErrDeleted) {
		t.Fatalf("update of a tombstone: %v", err)
	}
	h := must2[[]store.Revision](t)(s.History(ctx(), r.ID))
	if len(h) != 2 || h[0].Op != store.OpDelete || h[0].Def.Name != r.Name {
		t.Fatalf("history %+v", h)
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), authz.All(), Project, store.Query{})); len(l) != 0 {
		t.Fatalf("a tombstone listed by default: %+v", l)
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), authz.All(), Project, store.Query{Deleted: true})); len(l) != 1 || l[0].Deleted == nil {
		t.Fatalf("deleted=1: %+v", l)
	}
}

func historyBounds(t *testing.T, s store.Store) {
	r := must2[store.Record](t)(s.Create(ctx(), rec("home", "t", ""), w("", t0)))
	for i := range 60 {
		must2[store.Record](t)(s.Update(ctx(), r.ID, w("", t0), rename(fmt.Sprintf("n%d", i))))
	}
	h := must2[[]store.Revision](t)(s.History(ctx(), r.ID))
	if len(h) != store.HistoryRevisions || h[0].Rev != 61 || h[len(h)-1].Rev != 12 {
		t.Fatalf("after 61 revisions: %d kept, newest %d, oldest %d", len(h), h[0].Rev, h[len(h)-1].Rev)
	}
	big := must2[store.Record](t)(s.Create(ctx(), rec("home", "big", ""), w("", t0)))
	for i := range 15 {
		must2[store.Record](t)(s.Update(ctx(), big.ID, w("", t0), func(c store.Record) (store.Definition, error) {
			d := c.Definition
			d.Body = strings.Repeat(string(rune('a'+i)), 100<<10)
			return d, nil
		}))
	}
	h = must2[[]store.Revision](t)(s.History(ctx(), big.ID))
	total := 0
	for _, v := range h {
		total += v.Bytes
	}
	if total > store.HistoryBytes || h[0].Rev != 16 || len(h) >= 16 || len(h) < 5 {
		t.Fatalf("big history: %d revisions, %d bytes, newest %d", len(h), total, h[0].Rev)
	}
	// The newest is kept even alone over the bound.
	huge := must2[store.Record](t)(s.Create(ctx(), rec("home", "huge", ""), w("", t0)))
	for range 2 {
		must2[store.Record](t)(s.Update(ctx(), huge.ID, w("", t0), func(c store.Record) (store.Definition, error) {
			d := c.Definition
			d.Fields = json.RawMessage(`{"x":"` + strings.Repeat("z", store.HistoryBytes) + `"}`)
			d.Body += "."
			return d, nil
		}))
	}
	if h := must2[[]store.Revision](t)(s.History(ctx(), huge.ID)); len(h) != 1 || h[0].Rev != 3 {
		t.Fatalf("huge history: %+v", len(h))
	}
}

func health(install string, applied int64, state string) orchapi.AutomationHealth {
	return orchapi.AutomationHealth{Install: install, State: state, AppliedRev: applied, AsOf: t0.Format(time.RFC3339)}
}

func healthAndPurge(t *testing.T, s store.Store) {
	a := must2[store.Record](t)(s.Create(ctx(), rec("home", "a", ""), w("", t0)))
	b := must2[store.Record](t)(s.Create(ctx(), rec("home", "b", ""), w("", t0)))
	prev := must2[*orchapi.AutomationHealth](t)(s.PutHealth(ctx(), a.ID, health("i1", 1, orchapi.AutomationHealthy), t0))
	if prev != nil {
		t.Fatalf("first health replaced %+v", prev)
	}
	prev = must2[*orchapi.AutomationHealth](t)(s.PutHealth(ctx(), a.ID, health("i1", 1, orchapi.AutomationUnapplied), t0.Add(time.Minute)))
	if prev == nil || prev.State != orchapi.AutomationHealthy {
		t.Fatalf("second health replaced %+v", prev)
	}
	if got := must2[store.Record](t)(s.Get(ctx(), a.ID)); got.Health == nil || got.Health.State != orchapi.AutomationUnapplied || got.Rev != 1 {
		t.Fatalf("health is not a definition write: %+v", got)
	}
	if _, err := s.PutHealth(ctx(), "01JNOPE0000000000000000000", health("i1", 1, orchapi.AutomationHealthy), t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("health of a missing record: %v", err)
	}
	// a: deleted, never applied: gone 30 days after the delete.
	// b: deleted, the home applied the delete 10 days later: kept 30 days from then.
	must2[store.Record](t)(s.Delete(ctx(), a.ID, 0, w("", t0)))
	db := must2[store.Record](t)(s.Delete(ctx(), b.ID, 0, w("", t0)))
	must2[*orchapi.AutomationHealth](t)(s.PutHealth(ctx(), b.ID, health("i1", db.Rev, orchapi.AutomationHealthy), t0.Add(10*24*time.Hour)))
	if n := must2[int](t)(s.Purge(ctx(), t0.Add(29*24*time.Hour))); n != 0 {
		t.Fatalf("purged %d early", n)
	}
	if n := must2[int](t)(s.Purge(ctx(), t0.Add(31*24*time.Hour))); n != 1 {
		t.Fatalf("day 31 purged %d", n)
	}
	if _, err := s.Get(ctx(), a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a after purge: %v", err)
	}
	if _, err := s.History(ctx(), a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a's history after purge: %v", err)
	}
	if n := must2[int](t)(s.Purge(ctx(), t0.Add(41*24*time.Hour))); n != 1 {
		t.Fatalf("day 41 purged %d", n)
	}
	// A purged task id is free again.
	must2[store.Record](t)(s.Create(ctx(), rec("home", "a", ""), w("", t0)))
}

func listScope(t *testing.T, s store.Store) {
	solver := must2[store.Record](t)(s.Create(ctx(), rec("home", "s", "solver"), w("", t0)))
	must2[store.Record](t)(s.Create(ctx(), rec("home", "b", "billing"), w("", t0)))
	must2[store.Record](t)(s.Create(ctx(), rec("home", "p", ""), w("", t0)))
	other := must2[store.Record](t)(s.Create(ctx(), rec("other", "o", "solver"), w("", t0)))
	guest := authz.FromTuples([]authz.Tuple{{Subject: "g", Relation: authz.Guest, Object: authz.AreaObject(Project, "solver")}}, nil)
	l, _ := must3[[]store.Record](t)(s.List(ctx(), guest, Project, store.Query{}))
	if len(l) != 2 || l[0].ID != solver.ID || l[1].ID != other.ID {
		t.Fatalf("solver guest lists %+v", l)
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), authz.FromTuples(nil, nil), Project, store.Query{})); len(l) != 0 {
		t.Fatalf("no scope lists %+v", l)
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), authz.All(), Project, store.Query{})); len(l) != 4 {
		t.Fatalf("all lists %d", len(l))
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), guest, Project, store.Query{Home: "other"})); len(l) != 1 || l[0].ID != other.ID {
		t.Fatalf("home=other lists %+v", l)
	}
	if l, _ := must3[[]store.Record](t)(s.List(ctx(), authz.All(), Project, store.Query{Kind: "loop"})); len(l) != 0 {
		t.Fatalf("kind=loop lists %+v", l)
	}
	if l, tr := must3[[]store.Record](t)(s.List(ctx(), authz.All(), Project, store.Query{Limit: 3})); len(l) != 3 || !tr {
		t.Fatalf("limit 3: %d %v", len(l), tr)
	}
}

func claims(t *testing.T, s store.Store) {
	if _, err := s.Home(ctx(), "mac"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unclaimed: %v", err)
	}
	h, held := must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "i1", Store: "z3", Path: "/u/one"}, t0))
	if !held || h.Install != "i1" || h.Store != "z3" || !h.Claimed.Equal(t0) || h.Wanted != nil {
		t.Fatalf("first claim %+v %v", h, held)
	}
	h, held = must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "i1", Store: "zm", Path: "/u/one-b"}, t0.Add(time.Hour)))
	if !held || h.Store != "zm" || h.Path != "/u/one-b" || !h.Claimed.Equal(t0) || !h.Updated.Equal(t0.Add(time.Hour)) {
		t.Fatalf("holder's repeat %+v", h)
	}
	h, held = must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "i2", Store: "zm", Path: "/u/two"}, t0.Add(2*time.Hour)))
	if held || h.Install != "i1" || h.Wanted == nil || h.Wanted.Install != "i2" || h.Wanted.Path != "/u/two" || !h.Wanted.First.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("second install %+v %v", h, held)
	}
	h, _ = must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "i2", Store: "z3", Path: "/u/two-b"}, t0.Add(3*time.Hour)))
	if h.Wanted.Install != "i2" || h.Wanted.Store != "z3" || h.Wanted.Path != "/u/two-b" || !h.Wanted.First.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("wanted's repeat %+v", h.Wanted)
	}
	h, _ = must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "i3", Store: "zm", Path: "/u/three"}, t0.Add(4*time.Hour)))
	if h.Wanted.Install != "i3" || !h.Wanted.First.Equal(t0.Add(4*time.Hour)) || h.Install != "i1" {
		t.Fatalf("a different install replaces wanted %+v", h)
	}
	if hs := must2[[]store.Home](t)(s.Homes(ctx())); len(hs) != 1 || hs[0].Home != "mac" || !hs[0].LastHealth.IsZero() {
		t.Fatalf("homes %+v", hs)
	}
	r := must2[store.Record](t)(s.Create(ctx(), rec("mac", "x", ""), w("", t0)))
	must2[*orchapi.AutomationHealth](t)(s.PutHealth(ctx(), r.ID, health("i1", 1, orchapi.AutomationHealthy), t0.Add(5*time.Hour)))
	if hs := must2[[]store.Home](t)(s.Homes(ctx())); !hs[0].LastHealth.Equal(t0.Add(5 * time.Hour)) {
		t.Fatalf("last health %+v", hs[0])
	}
}

func setHolder(t *testing.T, s store.Store) {
	must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "old", Store: "zm", Path: "/old"}, t0))
	if _, err := s.SetHolder(ctx(), "nowhere", "old", "new", "01JASK00000000000000000001", t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unclaimed home: %v", err)
	}
	if _, err := s.SetHolder(ctx(), "mac", "old", "new", "01JASK00000000000000000001", t0); !errors.Is(err, store.ErrNotWanted) {
		t.Fatalf("no wanted install: %v", err)
	}
	must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "new", Store: "z3", Path: "/new"}, t0.Add(time.Hour)))
	if _, err := s.SetHolder(ctx(), "mac", "old", "new", "01JASK00000000000000000001", t0); !errors.Is(err, store.ErrWantedZ3) {
		t.Fatalf("wanted install on z3: %v", err)
	}
	must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "new", Store: "zm", Path: "/new"}, t0.Add(2*time.Hour)))
	if _, err := s.SetHolder(ctx(), "mac", "someone-else", "new", "01JASK00000000000000000001", t0); !errors.Is(err, store.ErrChanged) {
		t.Fatalf("a holder other than expected: %v", err)
	}
	at := t0.Add(3 * time.Hour)
	h := must2[store.Home](t)(s.SetHolder(ctx(), "mac", "old", "new", "01JASK00000000000000000001", at))
	if h.Install != "new" || h.Store != "zm" || h.Path != "/new" || !h.Claimed.Equal(at) || h.WriteID != "01JASK00000000000000000001" || h.Wanted != nil {
		t.Fatalf("takeover %+v", h)
	}
	// The same claim change again makes no second change.
	again := must2[store.Home](t)(s.SetHolder(ctx(), "mac", "old", "new", "01JASK00000000000000000001", at.Add(time.Hour)))
	if again.Install != "new" || !again.Claimed.Equal(at) || !again.Updated.Equal(at) {
		t.Fatalf("repeated takeover %+v", again)
	}
	// The new holder's repeat claim keeps the takeover's write id and claim time.
	h, held := must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "new", Store: "zm", Path: "/new-b"}, at.Add(2*time.Hour)))
	if !held || h.WriteID != "01JASK00000000000000000001" || !h.Claimed.Equal(at) || h.Path != "/new-b" {
		t.Fatalf("holder's repeat after takeover %+v", h)
	}
	// The old install is now refused, and becomes the wanted one.
	h, held = must3[store.Home](t)(s.Claim(ctx(), "mac", store.Claim{Install: "old", Store: "zm", Path: "/old"}, at.Add(3*time.Hour)))
	if held || h.Wanted == nil || h.Wanted.Install != "old" {
		t.Fatalf("old install after takeover %+v %v", h, held)
	}
	// A different write id from a stale expected holder is refused.
	if _, err := s.SetHolder(ctx(), "mac", "old", "new", "01JASK00000000000000000002", t0); !errors.Is(err, store.ErrChanged) {
		t.Fatalf("stale takeover: %v", err)
	}
}

func scrub(t *testing.T, s store.Store) {
	const secret = "sk-live-0123456789"
	a := rec("home", "a", "")
	a.Body = "token " + secret + " here"
	a.Fields = json.RawMessage(`{"note":"` + secret + `","on":{"manual":true}}`)
	ra := must2[store.Record](t)(s.Create(ctx(), a, w("", t0)))
	must2[store.Record](t)(s.Update(ctx(), ra.ID, w("", t0), rename("still "+secret)))
	h := health("i1", 1, orchapi.AutomationUnapplied)
	h.ApplyError = "could not compile: " + secret
	must2[*orchapi.AutomationHealth](t)(s.PutHealth(ctx(), ra.ID, h, t0))
	b := rec("home", "b", "")
	b.Body = "short xyz and xyz"
	rb := must2[store.Record](t)(s.Create(ctx(), b, w("", t0)))
	c := rec("home", "c", "")
	c.Body = "other xyz"
	rc := must2[store.Record](t)(s.Create(ctx(), c, w("", t0)))

	if err := s.Scrub(ctx(), []string{rb.ID}, []string{secret, "xyz"}, core.MinUnscoped); err != nil {
		t.Fatal(err)
	}
	got := must2[store.Record](t)(s.Get(ctx(), ra.ID))
	hist := must2[[]store.Revision](t)(s.History(ctx(), ra.ID))
	all := got.Body + string(got.Fields) + got.Name + got.Health.ApplyError
	for _, v := range hist {
		all += v.Def.Body + string(v.Def.Fields) + v.Def.Name
	}
	if strings.Contains(all, secret) || !strings.Contains(got.Body, core.Redacted) || !json.Valid(got.Fields) || got.Rev != 2 {
		t.Fatalf("after scrub: %s", all)
	}
	if got := must2[store.Record](t)(s.Get(ctx(), rb.ID)); strings.Contains(got.Body, "xyz") {
		t.Fatalf("named record keeps a short text: %q", got.Body)
	}
	if got := must2[store.Record](t)(s.Get(ctx(), rc.ID)); got.Body != "other xyz" {
		t.Fatalf("a short text scrubbed outside the named records: %q", got.Body)
	}
}
