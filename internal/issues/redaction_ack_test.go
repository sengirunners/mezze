package issues

import (
	"context"
	"strings"
	"testing"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// An operator acknowledgement clears one named redaction gap.

func (f *recoveryFixture) ackAs(t *testing.T, sub *authz.Subject, pid, id, cid, stop string) (orchapi.RedactionGapAck, error) {
	t.Helper()
	ps, err := f.s.st.Project(context.Background(), pid, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.s.ackGap(context.Background(), sub, located{project: pid, id: id, ps: ps, vis: istore.Areas{All: true}}, cid, stop)
	if err != nil {
		return orchapi.RedactionGapAck{}, err
	}
	return v.(orchapi.RedactionGapAck), nil
}

func (f *recoveryFixture) acks(t *testing.T, cid string) []string {
	t.Helper()
	rows, err := f.s.d.Store.Registry().Query(`SELECT project || ':' || issue_id || ' ' || why || ' ' || who FROM issue_redaction_acks WHERE comment_id = ? AND at > 0`, cid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// textGoneAtSource leaves a moved issue's comment with a "text gone" gap at
// its source: a redaction cut off after the store write, then retried.
func textGoneAtSource(t *testing.T, f *recoveryFixture, slug, secret string) (dest, id, nid, cid string) {
	t.Helper()
	id, cid = f.commented(t, f.ps, f.pid, secret)
	dest, ps := f.anotherProject(t, slug, "dst")
	nid = "dst-" + id[len("rec-"):]
	recoveryMove(t, f.ps, f.pid, ps, dest, id, nid)
	cutRedactionAt(t, f, "written")
	_ = f.redactAt(t, dest, nid, cid)
	f.s.redactCut = nil
	if err := f.redactAt(t, dest, nid, cid); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), f.pid+":"+id+" (text gone)") {
		t.Fatalf("the fixture's gap: %v", err)
	}
	return dest, id, nid, cid
}

func TestAckGapClearsTheStopAndTheNextRedactSucceeds(t *testing.T) {
	f := newRecoveryFixture(t)
	const secret = "ack-clears-secret-0101"
	dest, id, nid, cid := textGoneAtSource(t, f, "ack-clears", secret)
	admin := &authz.Subject{Identity: api.Identity{Name: "ana", Kind: api.IdentityHuman}, Address: "human:ana", Via: authz.ViaToken, // moderates both stops
		Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Owner, Object: authz.ProjectObject(dest)}, {Relation: authz.Owner, Object: authz.ProjectObject(f.pid)}}, nil)}
	// Named as the caller's conflict names it, pasted with its "(why)".
	a, err := f.ackAs(t, admin, dest, nid, cid, f.pid+":"+id+" (text gone)")
	if err != nil {
		t.Fatal(err)
	}
	// The cut lost the text at both stops; the destination's gap stays open.
	left := dest + ":" + nid + " (text gone)"
	if a.Stop != f.pid+":"+id || a.Why != gapTextGone || a.Comment != cid || len(a.Remaining) != 1 || a.Remaining[0] != left || a.At.IsZero() || a.By == "" || strings.Contains(a.Stop+a.Why, secret) {
		t.Fatalf("ack %+v", a)
	}
	if why := f.stopState(t, cid, f.pid, id); why != stopClean {
		t.Fatalf("stop %q after the ack", why)
	}
	if got := f.acks(t, cid); len(got) != 1 || got[0] != f.pid+":"+id+" "+gapTextGone+" human:ana" {
		t.Fatalf("audit rows %q", got)
	}
	// The next redact reports only the stop not acknowledged.
	err = f.redactAt(t, dest, nid, cid)
	if !api.IsCode(err, api.CodeConflict) || strings.Contains(err.Error(), f.pid+":"+id) || !strings.Contains(err.Error(), left) {
		t.Fatalf("redact after one ack: %v", err)
	}
	if a, err = f.ackAs(t, admin, dest, nid, cid, dest+":"+nid); err != nil || len(a.Remaining) != 0 {
		t.Fatalf("second stop: %+v %v", a, err)
	}
	if err := f.redactAt(t, dest, nid, cid); err != nil {
		t.Fatalf("redact after the acks: %v", err)
	}
	// It cleared the record only: no text search, no scrub at the stop.
	if !f.eventsHold(t, f.pid, secret) {
		t.Fatal("the ack (or the redact after it) scrubbed the source: it must only clear the record")
	}
	// Once cleared, the same stop is no gap any more.
	if _, err := f.ackAs(t, admin, dest, nid, cid, f.pid+":"+id); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a second ack: %v", err)
	}
}

func TestAckGapFollowsTheRedactScope(t *testing.T) {
	f := newRecoveryFixture(t)
	dest, id, nid, cid := textGoneAtSource(t, f, "ack-scope", "ack-scope-secret-0102")
	stop := f.pid + ":" + id
	member := &authz.Subject{Identity: api.Identity{Name: "bo", Kind: api.IdentityHuman}, Address: "human:bo", Via: authz.ViaToken,
		Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(dest)}, {Relation: authz.Owner, Object: authz.ProjectObject(f.pid)}}, nil)}
	if _, err := f.ackAs(t, member, dest, nid, cid, stop); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member of the issue's project: %v", err)
	}
	// An owner of the destination only: the source stop is hidden to them,
	// named or as its placeholder, and answers like no gap.
	owner := &authz.Subject{Identity: api.Identity{Name: "cy", Kind: api.IdentityHuman}, Address: "human:cy", Via: authz.ViaToken,
		Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Owner, Object: authz.ProjectObject(dest)}}, nil)}
	for _, s := range []string{stop, "a stop in another project", "a stop in another project (text gone)"} {
		_, err := f.ackAs(t, owner, dest, nid, cid, s)
		if !api.IsCode(err, api.CodeNotFound) || strings.Contains(err.Error(), id) && s != stop {
			t.Fatalf("hidden stop %q: %v", s, err)
		}
	}
	_, hid := f.ackAs(t, owner, dest, nid, cid, stop)
	_, mis := f.ackAs(t, owner, dest, nid, cid, "nope-zz:"+id)
	if strings.ReplaceAll(hid.Error(), f.pid, "X") != strings.ReplaceAll(mis.Error(), "nope-zz", "X") {
		t.Fatalf("hidden %q != missing %q", hid, mis)
	}
	// Reads the source but does not moderate it: refused, not cleared.
	reader := &authz.Subject{Identity: api.Identity{Name: "di", Kind: api.IdentityHuman}, Address: "human:di", Via: authz.ViaToken,
		Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Owner, Object: authz.ProjectObject(dest)}, {Relation: authz.Member, Object: authz.ProjectObject(f.pid)}}, nil)}
	if _, err := f.ackAs(t, reader, dest, nid, cid, stop); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a reader, not moderator, of the stop's project: %v", err)
	}
	if why := f.stopState(t, cid, f.pid, id); why != gapTextGone {
		t.Fatalf("a refused ack changed the stop: %q", why)
	}
	if got := f.acks(t, cid); len(got) != 0 {
		t.Fatalf("a refused ack was logged: %q", got)
	}
}

func TestAckGapUnknownStopOrCommentIsNotFound(t *testing.T) {
	f := newRecoveryFixture(t)
	dest, id, nid, cid := textGoneAtSource(t, f, "ack-unknown", "ack-unknown-secret-0103")
	sys := f.s.d.System()
	if _, err := f.ackAs(t, sys, dest, nid, cid, dest+":"+nid); err != nil { // clear the destination's own gap
		t.Fatal(err)
	}
	for _, s := range []string{dest + ":" + nid, f.pid + ":rec-nope", "a>b", dest + ":" + id} {
		if _, err := f.ackAs(t, sys, dest, nid, cid, s); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("stop %q: %v", s, err)
		}
	}
	if _, err := f.ackAs(t, sys, dest, nid, "01BOGUSCOMMENT", f.pid+":"+id); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a bogus comment: %v", err)
	}
	if _, err := f.ackAs(t, sys, dest, nid, cid, "  "); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no stop: %v", err)
	}
	if why := f.stopState(t, cid, f.pid, id); why != gapTextGone {
		t.Fatalf("stop %q", why)
	}
}

// PR477 review 3, finding 2: a crash after a good purge and before the
// stop's clean mark leaves it pending; the next redact finds the text gone
// and records a false gap. The ack clears it.
func TestAckGapClearsTheFalseGapACrashAfterThePurgeLeaves(t *testing.T) {
	f := newRecoveryFixture(t)
	id, cid := f.commented(t, f.ps, f.pid, "crash-after-purge-secret-0104")
	if err := f.redactAt(t, f.pid, id, cid); err != nil {
		t.Fatal(err)
	}
	if f.eventsHold(t, f.pid, "crash-after-purge-secret-0104") {
		t.Fatal("the redaction left a copy: the test proves nothing")
	}
	// The state that crash leaves: scrubbed and purged, never marked clean.
	if _, err := f.s.d.Store.Registry().Exec(`UPDATE issue_redaction_stops SET why = ? WHERE comment_id = ? AND project = ? AND issue_id = ?`, stopPending, cid, f.pid, id); err != nil {
		t.Fatal(err)
	}
	err := f.redactAt(t, f.pid, id, cid)
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), f.pid+":"+id+" (text gone)") {
		t.Fatalf("after the crash: %v", err)
	}
	if _, err := f.ackAs(t, f.s.d.System(), f.pid, id, cid, f.pid+":"+id); err != nil {
		t.Fatal(err)
	}
	if err := f.redactAt(t, f.pid, id, cid); err != nil {
		t.Fatalf("redact after the ack: %v", err)
	}
	if got := f.acks(t, cid); len(got) != 1 || !strings.HasPrefix(got[0], f.pid+":"+id+" "+gapTextGone+" ") {
		t.Fatalf("audit rows %q", got)
	}
}
