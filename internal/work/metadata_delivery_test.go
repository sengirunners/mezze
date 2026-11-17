package work

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/sessiondelivery"
	"github.com/sengirunners/potluck/kit/core"
)

// This optional session door supplies the current safe projection after the
// work handler has already cached Lookup's old value. It models both a durable
// hold and a completed scrub without reading any session-owned tables.
type metadataDeliverySessions struct {
	*fakeSessions
	before func([]orchapi.Session)
	held   bool
	calls  int
}

func (f *metadataDeliverySessions) DeliverSessionMetadata(ctx context.Context, snapshots []orchapi.Session, deliver func([]orchapi.Session) error) error {
	f.calls++
	if f.before != nil {
		f.before(snapshots)
	}
	safe := append([]orchapi.Session(nil), snapshots...)
	for i, old := range snapshots {
		current, ok := f.Lookup(ctx, old.Address)
		safe[i].Title, safe[i].Note = "", ""
		safe[i].Command = nil
		safe[i].Cwd = ""
		if ok && !f.held && current.Project == old.Project && current.Area == old.Area {
			safe[i].Title, safe[i].Note = current.Title, current.Note
			// A real session mask may reload full-row content. The holder consumer
			// must reapply its narrower projection before serializing it.
			safe[i].Command, safe[i].Cwd = current.Command, current.Cwd
		}
	}
	return deliver(safe)
}

func metadataDeliveryFixture(t *testing.T) (*harness, *metadataDeliverySessions, orchapi.WorkItem, orchapi.Session) {
	t.Helper()
	h := newHarness(t)
	ss := orchapi.Session{Address: "mini/aaaaaaaa", ID: "aaaaaaaa", Machine: "mini", Project: h.harbor.ID, Area: "solver", Agent: "codex", State: orchapi.StateRunning, Title: "cached session private secret", Note: "cached session private secret", Command: []string{"private command"}, Cwd: "/private/path"}
	h.sessions.put(ss)
	f := &metadataDeliverySessions{fakeSessions: h.sessions}
	orchcore.Wire(h.d).Sessions = f
	c := h.as(h.owner, "harbor")
	item, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "delivery item", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(h.ctx, h.as(h.d.Projects.Local(ss.Address), "harbor"), item.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	return h, f, item, ss
}

func TestWorkSessionMetadataFinalDelivery(t *testing.T) {
	for _, state := range []string{"completed_scrub", "active_hold", "moved_scope"} {
		for _, endpoint := range []string{"inflight", "holder"} {
			t.Run(state+"/"+endpoint, func(t *testing.T) {
				h, f, item, old := metadataDeliveryFixture(t)
				f.held = state == "active_hold"
				f.before = func(snapshots []orchapi.Session) {
					if len(snapshots) != 1 || snapshots[0].Title != old.Title {
						t.Fatalf("did not capture old session before delivery: %+v", snapshots)
					}
					current := old
					current.Title, current.Note = core.Redacted, core.Redacted
					if state == "moved_scope" {
						current.Project = h.side.ID
						current.Title, current.Note = "other project private title", "other project private note"
					}
					f.put(current)
				}
				c := h.as(h.owner, "harbor")
				var delivered *orchapi.Session
				if endpoint == "inflight" {
					rows, err := orchclient.Inflight(h.ctx, c, "")
					if err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 {
						t.Fatalf("inflight: %+v", rows)
					}
					delivered = rows[0].Session
				} else {
					response, err := orchclient.Holder(h.ctx, c, item.ID)
					if err != nil {
						t.Fatal(err)
					}
					delivered = response.Session
				}
				if f.calls != 1 || delivered == nil {
					t.Fatalf("delivery calls=%d session=%+v", f.calls, delivered)
				}
				data, _ := json.Marshal(delivered)
				if strings.Contains(string(data), old.Title) || strings.Contains(string(data), "other project private") {
					t.Fatalf("cached or moved metadata leaked: %s", data)
				}
				if state == "completed_scrub" && delivered.Title != core.Redacted {
					t.Fatalf("did not use current scrubbed metadata: %+v", delivered)
				}
				if state != "completed_scrub" && (delivered.Title != "" || delivered.Note != "") {
					t.Fatalf("held or moved metadata not withheld: %+v", delivered)
				}
				if delivered.Address != old.Address || delivered.Project != old.Project || delivered.State != old.State {
					t.Fatalf("snapshot identity/lifecycle changed: %+v", delivered)
				}
				if endpoint == "holder" && (len(delivered.Command) != 0 || delivered.Cwd != "") {
					t.Fatalf("holder projection expanded after reload: %+v", delivered)
				}
			})
		}
	}
}

func TestWorkSessionMetadataLegacyFallback(t *testing.T) {
	h, _, item, old := metadataDeliveryFixture(t)
	orchcore.Wire(h.d).Sessions = h.sessions // no optional delivery method
	c := h.as(h.owner, "harbor")
	rows, err := orchclient.Inflight(h.ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Session == nil || rows[0].Session.Title != old.Title {
		t.Fatalf("legacy inflight changed: %+v", rows)
	}
	response, err := orchclient.Holder(h.ctx, c, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Session == nil || response.Session.Title != old.Title || len(response.Session.Command) != 0 || response.Session.Cwd != "" {
		t.Fatalf("legacy holder changed: %+v", response)
	}
}

func TestWorkSessionMetadataKeepsHiddenHolderOmitted(t *testing.T) {
	h, f, item, old := metadataDeliveryFixture(t)
	old.Project = h.side.ID
	f.put(old)
	f.before = func([]orchapi.Session) { t.Fatal("hidden holder entered metadata delivery") }
	c := h.as(h.guest(), "harbor")
	response, err := orchclient.Holder(h.ctx, c, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Session != nil || f.calls != 0 {
		t.Fatalf("hidden holder entered delivery: calls=%d response=%+v", f.calls, response)
	}
	rows, err := orchclient.Inflight(h.ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Session != nil || f.calls != 0 {
		t.Fatalf("hidden inflight entered delivery: calls=%d rows=%+v", f.calls, rows)
	}
}

func TestWorkSessionMetadataBatchKeepsOrderingAndOmissions(t *testing.T) {
	h, f, _, first := metadataDeliveryFixture(t)
	second := first
	second.Address, second.ID = "mini/bbbbbbbb", "bbbbbbbb"
	f.put(second)
	f.before = func(snapshots []orchapi.Session) {
		if len(snapshots) != 2 || snapshots[0].Address != first.Address || snapshots[1].Address != second.Address {
			t.Fatalf("batch order: %+v", snapshots)
		}
		first.Title, second.Title = "first scrubbed", "second scrubbed"
		f.put(first)
		f.put(second)
	}
	value := orchapi.InflightList{Inflight: []orchapi.Inflight{{Session: &first}, {}, {Session: &second}}}
	w := httptest.NewRecorder()
	if err := sessiondelivery.WriteJSON(w, httptest.NewRequest(http.MethodGet, "/v0/work/inflight", nil), orchcore.Sessions(h.s.d), value); err != nil {
		t.Fatal(err)
	}
	var response orchapi.InflightList
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Inflight) != 3 || response.Inflight[1].Session != nil || response.Inflight[0].Session.Title != "first scrubbed" || response.Inflight[2].Session.Title != "second scrubbed" {
		t.Fatalf("delivery remapped sessions: %+v", response)
	}
}
