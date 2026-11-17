package team_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Redaction, part 2 (identifiers design, stage 2b): a comment written from
// a replica is kept there twice, as the copy's comment and in the outbox
// (the request and the authority's answer). When the authority redacts it,
// no byte of any file under the replica's home keeps the text.
func TestRedactReachesReplica(t *testing.T) { redactReachesReplica(t, false) }

// The same when the replica is away during the redaction and comes back
// below what the authority's log keeps: the snapshot brings the comment
// back redacted, and the copy scrubs what it kept.
func TestRedactReachesReplicaAfterResync(t *testing.T) { redactReachesReplica(t, true) }

func redactReachesReplica(t *testing.T, resync bool) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	if res, err := orchclient.AddTracker(ctx, ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "a leaked secret", Area: "solver"}))(t)
	wid := "issue:" + is.ID
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 5*time.Second, func() error {
		if _, err := orchclient.GetWork(ctx, rs, wid); err != nil {
			return err
		}
		return nil
	})
	const secret = "AKIA-REPLICA-SECRET-0004"
	const other = "a replica comment that stays"
	mustf(orchclient.Comment(ctx, rs, wid, orchapi.CommentRequest{Body: "key is " + secret}))(t)
	mustf(orchclient.Comment(ctx, rs, wid, orchapi.CommentRequest{Body: other}))(t)
	waitDrained(t, r)
	if fileWith(t, r.cfg.Home, secret) == "" {
		t.Fatal("the replica does not hold the secret: the test proves nothing")
	}

	ncs := mustf(orchclient.IssueComments(ctx, ao, is.ID))(t)
	cid := ""
	for _, c := range ncs {
		if strings.Contains(c.Text, secret) {
			cid = c.ID
		}
	}
	if cid == "" {
		t.Fatalf("no native comment with the secret: %+v", ncs)
	}
	if resync {
		tn.Cut("potluck-bea-mac", "potluck-mini", true)
		waitReachable(t, r, false)
	}
	req := httptest.NewRequest("POST", "/v0/issues/"+is.ID+"/comments/"+cid+"/redact", strings.NewReader("{}"))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderProject, "harbor")
	rec := httptest.NewRecorder()
	a.dm.SocketHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("redact: %d %s", rec.Code, rec.Body.String())
	}
	if resync {
		for i := 0; i < 5; i++ {
			mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprint("later ", i), Area: "solver"}))(t)
		}
		a.dm.Deps.Events.PruneTeam(ctx, j.Remote.Project.ID, 2, time.Now().Add(time.Hour))
		tn.Cut("potluck-bea-mac", "potluck-mini", false)
	}
	eventually(t, 10*time.Second, func() error {
		if p := fileWith(t, r.cfg.Home, secret); p != "" {
			return fmt.Errorf("the secret is still in %s", p)
		}
		return nil
	})
	got := mustf(orchclient.GetWork(ctx, rs, wid))(t)
	var bodies []string
	for _, c := range got.Comments {
		bodies = append(bodies, c.Body)
	}
	if j := strings.Join(bodies, "|"); !strings.Contains(j, "[redacted]") || !strings.Contains(j, other) {
		t.Errorf("replica comments after the redaction: %q", bodies)
	}
}

// fileWith is the first file under dir whose bytes contain s ("" = none).
func fileWith(t *testing.T, dir, s string) string {
	t.Helper()
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(s)) {
			found = p
		}
		return nil
	})
	return found
}
