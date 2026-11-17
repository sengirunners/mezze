package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
)

// wantLimit checks err is a send-limit refusal of rule, with a retry time
// a reader of the message alone can act on.
func wantLimit(t *testing.T, err error, rule string) *api.Error {
	t.Helper()
	e := wantCode(t, err, api.CodeRateLimited)
	var d limitData
	if json.Unmarshal(e.Data, &d) != nil || d.Limit != rule || d.WindowS <= 0 {
		t.Fatalf("want a %s limit, got data %s (%v)", rule, e.Data, e)
	}
	if e.RetryAfter <= 0 || !strings.Contains(e.Message, "again in "+(time.Duration(e.RetryAfter)*time.Second).String()) {
		t.Fatalf("refusal names no retry time: %+v", e)
	}
	return e
}

// SPEC 9 "Send limits": two sessions answering each other stop on their
// own, both told why and when they may mail again; nothing refused is stored.
func TestReplyLoopStops(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	a, b := f.as("mini/aaaaaaaa", "harbor"), f.as("mini/bbbbbbbb", "harbor")
	start := f.clock
	last := must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "done?"}))
	turns, sent := [2]*client.Client{b, a}, 1 // b answers first
	var refused error
	for i := 0; i < 10*DefaultLimits.PerPair; i++ {
		f.clock = f.clock.Add(5 * time.Second)
		m, err := orchclient.Reply(ctx, turns[i%2], last.ID, orchapi.ReplyRequest{Body: fmt.Sprintf("got it (%d)", i)})
		if err != nil {
			refused = err
			break
		}
		last, sent = m, sent+1
	}
	if sent != DefaultLimits.PerPair {
		t.Fatalf("the loop stopped after %d mails, want %d (err %v)", sent, DefaultLimits.PerPair, refused)
	}
	// The side whose turn it was is refused; the other is refused too when
	// it mails again: the pair counts both ways.
	e1 := wantLimit(t, refused, "pair")
	_, err := orchclient.Send(ctx, b, orchapi.SendMailRequest{To: "mini/aaaaaaaa", Body: "still there?"})
	e2 := wantLimit(t, err, "pair")
	// A reply names nobody (the daemon chose the address); fresh mail names
	// what its sender wrote.
	if !strings.Contains(e1.Hint, "zm ack") || e1.RetryAfter != e2.RetryAfter ||
		!strings.Contains(e1.Message, "this recipient") || !strings.Contains(e2.Message, "mini/aaaaaaaa") {
		t.Fatalf("refusals %+v %+v", e1, e2)
	}
	// The wait is honest: the window's oldest mail leaves it then.
	if want := start.Add(10*time.Minute).Sub(f.clock) / time.Second; e1.RetryAfter != int(want) {
		t.Fatalf("retry_after %d, want %d", e1.RetryAfter, want)
	}
	db := must(f.d.Store.Project(authz.All(), authz.Read, f.harbor.ID))
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE sender IN ('mini/aaaaaaaa','mini/bbbbbbbb')`).Scan(&n); err != nil || n != DefaultLimits.PerPair {
		t.Fatalf("stored %d (%v), want %d: a refused mail was stored", n, err, DefaultLimits.PerPair)
	}
	if p, err := getRow(ctx, db, f.harbor.ID, last.ID); err != nil || p.AckedAt != nil {
		t.Fatalf("a refused reply took its parent: %+v %v", p, err)
	}
	// Another recipient is not held back by this pair.
	f.sess.add("mini/cccccccc", f.harbor.ID, "", true, orchapi.StateRunning)
	must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/cccccccc", Body: "can you take this?"}))
	f.clock = f.clock.Add(time.Duration(e1.RetryAfter) * time.Second)
	must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "back after the wait"}))
}

// The same mail again is refused (naming the first); a person's mail is
// not counted, nor is the daemon's or an ask's.
func TestSendLimitRepeatPeopleAndDaemon(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	a := f.as("mini/aaaaaaaa", "harbor")
	first := must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "PR is up"}))
	f.clock = f.clock.Add(time.Minute)
	_, err := orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "PR is up"})
	if e := wantLimit(t, err, "repeat"); !strings.Contains(e.Message, short(first.ID)) || e.RetryAfter != 9*60 {
		t.Fatalf("repeat refusal %+v", e)
	}
	// Not the same mail: another body, a ref, or an answer to other mail.
	must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "PR is up again"}))
	must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "PR is up", ReplyTo: first.ID}))
	// The same text filed in another area is other mail.
	must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "PR is up", Area: "solver"}))
	// A replayed write (a replica's write id, a client_request_id) is the
	// mail stored before: never counted again, never refused as a repeat.
	sess := f.d.Projects.Local("mini/aaaaaaaa")
	wid := "01WR" + strings.Repeat("C", 22)
	for range 2 {
		if _, err := f.s.Send(ctx, sess, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "queued once", ID: wid}); err != nil {
			t.Fatalf("a replayed write was refused: %v", err)
		}
		must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "keyed once", ClientRequestID: "k1"}))
	}
	db := must(f.d.Store.Project(authz.All(), authz.Read, f.harbor.ID))
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail_sends WHERE sender = 'mini/aaaaaaaa'`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("ledger rows %d (%v), want 6: a replay was counted", n, err)
	}
	// A person is not counted: not even the same mail again.
	h := f.as("", "harbor")
	for range DefaultLimits.PerPair + 5 {
		must(orchclient.Send(ctx, h, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "ship it"}))
	}
	// An agent-kind identity is counted though its mailbox reads human:<name>.
	must(f.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "ci-bot", Kind: api.IdentityAgent}))
	bot := must(f.d.Projects.Mint(ctx, f.d.Projects.Local(""), api.MintTokenRequest{As: "ci-bot", Project: "harbor", Role: "member"}))
	bc := client.WithHandler(f.h)
	bc.Token, bc.Project, bc.ProjectFrom = bot.Secret, "harbor", "flag"
	must(orchclient.Send(ctx, bc, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "build is red"}))
	_, err = orchclient.Send(ctx, bc, orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "build is red"})
	wantLimit(t, err, "repeat")
	// The daemon's own mail and an ask's mail are never counted, not even
	// as repeats.
	askA, askB := "01ASK"+strings.Repeat("A", 21), "01ASK"+strings.Repeat("B", 21)
	f.s.SetAsks(fakeAsks{askA: true, askB: true})
	for range DefaultLimits.PerPair + 5 {
		if _, err := f.s.Send(ctx, f.d.System(), f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: "lease ends soon"}); err != nil {
			t.Fatalf("daemon mail refused: %v", err)
		}
	}
	for _, id := range []string{askA, askB} {
		if _, err := f.s.Send(ctx, f.d.Projects.Local("mini/aaaaaaaa"), f.harbor.Ref(), "",
			orchapi.SendMailRequest{To: "owners", Body: "merge #12?", Needs: orchapi.NeedsHuman, ID: id}); err != nil {
			t.Fatalf("an ask's mail refused: %v", err)
		}
	}
}

// The limits are project.toml settings with defaults; the per-sender limit
// counts every recipient.
func TestSendLimitSettings(t *testing.T) {
	f := newFixture(t)
	if got := f.s.limits(f.harbor.ID); got != DefaultLimits {
		t.Fatalf("defaults %+v", got)
	}
	if _, err := (limitsTable{}).Parse("project.toml", map[string]any{"per_pair": int64(-1)}); err == nil {
		t.Fatal("a negative limit parsed")
	}
	if _, err := (limitsTable{}).Parse("project.toml", map[string]any{"per_hour": int64(5)}); err == nil {
		t.Fatal("an unknown key parsed")
	}
	if _, err := f.d.Projects.SetSection(ctx, f.harbor.ID, limitsSection, func(any) (any, error) {
		return Limits{PerSender: 3, WindowSeconds: 60}, nil
	}); err != nil {
		t.Fatal(err)
	}
	want := DefaultLimits
	want.PerSender, want.WindowSeconds = 3, 60
	if got := f.s.limits(f.harbor.ID); got != want {
		t.Fatalf("limits %+v", got)
	}
	if enc := (limitsTable{}).Encode(Limits{PerSender: 3, WindowSeconds: 60}); enc != "\n[mail_limits]\nper_sender = 3\nwindow_seconds = 60\n" {
		t.Fatalf("encode %q", enc)
	}
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	a := f.as("mini/aaaaaaaa", "harbor")
	for i := range 3 {
		to := fmt.Sprintf("mini/%08d", i)
		f.sess.add(to, f.harbor.ID, "", true, orchapi.StateRunning)
		must(orchclient.Send(ctx, a, orchapi.SendMailRequest{To: to, Body: "take a lane"}))
	}
	f.sess.add("mini/dddddddd", f.harbor.ID, "", true, orchapi.StateRunning)
	_, err := orchclient.Send(ctx, a, orchapi.SendMailRequest{To: "mini/dddddddd", Body: "take a lane"})
	if e := wantLimit(t, err, "sender"); e.RetryAfter != 60 {
		t.Fatalf("sender refusal %+v", e)
	}
	// The ledger keeps nothing past the longest window.
	f.clock = f.clock.Add(11 * time.Minute)
	if err := f.s.pruneSends(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	db := must(f.d.Store.Project(authz.All(), authz.Read, f.harbor.ID))
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mail_sends`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ledger rows after prune: %d %v", n, err)
	}
}

// countingJudge counts the report judge's calls and answers as the test fake.
type countingJudge struct{ n *int }

func (j countingJudge) Call(ctx context.Context, prompt string, schema json.RawMessage) (json.RawMessage, error) {
	*j.n++
	return judge.Fake{}.Call(ctx, prompt, schema)
}

// A report the limits refuse anyway costs no judge call.
func TestSendLimitBeforeReportJudge(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	var calls int
	f.s.SetJudge(countingJudge{&calls})
	a := f.as("mini/aaaaaaaa", "harbor")
	report := orchapi.SendMailRequest{To: "human:ana", Body: "nightly build is green", Kind: orchapi.MailKindReport}
	must(orchclient.Send(ctx, a, report))
	_, err := orchclient.Send(ctx, a, report)
	wantLimit(t, err, "repeat")
	if calls != 1 {
		t.Fatalf("the judge was asked %d times, want 1", calls)
	}
}
