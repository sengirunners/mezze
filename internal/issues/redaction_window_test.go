package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// A stop counts as having held a comment's text until the store
// overwrote it, not until the first attempt listed it. Fixed mutation and
// service times, a second apart, so no millisecond tie decides a case.

func (f *recoveryFixture) commentedAt(t *testing.T, at time.Time, text string) (string, string) {
	t.Helper()
	id := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpCreate, At: at, Create: &orchapi.NewIssueRequest{Title: "window fixture"}}).Issue.ID
	cid := recoveryApply(t, f.ps, istore.Mutation{Op: istore.OpComment, ID: id, At: at, Comment: &orchapi.IssueCommentRequest{Text: text}}).Comment.ID
	f.event(t, f.pid, id, text)
	return id, cid
}

func (f *recoveryFixture) clockAt(at time.Time) { f.s.now = func() time.Time { return at } }

// A cut-off attempt lists the stops, the issue moves on, and a retry
// overwrites the text: the stop the move reached held it until then, so
// it is scrubbed or reported, never marked clean unscrubbed. The retry is
// cut after its store write, so the attempt after it has no text: the new
// stop must be a gap. (The earliest attempt's time marked it clean.)
func TestRedactionWindowEndsWhenTheStoreOverwroteTheText(t *testing.T) {
	at := time.UnixMilli(1_800_000_000_000)
	sec := func(n int) time.Time { return at.Add(time.Duration(n) * time.Second) }

	f := newRecoveryFixture(t)
	const text = "window-new-stop-secret-0021"
	id, cid := f.commentedAt(t, at, text)
	f.clockAt(sec(1))
	cutRedactionAt(t, f, "listed")
	if err := f.redactAt(t, f.pid, id, cid); err == nil {
		t.Fatal("the cut did not happen")
	}
	dest, ps := f.anotherProject(t, "window-new", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMoveAt(t, f.ps, f.pid, ps, dest, id, nid, sec(2))
	f.event(t, dest, nid, text)
	f.clockAt(sec(3))
	cutRedactionAt(t, f, "written")
	if err := f.redactAt(t, dest, nid, cid); err == nil {
		t.Fatal("the second cut did not happen")
	}
	f.s.redactCut = nil
	f.clockAt(sec(4))
	err := f.redactAt(t, dest, nid, cid)
	if !f.eventsHold(t, dest, text) {
		t.Fatal("the fixture's destination copy is gone: the test proves nothing")
	}
	if why := f.stopState(t, cid, dest, nid); why != gapTextGone {
		t.Fatalf("the stop the move reached is %q with its copy unscrubbed (err %v)", why, err)
	}
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), dest+":"+nid+" ("+gapTextGone+")") {
		t.Fatalf("retry after the text was lost: %v", err)
	}
}

// No false alarm: a comment redacted, then moved twice, then redacted again
// never had its text at the later stops. Ending the window at the latest
// attempt instead would report them as text gone.
func TestRedactionAgainAfterALaterMoveIsNoGap(t *testing.T) {
	at := time.UnixMilli(1_800_000_000_000)
	sec := func(n int) time.Time { return at.Add(time.Duration(n) * time.Second) }
	f := newRecoveryFixture(t)
	ctx := context.Background()
	id, cid := f.commentedAt(t, at, "window-redacted-first-0022")
	f.clockAt(sec(1))
	if err := f.redactAt(t, f.pid, id, cid); err != nil {
		t.Fatal(err)
	}
	dest, ps := f.anotherProject(t, "window-later", "dst")
	nid := "dst-" + id[len("rec-"):]
	recoveryMoveAt(t, f.ps, f.pid, ps, dest, id, nid, sec(2))
	last, lps := f.anotherProject(t, "window-last", "lst")
	lid := "lst-" + id[len("rec-"):]
	recoveryMoveAt(t, ps, dest, lps, last, nid, lid, sec(3))
	f.clockAt(sec(4))
	if err := f.redactAt(t, last, lid, cid); err != nil {
		t.Fatalf("redacting again after later moves: %v", err)
	}
	if gaps, err := f.s.redactionGaps(ctx, cid); err != nil || len(gaps) != 0 {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
	for _, st := range [][2]string{{dest, nid}, {last, lid}} {
		if why := f.stopState(t, cid, st[0], st[1]); why != stopClean {
			t.Fatalf("stop %s:%s %q", st[0], st[1], why)
		}
	}
}
