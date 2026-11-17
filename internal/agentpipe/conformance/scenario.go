package conformance

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
)

// A Scenario drives a live adapter (with its real runtime) to make a
// recording. Prompts are plain and cheap; every scenario ends with a stop.
type Scenario struct {
	Name      string
	Approvals string // Start.Approvals
	Run       func(l *Live) error
}

// Live is a scenario's handle on the adapter.
type Live struct {
	P       *Proc
	n       int
	Timeout time.Duration
	// Answer decides pending approvals the scenario does not handle itself
	// (default: accept).
	Answer func(agentpipe.Event) string
	seen   map[string]bool
}

// Op sends an op with a fresh id and returns the id.
func (l *Live) Op(op agentpipe.Op) string {
	l.n++
	op.ID = "op" + strconv.Itoa(l.n)
	_ = l.P.Send(op)
	return op.ID
}

func lastTurn(evs []agentpipe.Event) (string, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == agentpipe.EvTurn {
			return evs[i].Turn, agentpipe.Terminal(evs[i].Status)
		}
	}
	return "", false
}

// Until waits for pred, answering approvals on the way with l.Answer.
func (l *Live) Until(what string, pred func([]agentpipe.Event) bool) error {
	deadline := time.Now().Add(l.Timeout)
	for {
		var pend []agentpipe.Event
		ok := l.P.Wait(func(evs []agentpipe.Event) bool {
			if pred(evs) {
				return true
			}
			pend = l.pending(evs)
			return len(pend) > 0
		}, time.Until(deadline))
		evs := l.P.Events()
		if pred(evs) {
			return nil
		}
		if !ok {
			return fmt.Errorf("timed out waiting for %s", what)
		}
		for _, ev := range pend {
			l.seen[ev.Request] = true
			d := agentpipe.DecisionAccept
			if l.Answer != nil {
				d = l.Answer(ev)
			}
			l.Op(agentpipe.Op{Op: agentpipe.OpAnswer, Request: ev.Request, Decision: d})
		}
	}
}

func (l *Live) pending(evs []agentpipe.Event) []agentpipe.Event {
	st := map[string]agentpipe.Event{}
	for _, ev := range evs {
		if ev.Type == agentpipe.EvApproval {
			st[ev.Request] = ev
		}
	}
	var out []agentpipe.Event
	for req, ev := range st {
		if ev.Status == agentpipe.ItemPending && !l.seen[req] {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Turn sends a turn and waits for it to end.
func (l *Live) Turn(text string) error {
	before := len(l.P.Events())
	l.Op(agentpipe.Op{Op: agentpipe.OpTurn, Text: text})
	return l.Until("the turn's end", func(evs []agentpipe.Event) bool { return turnEndedAfter(evs, before) })
}

func turnEndedAfter(evs []agentpipe.Event, before int) bool {
	for _, ev := range evs[min(before, len(evs)):] {
		if ev.Type == agentpipe.EvTurn && agentpipe.Terminal(ev.Status) {
			return true
		}
	}
	return false
}

func toolRunningAfter(evs []agentpipe.Event, before int) bool {
	for _, ev := range evs[min(before, len(evs)):] {
		if ev.Type == agentpipe.EvTool && ev.Kind == agentpipe.ToolCommand && ev.Status == agentpipe.ItemRunning {
			return true
		}
	}
	return false
}

// Scenarios are the recorded ones (testdata/<driver>/<name>.rec.jsonl).
var Scenarios = []Scenario{
	{Name: "turns", Run: func(l *Live) error {
		if err := l.Turn("Reply with exactly the word: alpha"); err != nil {
			return err
		}
		return l.Turn("Now reply with exactly the word: beta")
	}},
	{Name: "approve", Approvals: "ask", Run: func(l *Live) error {
		decision := agentpipe.DecisionAccept
		l.Answer = func(agentpipe.Event) string { return decision }
		if err := l.Turn("Use the shell to run exactly this command: touch zm-approve.txt\nThen reply in one short sentence."); err != nil {
			return err
		}
		decision = agentpipe.DecisionDecline
		return l.Turn("Use the shell to run exactly this command: rm zm-approve.txt\nIf you cannot, reply in one short sentence saying so.")
	}},
	{Name: "interrupt", Run: func(l *Live) error {
		before := len(l.P.Events())
		l.Op(agentpipe.Op{Op: agentpipe.OpTurn, Text: "Use the shell to run exactly this command: sleep 30 && echo slept\nThen reply in one short sentence."})
		if err := l.Until("a running command", func(evs []agentpipe.Event) bool { return toolRunningAfter(evs, before) }); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		l.Op(agentpipe.Op{Op: agentpipe.OpInterrupt})
		if err := l.Until("the interrupted turn's end", func(evs []agentpipe.Event) bool { return turnEndedAfter(evs, before) }); err != nil {
			return err
		}
		return l.Turn("Reply with exactly the word: after")
	}},
	{Name: "steer", Run: func(l *Live) error {
		before := len(l.P.Events())
		l.Op(agentpipe.Op{Op: agentpipe.OpTurn, Text: "Use the shell to run exactly this command: sleep 8 && echo eight\nThen reply in one short sentence."})
		if err := l.Until("a running command", func(evs []agentpipe.Event) bool { return toolRunningAfter(evs, before) }); err != nil {
			return err
		}
		time.Sleep(time.Second)
		l.Op(agentpipe.Op{Op: agentpipe.OpSteer, Text: "Also: end your reply with the word STEERED."})
		return l.Until("the steered turn's end", func(evs []agentpipe.Event) bool { return turnEndedAfter(evs, before) })
	}},
}

// ScenarioByName finds one.
func ScenarioByName(name string) (Scenario, bool) {
	for _, s := range Scenarios {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}

// Record runs scenario sc against adapter (argv before "--"), which runs
// cli in cwd and records to out. It returns the live events.
func Record(adapter, cli []string, sc Scenario, cwd, model, out string, env []string, timeout time.Duration) ([]agentpipe.Event, string, error) {
	argv := append(append(append([]string(nil), adapter...), "--record", out, "--"), cli...)
	p, err := StartProc(argv, env)
	if err != nil {
		return nil, "", err
	}
	l := &Live{P: p, Timeout: timeout, seen: map[string]bool{}}
	start := l.Op(agentpipe.Op{Op: agentpipe.OpStart, Start: &agentpipe.Start{Cwd: cwd, Model: model, Approvals: sc.Approvals}})
	err = l.Until("the start", func(evs []agentpipe.Event) bool {
		for _, ev := range evs {
			if ev.Type == agentpipe.EvOp && ev.Op == start {
				return true
			}
		}
		return false
	})
	if err == nil {
		err = sc.Run(l)
	}
	l.Op(agentpipe.Op{Op: agentpipe.OpStop})
	p.Wait(func(evs []agentpipe.Event) bool {
		n := len(evs)
		return n > 0 && evs[n-1].Type == agentpipe.EvSession && (evs[n-1].Status == agentpipe.StateStopped || evs[n-1].Status == agentpipe.StateError)
	}, 30*time.Second)
	stderr, bad := p.Close(15 * time.Second)
	if err == nil && len(bad) > 0 {
		err = fmt.Errorf("%v", bad)
	}
	return p.Events(), stderr, err
}
