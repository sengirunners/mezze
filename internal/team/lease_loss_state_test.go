package team

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

type leaseLossSessions struct {
	orchcore.SessionService
	current orchapi.Session
	writes  []orchapi.StateRequest
	nudges  int
}

func (f *leaseLossSessions) Lookup(context.Context, string) (orchapi.Session, bool) {
	return f.current, true
}
func (f *leaseLossSessions) SetState(_ context.Context, _ string, req orchapi.StateRequest) error {
	f.writes = append(f.writes, req)
	f.current.State, f.current.Note = req.State, req.Note
	return nil
}
func (f *leaseLossSessions) Nudge(context.Context, string, string) (bool, error) {
	f.nudges++
	return false, nil
}

// No ticker, authority or notification worker: drive the precise heartbeat
// answers directly, including loss observed before an expiry notice can arrive.
func leaseLossFixture(state orchapi.SessionState, note string) (*Service, *leaseLossSessions, *bytes.Buffer, api.Remote) {
	f := &leaseLossSessions{current: orchapi.Session{Address: "replica/aaaaaaaa", Project: "p", State: state, Note: note}}
	log := new(bytes.Buffer)
	s := &Service{d: withSessions(&core.Deps{
		Config: &config.Config{Machine: "replica", Human: "person"},
		Log:    slog.New(slog.NewTextHandler(log, nil)),
	}, f)}
	s.cache.claimedIn("p", f.current.Address, "bd:issue")
	return s, f, log, api.Remote{Project: api.ProjectRef{ID: "p", Slug: "project"}, Authority: "authority"}
}

func TestLeaseLossPreservesSessionRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   orchapi.SessionState
		note    string
		waiting int
	}{
		{"running", orchapi.StateRunning, "working", 0},
		{"idle", orchapi.StateIdle, "turn ended", 0},
		{"permission", orchapi.StateNeedsHuman, "permission to use a tool", 0},
		{"human mail", orchapi.StateNeedsHuman, "waiting on mail real-question", 1},
		{"legacy lease note", orchapi.StateNeedsHuman, "lease lost: bd:issue", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, log, snap := leaseLossFixture(tc.state, tc.note)
			before := f.current
			s.applyPresence(context.Background(), snap, map[string]orchapi.Session{before.Address: before}, nil,
				map[string][]string{before.Address: {"bd:issue"}},
				orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address, Waiting: tc.waiting}}})
			if f.current.State != before.State || f.current.Note != before.Note || len(f.writes) != 0 {
				t.Fatalf("lease loss changed session request: before=%+v after=%+v writes=%+v", before, f.current, f.writes)
			}
			if s.cache.holds(before.Address, "bd:issue") || s.cache.leaseIn[before.Address]["p"] {
				t.Fatal("lost lease remained reported held")
			}
			if !strings.Contains(log.String(), "team: lease lost") || !strings.Contains(log.String(), "bd:issue") {
				t.Fatalf("lost lease stopped being logged: %s", log.String())
			}
		})
	}
}

func TestLeaseLossDoesNotOverwriteRequestAfterHeartbeatSnapshot(t *testing.T) {
	s, f, _, snap := leaseLossFixture(orchapi.StateRunning, "working")
	before := f.current // request arrives after the heartbeat's local snapshot
	f.current.State, f.current.Note = orchapi.StateNeedsHuman, "permission just requested"
	s.applyPresence(context.Background(), snap, map[string]orchapi.Session{before.Address: before}, nil,
		map[string][]string{before.Address: {"bd:issue"}},
		orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address}}})
	if f.current.State != orchapi.StateNeedsHuman || f.current.Note != "permission just requested" || len(f.writes) != 0 {
		t.Fatalf("loss overwrote a newer permission request: %+v writes=%+v", f.current, f.writes)
	}
}

func TestLeaseLossStillAllowsAnsweredHumanRequest(t *testing.T) {
	s, f, _, snap := leaseLossFixture(orchapi.StateNeedsHuman, "waiting on mail real-question")
	before := f.current
	s.applyPresence(context.Background(), snap, map[string]orchapi.Session{before.Address: before}, nil,
		map[string][]string{before.Address: {"bd:issue"}},
		orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address, Waiting: 0}}})
	if f.current.State != orchapi.StateIdle || f.current.Note != "answered" || len(f.writes) != 1 {
		t.Fatalf("lease loss interfered with the genuine answer: %+v writes=%+v", f.current, f.writes)
	}
}

func TestLeaseLossBeforeNoticeAndAfterDelivery(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		name := "notice failed"
		if delivered {
			name = "notice delayed until after held cache cleared"
		}
		t.Run(name, func(t *testing.T) {
			s, f, log, snap := leaseLossFixture(orchapi.StateRunning, "working")
			before := f.current
			local := map[string]orchapi.Session{before.Address: before}
			// HeldBy can omit an expired key before the expiry tick drops it,
			// or while that worker's holder notice is failing/blocked. Finished
			// remains absent: no optimistic notice-success proof is needed.
			s.applyPresence(context.Background(), snap, local, nil, map[string][]string{before.Address: {"bd:issue"}},
				orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address}}})
			if s.cache.holds(before.Address, "bd:issue") {
				t.Fatal("pre-notice expiry remained held")
			}
			var notice []orchapi.NoticePart
			if delivered {
				notice = []orchapi.NoticePart{{Project: "p", Unread: 1, Quiet: 1}}
			}
			// The next answer carries the late quiet notice, or still has none
			// if delivery failed. Neither may manufacture a human request.
			s.applyPresence(context.Background(), snap, local, nil, map[string][]string{before.Address: nil},
				orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address, Notice: notice}}})
			if f.current.State != before.State || f.current.Note != before.Note || len(f.writes) != 0 || f.nudges != 0 {
				t.Fatalf("expiry/notice changed state or woke a session: %+v writes=%+v nudges=%d", f.current, f.writes, f.nudges)
			}
			got := s.cache.notices["p"][before.Address]
			if len(got) != len(notice) || delivered && (got[0].Unread != 1 || got[0].Quiet != 1) {
				t.Fatalf("late holder notice lost: %+v", got)
			}
			if strings.Count(log.String(), "team: lease lost") != 1 {
				t.Fatalf("loss repeated after cache eviction: %s", log.String())
			}
		})
	}
}

func TestLeaseLossThenReclaimPreservesHeldCache(t *testing.T) {
	s, f, _, snap := leaseLossFixture(orchapi.StateRunning, "working")
	before := f.current
	local := map[string]orchapi.Session{before.Address: before}
	s.applyPresence(context.Background(), snap, local, nil, map[string][]string{before.Address: {"bd:issue"}},
		orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address}}})
	s.cache.claimed(before.Address, "bd:issue")
	s.cache.claimedIn("p", before.Address, "bd:issue")
	// An answer sent before the reclaim did not report the key: it must not
	// erase the newer local claim or change any state as another "loss".
	s.applyPresence(context.Background(), snap, local, nil, map[string][]string{before.Address: nil},
		orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address}}})
	if !s.cache.holds(before.Address, "bd:issue") || !s.cache.leaseIn[before.Address]["p"] {
		t.Fatal("late pre-reclaim response erased the new claim")
	}
	s.applyPresence(context.Background(), snap, local, nil, map[string][]string{before.Address: {"bd:issue"}},
		orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: before.Address, Held: []string{"bd:issue"}}}})
	if !s.cache.holds(before.Address, "bd:issue") || f.current.State != before.State || f.current.Note != before.Note || len(f.writes) != 0 {
		t.Fatalf("reclaim changed session or lost claim: %+v writes=%+v", f.current, f.writes)
	}
}
