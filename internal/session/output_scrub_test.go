package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

func TestSessionOutputScrubCoversPipeCopiesAndWarmIndex(t *testing.T) {
	const secret = "sssecretss" // replacement has the same byte length
	nested, _ := json.Marshal(map[string]any{"args": map[string]string{"text": secret}})
	event, _ := json.Marshal(map[string]any{"seq": 1, "at": "2026-11-05T00:00:00Z", "type": "output", "text": secret, "tool": string(nested)})
	data := append(event, '\n')
	h, r := streamOutputFixture(t, orchapi.SessionModePipe, data)
	if err := os.WriteFile(h.s.scrollbackPath(r.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(holder.PipeErrorPath(h.home, r.ID), []byte("stderr "+secret), 0600); err != nil {
		t.Fatal(err)
	}
	oldIndex := h.s.index(r.ID)
	if got, _, _, err := h.s.readEvents(r.ID, 0, 10); err != nil || len(got) != 1 {
		t.Fatalf("warm index: %v %v", got, err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET title=?,activity=?,term_title=?,note=? WHERE id=?`, secret, secret, secret, secret, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.d.ScrubAll(context.Background(), core.Scrub{Project: r.Project, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range h.s.sessionOutputPaths(r.ID) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), secret) {
			t.Fatalf("retained secret in %s: %s", path, b)
		}
		if path != holder.PipeErrorPath(h.home, r.ID) {
			var v map[string]any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatalf("invalid event: %s %v", b, err)
			}
			if v["seq"] != float64(1) || v["type"] != "output" || v["text"] != core.Redacted {
				t.Fatalf("event envelope/text: %v", v)
			}
			var tool map[string]any
			if err := json.Unmarshal([]byte(v["tool"].(string)), &tool); err != nil {
				t.Fatalf("nested JSON broken: %v", err)
			}
		}
	}
	if h.s.index(r.ID) == oldIndex {
		t.Fatal("warm index retained across inode replacement")
	}
	got, _, _, err := h.s.readEvents(r.ID, 0, 10)
	if err != nil || len(got) != 1 || strings.Contains(string(got[0]), secret) {
		t.Fatalf("indexed read: %s %v", got, err)
	}
	cleaned, ok := h.s.load(context.Background(), r.ID)
	if !ok || strings.Contains(cleaned.Title+cleaned.Activity+cleaned.Note+cleaned.TermTitle, secret) {
		t.Fatalf("derived text retained: %+v", cleaned)
	}
}

func TestSessionOutputScrubPreservesUnscopedShortText(t *testing.T) {
	h, r := streamOutputFixture(t, "", []byte("cat next to concatenate"))
	sc := core.Scrub{Project: r.Project, Subjects: []string{"zm-short"}, Texts: []string{"cat"}}
	if err := h.s.scrubSessionOutputs(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(h.s.scrollbackPath(r.ID))
	if err != nil || string(b) != "cat next to concatenate" {
		t.Fatalf("unscoped short text changed: %q %v", b, err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET work='bd:zm-short' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.scrubSessionOutputs(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(h.s.scrollbackPath(r.ID))
	if err != nil || strings.Contains(string(b), "cat") {
		t.Fatalf("scoped short text survived: %q %v", b, err)
	}
}

func TestSessionOutputUnknownWriterHoldSurvivesRestart(t *testing.T) {
	h, r := streamOutputFixture(t, orchapi.SessionModePipe, []byte("{\"seq\":1,\"type\":\"output\",\"text\":\"unknown writer secret\"}\n"))
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET output_drained=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	sc := core.Scrub{Project: r.Project, Texts: []string{"unknown writer secret"}}
	if err := h.s.scrubSessionOutputs(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("unverified writer released: %v", err)
	}
	h.close()
	h2 := startHarness(t, h.home, nil, true)
	t.Cleanup(h2.close)
	if held, err := h2.s.sessionOutputHeld(context.Background(), r.ID); err != nil || !held {
		t.Fatalf("restart lost hold: %v %v", held, err)
	}
	if _, err := orchclient.SessionEvents(context.Background(), h2.c, r.ID, 0, 10, 0); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("held event spool exposed: %v", err)
	}
	if err := h2.s.scrubSessionOutputs(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("ended label substituted for writer proof: %v", err)
	}
}

func TestSessionOutputScrubHoldsLiveWriterAcrossAdoption(t *testing.T) {
	home := keptHome(t)
	h := startHarness(t, home, nil, true)
	t.Cleanup(h.close)
	project := h.project("session-scrub-live", home)
	marker := filepath.Join(home, "release-output")
	const secret = "private live session output"
	ss := h.spawn("shell", `printf 'private live \033[1msession\033[0m\r\n output\n'; while [ ! -e '`+marker+`' ]; do sleep .02; done; printf '`+secret+`\n'; exit 0`, nil)
	atkTrackSession(t, h, ss)
	h.waitOutput(ss.ID, "session")
	sc := core.Scrub{Project: project.ID, Texts: []string{secret}}
	if err := h.d.ScrubAll(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("live writer did not hold: %v", err)
	}
	if _, err := orchclient.Output(context.Background(), h.c, ss.ID, 0); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("live output exposed: %v", err)
	}
	destination := h.project("session-scrub-destination")
	moved, err := orchclient.MoveSession(context.Background(), h.c, ss.ID, orchapi.MoveRequest{Project: destination.ID})
	if err != nil || moved.Project != destination.ID {
		t.Fatalf("move during output hold: %+v %v", moved, err)
	}
	h.close()
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	if _, err := orchclient.Output(context.Background(), h2.c, ss.ID, 0); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("adoption lost output barrier: %v", err)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h2.wait(ss.ID, "ended", ended)
	deadline := time.Now().Add(5 * time.Second)
	for h2.s.proc(ss.ID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("session proc did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h2.s.recoverSessionOutputHolds(context.Background())
	if held, err := h2.s.sessionOutputHeld(context.Background(), ss.ID); err != nil || held {
		t.Fatalf("drained session stayed held: %v %v", held, err)
	}
	output, err := orchclient.Output(context.Background(), h2.c, ss.ID, 0)
	if err != nil || strings.Contains(output.Data, "private live") || strings.Contains(output.Data, "session") {
		t.Fatalf("finished output leaked: %+v %v", output, err)
	}
	b, err := os.ReadFile(h2.s.scrollbackPath(ss.ID))
	if err != nil || strings.Contains(string(b), "private live") || strings.Contains(string(b), "session") {
		t.Fatalf("final save recreated secret: %q %v", b, err)
	}
}

func TestSessionOutputMalformedEventsDiscardAfterWriterProof(t *testing.T) {
	h, r := streamOutputFixture(t, orchapi.SessionModePipe, []byte("not JSON: malformed event secret\n"))
	if err := h.s.scrubSessionOutputs(context.Background(), core.Scrub{Project: r.Project, Texts: []string{"malformed event secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(holder.PipeEventsPath(h.home, r.ID)); !os.IsNotExist(err) {
		t.Fatalf("unverifiable spool retained: %v", err)
	}
	if held, err := h.s.sessionOutputHeld(context.Background(), r.ID); err != nil || held {
		t.Fatalf("discarded closed spool remained held: %v %v", held, err)
	}
}

func TestSessionOutputHoldDefersProjectCompaction(t *testing.T) {
	h, r := streamOutputFixture(t, "", []byte("compaction pending secret"))
	ctx := context.Background()
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET output_drained=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.scrubSessionOutputs(ctx, core.Scrub{Project: r.Project, Texts: []string{"compaction pending secret"}}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("writer not held: %v", err)
	}
	if _, err := h.d.Store.Registry().Exec(`INSERT OR IGNORE INTO session_task_compactions(project) VALUES(?)`, r.Project); err != nil {
		t.Fatal(err)
	}
	h.s.compactTaskScrubs(ctx)
	var pending int
	if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_compactions WHERE project=?`, r.Project).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("compaction ran through generic output hold: pending=%d err=%v", pending, err)
	}
}

// There is currently no public API for changing sessions.work. This direct
// SQL fixture checks that a durable hold's match policy remains immutable even
// if a future writer changes that linkage before recovery or the final save.
func TestSessionOutputHoldRetainsCreationMatchPolicy(t *testing.T) {
	h, r := streamOutputFixture(t, "", []byte("cat"))
	ctx := context.Background()
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET work='bd:zm-original',title='cat',output_drained=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	sc := core.Scrub{Project: r.Project, Subjects: []string{"zm-original"}, Texts: []string{"cat"}}
	if err := h.s.scrubSessionOutputs(ctx, sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("writer not held: %v", err)
	}
	var minLen int
	if err := h.d.Store.Registry().QueryRow(`SELECT min_len FROM session_output_holds WHERE session_id=?`, r.ID).Scan(&minLen); err != nil || minLen != 0 {
		t.Fatalf("creation-time scope lost: min_len=%d err=%v", minLen, err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET work='bd:zm-other',output_drained=1 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.saveSessionOutput(r.ID, []byte("cat")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(h.s.scrollbackPath(r.ID))
	if err != nil || strings.Contains(string(b), "cat") {
		t.Fatalf("final save weakened recorded scope: %q %v", b, err)
	}
	h.s.recoverSessionOutputHolds(ctx)
	if held, err := h.s.sessionOutputHeld(ctx, r.ID); err != nil || held {
		t.Fatalf("recorded policy did not recover: %v %v", held, err)
	}
	current, ok := h.s.load(ctx, r.ID)
	if !ok || strings.Contains(current.Title, "cat") {
		t.Fatalf("metadata recovery weakened recorded scope: %+v", current)
	}
}

func TestSessionOutputHoldRollsBackUnstartedSpawnWithoutOrphan(t *testing.T) {
	h, r := streamOutputFixture(t, "", nil)
	ctx := context.Background()
	if err := os.Remove(h.s.scrollbackPath(r.ID)); err != nil {
		t.Fatal(err)
	}
	// Scoped to the row's own work item: a live session is held for it even
	// with no copy of the text yet.
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='running',pid=0,output_drained=0,work='local:abc123' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	sc := core.Scrub{Project: r.Project, Subjects: []string{"local:abc123"}, Texts: []string{"unstarted spawn redaction"}}
	if err := h.s.scrubSessionOutputs(ctx, sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("unstarted row did not hold: %v", err)
	}
	if err := h.s.rollbackUnstartedSessionRow(ctx, h.d.Store.Registry(), r.ID); err != nil {
		t.Fatal(err)
	}
	var rows, holds, pending int
	if err := h.d.Store.Registry().QueryRow(`SELECT (SELECT count(*) FROM sessions WHERE id=?),(SELECT count(*) FROM session_output_holds WHERE session_id=?),(SELECT count(*) FROM session_task_compactions WHERE project=?)`, r.ID, r.ID, r.Project).Scan(&rows, &holds, &pending); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || holds != 0 || pending != 1 {
		t.Fatalf("rollback orphaned state: rows=%d holds=%d pending=%d", rows, holds, pending)
	}
	if err := h.s.scrubSessionOutputs(ctx, sc); err != nil {
		t.Fatalf("later scrub recreated orphan: %v", err)
	}
	if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_output_holds WHERE session_id=?`, r.ID).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("later scrub hold count=%d err=%v", holds, err)
	}
}

func TestSessionOutputRollbackRefusesRecordedWriter(t *testing.T) {
	h, r := streamOutputFixture(t, "", []byte("recorded writer output"))
	ctx := context.Background()
	if err := h.s.recordStartedHolder(ctx, r.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := h.s.rollbackUnstartedSessionRow(ctx, h.d.Store.Registry(), r.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("rollback accepted recorded holder: %v", err)
	}
	if _, ok := h.s.load(ctx, r.ID); !ok {
		t.Fatal("rollback deleted a row with recorded writer identity")
	}
}

func TestSessionOutputScrubWaitsForTaskDesignation(t *testing.T) {
	h, parent := streamOutputFixture(t, "", nil)
	ctx := context.Background()
	// This parent is a registered fixture, with no process to leak or signal.
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='idle',managed=0 WHERE id=?`, parent.ID); err != nil {
		t.Fatal(err)
	}
	candidate := row{Session: orchapi.Session{ID: api.NewSession8(), Machine: parent.Machine, Project: parent.Project, Agent: "codex", State: orchapi.StateRunning, Managed: true, Started: time.Now(), Updated: time.Now()}}
	candidate.Address = api.SessionAddress(candidate.Machine, candidate.ID)
	inserted := make(chan struct{})
	finishDesignation := make(chan struct{})
	designationDone := make(chan error, 1)
	designationFinished := make(chan struct{})
	var scrubFinished chan struct{}
	go func() {
		defer close(designationFinished)
		designationDone <- h.s.withSessionDesignation(func() error {
			if err := insertRow(ctx, h.d.Store.Registry(), candidate); err != nil {
				return err
			}
			close(inserted)
			<-finishDesignation
			return h.s.insertTask(ctx, candidate.Session, &taskStart{parent: parent.Address, depth: 1, requestJSON: `{"agent":"codex","prompt":"designation secret"}`})
		})
	}()
	// Always release and join the setup goroutine, including on assertion failure.
	released := false
	t.Cleanup(func() {
		if !released {
			close(finishDesignation)
		}
		select {
		case <-designationFinished:
		case <-time.After(3 * time.Second):
			t.Error("task designation goroutine did not finish")
		}
		if scrubFinished != nil {
			select {
			case <-scrubFinished:
			case <-time.After(3 * time.Second):
				t.Error("generic scrub goroutine did not finish")
			}
		}
	})
	select {
	case <-inserted:
	case err := <-designationDone:
		t.Fatalf("session insert failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("session was not inserted")
	}
	scrubStarted := make(chan struct{})
	scrubDone := make(chan error, 1)
	scrubFinished = make(chan struct{})
	go func() {
		defer close(scrubFinished)
		close(scrubStarted)
		scrubDone <- h.s.scrubSessionOutputs(ctx, core.Scrub{Project: parent.Project, Texts: []string{"designation secret"}})
	}()
	<-scrubStarted
	select {
	case err := <-scrubDone:
		t.Fatalf("generic scrub crossed pending task designation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishDesignation)
	released = true
	select {
	case err := <-designationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task designation did not commit")
	}
	select {
	case err := <-scrubDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("generic scrub did not resume")
	}
	var tasks, holds int
	if err := h.d.Store.Registry().QueryRow(`SELECT (SELECT count(*) FROM session_tasks WHERE id=?),(SELECT count(*) FROM session_output_holds WHERE session_id=?)`, candidate.ID, candidate.ID).Scan(&tasks, &holds); err != nil || tasks != 1 || holds != 0 {
		t.Fatalf("task was captured by generic redaction: tasks=%d holds=%d err=%v", tasks, holds, err)
	}
}

// TestSessionOutputLiveHoldIsScoped: a live session working the redacted
// issue is held even before the text shows (its agent may have it); a live
// session working something else, with no copy of the text, never blocks
// the redaction.
func TestSessionOutputLiveHoldIsScoped(t *testing.T) {
	h := newHarness(t, nil)
	t.Cleanup(h.close)
	project := h.project("session-scrub-scoped", h.home)
	unrelated := h.spawn("shell", "exec sleep 30", nil)
	atkTrackSession(t, h, unrelated)
	sc := core.Scrub{Project: project.ID, Subjects: []string{"local:abc123"}, Texts: []string{"scoped issue secret text"}}
	if err := h.d.ScrubAll(context.Background(), sc); err != nil {
		t.Fatalf("an unrelated live session blocked the redaction: %v", err)
	}
	if held, err := h.s.sessionOutputHeld(context.Background(), unrelated.ID); err != nil || held {
		t.Fatalf("unrelated live session held: %v %v", held, err)
	}
	scoped := h.spawn("shell", "exec sleep 30", func(r *orchapi.SpawnRequest) { r.Work = "local:abc123" })
	atkTrackSession(t, h, scoped)
	if err := h.d.ScrubAll(context.Background(), sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("a live session on the redacted issue was not held: %v", err)
	}
	if held, err := h.s.sessionOutputHeld(context.Background(), scoped.ID); err != nil || !held {
		t.Fatalf("scoped live session not held: %v %v", held, err)
	}
}

// The only copy is styled/wrapped, so literal matching cannot place the hold.
func TestSessionOutputTerminalSplitHoldAndScrub(t *testing.T) {
	const secret = "sk-live-PLANTED2secret0123456789abcdef"
	for name, split := range map[string]string{
		"SGR":        "\x1b[1m",
		"wrap":       "\r\r\n",
		"cursor":     "\r\n\x1b[12;1H",
		"OSC":        "\x1b]8;;https://example.test\x1b\\",
		"escaped":    `\u001b[1m\r\r\n`,
		"overstrike": "X\b",
		"indented":   "\r\n    ",
		"box":        " \u2502\r\n\u2502 ",
		"C1 CSI":     "\u009b1m",
	} {
		t.Run(name, func(t *testing.T) {
			data := "before:sk-live-PLANTED2" + split + "secret0123456789abcdef:after"
			h, r := streamOutputFixture(t, "", []byte(data))
			ctx := context.Background()
			// No real writer is needed to exercise the durable writer-proof barrier.
			if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='running',output_drained=0 WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			sc := core.Scrub{Project: r.Project, Texts: []string{secret}}
			if err := h.d.ScrubAll(ctx, sc); !api.IsCode(err, api.CodeUnavailable) {
				t.Fatalf("split-only copy did not hold unscoped writer: %v", err)
			}
			if _, err := orchclient.Output(ctx, h.c, r.ID, 0); !api.IsCode(err, api.CodeConflict) {
				t.Fatalf("held output served: %v", err)
			}
			if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='ended',output_drained=1 WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			if err := h.d.ScrubAll(ctx, sc); err != nil {
				t.Fatal(err)
			}
			if held, err := h.s.sessionOutputHeld(ctx, r.ID); err != nil || held {
				t.Fatalf("drained output remained held: %v %v", held, err)
			}
			b, err := os.ReadFile(h.s.scrollbackPath(r.ID))
			if name == "overstrike" {
				if !os.IsNotExist(err) {
					t.Fatalf("ambiguous overstrike output retained: %q %v", b, err)
				}
				return
			}
			const want = "before:[redacted]:after"
			if err != nil || string(b) != want {
				t.Fatalf("disk output: %q %v; want %q", b, err, want)
			}
			out, err := orchclient.Output(ctx, h.c, r.ID, 0)
			if err != nil || out.Data != want {
				t.Fatalf("served output: %+v %v", out, err)
			}
		})
	}
}

// Backspaces, cut-off escapes and cursor moves are everyday TUI output. With no
// fragment of the text present they must never hold an unrelated session.
// The same holds for a different secret of the redacted one's kind, which
// shares its public prefix (sharedPrefixCases).
func TestSessionOutputUnrelatedTerminalNoiseNotHeld(t *testing.T) {
	cases := append([]struct{ name, secret, output string }{{"noise", "sk-live-PLANTED2secret0123456789abcdef",
		"grep -P '\\bfoo\\b'\r\nfmt.Print(\"\\x1b[1m\")\r\n\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\" +
			"progress 45%\r\x1b[5;1Hdone\bX\x1b[2;3H\u009b\x1b["}}, sharedPrefixCases...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, r := streamOutputFixture(t, "", []byte(c.output))
			ctx := context.Background()
			if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='running',output_drained=0 WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			sc := core.Scrub{Project: r.Project, Texts: []string{c.secret}}
			if err := h.d.ScrubAll(ctx, sc); err != nil {
				t.Fatalf("unrelated live session blocked the redaction: %v", err)
			}
			if held, err := h.s.sessionOutputHeld(ctx, r.ID); err != nil || held {
				t.Fatalf("unrelated live session held: %v %v", held, err)
			}
			if b, err := os.ReadFile(h.s.scrollbackPath(r.ID)); err != nil || string(b) != c.output {
				t.Fatalf("unrelated scrollback changed: %q %v", b, err)
			}
		})
	}
	// The redacted secret itself, swapped by cursor moves, still holds.
	c := sharedPrefixCases[0]
	for _, x := range sharedPrefixCases {
		if x.name == "jwt-reissued" {
			c = x
		}
	}
	cut := len(c.secret) / 3
	data := "\x1b[1;" + strconv.Itoa(cut+1) + "H" + c.secret[cut:] + "\x1b[1;1H" + c.secret[:cut] + "\r\n"
	h, r := streamOutputFixture(t, "", []byte(data))
	ctx := context.Background()
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='running',output_drained=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.d.ScrubAll(ctx, core.Scrub{Project: r.Project, Texts: []string{c.secret}}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("swapped copy of the secret did not hold its writer: %v", err)
	}
	if held, err := h.s.sessionOutputHeld(ctx, r.ID); err != nil || !held {
		t.Fatalf("swapped copy not held: %v %v", held, err)
	}
}
