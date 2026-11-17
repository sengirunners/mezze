package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Dogfood: `zm kill` on an interactive shell (which ignores TERM) printed
// "still running" and exited 0.
func TestKillEscalates(t *testing.T) {
	// fake is a session that ignores the signals in ignored; it records what was sent.
	fake := func(ignored ...string) (func(string, time.Duration) (orchapi.Session, error), func() (orchapi.Session, error), *[]string) {
		var got []string
		state := orchapi.StateRunning
		stop := func(sig string, _ time.Duration) (orchapi.Session, error) {
			if state == orchapi.StateEnded {
				return orchapi.Session{}, api.Errorf(api.CodeConflict, "session has ended")
			}
			got = append(got, sig)
			for _, x := range ignored {
				if x == sig {
					return orchapi.Session{ID: "s1", State: state}, nil
				}
			}
			state = orchapi.StateEnded
			return orchapi.Session{ID: "s1", State: state, Outcome: &orchapi.Outcome{Kind: orchapi.OutcomeKilled, Signal: sig}}, nil
		}
		get := func() (orchapi.Session, error) { return orchapi.Session{ID: "s1", State: state}, nil }
		return stop, get, &got
	}
	cases := []struct {
		name      string
		ignored   []string
		signal    string
		force     bool
		wantSent  []string
		wantEnded bool
	}{
		{"an agent ends on TERM", nil, "", false, []string{"TERM"}, true},
		{"an interactive shell ends on HUP", []string{"TERM"}, "", false, []string{"TERM", "HUP"}, true},
		{"ignores TERM and HUP: still running (exit 1)", []string{"TERM", "HUP"}, "", false, []string{"TERM", "HUP"}, false},
		{"--force adds KILL", []string{"TERM", "HUP"}, "", true, []string{"TERM", "HUP", "KILL"}, true},
		{"--signal sends only that one", []string{"INT"}, "sigint", false, []string{"INT"}, false},
	}
	for _, c := range cases {
		stop, get, got := fake(c.ignored...)
		s, sent, err := escalateStop(stop, get, stopPlan(c.signal, c.force))
		if err != nil || !reflect.DeepEqual(sent, c.wantSent) || !reflect.DeepEqual(*got, c.wantSent) || (s.State == orchapi.StateEnded) != c.wantEnded {
			t.Errorf("%s: sent %v (daemon saw %v), state %s, err %v; want %v ended=%v", c.name, sent, *got, s.State, err, c.wantSent, c.wantEnded)
		}
	}
	// Review: TERM must leave an agent time to flush and exit (it was cut to
	// 1 s, so a graceful agent got HUP mid-cleanup); only HUP is short.
	if p := stopPlan("", true); len(p) != 3 || p[0].signal != "TERM" || p[0].wait < 3*time.Second ||
		p[1].signal != "HUP" || p[1].wait > time.Second || p[2].signal != "KILL" {
		t.Fatalf("plan %+v", p)
	}
	// The session ending between two steps (a conflict) is success, not an error.
	calls := 0
	stop := func(sig string, _ time.Duration) (orchapi.Session, error) {
		calls++
		if calls == 1 {
			return orchapi.Session{ID: "s1", State: orchapi.StateRunning}, nil
		}
		return orchapi.Session{}, api.Errorf(api.CodeConflict, "session has ended")
	}
	get := func() (orchapi.Session, error) { return orchapi.Session{ID: "s1", State: orchapi.StateEnded}, nil }
	if s, _, err := escalateStop(stop, get, stopPlan("", false)); err != nil || s.State != orchapi.StateEnded {
		t.Fatalf("ended between steps: %+v %v", s, err)
	}
	// A refusal on the first step is the caller's error.
	stop = func(string, time.Duration) (orchapi.Session, error) {
		return orchapi.Session{}, api.NotFound("session", "x")
	}
	if _, _, err := escalateStop(stop, get, stopPlan("", true)); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("first-step error: %v", err)
	}
}
