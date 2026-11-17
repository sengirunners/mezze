package team_test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A member checks the process behind its socket before it forwards a signed
// answer, a key sign-in or a key removal: from inside a registered session
// they are refused there and never reach the authority; from outside they go
// on marked origin_checked=1 (a caller's own mark is dropped). An
// answer-only token mint is refused from a session the same way. choose is
// never refused, and each one is logged with where it came from.
func TestMemberRefusesPersonOnlyCallsFromSessions(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	var seenMu sync.Mutex
	var seen []string // what reached the authority: "METHOD path?query"
	a.dm.Deps.AddRequestGate(func(w http.ResponseWriter, r *http.Request, _ func(http.ResponseWriter, *http.Request)) bool {
		if strings.HasSuffix(r.URL.Path, "/answer") || strings.HasSuffix(r.URL.Path, "/choose") || strings.HasPrefix(r.URL.Path, "/v0/login/github/") || r.URL.Path == "/v0/approvals/key" && r.Method == http.MethodDelete {
			seenMu.Lock()
			seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
			seenMu.Unlock()
		}
		return false
	})
	reached := func() []string { seenMu.Lock(); defer seenMu.Unlock(); return append([]string(nil), seen...) }
	logs := &syncBuf{}
	r := startDaemonLog(t, tn, "lane-mac", tailnettest.Identity{NodeID: "nLANE", NodeKey: "nodekey:lane-mac", Login: "pat@example.com", Host: "lane-mac"}, slog.New(slog.NewTextHandler(logs, nil)))
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "pat", Role: "member"}))(t)
	if j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t); j.Remote == nil {
		t.Fatalf("join: %+v", j)
	}
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "a decision", Area: "solver"}))(t)
	card := &orchapi.AskCard{Context: "Two people want the same day off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The first to ask"}, {Label: "The one with fewer days off"}},
		Recommend: "a", Why: "First come is the rule they already know.", Searched: "past rulings: nothing on this"}
	ask := mustf(orchclient.NewAsk(ctx, at(r.owner, "harbor", ""), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card}))(t)
	answerPath := "/v0/asks/" + ask.ID + "/answer?project=harbor"
	signed := orchapi.SignedKeyAnswer{Approver: "made-up-key", Payload: "e30=", Sig: "AAAA"}

	refused := func(t *testing.T, err error, what string) {
		t.Helper()
		if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "inside agent session") {
			t.Fatalf("%s from a session: %v", what, err)
		}
	}

	// Inside a session, by the real socket: the test's parent is registered
	// as a session, so the peer pid walk finds the test process inside it.
	sess := mustf(orchclient.Register(ctx, at(r.owner, "harbor", ""), orchapi.RegisterRequest{Agent: "shell", RuntimeID: "origin-parent", State: orchapi.StateRunning, PID: os.Getppid()}))(t)
	sock := at(r.sock, "harbor", "")
	refused(t, sock.Do(ctx, http.MethodPost, answerPath, signed, nil), "a signed answer")
	refused(t, sock.Do(ctx, http.MethodPost, answerPath+"&"+orchapi.OriginChecked+"=1", signed, nil), "a signed answer with a forged mark")
	refused(t, sock.Do(ctx, http.MethodPost, "/v0/login/github/start?project=harbor", map[string]string{"purpose": "key"}, nil), "a key sign-in")
	refused(t, sock.Do(ctx, http.MethodPost, "/v0/login/github/start?project=harbor", "not a login", nil), "an unreadable sign-in")
	refused(t, sock.Do(ctx, http.MethodDelete, "/v0/approvals/key?project=harbor", map[string]string{"key": "made-up"}, nil), "a key removal")
	answerMint := map[string]any{"limit": orchapi.AnswerLimit, "project": "harbor", "node": "phone", "ttl_seconds": 3600}
	refused(t, sock.Do(ctx, http.MethodPost, "/v0/tokens", answerMint, nil), "an answer-only token mint")
	// In process, a session-addressed caller is its session.
	refused(t, at(r.owner, "harbor", sess.Address).Do(ctx, http.MethodPost, answerPath, signed, nil), "an in-process session's answer")
	if got := reached(); len(got) != 0 {
		t.Fatalf("refused calls reached the authority: %q", got)
	}
	// choose from the session is not refused here; its origin is logged.
	// (a member is no owner: the authority, not this gate, answers it)
	if _, err := orchclient.Choose(ctx, sock, "harbor", ask.ID, orchapi.DecideRequest{Decision: "a"}); err != nil && strings.Contains(err.Error(), "agent session") {
		t.Fatalf("choose from a session refused by the gate: %v", err)
	}
	if got := reached(); len(got) != 1 || !strings.Contains(got[0], "/choose") || strings.Contains(got[0], orchapi.OriginChecked) {
		t.Fatalf("choose at the authority: %q", got)
	}
	if l := logs.String(); !strings.Contains(l, "ask choose on the socket") || !strings.Contains(l, "origin="+sess.Address) {
		t.Fatalf("choose origin not logged:\n%s", l)
	}

	// Outside any session: forwarded once, with exactly the member's mark.
	mustf(orchclient.Register(ctx, at(r.owner, "harbor", ""), orchapi.RegisterRequest{Agent: "shell", RuntimeID: "origin-parent", End: true}))(t)
	before := len(reached())
	err := sock.Do(ctx, http.MethodPost, answerPath+"&"+orchapi.OriginChecked+"=forged", signed, nil)
	if err != nil && strings.Contains(err.Error(), "agent session") {
		t.Fatalf("an answer from outside any session: %v", err)
	}
	got := reached()[before:]
	if len(got) != 1 || !strings.Contains(got[0], orchapi.OriginChecked+"=1") || strings.Contains(got[0], "forged") {
		t.Fatalf("the answer at the authority: %q", got)
	}
	// An answer-only mint from outside any session passes the gate (the
	// mint's own rules answer it: this machine has no such device).
	if err := sock.Do(ctx, http.MethodPost, "/v0/tokens", answerMint, nil); err == nil || strings.Contains(err.Error(), "agent session") {
		t.Fatalf("an answer-only mint from outside any session: %v", err)
	}
	// A sign-in that binds no key is not checked and carries no mark.
	before = len(reached())
	_ = sock.Do(ctx, http.MethodPost, "/v0/login/github/start?project=harbor", map[string]string{"purpose": "bind", orchapi.OriginChecked: "1"}, nil)
	for _, g := range reached()[before:] {
		if strings.Contains(g, orchapi.OriginChecked) {
			t.Fatalf("a plain sign-in was marked: %q", g)
		}
	}
}
