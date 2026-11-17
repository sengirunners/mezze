package conformance

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
	_ "github.com/sengirunners/mezze/internal/agentpipe/builtin"
)

var update = flag.Bool("update", false, "rewrite the golden event files from the recordings")

func recordings(t *testing.T) []string {
	t.Helper()
	recs, err := filepath.Glob("../testdata/*/*.rec.jsonl")
	if err != nil || len(recs) == 0 {
		t.Fatalf("no recordings: %v", err)
	}
	return recs
}

func driverOf(rec string) string { return filepath.Base(filepath.Dir(rec)) }

// TestReplayGolden: each recorded session, replayed through its built-in
// driver in process, writes exactly the recorded runtime lines and emits
// exactly the golden events (-update rewrites them).
func TestReplayGolden(t *testing.T) {
	for _, rec := range recordings(t) {
		t.Run(driverOf(rec)+"/"+filepath.Base(rec), func(t *testing.T) {
			d, ok := agentpipe.New(driverOf(rec))
			if !ok {
				t.Fatalf("no driver %s", driverOf(rec))
			}
			_, entries, err := Load(rec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Replay(d, entries)
			if err != nil {
				t.Fatal(err)
			}
			if bad := Check(got, Ops(entries)); len(bad) > 0 {
				t.Fatalf("breaks the protocol:\n%s", strings.Join(bad, "\n"))
			}
			golden := strings.TrimSuffix(rec, ".rec.jsonl") + ".events.jsonl"
			if *update {
				if err := os.WriteFile(golden, WriteEvents(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			b, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			want, err := ReadEvents(b)
			if err != nil {
				t.Fatal(err)
			}
			if diff := Exact(want, got); len(diff) > 0 {
				t.Fatalf("events differ from %s (go test -update rewrites it):\n%s", golden, strings.Join(diff, "\n"))
			}
		})
	}
}

// TestProcessReplay runs each recording against the built-in adapter as a
// process (this test binary), with the fake runtime: what
// zm-agent-conformance check does for any adapter.
func TestProcessReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recordings(t) {
		t.Run(driverOf(rec)+"/"+filepath.Base(rec), func(t *testing.T) {
			_, entries, err := Load(rec)
			if err != nil {
				t.Fatal(err)
			}
			got, problems, err := Run([]string{self, agentpipe.Arg, driverOf(rec)}, []string{self, agentpipe.FakeArg}, rec, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			problems = append(problems, Check(got, Ops(entries))...)
			b, err := os.ReadFile(strings.TrimSuffix(rec, ".rec.jsonl") + ".events.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			want, err := ReadEvents(b)
			if err != nil {
				t.Fatal(err)
			}
			problems = append(problems, Loose(want, got)...)
			if len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
		})
	}
}

// TestCheckCatches: the protocol checks catch what they claim to.
func TestCheckCatches(t *testing.T) {
	yes := true
	caps := &agentpipe.Capabilities{Approvals: true}
	base := []agentpipe.Event{
		{Type: agentpipe.EvSession, Status: agentpipe.StateStarting, Capabilities: caps},
		{Type: agentpipe.EvOp, Op: "a", OK: &yes},
		{Type: agentpipe.EvTurn, Turn: "t1", Status: agentpipe.TurnRunning},
	}
	seq := func(evs []agentpipe.Event) []agentpipe.Event {
		for i := range evs {
			evs[i].Seq = int64(i + 1)
		}
		return evs
	}
	cases := map[string][]agentpipe.Event{
		"never ended": append(append([]agentpipe.Event(nil), base...), agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped}),
		"pending approval at the end": append(append([]agentpipe.Event(nil), base...),
			agentpipe.Event{Type: agentpipe.EvApproval, Turn: "t1", Request: "r", Status: agentpipe.ItemPending, Options: []string{"accept"}},
			agentpipe.Event{Type: agentpipe.EvTurn, Turn: "t1", Status: agentpipe.TurnCompleted},
			agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped}),
		"no last session": append(append([]agentpipe.Event(nil), base...), agentpipe.Event{Type: agentpipe.EvTurn, Turn: "t1", Status: agentpipe.TurnCompleted}),
		"delta without partial": append(append([]agentpipe.Event(nil), base...),
			agentpipe.Event{Type: agentpipe.EvMessageDelta, Turn: "t1", Item: "m", Text: "x"},
			agentpipe.Event{Type: agentpipe.EvTurn, Turn: "t1", Status: agentpipe.TurnCompleted},
			agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped}),
		"failed without error": append(append([]agentpipe.Event(nil), base...),
			agentpipe.Event{Type: agentpipe.EvTurn, Turn: "t1", Status: agentpipe.TurnFailed},
			agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped}),
	}
	for name, evs := range cases {
		if bad := Check(seq(evs), nil); len(bad) == 0 {
			t.Errorf("%s: no problem found", name)
		}
	}
	ok := seq(append(append([]agentpipe.Event(nil), base...),
		agentpipe.Event{Type: agentpipe.EvTurn, Turn: "t1", Status: agentpipe.TurnCompleted},
		agentpipe.Event{Type: agentpipe.EvSession, Status: agentpipe.StateStopped}))
	if bad := Check(ok, []agentpipe.Op{{ID: "a"}}); len(bad) > 0 {
		t.Errorf("a good run: %v", bad)
	}
	if bad := Check(ok, []agentpipe.Op{{ID: "a"}, {ID: "b", Op: "turn"}}); len(bad) == 0 {
		t.Error("an unanswered op passed")
	}
}

// TestFakeRefusesAnUnrecordedLine: a driver writing what the recording does
// not have fails the replay.
func TestFakeRefusesAnUnrecordedLine(t *testing.T) {
	_, entries, err := Load("../testdata/codex/turns.rec.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		if e.T == agentpipe.RecIn {
			var f map[string]any
			_ = json.Unmarshal(e.Frame, &f)
			f["extra"] = true
			entries[i].Frame, _ = json.Marshal(f)
			break
		}
	}
	d, _ := agentpipe.New("codex")
	if _, err := Replay(d, entries); err == nil || !strings.Contains(err.Error(), "the driver wrote") {
		t.Fatalf("replay: %v", err)
	}
}

// TestScrub: paths, emails and setup fields leave a recording.
func TestScrub(t *testing.T) {
	in := []agentpipe.RecEntry{
		{T: agentpipe.RecOut, Frame: json.RawMessage(`{"type":"system","subtype":"init","cwd":"/Users/ana/w","tools":["Bash"],"account":{"email":"ana@example.org"},"note":"mail ana@corp.io"}`)},
		{T: agentpipe.RecOut, Frame: json.RawMessage(`{"method":"mcpServer/startupStatus/updated","params":{"name":"secret-server"}}`)},
		{T: agentpipe.RecOp, Op: json.RawMessage(`{"op":"start","start":{"cwd":"/Users/ana/w"}}`)},
	}
	out := Scrub(in, [][2]string{{"/Users/ana/w", "/work"}, {"/Users/ana", "/home/user"}})
	b, _ := json.Marshal(out)
	s := string(b)
	for _, bad := range []string{"/Users/ana", "ana@", "tools", "account", "secret-server"} {
		if strings.Contains(s, bad) {
			t.Errorf("scrubbed recording still has %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, `/work`) || !strings.Contains(s, "user@example.com") || len(out) != 2 {
		t.Errorf("scrub: %s", s)
	}
}
