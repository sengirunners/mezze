package issues

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
)

// The three retained-copy cases, on the real service in a temp
// ZM_HOME: (a) a clean destination cannot supply a source's text, (b) a round
// trip erases the redirect edges that list earlier stops, (c) one registry
// issue id per (project, comment) misses a changed id in the same project.

func (f *recoveryFixture) commented(t *testing.T, ps istore.ProjectStore, pid, text string) (string, string) {
	t.Helper()
	id := recoveryApply(t, ps, istore.Mutation{Op: istore.OpCreate, Create: &orchapi.NewIssueRequest{Title: "copies fixture"}}).Issue.ID
	c := recoveryApply(t, ps, istore.Mutation{Op: istore.OpComment, ID: id, Comment: &orchapi.IssueCommentRequest{Text: text}}).Comment
	f.event(t, pid, id, text)
	return id, c.ID
}

func (f *recoveryFixture) event(t *testing.T, pid, subject, text string) {
	t.Helper()
	if _, err := f.s.d.Events.Publish(context.Background(), events.Input{Project: pid, Type: orchapi.EventWorkCommented, Subject: subject, Data: map[string]string{"body": text}}); err != nil {
		t.Fatal(err)
	}
}

func (f *recoveryFixture) eventsHold(t *testing.T, pid, text string) bool {
	t.Helper()
	db, err := f.s.d.Store.Project(f.s.d.System().Scope, authz.Read, pid)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE instr(json, ?) > 0`, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// redactAt is the admin route's redaction, at the issue's live location.
func (f *recoveryFixture) redactAt(t *testing.T, pid, id, cid string) error {
	t.Helper()
	ps, err := f.s.st.Project(context.Background(), pid, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.redact(context.Background(), f.s.d.System(), located{project: pid, id: id, ps: ps, vis: istore.Areas{All: true}}, cid)
	return err
}

func (f *recoveryFixture) snapshot(t *testing.T, pid string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	if err := f.native.Copy(context.Background(), pid, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `VACUUM INTO ?`, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

// (a), new redactions: redacting at the destination scrubs the source while
// the text is still at hand, so no later pass needs it.
func TestRedactScrubsTheSourceOfAMovedIssue(t *testing.T) {
	f := newRecoveryFixture(t)
	const text = "moved-source-secret-0001"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-destination", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	if err := f.redactAt(t, dest, nid, cid); err != nil {
		t.Fatal(err)
	}
	if f.eventsHold(t, f.pid, text) {
		t.Fatal("the source project's events keep the text after a redaction at the destination")
	}
}

// (b): a round trip back to the same id erases the source's redirect, so
// the chain from the live issue has one stop; the middle one still held it.
func TestRedactScrubsTheMiddleOfARoundTrip(t *testing.T) {
	f := newRecoveryFixture(t)
	const text = "round-trip-secret-0002"
	id, cid := f.commented(t, f.ps, f.pid, text)
	mid, ps := f.anotherProject(t, "copies-middle", "mid")
	nid := "mid-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, mid, id, nid)
	f.event(t, mid, nid, text)
	recoveryMove(t, ps, mid, f.ps, f.pid, nid, id)
	if err := f.redactAt(t, f.pid, id, cid); err != nil {
		t.Fatal(err)
	}
	if f.eventsHold(t, mid, text) {
		t.Fatal("the round trip's middle project keeps the text")
	}
}

// (c): the comment lived in one project under two ids (its prefix changed
// between visits). A restore of the copy holding the second id reapplies the
// redaction even though the registry row names the first.
func TestRestoreReappliesARedactionUnderAChangedID(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const text = "changed-id-secret-0003"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-away", "dst")
	suffix := id[len("rec-"):]
	copies := map[string]string{id: f.snapshot(t, f.pid)} // each visit's copy, with the text
	recoveryMove(t, f.ps, f.pid, ps, dest, id, "dst-"+suffix)
	if err := f.ps.SetPrefix(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	recoveryMove(t, ps, dest, f.ps, f.pid, "dst-"+suffix, "new-"+suffix)
	copies["new-"+suffix] = f.snapshot(t, f.pid)
	recoveryMove(t, f.ps, f.pid, ps, dest, "new-"+suffix, "dst-"+suffix)
	if err := f.redactAt(t, dest, "dst-"+suffix, cid); err != nil {
		t.Fatal(err)
	}
	// The redaction listed both stops here; the (project, comment) key kept
	// one row, and the collision dropped the other id. Restore the copy
	// that holds the dropped one.
	var kept string
	if err := f.s.d.Store.Registry().QueryRow(`SELECT issue_id FROM issue_redactions WHERE project = ? AND comment_id = ?`, f.pid, cid).Scan(&kept); err != nil || copies[kept] == "" {
		t.Fatalf("the registry row names %q (%v), want one of this project's stops", kept, err)
	}
	for sid := range copies {
		if f.stopState(t, cid, f.pid, sid) != stopClean {
			t.Fatalf("stop %s not scrubbed", sid)
		}
	}
	dropped := id
	if kept == id {
		dropped = "new-" + suffix
	}
	old := copies[dropped]
	cp, done, err := f.native.OpenCopy(ctx, f.pid, old)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if n, err := f.s.redactListed(ctx, f.pid, cp, nil); err != nil || n != 1 {
		t.Fatalf("restore reapplied %d redactions (%v), want the moved-back comment", n, err)
	}
	cs, err := cp.Comments(ctx, dropped, istore.Areas{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Text == text {
			t.Fatal("the restored copy serves the redacted text")
		}
	}
}

// (a), old redactions: an older zm redacted at the destination and scrubbed
// only there. The source's events keep the text and nothing zm can read
// still has it, so recovery records the source as a gap and the route
// reports it, instead of claiming the cleanup finished.
func TestRedactionReportsASourceOnlyAnOlderScrubCouldClean(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const text = "legacy-clean-destination-0004"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-clean", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	// The older route: the store, this project's copies, one registry row.
	at := f.s.now()
	res := recoveryApply(t, ps, istore.Mutation{Op: istore.OpCommentRedact, ID: nid, CommentID: cid, Moderate: func(string) bool { return true }})
	if err := f.s.scrubCopies(ctx, dest, nid, res.Redacted, at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.d.Store.Registry().Exec(`INSERT INTO issue_redactions (project, comment_id, issue_id, who, at) VALUES (?, ?, ?, 'machine:mini', ?)`, dest, cid, nid, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.s.recoverRedactions(ctx)
	if f.s.recovery.retry {
		t.Fatal("a gap no retry can close was retried")
	}
	gaps, err := f.s.redactionGaps(ctx, cid)
	if err != nil || len(gaps) != 1 || gaps[0] != (redactionGap{f.pid, id, gapTextGone}) {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
	if !f.eventsHold(t, f.pid, text) {
		t.Fatal("the fixture's source copy is gone: the test proves nothing")
	}
	err = f.redactAt(t, dest, nid, cid)
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), f.pid+":"+id+" (text gone)") || strings.Contains(err.Error(), text) {
		t.Fatalf("redacting again: %v", err)
	}
}

// (b), lost records: the middle project's redirect is gone (its store was
// restored, say). The round trip's hop is still in the issue's history, so
// the redaction names it as a gap with the ids it has.
func TestRedactReportsAHopNoRecordPlaces(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const text = "erased-hop-secret-0005"
	id, cid := f.commented(t, f.ps, f.pid, text)
	mid, ps := f.anotherProject(t, "copies-erased", "mid")
	nid := "mid-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, mid, id, nid)
	recoveryMove(t, ps, mid, f.ps, f.pid, nid, id)
	for _, pid := range []string{f.pid, mid} {
		if err := f.native.Copy(ctx, pid, func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `DELETE FROM moves`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := f.redactAt(t, f.pid, id, cid)
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), id+">"+nid+" (no move record)") {
		t.Fatalf("redaction with an erased hop: %v", err)
	}
	if recoveryHasComment(t, f.ps, id, text) {
		t.Fatal("the report replaced the redaction")
	}
}

// No false alarm: a comment redacted before its issue moved left no text at
// the destination, and one written after a move never reached the source.
func TestRedactionGapsSkipStopsThatNeverHeldTheText(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	id, early := f.commented(t, f.ps, f.pid, "redacted-before-the-move-0006")
	if err := f.redactAt(t, f.pid, id, early); err != nil {
		t.Fatal(err)
	}
	dest, ps := f.anotherProject(t, "copies-later", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	late := recoveryApply(t, ps, istore.Mutation{Op: istore.OpComment, ID: nid, Comment: &orchapi.IssueCommentRequest{Text: "written-after-the-move-0007"}}).Comment.ID
	if err := f.redactAt(t, dest, nid, late); err != nil {
		t.Fatal(err)
	}
	f.s.recoverRedactions(ctx)
	for _, cid := range []string{early, late} {
		if gaps, err := f.s.redactionGaps(ctx, cid); err != nil || len(gaps) != 0 {
			t.Fatalf("comment %s gaps %+v %v", cid, gaps, err)
		}
		if err := f.redactAt(t, dest, nid, cid); err != nil {
			t.Fatalf("redacting %s again: %v", cid, err)
		}
	}
}

func recoveryHasComment(t *testing.T, ps istore.ProjectStore, id, text string) bool {
	t.Helper()
	cs, err := ps.Comments(context.Background(), id, istore.Areas{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Text == text {
			return true
		}
	}
	return false
}

func (f *recoveryFixture) stopState(t *testing.T, cid, project, id string) string {
	t.Helper()
	var why string
	if err := f.s.d.Store.Registry().QueryRow(`SELECT why FROM issue_redaction_stops WHERE comment_id = ? AND project = ? AND issue_id = ?`, cid, project, id).Scan(&why); err != nil {
		t.Fatalf("stop %s:%s: %v", project, id, err)
	}
	return why
}

func (f *recoveryFixture) redactAs(t *testing.T, sub *authz.Subject, pid, id, cid string) error {
	t.Helper()
	ps, err := f.s.st.Project(context.Background(), pid, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.redact(context.Background(), sub, located{project: pid, id: id, ps: ps, vis: istore.Areas{All: true}}, cid)
	return err
}

func cutRedactionAt(t *testing.T, f *recoveryFixture, step string) {
	t.Helper()
	f.s.redactCut = func(s string) error {
		if s == step {
			return errors.New("cut off at " + s)
		}
		return nil
	}
	t.Cleanup(func() { f.s.redactCut = nil })
}

// (1): a redaction cut off after it listed every stop must not let the next
// attempt report success. Cut before the store write, the next attempt
// still has the text and cleans the source; cut after it, the text is gone
// and the next attempt reports each stop it could not scrub.
func TestRedactionCutOffAfterListingNeverReportsSuccessWithACopyLeft(t *testing.T) {
	for _, step := range []string{"listed", "written"} {
		t.Run(step, func(t *testing.T) {
			f := newRecoveryFixture(t)
			text := "cut-off-" + step + "-secret-0008"
			id, cid := f.commented(t, f.ps, f.pid, text)
			dest, ps := f.anotherProject(t, "copies-cut", "dst")
			nid := "dst-" + id[len("rec-"):]
			recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
			cutRedactionAt(t, f, step)
			if err := f.redactAt(t, dest, nid, cid); err == nil {
				t.Fatal("the cut did not happen")
			}
			f.s.redactCut = nil
			err := f.redactAt(t, dest, nid, cid)
			if step == "listed" {
				if err != nil || f.eventsHold(t, f.pid, text) {
					t.Fatalf("retry with the text at hand: %v, source still holds it: %v", err, f.eventsHold(t, f.pid, text))
				}
				return
			}
			if !f.eventsHold(t, f.pid, text) {
				t.Fatal("the fixture's source copy is gone: the test proves nothing")
			}
			if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), f.pid+":"+id+" (text gone)") {
				t.Fatalf("retry after the text was lost: %v", err)
			}
		})
	}
}

// (2): a stop the walk names but cannot scrub (here, a project this daemon
// does not hold) is a gap only for a comment it can have held: one written
// before the issue left it, not one written after.
func TestRedactionFarStopIsAGapOnlyForCommentsItHeld(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const remote = "01REMOTEPROJECT0000000000"
	rps, err := f.native.Project(ctx, remote, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := rps.SetPrefix(ctx, "rem"); err != nil {
		t.Fatal(err)
	}
	// Fixed times, not the wall clock: the early comment ties the move (a
	// tie is held), the late one is a second after it, so it cannot be.
	at := time.UnixMilli(1_800_000_000_000)
	f.s.now = func() time.Time { return at.Add(2 * time.Second) }
	rid := recoveryApply(t, rps, istore.Mutation{Op: istore.OpCreate, At: at, Create: &orchapi.NewIssueRequest{Title: "remote fixture"}}).Issue.ID
	early := recoveryApply(t, rps, istore.Mutation{Op: istore.OpComment, ID: rid, At: at, Comment: &orchapi.IssueCommentRequest{Text: "written-before-the-move-0009"}}).Comment.ID
	id := "rec-" + rid[len("rem-"):]
	recoveryMoveAt(t, rps, remote, f.ps, f.pid, rid, id, at)
	late := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpComment, ID: id, At: at.Add(time.Second), Comment: &orchapi.IssueCommentRequest{Text: "written-after-the-move-0010"}}).Comment.ID
	if err := f.redactAt(t, f.pid, id, late); err != nil {
		t.Fatalf("a comment written after the move: %v", err)
	}
	err = f.redactAt(t, f.pid, id, early)
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), remote+":"+rid+" ("+gapRemote+")") || strings.Contains(err.Error(), gapNoRecord) {
		t.Fatalf("a comment the remote stop held: %v", err)
	}
}

// A millisecond tie counts as held: a comment written in the move's ms (the
// source left as it was created), or a move that arrived in the redaction's
// ms, may have left the text at that stop. The tie is forced with fixed
// mutation and service times. Uncut, the tied stop is scrubbed and purged
// fresh; cut after the store write, the retry records it as a gap. Never
// clean without a scrub.
func TestRedactionMillisecondTieCountsAsHeld(t *testing.T) {
	at := time.UnixMilli(1_800_000_000_000)
	for _, tc := range []struct {
		name                  string
		comment, move, redact time.Time
		source                bool // the tied stop: the source, else the destination
	}{
		{"comment in the move's ms", at, at, at.Add(time.Second), true},
		{"arrival in the redaction's ms", at.Add(-time.Second), at, at, false},
	} {
		for _, step := range []string{"", "written"} {
			t.Run(tc.name+"/cut="+step, func(t *testing.T) {
				f := newRecoveryFixture(t)
				f.s.now = func() time.Time { return tc.redact }
				text := "tie-secret-0016"
				id := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpCreate, At: tc.comment, Create: &orchapi.NewIssueRequest{Title: "tie fixture"}}).Issue.ID
				cid := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpComment, ID: id, At: tc.comment, Comment: &orchapi.IssueCommentRequest{Text: text}}).Comment.ID
				dest, ps := f.anotherProject(t, "copies-tie", "dst")
				nid := "dst-" + id[len("rec-"):]
				recoveryMoveAt(t, f.ps, f.pid, ps, dest, id, nid, tc.move)
				tied, tid := dest, nid
				if tc.source {
					tied, tid = f.pid, id
				}
				f.event(t, tied, tid, text)
				if step == "" {
					if err := f.redactAt(t, dest, nid, cid); err != nil {
						t.Fatal(err)
					}
					if f.eventsHold(t, tied, text) {
						t.Fatal("the tied stop was not scrubbed")
					}
					calls := f.homes.callsFor(tied)
					if len(calls) == 0 || !calls[len(calls)-1].fresh || !calls[len(calls)-1].ran {
						t.Fatalf("the tied stop's backups were not purged fresh: %+v", calls)
					}
					if why := f.stopState(t, cid, tied, tid); why != stopClean {
						t.Fatalf("tied stop %q", why)
					}
					return
				}
				cutRedactionAt(t, f, step)
				_ = f.redactAt(t, dest, nid, cid)
				f.s.redactCut = nil
				err := f.redactAt(t, dest, nid, cid)
				if !f.eventsHold(t, tied, text) {
					t.Fatal("the fixture's copy is gone: the test proves nothing")
				}
				if why := f.stopState(t, cid, tied, tid); why != gapTextGone {
					t.Fatalf("tied stop %q with its copy unscrubbed (err %v)", why, err)
				}
				if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), tied+":"+tid+" (text gone)") {
					t.Fatalf("retry after the text was lost: %v", err)
				}
			})
		}
	}
}

// (3): a "text gone" gap stays recorded, even after the stops' logs drop
// every event filed under the issue: other copies (session holds, outbox
// rows, work comment copies, backups) cannot be proven gone by id, so zm
// keeps answering conflict until an operator acknowledges it.
func TestRedactionGapDoesNotClearAfterEventsArePruned(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	const text = "gap-stays-secret-0011"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-stays", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	cutRedactionAt(t, f, "written")
	_ = f.redactAt(t, dest, nid, cid)
	f.s.redactCut = nil
	if err := f.redactAt(t, dest, nid, cid); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("while the source's events hold the issue: %v", err)
	}
	for pid, sub := range map[string]string{f.pid: id, dest: nid} {
		if _, err := f.s.d.Store.Writer(pid).ExecContext(ctx, `DELETE FROM events WHERE subject = ?`, sub); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		err := f.redactAt(t, dest, nid, cid)
		if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), f.pid+":"+id+" (text gone)") || strings.Contains(err.Error(), text) {
			t.Fatalf("after the events were pruned: %v", err)
		}
	}
	f.s.recoverRedactions(ctx)
	// Both stops: the cut came before either was scrubbed.
	gaps, err := f.s.redactionGaps(ctx, cid)
	if err != nil || len(gaps) != 2 {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
	for _, g := range gaps {
		if g.why != gapTextGone || g != (redactionGap{f.pid, id, gapTextGone}) && g != (redactionGap{dest, nid, gapTextGone}) {
			t.Fatalf("gaps %+v", gaps)
		}
	}
}

// A stop is clean only once its backups were purged: a failed purge leaves
// it unsettled, so the next call purges again instead of reporting settled.
func TestRedactionStopStaysUnsettledWhenThePurgeFails(t *testing.T) {
	f := newRecoveryFixture(t)
	const text = "purge-fails-secret-0014"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-purge", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	f.homes.fail = map[string]bool{f.pid: true}
	if err := f.redactAt(t, dest, nid, cid); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("with the source's purge failing: %v", err)
	}
	if f.eventsHold(t, f.pid, text) {
		t.Fatal("the source was not scrubbed")
	}
	if why := f.stopState(t, cid, f.pid, id); why != stopPurge {
		t.Fatalf("source stop %q after a failed purge", why)
	}
	if why := f.stopState(t, cid, dest, nid); why != stopClean {
		t.Fatalf("destination stop %q", why)
	}
	if err := f.redactAt(t, dest, nid, cid); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("again, the purge still failing: %v", err)
	}
	f.homes.fail = nil
	for len(f.homes.purged) > 0 {
		<-f.homes.purged
	}
	if err := f.redactAt(t, dest, nid, cid); err != nil {
		t.Fatalf("once the purge works: %v", err)
	}
	if why := f.stopState(t, cid, f.pid, id); why != stopClean {
		t.Fatalf("source stop %q after the purge", why)
	}
	if gaps, err := f.s.redactionGaps(context.Background(), cid); err != nil || len(gaps) != 0 {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
}

// A purge that failed before it marked the backups leaves nothing waiting
// in homes, so the retry of the unsettled stop must purge fresh: a purge of
// only marked backups replaces nothing, and the stop must not turn clean.
func TestRedactionPurgeRetryIsFreshWhenTheFirstFailedBeforeItsMark(t *testing.T) {
	f := newRecoveryFixture(t)
	const text = "purge-before-mark-secret-0015"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-premark", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	f.homes.fail = map[string]bool{f.pid: true}
	f.homes.failBeforeMark = true
	if err := f.redactAt(t, dest, nid, cid); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("with the source's purge failing: %v", err)
	}
	if why := f.stopState(t, cid, f.pid, id); why != stopPurge {
		t.Fatalf("source stop %q after a failed purge", why)
	}
	f.homes.fail = nil
	if err := f.redactAt(t, dest, nid, cid); err != nil {
		t.Fatalf("once the purge works: %v", err)
	}
	calls := f.homes.callsFor(f.pid)
	if len(calls) == 0 {
		t.Fatal("no purge of the source")
	}
	last := calls[len(calls)-1]
	if last.err || !last.fresh || !last.ran {
		t.Fatalf("the retry purged %+v (all %+v): the old backups keep the text", last, calls)
	}
	if why := f.stopState(t, cid, f.pid, id); why != stopClean {
		t.Fatalf("source stop %q after a fresh purge", why)
	}
	if gaps, err := f.s.redactionGaps(context.Background(), cid); err != nil || len(gaps) != 0 {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
}

// (4): the conflict names a stop only to a caller who can read its project;
// the scrub there runs either way.
func TestRedactionConflictHidesStopsTheCallerCannotRead(t *testing.T) {
	f := newRecoveryFixture(t)
	const text = "hidden-stop-secret-0012"
	id, cid := f.commented(t, f.ps, f.pid, text)
	dest, ps := f.anotherProject(t, "copies-hidden", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	cutRedactionAt(t, f, "written")
	_ = f.redactAt(t, dest, nid, cid)
	f.s.redactCut = nil
	owner := &authz.Subject{Identity: api.Identity{Name: "ana", Kind: api.IdentityHuman}, Address: "human:ana", Via: authz.ViaLocal,
		Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Owner, Object: authz.ProjectObject(dest)}}, nil)}
	err := f.redactAs(t, owner, dest, nid, cid)
	if !api.IsCode(err, api.CodeConflict) || strings.Contains(err.Error(), f.pid) || strings.Contains(err.Error(), id+" ") ||
		!strings.Contains(err.Error(), "a stop in another project (text gone)") || !strings.Contains(err.Error(), dest+":"+nid) {
		t.Fatalf("an owner of the destination only: %v", err)
	}
}

// (5): nothing is listed for a comment the issue does not have, or for a
// caller who may not redact it.
func TestRedactionListsNothingBeforeTheCommentAndCallerAreChecked(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	id, cid := f.commented(t, f.ps, f.pid, "checked-first-secret-0013")
	dest, ps := f.anotherProject(t, "copies-checked", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	if err := f.redactAt(t, dest, nid, "01BOGUSCOMMENT"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a bogus comment: %v", err)
	}
	member := &authz.Subject{Identity: api.Identity{Name: "bo", Kind: api.IdentityHuman}, Address: "human:bo", Via: authz.ViaToken,
		Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(dest)}}, nil)}
	if _, err := f.s.redactEverywhere(ctx, ctx, member, located{project: dest, id: nid, ps: ps, vis: istore.Areas{All: true}}, cid, member.Address, 1); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member: %v", err)
	}
	var n int
	if err := f.s.d.Store.Registry().QueryRow(`SELECT (SELECT count(*) FROM issue_redactions) + (SELECT count(*) FROM issue_redaction_stops)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows listed (%v)", n, err)
	}
}

// An issue that never moved held its comment at its one stop: a redaction
// cut off after the store write must not pass as clean on the retry while
// the stop's event log still holds the text.
func TestRedactionCutOffOnAnUnmovedIssueIsAGap(t *testing.T) {
	f := newRecoveryFixture(t)
	const text = "unmoved-cut-secret-0016"
	id, cid := f.commented(t, f.ps, f.pid, text)
	cutRedactionAt(t, f, "written")
	if err := f.redactAt(t, f.pid, id, cid); err == nil {
		t.Fatal("the cut did not happen")
	}
	f.s.redactCut = nil
	if !f.eventsHold(t, f.pid, text) {
		t.Fatal("the fixture's copy is gone: the test proves nothing")
	}
	err := f.redactAt(t, f.pid, id, cid)
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), f.pid+":"+id+" (text gone)") {
		t.Fatalf("retry after the text was lost: %v (stop %q)", err, f.stopState(t, cid, f.pid, id))
	}
}

// No false gap on an unmoved issue (PR484 review): a repeat redaction whose
// registry row names an older issue id (an older zm, before stop rows), and
// a first redaction of a comment whose text is already gone. Neither stop
// has a pending row, so neither counts as held.
func TestRedactionUnmovedIssueNoFalseGap(t *testing.T) {
	f := newRecoveryFixture(t)
	id, cid := f.commented(t, f.ps, f.pid, "unmoved-repeat-secret-0017")
	if err := f.redactAt(t, f.pid, id, cid); err != nil {
		t.Fatal(err)
	}
	reg := f.s.d.Store.Registry()
	if _, err := reg.Exec(`DELETE FROM issue_redaction_stops WHERE comment_id = ?`, cid); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Exec(`UPDATE issue_redactions SET issue_id = 'rec-older' WHERE comment_id = ?`, cid); err != nil {
		t.Fatal(err)
	}
	if err := f.redactAt(t, f.pid, id, cid); err != nil {
		t.Fatalf("repeat redact under an older id: %v", err)
	}
	id2, cid2 := f.commented(t, f.ps, f.pid, "[redacted]")
	if err := f.redactAt(t, f.pid, id2, cid2); err != nil {
		t.Fatalf("first redact of a comment with no text left: %v", err)
	}
}
