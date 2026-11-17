package team_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// SPEC 9 "Reports" on a replica: a replica session's mail to its person is
// filed as a report at the authority; the person's inbox and notice on the
// replica never show it, their reports do, and an ack marks it read.
func TestReplicaReports(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	s := register(t, r, "harbor", "bea-agent")
	heartbeat(t, r)

	// A fresh note is refused before it is queued. An explicit report still files.
	if _, err := orchclient.Send(ctx, at(r.owner, "harbor", s.Address), orchapi.SendMailRequest{To: "human:bea", Body: "should I ship it?"}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "not sent") || !strings.Contains(err.Error(), "zm ask new") {
		t.Fatalf("plain note to a person: %v", err)
	}
	// Mail to a bead a person created, with nobody holding it, is the same
	// refusal: the copy knows the creator, so it is not queued as sent.
	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "a person's bead"}))(t)
	seen := time.Now().Add(10 * time.Second)
	var created string
	for {
		got, err := orchclient.GetWork(ctx, r.owner, it.ID)
		if err == nil {
			created = got.CreatedBy
			break
		}
		if time.Now().After(seen) {
			t.Fatalf("replica never saw %s: %v", it.ID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(created, "human:") {
		t.Fatalf("the bead's creator is not a person: %q", created)
	}
	if _, err := orchclient.Send(ctx, at(r.owner, "harbor", s.Address), orchapi.SendMailRequest{To: it.ID, Body: "should I close this?"}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "not sent") || !strings.Contains(err.Error(), "zm ask new") {
		t.Fatalf("note to a person's bead: %v", err)
	}
	m := mustf(orchclient.Send(ctx, at(r.owner, "harbor", s.Address), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:bea", Body: "nightly digest"}))(t)
	if m.Kind != orchapi.MailKindReport || m.Needs != orchapi.NeedsHuman {
		t.Fatalf("send answer %+v", m)
	}
	person := at(r.owner, "", "")
	deadline := time.Now().Add(15 * time.Second)
	for {
		rs := mustf(orchclient.Inbox(ctx, person, orchapi.InboxQuery{Reports: true}))(t)
		if len(rs) == 1 && rs[0].ID == m.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no report on the replica: %+v", rs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, x := range mustf(orchclient.Inbox(ctx, person, orchapi.InboxQuery{}))(t) {
		if x.ID == m.ID {
			t.Fatalf("a report is in the person's inbox: %+v", x)
		}
	}
	if n := mustf(orchclient.Notice(ctx, person))(t); n.Unread != 0 {
		t.Fatalf("a report is counted: %+v", n)
	}
	if rs := mustf(orchclient.Inbox(ctx, at(r.owner, "", s.Address), orchapi.InboxQuery{Reports: true}))(t); len(rs) != 0 {
		t.Fatalf("an agent lists reports: %+v", rs)
	}
	mustf(orchclient.Ack(ctx, person, m.ID))(t)
	if rs := mustf(orchclient.Inbox(ctx, person, orchapi.InboxQuery{Reports: true}))(t); len(rs) != 0 {
		t.Fatalf("a read report is still unread: %+v", rs)
	}
}
