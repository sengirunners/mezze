package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A lease that expires at the authority reaches its holder's inbox on a
// replica, as agent mail, whichever project the session is registered in. The
// owners' mailbox stays empty.
func TestExpiryMailReachesAReplicaSession(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) { c.File.LeaseTickSeconds, c.File.BoundLeaseTTLSeconds = 1, 2 })
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "zm", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)

	home := register(t, r, "harbor", "bea-home")
	away := register(t, r, "zm", "bea-away")
	heartbeat(t, r)
	for _, s := range []orchapi.Session{home, away} {
		it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "lapses"}))(t)
		mustf(orchclient.Claim(ctx, at(r.owner, "harbor", s.Address), it.ID, orchapi.ClaimRequest{}))(t)
		mustf(orchclient.SetState(ctx, r.owner, s.Address, orchapi.StateRequest{State: orchapi.StateIdle}))(t) // not renewed from here
		deadline := time.Now().Add(15 * time.Second)
		for {
			heartbeat(t, r)
			var got []orchapi.Mail
			for _, m := range mustf(orchclient.Inbox(ctx, at(r.owner, "", s.Address), orchapi.InboxQuery{}))(t) {
				if m.Ref == it.ID {
					got = append(got, m)
				}
			}
			if len(got) == 1 {
				if got[0].Needs != orchapi.NeedsAgent || got[0].Kind != orchapi.MailKindNotice || !strings.Contains(got[0].Body, "your lease on "+it.ID) {
					t.Fatalf("%s: mail %+v", s.Address, got[0])
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no mail about the expired lease on %s (got %+v)", s.Address, it.ID, got)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if ms := mustf(orchclient.Mail(ctx, a.owner, orchapi.MailQuery{Project: "harbor", To: "owners"}))(t); len(ms) != 0 {
		t.Fatalf("owners were mailed: %+v", ms)
	}
}

// A notice wakes nobody, is never copied to a group mailbox, takes no reply,
// and a caller cannot send one.
func TestExpiryNoticeIsQuiet(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) { c.File.LeaseTickSeconds, c.File.BoundLeaseTTLSeconds = 1, 2 })
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	ao := at(a.owner, "harbor", "")
	s := register(t, a, "harbor", "quiet-one")
	sc := at(a.owner, "", s.Address)
	if _, err := orchclient.Send(ctx, sc, orchapi.SendMailRequest{To: s.Address, Kind: orchapi.MailKindNotice, Body: "forged"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a caller sent a notice: %v", err)
	}
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "lapses"}))(t)
	mustf(orchclient.Claim(ctx, sc, it.ID, orchapi.ClaimRequest{}))(t)
	mustf(orchclient.SetState(ctx, a.owner, s.Address, orchapi.StateRequest{State: orchapi.StateIdle}))(t)
	var got orchapi.Mail
	deadline := time.Now().Add(15 * time.Second)
	for got.ID == "" {
		for _, m := range mustf(orchclient.Inbox(ctx, sc, orchapi.InboxQuery{}))(t) {
			if m.Ref == it.ID {
				got = m
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no notice")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := orchclient.Reply(ctx, sc, got.ID, orchapi.ReplyRequest{Body: "ok"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a reply to a notice: %v", err)
	}
	if _, err := orchclient.Send(ctx, sc, orchapi.SendMailRequest{To: "machine:mini", ReplyTo: got.ID, Body: "by reply_to"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("reply_to a notice: %v", err)
	}
	if ms := mustf(orchclient.Mail(ctx, ao, orchapi.MailQuery{Project: "harbor", To: "machine:mini"}))(t); len(ms) != 0 {
		t.Fatalf("group mailbox: %+v", ms)
	}
}

// A notice wakes no replica session, whichever project it is registered in.
func TestExpiryNoticeWakesNoReplicaSession(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) { c.File.LeaseTickSeconds, c.File.BoundLeaseTTLSeconds = 1, 2 })
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nC", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "zm", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	sessionModule(t, r).Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 100000}
	}
	for _, proj := range []string{"harbor", "zm"} { // zm: registered outside the lease's project
		ss, log := zzAgent(t, r, proj)
		heartbeat(t, r)
		it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "x"}))(t)
		mustf(orchclient.Claim(ctx, at(r.owner, "harbor", ss.Address), it.ID, orchapi.ClaimRequest{}))(t)
		mustf(orchclient.SetState(ctx, r.owner, ss.Address, orchapi.StateRequest{State: orchapi.StateIdle}))(t)
		deadline := time.Now().Add(20 * time.Second)
		for found := false; !found; {
			heartbeat(t, r)
			for _, m := range mustf(orchclient.Inbox(ctx, at(r.owner, "", ss.Address), orchapi.InboxQuery{}))(t) {
				found = found || m.Ref == it.ID
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no notice", proj)
			}
			time.Sleep(200 * time.Millisecond)
		}
		for i := 0; i < 4; i++ {
			heartbeat(t, r)
			time.Sleep(300 * time.Millisecond)
		}
		if n := zzLines(log); n != 0 {
			t.Fatalf("%s: the notice typed %d lines into an idle session", proj, n)
		}
	}
}

func zzAgent(t *testing.T, d *daemon, proj string) (orchapi.Session, string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "zmcwd")
	t.Cleanup(func() { os.RemoveAll(dir) })
	log := filepath.Join(dir, "typed")
	script := fmt.Sprintf(`while IFS= read -r l; do printf '%%s\n' "$l" >> %s; done`, log)
	ss := mustf(orchclient.Spawn(context.Background(), at(d.owner, proj, ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", script}, Cwd: dir}))(t)
	t.Cleanup(func() {
		_, _ = orchclient.Stop(context.Background(), d.owner, ss.Address, orchapi.StopRequest{Signal: "KILL"})
	})
	return ss, log
}

func zzLines(p string) int {
	b, _ := os.ReadFile(p)
	return strings.Count(string(b), "you have mail")
}

// ATTACK 2: the loop. An idle managed session holding a bound lease; on every nudge it does what the mail
// says: reads, acks, claims again, goes quiet.
