package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
)

// fakeHostNow replaces Potluck's switch reader with h and err, and records
// the owner and drain deadline it was asked with.
func fakeHostNow(t *testing.T, h protocol.HostStatus, err error) *[2]int {
	t.Helper()
	asked := &[2]int{}
	old := lendHostNow
	t.Cleanup(func() { lendHostNow = old })
	lendHostNow = func(owner, drainS int) (protocol.HostStatus, error) {
		asked[0], asked[1] = owner, drainS
		return h, err
	}
	return asked
}

func TestLendSwitchReadsTheMode(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local)
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	cut := func(n int) *int { return &n }
	for _, c := range []struct {
		name      string
		h         protocol.HostStatus
		mode, cut string
		until     bool
		says      []string
	}{
		{name: "auto", h: protocol.HostStatus{Mode: "auto", State: "accepting"}, mode: "auto", says: []string{"Switch: auto"}},
		{name: "open until", h: protocol.HostStatus{Mode: "open", State: "accepting", Until: at(time.Hour)}, mode: "open", until: true, says: []string{"open: new jobs start", "until 11:00"}},
		{name: "drain, past its deadline", h: protocol.HostStatus{Mode: "drain", State: "closed", DrainSince: at(-5 * time.Minute), DrainDeadline: at(-3 * time.Minute), DrainCut: cut(1)}, mode: "drain", cut: at(-3 * time.Minute), says: []string{"drained: no new jobs start, and jobs still running were stopped at 09:57"}},
		{name: "drain, before its deadline", h: protocol.HostStatus{Mode: "drain", State: "draining", DrainSince: at(-5 * time.Minute), DrainDeadline: at(5 * time.Minute)}, mode: "drain", cut: at(5 * time.Minute), says: []string{"draining: no new jobs start; jobs running now finish", "10:05"}},
		{name: "drain, back to auto before its deadline", h: protocol.HostStatus{Mode: "drain", State: "draining", Until: at(time.Hour), DrainSince: at(-5 * time.Minute), DrainDeadline: at(2 * time.Hour)}, mode: "drain", until: true, says: []string{"draining", "until 11:00"}},
		{name: "closed", h: protocol.HostStatus{Mode: "closed", State: "closed"}, mode: "closed", says: []string{"closed: no new jobs start, and jobs running now are stopped"}},
		{name: "auto, the schedule drains", h: protocol.HostStatus{Mode: "auto", State: "draining", DrainSince: at(-5 * time.Minute), DrainDeadline: at(5 * time.Minute)}, mode: "auto", cut: at(5 * time.Minute), says: []string{"the schedule is draining this Mac", "10:05"}},
		{name: "auto, the schedule closed it", h: protocol.HostStatus{Mode: "auto", State: "closed"}, mode: "auto", says: []string{"the schedule has closed this Mac"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Potluck's reader with nothing lent, and a lent size's describe, say the same.
			fakeHostNow(t, c.h, nil)
			h := c.h
			for _, vs := range [][]computeapi.RunTargetView{nil, {{Describe: &computeapi.RunTargetDescribe{Host: &h}}}} {
				sw := lendSwitchOf(vs)
				if sw.Mode != c.mode || sw.Cut != c.cut || (sw.Until != "") != c.until {
					t.Fatalf("switch (%d lent): %+v", len(vs), sw)
				}
				words := sw.wordsAt(now)
				for _, w := range c.says {
					if !strings.Contains(words, w) {
						t.Errorf("says %q, not %q", words, w)
					}
				}
				if m := bannedLend.FindAllString(strings.ReplaceAll(words, "zm ", ""), -1); len(m) > 0 {
					t.Errorf("says %v: %s", m, words)
				}
			}
		})
	}
	// Potluck cannot read it: the driver holds, and status says why.
	fakeHostNow(t, protocol.HostStatus{Mode: "unreadable", State: "holding"}, errors.New("/x may be changed by others (mode 666)"))
	if sw := readLendSwitch(nil); sw.Mode != "unreadable" || sw.State != "holding" || !strings.Contains(sw.Why, "changed by others") || !strings.Contains(sw.Words, "cannot be read, so no new jobs start") {
		t.Errorf("an unreadable switch: %+v", sw)
	}
	// The driver could not read it: its describe says so, and why, whatever Potluck's reader here says.
	fakeHostNow(t, protocol.HostStatus{Mode: "open", State: "accepting"}, nil)
	v := computeapi.RunTargetView{Describe: &computeapi.RunTargetDescribe{Why: "the host override cannot be read, so no new work starts: /x may be written by others than its owner (mode 666)",
		Host: &protocol.HostStatus{Mode: "unreadable", State: "holding"}}}
	if sw := lendSwitchOf([]computeapi.RunTargetView{v}); sw.Mode != "unreadable" || !strings.Contains(sw.Why, "written by others") {
		t.Errorf("describe's unreadable: %+v", sw)
	}
	// The driver cannot read it but refuses first for another reason: the why is the switch's, not the other refusal.
	fakeHostNow(t, protocol.HostStatus{Mode: "unreadable", State: "holding"}, errors.New("/x is owned by uid 7, not the host's owner"))
	busy := computeapi.RunTargetView{Describe: &computeapi.RunTargetDescribe{Why: "no free darwin slot (2 in use)", Host: &protocol.HostStatus{Mode: "unreadable", State: "holding"}}}
	if sw := readLendSwitch([]computeapi.RunTargetView{busy}); sw.Mode != "unreadable" || strings.Contains(sw.Why, "slot") || !strings.Contains(sw.Why, "owned by uid") || strings.Contains(sw.Words, "sets it again") {
		t.Errorf("unreadable behind another refusal: %+v", sw)
	}
	closing := computeapi.RunTargetView{Describe: &computeapi.RunTargetDescribe{Why: "the host override cannot be read, so no new work starts: /x is a symlink; reclaimed: the owner is back",
		Host: &protocol.HostStatus{Mode: "unreadable", State: "closed"}}}
	if sw := readLendSwitch([]computeapi.RunTargetView{closing}); sw.Why != "/x is a symlink" || !strings.Contains(sw.Words, "jobs running now are stopped") {
		t.Errorf("unreadable while the schedule closes: %+v", sw)
	}
	fakeHostNow(t, protocol.HostStatus{Mode: "auto", State: "accepting"}, nil)
	if sw := lendSwitchOf([]computeapi.RunTargetView{busy}); sw.Mode != "unreadable" || sw.Why != "the driver could not read it" {
		t.Errorf("unreadable to the driver only: %+v", sw)
	}
	// A lent size from a driver with no host block: Potluck's reader, with that size's owner and drain deadline.
	asked := fakeHostNow(t, protocol.HostStatus{Mode: "auto", State: "accepting"}, nil)
	old := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Config: map[string]string{"host_owner_uid": "501", "drain_deadline_s": "600"}}, Describe: &computeapi.RunTargetDescribe{}}
	if sw := lendSwitchOf([]computeapi.RunTargetView{old}); sw.Mode != "auto" || *asked != [2]int{501, 600} {
		t.Errorf("an old driver: %+v, asked %v", sw, *asked)
	}
}

func TestLendSwitchSetsThroughPotluck(t *testing.T) {
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_NO_AUTOSTART", "1")
	var got [][]string
	old := lendHostSet
	t.Cleanup(func() { lendHostSet = old })
	lendHostSet = func(mode, forD, until, deadline string) error {
		got = append(got, []string{mode, forD, until, deadline})
		if forD == "bad" {
			return errors.New(`--for "bad" is not a duration (30m, 2h)`)
		}
		state := map[string]string{"closed": "closed", "drain": "draining"}[mode]
		lendHostNow = func(int, int) (protocol.HostStatus, error) { return protocol.HostStatus{Mode: mode, State: state}, nil }
		return nil
	}
	fakeHostNow(t, protocol.HostStatus{Mode: "auto", State: "accepting"}, nil)
	run := func(args ...string) (string, error) {
		cmd := lendSwitchCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	out, err := run("close")
	if err != nil || !strings.Contains(out, "Switch: closed") || !strings.HasSuffix(out, "done\n") {
		t.Fatalf("close: %v\n%s", err, out)
	}
	out, err = run("drain", "--for", "2h", "--deadline", "10m")
	if err != nil || !strings.Contains(out, "Switch: draining") {
		t.Fatalf("drain: %v\n%s", err, out)
	}
	if want := "[[closed   ] [drain 2h  10m]]"; fmt.Sprint(got) != want {
		t.Errorf("SetHost got %v, want %s", got, want)
	}
	if _, err := run("drain", "--for", "bad"); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("a bad --for: %v", err)
	}
	n := len(got)
	for _, args := range [][]string{{"shut"}, {"open", "--deadline", "5m"}, {}} {
		if _, err := run(args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	if len(got) != n {
		t.Errorf("a refused switch reached Potluck: %v", got[n:])
	}
}

func TestLendStatusSaysTheSwitch(t *testing.T) {
	_, c, home := lendRig(t)
	cutAt := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	fakeHostNow(t, protocol.HostStatus{Mode: "drain", State: "draining", DrainSince: time.Now().UTC().Format(time.RFC3339), DrainDeadline: cutAt}, nil)
	var out bytes.Buffer
	if err := lendStatus(context.Background(), c, "mac", home, &out); err != nil || !strings.Contains(out.String(), "This Mac lends nothing now.") || !strings.Contains(out.String(), "Switch: draining") {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	st, err := lendStatusResult(context.Background(), c, "mac", home)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"lent":[]`) || !strings.Contains(string(b), `"switch":{"mode":"drain"`) || !strings.Contains(string(b), `"cut":"`+cutAt+`"`) {
		t.Errorf("json: %s", b)
	}
	// auto with nothing lent: no switch line.
	fakeHostNow(t, protocol.HostStatus{Mode: "auto", State: "accepting"}, nil)
	out.Reset()
	if err := lendStatus(context.Background(), c, "mac", home, &out); err != nil || strings.Contains(out.String(), "Switch") {
		t.Errorf("status, auto: %v\n%s", err, out.String())
	}
}
