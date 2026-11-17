package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Exercise the real local source and sender, rather than constructing a large
// request that bypasses the UI list's former silent first-page truncation.
func TestPresenceCompleteLocalInventory(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	svc := teamService(t, r)
	const count = 1500
	locals := make([]orchapi.Session, 0, count)
	for i := range count {
		ss := mustf(orchclient.Register(ctx, at(r.owner, "harbor/solver", ""), orchapi.RegisterRequest{
			Agent: "claude", RuntimeID: fmt.Sprintf("presence-scale-%04d", i), State: orchapi.StateRunning}))(t)
		locals = append(locals, ss)
	}
	page := mustf(orchclient.SessionPage(ctx, r.owner, orchapi.SessionQuery{Limit: 500}))(t)
	if len(page.Sessions) != 500 || page.Next == "" {
		t.Fatalf("public list lost its pagination: %d sessions, next %q", len(page.Sessions), page.Next)
	}
	heartbeat(t, r)
	for _, ss := range locals {
		got := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t)
		if got.Project != j.Remote.Project.ID || got.State != orchapi.StateRunning || got.Seen == nil {
			t.Fatalf("complete heartbeat omitted %s: %+v", ss.Address, got)
		}
	}
	// The oldest registration is outside the first UI page. Its lease and
	// notice must survive a subsequent complete heartbeat too.
	oldest := locals[0]
	rs := at(r.owner, "harbor", oldest.Address)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "held beyond first page", Area: "solver"}))(t)
	mustf(orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{TTLSeconds: 300}))(t)
	oldMail := mustf(orchclient.Send(ctx, ao, orchapi.SendMailRequest{To: oldest.Address, Body: "notice beyond the first page"}))(t)
	newest := locals[count-1]
	newMail := mustf(orchclient.Send(ctx, ao, orchapi.SendMailRequest{To: newest.Address, Body: "newest session control"}))(t)
	heartbeat(t, r)
	if h := mustf(orchclient.Holder(ctx, ao, it.ID))(t); h.Holder != oldest.Address {
		t.Fatalf("complete heartbeat lost the held lease: %+v", h)
	}
	for _, target := range []struct {
		address, mail string
	}{{oldest.Address, oldMail.ID}, {newest.Address, newMail.ID}} {
		if got := svc.CachedPresenceUnread(j.Remote.Project.ID, target.address); got != 1 {
			t.Fatalf("heartbeat notice for %s: got%d want1", target.address, got)
		}
		// Notice is machine-wide; a project-qualified reader intentionally
		// excludes team notices. A ready mail copy can lag the heartbeat.
		reader := at(r.owner, "", target.address)
		eventually(t, 3*time.Second, func() error {
			if in := mustf(orchclient.Inbox(ctx, reader, orchapi.InboxQuery{}))(t); !hasMail(in, target.mail) {
				return fmt.Errorf("session inbox has not received %s", target.mail)
			}
			return nil
		})
		if n := mustf(orchclient.Notice(ctx, reader))(t); n.Unread != 1 {
			t.Fatalf("complete heartbeat lost the session notice: %+v", n)
		}
	}
	if ss := mustf(orchclient.Session(ctx, r.owner, oldest.Address))(t); ss.State == orchapi.StateNeedsHuman {
		t.Fatalf("a held lease was incorrectly reported lost: %+v", ss)
	}

	// Move600 sessions into another project on the same authority. The
	// address-only markers and Elsewhere list must remain complete as well.
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "side"))(t)
	invite := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "side", As: "bea", Role: "member"}))(t)
	side := mustf(r.owner.Join(ctx, api.JoinRequest{Code: invite.Code}))(t)
	for _, ss := range locals[500:1100] {
		mustf(orchclient.MoveSession(ctx, r.owner, ss.Address, orchapi.MoveRequest{Project: "side"}))(t)
	}
	for range 2 {
		svc.BeatProject(ctx, "harbor")
		svc.BeatProject(ctx, "side")
	}
	for _, ss := range locals[500:1100] {
		got := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t)
		if got.Project != side.Remote.Project.ID || got.State != orchapi.StateRunning {
			t.Fatalf("cross-project heartbeat ended or bounced a session: %+v", got)
		}
	}
	outside := locals[1000]
	outsideMail := mustf(orchclient.Send(ctx, ao, orchapi.SendMailRequest{To: outside.Address, Body: "mail while registered in side"}))(t)
	svc.BeatProject(ctx, "harbor")
	if got := svc.CachedPresenceUnread(j.Remote.Project.ID, outside.Address); got != 1 {
		t.Fatalf("Elsewhere heartbeat notice: got%d want1", got)
	}
	outsideReader := at(r.owner, "", outside.Address)
	eventually(t, 3*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, outsideReader, orchapi.InboxQuery{}))(t); !hasMail(in, outsideMail.ID) {
			return fmt.Errorf("Elsewhere inbox has not received %s", outsideMail.ID)
		}
		return nil
	})
	if n := mustf(orchclient.Notice(ctx, outsideReader))(t); n.Unread != 1 || !strings.Contains(n.Line, "harbor") {
		t.Fatalf("Elsewhere beyond500 lost its project notice: %+v", n)
	}
	if h := mustf(orchclient.Holder(ctx, ao, it.ID))(t); h.Holder != oldest.Address {
		t.Fatalf("cross-project heartbeat lost another session's lease: %+v", h)
	}
	// Genuine omission still ends a session after its local registration ends.
	last := locals[count-1]
	mustf(orchclient.Register(ctx, r.owner, orchapi.RegisterRequest{Agent: "claude", RuntimeID: fmt.Sprintf("presence-scale-%04d", count-1), End: true}))(t)
	svc.BeatProject(ctx, "harbor")
	if got := mustf(orchclient.Session(ctx, a.owner, last.Address))(t); got.State != orchapi.StateEnded {
		t.Fatalf("genuinely ended session stayed live: %+v", got)
	}
}

type legacyPresenceList struct {
	orchcore.SessionService
	rows []orchapi.Session
}

func (s legacyPresenceList) List(_ context.Context, _ authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error) {
	if q.State == orchapi.StateEnded {
		return nil, nil
	}
	return s.rows, nil
}

func TestPresenceLegacyPageFailsClosed(t *testing.T) {
	for _, count := range []int{499, 500, 501} {
		source := legacyPresenceList{rows: make([]orchapi.Session, count)}
		rows, err := team.PresenceInventory(context.Background(), source)
		if count < 500 {
			if err != nil || len(rows) != count {
				t.Fatalf("complete small legacy inventory: %d, %v", len(rows), err)
			}
		} else if !api.IsCode(err, api.CodeUnavailable) || rows != nil {
			t.Fatalf("possibly truncated legacy inventory was sent: %d, %v", len(rows), err)
		}
	}
}

func TestPresenceReservesEndedPromptAcks(t *testing.T) {
	live := map[string]orchapi.Session{}
	unacked := map[string][]string{}
	for i := range 10501 {
		addr := fmt.Sprintf("bea-mac/%08x", i)
		unacked[addr] = []string{fmt.Sprintf("prompt-%d", i)}
		if i >= 501 {
			live[addr] = orchapi.Session{Address: addr, State: orchapi.StateRunning}
		}
	}
	sent, ended := team.PresenceAckBatch(unacked, live)
	if len(ended) != 500 || len(live)+len(ended) != 10500 {
		t.Fatalf("ended acks lost their reserved capacity: %d live, %d ended", len(live), len(ended))
	}
	ids := 0
	for _, pending := range sent {
		ids += len(pending)
	}
	if ids != 10000 || len(unacked) != 10501 {
		t.Fatalf("ack allocation exceeded its limit or mutated unsent records: sent %d, pending %d", ids, len(unacked))
	}
	// Simulate only the ids in this successful heartbeat being acknowledged;
	// everything left remains eligible for the next complete heartbeat.
	for addr := range sent {
		delete(unacked, addr)
	}
	next, nextEnded := team.PresenceAckBatch(unacked, live)
	if len(next) != 501 || len(nextEnded) != 1 {
		t.Fatalf("unsent acknowledgments were lost: %d ids, %d ended rows", len(next), len(nextEnded))
	}
}

func rawPresenceBody(t *testing.T, tn *tailnettest.Net, r *daemon, tokenFile string, body []byte) (int, string) {
	t.Helper()
	hr := mustf(http.NewRequest(http.MethodPost, "http://potluck-mini:7433/v0/team/presence", bytes.NewReader(body)))(t)
	hr.Header.Set(api.HeaderProtocol, api.Protocol)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(api.HeaderAuthorization, "Bearer "+readToken(t, tokenFile))
	resp := mustf(tailnetClientAs(t, tn, r.cfg.Machine).Do(hr))(t)
	defer resp.Body.Close()
	b := mustf(io.ReadAll(resp.Body))(t)
	return resp.StatusCode, string(b)
}

func TestPresenceRejectsIncompleteOrOversizeInventoryBeforeMutation(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	seed := register(t, r, "harbor/solver", "presence-resource-seed")
	heartbeat(t, r)
	const fake = "bea-mac/ffffbeef"
	entry := orchapi.PresenceSession{Address: fake, State: orchapi.StateRunning}
	base := func() presenceReq { return presenceReq{Project: j.Remote.Project.ID} }
	for _, kind := range []string{"live", "ended", "elsewhere", "ended_acks", "acks", "work", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			req := base()
			want := http.StatusRequestEntityTooLarge
			switch kind {
			case "live":
				for range 10001 {
					req.Sessions = append(req.Sessions, entry)
				}
			case "ended":
				for range 501 {
					req.Ended = append(req.Ended, orchapi.PresenceSession{Address: fake, OutcomeKind: orchapi.OutcomeOK})
				}
			case "elsewhere":
				req.Sessions = []orchapi.PresenceSession{entry}
				for range 10001 {
					req.Elsewhere = append(req.Elsewhere, fake)
				}
			case "ended_acks":
				for range 501 {
					req.Sessions = append(req.Sessions, orchapi.PresenceSession{Address: fake, State: orchapi.StateEnded, Delivered: []string{"prompt"}})
				}
			case "acks":
				ss := entry
				ss.Delivered = make([]string, 10001)
				req.Sessions = []orchapi.PresenceSession{ss}
			case "work":
				ss := entry
				ss.Work = make([]string, 100001)
				req.Sessions = []orchapi.PresenceSession{ss}
			case "foreign":
				want = http.StatusForbidden
				for range 600 {
					req.Sessions = append(req.Sessions, entry)
				}
				req.Sessions = append(req.Sessions, orchapi.PresenceSession{Address: "another-mac/abcd1234", State: orchapi.StateRunning})
			}
			if code, body := presenceAs(t, tn, r, j.Remote.TokenFile, req); code != want {
				t.Fatalf("resource or machine guard: %d, want %d: %s", code, want, body)
			}
			if got := mustf(orchclient.Session(ctx, a.owner, seed.Address))(t); got.State == orchapi.StateEnded {
				t.Fatal("rejected heartbeat ended an existing session")
			}
			if _, err := orchclient.Session(ctx, a.owner, fake); !api.IsCode(err, api.CodeNotFound) {
				t.Fatalf("rejected heartbeat stored a session: %v", err)
			}
			if _, ok := orchcore.Sessions(a.dm.Deps).(orchcore.PresenceElsewhere).Elsewhere(j.Remote.Project.ID, fake); ok {
				t.Fatal("rejected heartbeat changed Elsewhere reachability")
			}
		})
	}
	seedRequest := base()
	seedRequest.Sessions = []orchapi.PresenceSession{{Address: seed.Address, Area: seed.Area, State: orchapi.StateRunning}}
	b := mustf(json.Marshal(seedRequest.wire()))(t)
	tooBig := append(append([]byte{}, b...), bytes.Repeat([]byte(" "), 8<<20)...)
	if code, body := rawPresenceBody(t, tn, r, j.Remote.TokenFile, tooBig); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize presence body: %d %s", code, body)
	}
	// Unknown fields remain tolerated, and the dedicated presence budget can
	// carry a complete valid body larger than the unchanged global1MiB limit.
	var padded map[string]any
	if err := json.Unmarshal(b, &padded); err != nil {
		t.Fatal(err)
	}
	padded["future_padding"] = strings.Repeat("p", api.MaxBody)
	if code, body := rawPresenceBody(t, tn, r, j.Remote.TokenFile, mustf(json.Marshal(padded))(t)); code != http.StatusOK {
		t.Fatalf("valid presence body above1MiB: %d %s", code, body)
	}
}
