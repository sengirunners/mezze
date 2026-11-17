package mail

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

func TestHookNoticeOnceWithoutTakingMail(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	f.work.add(f.harbor.ID, "local:bug", fakeItem{area: "solver", holder: addr})
	sender, reader := f.as("", "harbor"), f.as(addr, "")
	m := must(orchclient.Send(ctx, sender, orchapi.SendMailRequest{To: addr, Ref: "local:bug", Body: "secret-body-marker"}))
	n := must(orchclient.HookNotice(ctx, reader))
	if n.Unread != 1 || n.Line != "zm: 1 unread from human:ana; newest local:bug. Run: zm inbox" {
		t.Fatalf("first event: %+v", n)
	}
	// A new Service over the same registry models a daemon restart. The
	// hook seam and CLI/MCP endpoint must share the same delivery record.
	s := New(f.d)
	if n := must(s.HookNotice(ctx, f.d.Projects.Local(addr))); n.Line != "" || n.Unread != 1 {
		t.Fatalf("repeated after restart: %+v", n)
	}
	if n := must(orchclient.Notice(ctx, reader)); n.Line == "" || n.Unread != 1 {
		t.Fatalf("explicit unread snapshot was consumed: %+v", n)
	}
	in := must(orchclient.PeekInbox(ctx, reader, orchapi.InboxQuery{}))
	if len(in) != 1 || in[0].ID != m.ID || in[0].Body != "secret-body-marker" || in[0].AckedAt != nil {
		t.Fatalf("notice took mail: %+v", in)
	}
	// A different event with the same sender/ref/count still gets announced.
	must(orchclient.Ack(ctx, reader, m.ID))
	must(orchclient.Send(ctx, sender, orchapi.SendMailRequest{To: addr, Ref: "local:bug", Body: "different-secret"}))
	if next := must(orchclient.HookNotice(ctx, reader)); next.Line != n.Line || next.Unread != 1 {
		t.Fatalf("replacement event was suppressed: %+v", next)
	}
}

func TestHookNoticeGroupCountsAndSessionIsolation(t *testing.T) {
	f := newFixture(t)
	for _, addr := range []string{"mini/aaaaaaaa", "mini/bbbbbbbb"} {
		f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	}
	// Group-only notices reveal no sender, ref, body or mail id, even when
	// sent by another session about a completely unrelated CI result.
	f.work.add(f.harbor.ID, "local:other-ci", fakeItem{area: "solver"})
	must(orchclient.Send(ctx, f.as("mini/bbbbbbbb", "harbor"), orchapi.SendMailRequest{To: "machine:mini", Ref: "local:other-ci", Body: "secret-ci-body"}))
	announcement := must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "all", Kind: orchapi.MailKindAnnounce, Body: "secret-announcement"}))
	for _, addr := range []string{"mini/aaaaaaaa", "mini/bbbbbbbb"} {
		c := f.as(addr, "")
		n := must(orchclient.HookNotice(ctx, c))
		if n.Line != "zm: 2 unread (2 group). Run: zm inbox" || n.Unread != 2 {
			t.Fatalf("%s: %+v", addr, n)
		}
		if n := must(orchclient.HookNotice(ctx, c)); n.Line != "" {
			t.Fatalf("repeated group notice: %+v", n)
		}
		in := must(orchclient.PeekInbox(ctx, c, orchapi.InboxQuery{}))
		found := false
		for _, m := range in {
			found = found || m.ID == announcement.ID
		}
		if len(in) != 2 || !found {
			t.Fatalf("announcement was marked seen: %+v", in)
		}
	}
}

func TestHookNoticeScopesAndNewestMetadata(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	f.work.add(f.harbor.ID, "local:hidden-ref", fakeItem{area: "billing"})
	f.work.add(f.harbor.ID, "local:visible", fakeItem{area: "solver"})
	c := f.as("", "harbor")
	must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: addr, Area: "billing", Body: "secret-hidden-area"}))
	must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: addr, Area: "solver", Ref: "local:visible", Body: "old-body"}))
	f.clock = f.clock.Add(time.Second)
	newest := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: addr, Area: "solver", Ref: "local:hidden-ref", Body: "secret-newest-body"}))
	sub := &authz.Subject{Address: addr, Scope: mustGuestScope(t, f)}
	n := must(f.s.HookNotice(ctx, sub))
	if n.Unread != 2 || !strings.Contains(n.Line, "newest mail:"+short(newest.ID)) || strings.Contains(n.Line, "hidden") || strings.Contains(n.Line, "body") || strings.Contains(n.Line, "local:visible") {
		t.Fatalf("scope/newest metadata: %+v", n)
	}
	// Mail hidden from the first reader is still fresh for a wider scope.
	if n := must(f.s.HookNotice(ctx, f.d.Projects.Local(addr))); n.Unread != 3 || !strings.HasPrefix(n.Line, "zm: 1 unread") {
		t.Fatalf("hidden mail consumed a delivery record: %+v", n)
	}
}

func TestConcurrentHookNoticesAnnounceOnce(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: addr, Body: "secret"}))
	sub := f.d.Projects.Local(addr)
	var wg sync.WaitGroup
	results := make(chan orchapi.Notice, 8)
	for range 8 {
		wg.Go(func() {
			n, err := f.s.HookNotice(ctx, sub)
			if err != nil {
				t.Errorf("concurrent notice: %v", err)
			}
			results <- n
		})
	}
	wg.Wait()
	close(results)
	announced := 0
	for n := range results {
		if n.Line != "" {
			announced++
		}
	}
	if announced != 1 {
		t.Fatalf("announced %d times", announced)
	}
}

func TestHookNoticeMetadataBudget(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	ref := "local:" + "界\r\x1b[31m" + strings.Repeat("界", 40)
	f.work.add(f.harbor.ID, ref, fakeItem{area: "solver"})
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: addr, Ref: ref, Body: strings.Repeat("secret", 1000)}))
	n := must(orchclient.HookNotice(ctx, f.as(addr, "")))
	if len(n.Line) > orchapi.MaxNoticeLen || !utf8.ValidString(n.Line) || strings.ContainsAny(n.Line, "\r\n\x1b") || strings.Contains(n.Line, "secret") || !strings.HasSuffix(n.Line, "Run: zm inbox") {
		t.Fatalf("unsafe/oversize notice: %q", n.Line)
	}
}

type hookNoticeTeam struct {
	parts []orchapi.NoticePart
}

func (t *hookNoticeTeam) TeamNotice(string) []orchapi.NoticePart { return t.parts }
func (*hookNoticeTeam) TeamTarget(context.Context, string, string) (api.ProjectRef, string, bool, error) {
	return api.ProjectRef{}, "", false, nil
}

func TestHookNoticeTeamSummariesStayLocalAndCountOnly(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	tr := &hookNoticeTeam{parts: []orchapi.NoticePart{{Project: "remote", Slug: "remote", Unread: 2, Group: 2, From: []string{"secret-sender"}}}}
	orchcore.Wire(f.d).Team = tr
	sub := f.d.Projects.Local(addr)
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "zm: 2 unread (2 group, 2 team). Run: zm inbox" || n.Unread != 2 {
		t.Fatalf("team summary: %+v", n)
	}
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "" || n.Unread != 2 {
		t.Fatalf("repeated team summary: %+v", n)
	}
	tr.parts[0].Unread, tr.parts[0].Group = 3, 3
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "zm: 3 unread (3 group, 3 team). Run: zm inbox" {
		t.Fatalf("changed team summary: %+v", n)
	}
	tr.parts[0].Unread = 4
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "zm: 4 unread (3 group, 4 team) from someone. Run: zm inbox" {
		t.Fatalf("aggregate fallback guessed a direct sender: %+v", n)
	}
	tr.parts[0].Unread = 5
	sub.Scope = mustGuestScope(t, f)
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "" || n.Unread != 0 {
		t.Fatalf("narrow scope saw team aggregates: %+v", n)
	}
}

func TestHookNoticeTrackerFailureStreakOnce(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	f.work.failing = []orchapi.TrackerFailure{{Project: f.harbor.ID, Slug: "harbor", Prefix: "bd", Since: time.Now().Add(-time.Hour), Failures: 5}}
	sub := f.d.Projects.Local(addr)
	if n := must(f.s.HookNotice(ctx, sub)); !strings.Contains(n.Line, "mirror harbor/bd failing") {
		t.Fatalf("missing tracker notice: %+v", n)
	}
	f.work.failing[0].Failures++
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "" {
		t.Fatalf("same failure streak repeated: %+v", n)
	}
	f.work.failing[0].Since = time.Now().Add(-2 * time.Hour)
	if n := must(f.s.HookNotice(ctx, sub)); n.Line == "" {
		t.Fatal("new failure streak was suppressed")
	}
}

type readyHookNoticeTeam struct {
	hookNoticeTeam
	mail []orchcore.MailNotice
}

func (t *readyHookNoticeTeam) TeamMailNotices(context.Context, string) ([]orchcore.MailNotice, []orchapi.NoticePart) {
	return t.mail, t.parts
}

func TestHookNoticeReadyTeamMetadata(t *testing.T) {
	f := newFixture(t)
	const addr = "mini/aaaaaaaa"
	f.sess.add(addr, f.harbor.ID, "solver", false, orchapi.StateRunning)
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: addr, Body: "local-secret-body"}))
	tr := &readyHookNoticeTeam{mail: []orchcore.MailNotice{
		{Project: "remote", ID: "group", From: "secret-group-sender", Ref: "secret-group-ref", Group: true, Created: f.clock.Add(2 * time.Second)},
		{Project: "remote", ID: "first", From: "someone", Ref: "local:remote-bug", Created: f.clock.Add(time.Second)},
	}}
	orchcore.Wire(f.d).Team = tr
	sub := f.d.Projects.Local(addr)
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "zm: 3 unread (1 group) from someone; newest local:remote-bug. Run: zm inbox" || n.Unread != 3 {
		t.Fatalf("ready replica newest/redacted metadata: %+v", n)
	}
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "" || n.Unread != 3 {
		t.Fatalf("ready replica repeated an event: %+v", n)
	}
	tr.mail[1].ID = "second"
	if n := must(f.s.HookNotice(ctx, sub)); n.Line != "zm: 1 unread from someone; newest local:remote-bug. Run: zm inbox" {
		t.Fatalf("replacement remote event with unchanged counts lost: %+v", n)
	}
}
