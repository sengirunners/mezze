package address

import (
	"context"
	"errors"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
)

// fakeSessions answers Lookup/LookupErr by address and ResolveName by a
// live session's stable name.
type fakeSessions struct {
	orchcore.SessionService
	m   map[string]orchapi.Session
	err error
}

func (f *fakeSessions) Lookup(_ context.Context, a string) (orchapi.Session, bool) {
	s, ok := f.m[a]
	return s, ok
}

func (f *fakeSessions) LookupErr(ctx context.Context, a string) (orchapi.Session, bool, error) {
	if f.err != nil {
		return orchapi.Session{}, false, f.err
	}
	s, ok := f.Lookup(ctx, a)
	return s, ok, nil
}

func (f *fakeSessions) ResolveName(_ context.Context, ref string) (orchapi.Session, bool) {
	for _, s := range f.m {
		if s.Name != "" && "mini/"+s.Name == ref && s.State != orchapi.StateEnded {
			return s, true
		}
	}
	return orchapi.Session{}, false
}

func (f *fakeSessions) Visible(context.Context, authz.Scope, string) bool { return true }

func TestLiveRecipient(t *testing.T) {
	cfg := &config.Config{Machine: "mini"}
	ended := orchapi.Session{Address: "mini/0ld0ld00", Name: "zm-dispatch-tracker", Project: "P1", State: orchapi.StateEnded}
	successor := func(project, area string) orchapi.Session {
		return orchapi.Session{Address: "mini/11e11e11", Name: "zm-dispatch-tracker", Project: project, Area: area, State: orchapi.StateIdle}
	}
	for _, tt := range []struct {
		name     string
		sessions map[string]orchapi.Session
		err      error
		addr     string
		want     string
		wantVia  string
	}{
		{name: "live session", sessions: map[string]orchapi.Session{"mini/aaaaaaaa": {Address: "mini/aaaaaaaa", State: orchapi.StateRunning}},
			addr: "mini/aaaaaaaa", want: "mini/aaaaaaaa", wantVia: "creator of x"},
		{name: "ended, no successor", sessions: map[string]orchapi.Session{ended.Address: ended},
			addr: ended.Address, want: "machine:mini", wantVia: "creator of x (that session has ended)"},
		{name: "ended, successor holds its name", sessions: map[string]orchapi.Session{ended.Address: ended, "mini/11e11e11": successor("P1", "")},
			addr: ended.Address, want: "mini/11e11e11", wantVia: "creator of x (successor of an ended session)"},
		{name: "successor in the item's area", sessions: map[string]orchapi.Session{ended.Address: ended, "mini/11e11e11": successor("P1", "solver")},
			addr: ended.Address, want: "mini/11e11e11", wantVia: "creator of x (successor of an ended session)"},
		{name: "successor in another project", sessions: map[string]orchapi.Session{ended.Address: ended, "mini/11e11e11": successor("P2", "")},
			addr: ended.Address, want: "machine:mini", wantVia: "creator of x (that session has ended)"},
		{name: "successor cannot read the area", sessions: map[string]orchapi.Session{ended.Address: ended, "mini/11e11e11": successor("P1", "billing")},
			addr: ended.Address, want: "machine:mini", wantVia: "creator of x (that session has ended)"},
		{name: "another machine's ended session (presence)", sessions: map[string]orchapi.Session{"studio/aaaaaaaa": {Address: "studio/aaaaaaaa", Name: "zm-dispatch-tracker", Project: "P1", State: orchapi.StateEnded}},
			addr: "studio/aaaaaaaa", want: "machine:studio", wantVia: "creator of x (that session has ended)"},
		{name: "another machine's live session (presence)", sessions: map[string]orchapi.Session{"studio/aaaaaaaa": {Address: "studio/aaaaaaaa", State: orchapi.StateRunning}},
			addr: "studio/aaaaaaaa", want: "studio/aaaaaaaa", wantVia: "creator of x"},
		{name: "unknown local session", sessions: map[string]orchapi.Session{}, addr: "mini/deadbeef", want: "machine:mini", wantVia: "creator of x (that session has ended)"},
		{name: "registry not answering", sessions: map[string]orchapi.Session{ended.Address: ended}, err: errors.New("store closed"),
			addr: ended.Address, want: ended.Address, wantVia: "creator of x"},
		{name: "another machine's session gone from presence", sessions: map[string]orchapi.Session{}, addr: "studio/aaaaaaaa", want: "machine:studio", wantVia: "creator of x (that session has ended)"},
		{name: "a person", sessions: map[string]orchapi.Session{}, addr: "human:ana", want: "human:ana", wantVia: "creator of x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			to, via, err := LiveRecipient(context.Background(), &fakeSessions{m: tt.sessions, err: tt.err}, cfg, "P1", "solver", tt.addr, "creator of x")
			if err != nil || to != tt.want || via != tt.wantVia {
				t.Fatalf("got %q %q %v, want %q %q", to, via, err, tt.want, tt.wantVia)
			}
		})
	}
}
