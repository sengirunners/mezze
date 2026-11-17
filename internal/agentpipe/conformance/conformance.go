// Package conformance checks zm-agent/1 adapters (SPEC 8 "Structured
// sessions") against recordings of real sessions, in the style of
// potluck/conformance:
//
//   - Replay feeds a recording through a built-in driver in process; the
//     events must equal the recording's golden file exactly (seq included).
//   - Run replays it against any adapter process, with a fake runtime
//     (FakeArg) playing the recorded runtime side and checking every line
//     the adapter writes; the events must match the golden file item by
//     item (Loose), since two processes interleave.
//   - Check holds every run, live or replayed, to the protocol's rules.
//   - Record drives a live adapter through a Scenario to make a recording.
//
// Owned by the structured-sessions lane.
package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
)

// The fake runtime runs before anything else in a binary linking this
// package (the conformance command and its tests).
func init() {
	if len(os.Args) > 1 && os.Args[1] == agentpipe.FakeArg {
		os.Exit(agentpipe.FakeMain(os.Args[2:]))
	}
}

// Load reads a recording file.
func Load(path string) (agentpipe.RecEntry, []agentpipe.RecEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return agentpipe.RecEntry{}, nil, err
	}
	defer f.Close()
	return agentpipe.ReadRecording(f)
}

// Replay runs entries through d in process and returns its events (At
// zero). It fails when the driver writes a runtime line the recording does
// not have, or misses one it has.
func Replay(d agentpipe.Driver, entries []agentpipe.RecEntry) ([]agentpipe.Event, error) {
	var events []agentpipe.Event
	var sent [][]byte
	var logs []string
	s := &agentpipe.Session{D: d, Em: &agentpipe.Emitter{Out: func(ev agentpipe.Event) { events = append(events, ev) }},
		Log:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Send: func(b []byte) { sent = append(sent, append([]byte(nil), b...)) }}
	for i, e := range entries {
		switch e.T {
		case agentpipe.RecOp:
			if st := s.Op(e.Op); st != nil {
				if _, err := d.Argv(nil, *st); err != nil {
					return events, err
				}
				s.Started(nil)
			}
		case agentpipe.RecOut:
			line := []byte(e.Raw)
			if e.Frame != nil {
				line = e.Frame
			}
			s.Line(line)
		case agentpipe.RecIn:
			if len(sent) == 0 {
				return events, fmt.Errorf("entry %d: the driver did not write %s", i+1, e.Frame)
			}
			got, want := agentpipe.Canonical(sent[0]), agentpipe.Canonical(e.Frame)
			sent = sent[1:]
			if got != want {
				return events, fmt.Errorf("entry %d: the driver wrote\n  %s\nthe recording has\n  %s", i+1, got, want)
			}
		case agentpipe.RecExit:
			s.Exit(e.Code, e.Signaled)
		}
	}
	if len(sent) > 0 {
		return events, fmt.Errorf("the driver wrote %d line(s) the recording does not have, first %s", len(sent), sent[0])
	}
	return events, nil
}

// ReadEvents parses a JSON-lines event file.
func ReadEvents(b []byte) ([]agentpipe.Event, error) {
	var out []agentpipe.Event
	s := bufio.NewScanner(bytes.NewReader(b))
	s.Buffer(make([]byte, 64<<10), agentpipe.MaxLine)
	for s.Scan() {
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		var ev agentpipe.Event
		if err := json.Unmarshal(s.Bytes(), &ev); err != nil {
			return out, fmt.Errorf("event line %d: %v", len(out)+1, err)
		}
		out = append(out, ev)
	}
	return out, s.Err()
}

// WriteEvents is the golden file form: one event per line, At zeroed.
func WriteEvents(events []agentpipe.Event) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, ev := range events {
		ev.At = time.Time{}
		_ = enc.Encode(ev)
	}
	return buf.Bytes()
}

func norm(ev agentpipe.Event) string {
	ev.At, ev.Seq = time.Time{}, 0
	b, _ := json.Marshal(ev)
	return string(b)
}

// Exact compares two runs event by event (At ignored).
func Exact(want, got []agentpipe.Event) []string {
	var out []string
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			out = append(out, fmt.Sprintf("event %d missing: %s", i+1, norm(want[i])))
		case i >= len(want):
			out = append(out, fmt.Sprintf("event %d extra: %s", i+1, norm(got[i])))
		case norm(want[i]) != norm(got[i]) || want[i].Seq != got[i].Seq:
			out = append(out, fmt.Sprintf("event %d:\n  want %s\n  got  %s", i+1, norm(want[i]), norm(got[i])))
		}
		if len(out) >= 10 {
			break
		}
	}
	return out
}

func key(ev agentpipe.Event) string {
	switch ev.Type {
	case agentpipe.EvSession:
		return "session"
	case agentpipe.EvTurn:
		return "turn " + ev.Turn
	case agentpipe.EvOp:
		return "op " + ev.Op
	case agentpipe.EvApproval:
		return "approval " + ev.Request
	case agentpipe.EvUsage, agentpipe.EvPlan, agentpipe.EvNotice:
		return ev.Type + " " + ev.Turn
	}
	return "item " + ev.Item
}

// Loose compares two runs per key (the session, each turn, op, approval and
// item): each key's events must match in order; how keys interleave may
// differ between processes.
func Loose(want, got []agentpipe.Event) []string {
	group := func(evs []agentpipe.Event) map[string][]string {
		m := map[string][]string{}
		for _, ev := range evs {
			m[key(ev)] = append(m[key(ev)], norm(ev))
		}
		return m
	}
	w, g := group(want), group(got)
	keys := map[string]bool{}
	for k := range w {
		keys[k] = true
	}
	for k := range g {
		keys[k] = true
	}
	var ks []string
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []string
	for _, k := range ks {
		a, b := w[k], g[k]
		if strings.Join(a, "\n") == strings.Join(b, "\n") {
			continue
		}
		out = append(out, fmt.Sprintf("%s:\n  want %s\n  got  %s", k, strings.Join(a, "\n       "), strings.Join(b, "\n       ")))
	}
	return out
}

// Check returns every protocol rule the events break.
func Check(events []agentpipe.Event, ops []agentpipe.Op) []string {
	var bad []string
	fail := func(f string, a ...any) { bad = append(bad, fmt.Sprintf(f, a...)) }
	if len(events) == 0 {
		return []string{"no events"}
	}
	var caps agentpipe.Capabilities
	if e := events[0]; e.Type != agentpipe.EvSession || e.Status != agentpipe.StateStarting || e.Capabilities == nil {
		fail("event 1 must be the session starting, with capabilities: %s", norm(e))
	} else {
		caps = *e.Capabilities
	}
	turnEnd := map[string]bool{}
	turnSeen := map[string]bool{}
	opSeen := map[string]int{}
	approval := map[string]string{} // request -> last status
	approvalTurn := map[string]string{}
	toolLast := map[string]agentpipe.Event{}
	deltas := map[string]bool{}
	messages := map[string]bool{}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			fail("event %d has seq %d", i+1, ev.Seq)
		}
		if ev.Turn != "" && turnEnd[ev.Turn] && ev.Type != agentpipe.EvOp && ev.Type != agentpipe.EvNotice {
			fail("event %d belongs to ended turn %s: %s", ev.Seq, ev.Turn, norm(ev))
		}
		switch ev.Type {
		case agentpipe.EvSession:
			switch ev.Status {
			case agentpipe.StateStarting, agentpipe.StateReady, agentpipe.StateRunning, agentpipe.StateWaiting, agentpipe.StateStopped, agentpipe.StateError:
			default:
				fail("event %d: session status %q", ev.Seq, ev.Status)
			}
		case agentpipe.EvTurn:
			if ev.Turn == "" {
				fail("event %d: a turn event without a turn", ev.Seq)
			}
			if !turnSeen[ev.Turn] && ev.Status != agentpipe.TurnRunning {
				fail("event %d: turn %s starts %q, not running", ev.Seq, ev.Turn, ev.Status)
			}
			turnSeen[ev.Turn] = true
			if agentpipe.Terminal(ev.Status) {
				turnEnd[ev.Turn] = true
				if ev.Status == agentpipe.TurnFailed && ev.Error == nil {
					fail("event %d: failed turn %s without an error", ev.Seq, ev.Turn)
				}
				for req, st := range approval {
					if approvalTurn[req] == ev.Turn && st == agentpipe.ItemPending {
						fail("event %d: turn %s ended with approval %s pending", ev.Seq, ev.Turn, req)
					}
				}
			} else if ev.Status != agentpipe.TurnRunning {
				fail("event %d: turn status %q", ev.Seq, ev.Status)
			}
		case agentpipe.EvOp:
			opSeen[ev.Op]++
			if ev.OK == nil || (!*ev.OK && ev.Error == nil) {
				fail("event %d: op %s without ok, or refused without an error", ev.Seq, ev.Op)
			}
		case agentpipe.EvApproval:
			if ev.Request == "" {
				fail("event %d: approval without a request id", ev.Seq)
			}
			if ev.Status == agentpipe.ItemPending {
				if len(ev.Options) == 0 {
					fail("event %d: pending approval without options", ev.Seq)
				}
				if !caps.Approvals {
					fail("event %d: an approval from an adapter without the approvals capability", ev.Seq)
				}
				approvalTurn[ev.Request] = ev.Turn
			} else if approval[ev.Request] != agentpipe.ItemPending {
				fail("event %d: approval %s %s was never pending", ev.Seq, ev.Request, ev.Status)
			}
			approval[ev.Request] = ev.Status
		case agentpipe.EvTool:
			if ev.Item == "" || ev.Kind == "" {
				fail("event %d: tool without item or kind", ev.Seq)
			}
			toolLast[ev.Item] = ev
		case agentpipe.EvMessageDelta:
			if !caps.Partial {
				fail("event %d: a delta from an adapter without the partial capability", ev.Seq)
			}
			deltas[ev.Item] = true
		case agentpipe.EvMessage:
			if ev.Status == agentpipe.ItemCompleted {
				messages[ev.Item] = true
			}
		case agentpipe.EvOutput:
			if !caps.ToolOutput {
				fail("event %d: output from an adapter without the tool_output capability", ev.Seq)
			}
		case agentpipe.EvUsage:
			if !caps.Usage || ev.Usage == nil {
				fail("event %d: usage without the capability or numbers", ev.Seq)
			}
		case agentpipe.EvPlan:
			if !caps.Plan {
				fail("event %d: a plan from an adapter without the plan capability", ev.Seq)
			}
		case agentpipe.EvReasoning, agentpipe.EvNotice:
		default:
			fail("event %d: unknown type %q", ev.Seq, ev.Type)
		}
	}
	last := events[len(events)-1]
	if last.Type != agentpipe.EvSession || (last.Status != agentpipe.StateStopped && last.Status != agentpipe.StateError) {
		fail("the last event must be the session stopped or error: %s", norm(last))
	}
	for t := range turnSeen {
		if !turnEnd[t] {
			fail("turn %s never ended", t)
		}
	}
	for req, st := range approval {
		if st == agentpipe.ItemPending {
			fail("approval %s still pending at the end", req)
		}
	}
	for item, ev := range toolLast {
		if ev.Status == agentpipe.ItemRunning {
			fail("tool %s still running at the end", item)
		}
	}
	for item := range deltas {
		if !messages[item] {
			fail("message %s streamed but never completed", item)
		}
	}
	for _, op := range ops {
		if op.ID == "" {
			continue
		}
		if opSeen[op.ID] != 1 {
			fail("op %s (%s) answered %d times, want once", op.ID, op.Op, opSeen[op.ID])
		}
	}
	return bad
}

// Ops lists a recording's operations.
func Ops(entries []agentpipe.RecEntry) []agentpipe.Op {
	var out []agentpipe.Op
	for _, e := range entries {
		if e.T == agentpipe.RecOp {
			var op agentpipe.Op
			if json.Unmarshal(e.Op, &op) == nil {
				out = append(out, op)
			}
		}
	}
	return out
}
