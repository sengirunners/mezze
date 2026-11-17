package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// The backed-up cursor survives, but a forwarded create has
// populated a newer row without advancing that cursor. Merely rejecting
// nonexistent cursors cannot detect this history rollback; the home's
// restore generation does. A write queued on the lost item must fail
// saying why, and one queued on a kept item must land exactly once.
func TestReplicaRetainedCursorRestoreDropsForwardedCreate(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	authorityMachine, authorityHuman := a.cfg.Machine, a.cfg.Human
	ao := at(a.owner, "harbor", "")
	kept := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "kept by the backup", Area: "solver"}))(t)
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	gate := &retainedCursorTransport{
		altTransport: altTransport{r.dm.Deps.Transport(core.ViaTailnet)},
		held:         make(chan string, 1), resumed: make(chan string, 1),
		release: make(chan struct{}), finish: make(chan struct{}),
	}
	r.dm.Deps.AddTransport(gate)
	defer gate.unblock()
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: "alt+" + inv.Code}))(t)
	svc := teamService(t, r)
	rc := at(r.owner, "harbor", "")
	cursor := awaitRetainedCursor(t, gate.held)
	if cursor == "" || svc.CopyCursor("harbor") != cursor || !svc.CopyHolds(ctx, kept.ID) {
		t.Fatal("sync barrier did not hold a ready copy at its nonempty snapshot cursor")
	}

	root := t.TempDir()
	const home = "retained-cursor-home"
	if err := a.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	mustf(orchclient.SetBackup(ctx, a.owner, orchapi.OffsiteRequest{Target: "file://" + root, Home: home, EverySeconds: 3600}))(t)
	backup := mustf(orchclient.RunBackup(ctx, a.owner))(t)
	if !backup.Ran || backup.Set == "" {
		t.Fatalf("home backup did not complete: %+v", backup)
	}
	lost := mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "forwarded after the backup", Area: "solver"}))(t)
	if !svc.CopyHolds(ctx, lost.ID) || svc.CopyCursor("harbor") != cursor {
		t.Fatal("forwarded create did not populate a row newer than the held sync cursor")
	}
	// A write the old home takes after the set (acked, still overlaid: the
	// copy's sync is held) is lost with the restore: it must stop showing
	// and say so, not linger as a ghost overlay.
	ackSess := register(t, r, "harbor/solver", "bea-acked-writer")
	takenLost := mustf(orchclient.Comment(ctx, at(r.owner, "harbor", ackSess.Address), kept.ID, orchapi.CommentRequest{Body: "taken, then lost with the restore"}))(t)
	eventually(t, 10*time.Second, func() error {
		for _, e := range mustf(r.owner.Outbox(ctx, "harbor", true))(t) {
			if e.ID == takenLost.ID && e.State == api.OutboxAcked {
				return nil
			}
		}
		return fmt.Errorf("the old home has not acked the comment")
	})

	// A pending write belongs to the member's durable outbox, not to the
	// authority backup. Recovery must retain it and let it reach its target.
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	defer tn.Cut("potluck-bea-mac", "potluck-mini", false)
	queued := mustf(orchclient.Comment(ctx, rc, kept.ID, orchapi.CommentRequest{Body: "queued across a full restore"}))(t)
	sess := register(t, r, "harbor/solver", "bea-ghost-writer")
	doomed := mustf(orchclient.Comment(ctx, at(r.owner, "harbor", sess.Address), lost.ID, orchapi.CommentRequest{Body: "queued on an item the restore loses"}))(t)
	entries := mustf(r.owner.Outbox(ctx, "harbor", false))(t)
	pending := map[string]bool{}
	for _, e := range entries {
		pending[e.ID] = e.State == api.OutboxPending
	}
	if len(entries) != 2 || !pending[queued.ID] || !pending[doomed.ID] {
		t.Fatalf("pending member outcomes were not durably queued: %+v", entries)
	}
	a.halt()
	a.halt = func() {} // cleanup must not wait twice if restoration fails
	authorityHome := a.cfg.Home
	key, err := bkcrypt.Open(authorityHome) // the owner kept a copy of the home's backup key
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(authorityHome); err != nil {
		t.Fatal(err)
	}
	if _, err := offsite.Restore(ctx, offsite.Dir{Root: root}, home, backup.Set, authorityHome, false, key); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(authorityHome)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Machine != authorityMachine {
		t.Fatalf("backup changed the authority's persisted identity: got%q want%q", cfg.File.Machine, authorityMachine)
	}
	// Match startDaemonWith's synthetic runtime identity. LoadFrom honors
	// inherited ZM_MACHINE/ZM_HUMAN, which must not rename the restored fake
	// node or change the local human used by this two-daemon fixture.
	cfg.Machine, cfg.Human = authorityMachine, authorityHuman
	a.cfg = cfg
	// Only test transport identity is supplied separately: no real tailnet,
	// token widening, production secret or re-invite participates in recovery.
	a.boot(tn, tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if tn.Node(api.NodePrefixNew+authorityMachine) == nil {
		t.Fatalf("restored fake tailnet route missing: machine%q file_machine%q hostname%q", cfg.Machine, cfg.File.Machine, cfg.File.Tailnet.Hostname)
	}
	retained := false
	for _, event := range projectEvents(t, a) {
		retained = retained || event.ID == cursor
		if event.Subject == lost.ID {
			t.Fatal("restored event log still contains the post-backup creation")
		}
	}
	if !retained {
		t.Fatal("backup lost the cursor; this would exercise the earlier missing-cursor fix")
	}
	if _, err := orchclient.GetWork(ctx, at(a.owner, "harbor", ""), lost.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("restored authority did not lose the post-backup item: %v", err)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)
	comments := mustf(orchclient.Comments(ctx, at(a.owner, "harbor", ""), kept.ID))(t)
	if len(comments) != 1 || comments[0].ID != queued.ID || comments[0].Body != queued.Body {
		t.Fatalf("restoration lost or duplicated the pending member outcome: %+v", comments)
	}
	var failed *api.OutboxEntry
	for _, e := range mustf(r.owner.Outbox(ctx, "harbor", true))(t) {
		if e.ID == doomed.ID {
			failed = &e
		}
	}
	if failed == nil || failed.State != api.OutboxFailed || !strings.Contains(failed.Error, "restored from a backup") {
		t.Fatalf("a queued write on the lost item did not fail saying why: %+v", failed)
	}
	if got := findSession(mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t), sess.Address); got == nil || got.State != orchapi.StateNeedsHuman {
		t.Fatalf("the session whose queued write cannot land was not told: %+v", got)
	}
	var lostAck *api.OutboxEntry
	for _, e := range mustf(r.owner.Outbox(ctx, "harbor", true))(t) {
		if e.ID == takenLost.ID {
			lostAck = &e
		}
	}
	// The ack put the comment into the copy, so its outbox entry may already
	// be collected (then nothing records it: a known limit). Either way the
	// member no longer shows it, and a kept entry says it was lost.
	if lostAck != nil && (lostAck.State != api.OutboxFailed || !strings.Contains(lostAck.Error, "restored from a backup")) {
		t.Fatalf("an acked write the restore lost did not fail saying so: %+v", lostAck)
	}
	for _, c := range mustf(orchclient.Comments(ctx, rc, kept.ID))(t) {
		if c.ID == takenLost.ID {
			t.Fatal("the member still shows a comment the restored home lost")
		}
	}
	_ = ackSess
	gate.releaseOnce.Do(func() { close(gate.release) })
	after := awaitRetainedCursor(t, gate.resumed)
	// The next sync request begins only after the resumed page (or recovery
	// snapshot) has applied. Hold it too so no later edit repairs the ghost.
	if !svc.CopyHolds(ctx, kept.ID) {
		t.Fatal("recovery discarded the item retained by the backup")
	}
	if svc.CopyHolds(ctx, lost.ID) {
		t.Fatalf("retained-cursor restore left a ghost: backed-up cursor %s survived, next sync cursor %s, item %s", cursor, after, lost.ID)
	}
}

func awaitRetainedCursor(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case cursor := <-ch:
		return cursor
	case <-time.After(15 * time.Second):
		t.Fatal("sync did not reach the retained-cursor barrier")
		return ""
	}
}

// retainedCursorTransport gates sync before I/O, preserving the real join,
// snapshot, create and outbox routes. The first resumed sync does not long
// poll; the next request is a deterministic after-application barrier.
type retainedCursorTransport struct {
	altTransport
	held, resumed           chan string
	release, finish         chan struct{}
	releaseOnce, finishOnce sync.Once
	stage                   atomic.Uint32
}

func (g *retainedCursorTransport) unblock() {
	g.releaseOnce.Do(func() { close(g.release) })
	g.finishOnce.Do(func() { close(g.finish) })
}

func (g *retainedCursorTransport) Client(target core.JoinTarget, opts core.ClientOptions) *http.Client {
	hc := *g.TeamTransport.Client(target, opts)
	hc.Transport = retainedCursorRoundTripper{RoundTripper: hc.Transport, gate: g}
	return &hc
}

type retainedCursorRoundTripper struct {
	http.RoundTripper
	gate *retainedCursorTransport
}

func (r retainedCursorRoundTripper) CloseIdleConnections() {
	if closer, ok := r.RoundTripper.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (r retainedCursorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.URL.Path != "/v0/sync" {
		return r.RoundTripper.RoundTrip(req)
	}
	g := r.gate
	if g.stage.CompareAndSwap(0, 1) {
		g.held <- req.URL.Query().Get("since")
	}
	if g.stage.Load() == 1 {
		select {
		case <-g.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	} else if g.stage.Load() == 2 {
		select {
		case g.resumed <- req.URL.Query().Get("since"):
		default:
		}
		select {
		case <-g.finish:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	call := req.Clone(req.Context())
	u := *req.URL
	q := u.Query()
	q.Set("wait_ms", "0")
	u.RawQuery = q.Encode()
	call.URL = &u
	resp, err := r.RoundTripper.RoundTrip(call)
	if err == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusGone) {
		g.stage.CompareAndSwap(1, 2)
	}
	return resp, err
}
