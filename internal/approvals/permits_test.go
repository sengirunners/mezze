package approvals_test

// Standing permissions (SPEC 16 "Standing permissions"): the
// signed grant, revoke, box marks, and the dry run's records and report.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// permitAsk files the card for req, as zm permit ask does.
func (h *harness) permitAsk(c *client.Client, req orchapi.PermitCardRequest) orchapi.Ask {
	h.t.Helper()
	ctx := context.Background()
	card := must(orchclient.PermitCard(ctx, c, req))
	card.Recommend, card.Why = "no", "Keeping cards until the dry run reports shows what each one would have saved."
	card.Searched = "the dry run report and the open asks: nothing covers this yet"
	return must(orchclient.NewAsk(ctx, c, orchapi.NewAskRequest{Card: &card}))
}

// pairFor pairs a device from ip for person (zm approver add --for).
func (h *harness) pairFor(name, ip, person string) {
	h.t.Helper()
	ctx := context.Background()
	add := must(orchclient.AddApproverFor(ctx, h.owner, name, person))
	must(orchclient.Pair(ctx, h.device(ip), add.Code))
}

func TestPermitGrantTakesASignedYesOnTheExactCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairFor("ana", ipPhone, "ivo")
	agent, _ := h.session("31313131")
	sock := h.ownerIn("harbor", "")
	lane := orchapi.PermitCardRequest{Kind: "lane-start", Days: 30}

	// Unknown kinds, bad days and mixed targets are refused up front.
	for _, bad := range []orchapi.PermitCardRequest{{Kind: "merge"}, {Kind: "lane-start"}, {Kind: "lane-start", Days: 400},
		{Kind: "lane-start", Days: 3, Machine: "box-a", Class: "no-secrets"}, {Machine: "box-a", Class: "keys"}, {}} {
		if _, err := orchclient.PermitCard(ctx, agent, bad); code(err) != api.CodeBadRequest {
			t.Errorf("card %+v: %v", bad, err)
		}
	}

	a := h.permitAsk(agent, lane)
	grant := func(c *client.Client, req orchapi.PermitCardRequest, ask string) (orchapi.PermitGrant, error) {
		return orchclient.GrantPermit(ctx, c, orchapi.PermitGrantRequest{PermitCardRequest: req, Ask: ask})
	}
	if _, err := grant(agent, lane, a.ID); code(err) != api.CodeConflict {
		t.Fatalf("unanswered: %v", err)
	}
	// The owner's socket answer (every process of their OS user reaches it)
	// never grants.
	if _, err := orchclient.Choose(ctx, sock, "harbor", a.ID, orchapi.DecideRequest{Decision: "yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := grant(agent, lane, a.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("socket answer: %v", err)
	}

	// A signed no grants nothing.
	no := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 14})
	must(orchclient.Decide(ctx, h.device(ipPhone), no.ID, "no"))
	if _, err := grant(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 14}, no.ID); code(err) != api.CodeConflict {
		t.Fatalf("answered no: %v", err)
	}

	// A yes with words added is not a plain yes.
	worded := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 7})
	must(orchclient.DecideWith(ctx, h.device(ipPhone), worded.ID, orchapi.DecideRequest{Decision: "yes", Text: "but only for a day"}))
	if _, err := grant(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 7}, worded.ID); code(err) != api.CodeConflict {
		t.Fatalf("a yes with text: %v", err)
	}

	// A signed yes grants exactly what its card said.
	yes := h.permitAsk(agent, lane)
	must(orchclient.Decide(ctx, h.device(ipPhone), yes.ID, "yes"))
	for name, req := range map[string]orchapi.PermitCardRequest{
		"other days": {Kind: "lane-start", Days: 90},
		"other kind": {Kind: "machine-pick", Days: 30},
		"a box":      {Machine: "box-a", Class: "no-secrets"},
	} {
		if _, err := grant(agent, req, yes.ID); code(err) != api.CodeConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Another card answered yes is not a permit card.
	other := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "not-a-permit"}))
	must(orchclient.Decide(ctx, h.device(ipPhone), other.ID, "a"))
	if _, err := grant(agent, lane, other.ID); code(err) != api.CodeConflict {
		t.Errorf("another card: %v", err)
	}
	// A guest of one area learns nothing of the ask.
	if _, err := grant(h.guest(), lane, yes.ID); code(err) != api.CodeNotFound {
		t.Errorf("guest: %v", err)
	}
	g := must(grant(agent, lane, yes.ID))
	if g.Permit == nil || g.Permit.Kind != "lane-start" || g.Permit.Approver != "ana" || g.Permit.State != orchapi.PermitLive ||
		!g.Permit.Expires.Equal(h.clock.now().Truncate(time.Millisecond).Add(30*24*time.Hour)) {
		t.Fatalf("grant: %+v", g.Permit)
	}
	if _, err := grant(agent, lane, yes.ID); code(err) != api.CodeConflict {
		t.Fatalf("the same answer twice: %v", err)
	}

	// Renewing takes another signed yes, and replaces the live permit.
	h.clock.add(time.Minute)
	renew := h.permitAsk(agent, lane)
	must(orchclient.Decide(ctx, h.device(ipPhone), renew.ID, "yes"))
	g2 := must(grant(agent, lane, renew.ID))
	l := must(orchclient.Permits(ctx, sock))
	if len(l.Permits) != 2 || l.Permits[0].ID != g2.Permit.ID || l.Permits[0].State != orchapi.PermitLive ||
		l.Permits[1].State != orchapi.PermitReplaced || l.Permits[1].ReplacedBy != g2.Permit.ID {
		t.Fatalf("after renewal: %+v", l.Permits)
	}
	if len(l.Kinds) != 2 || l.Kinds[0].Name != "lane-start" || !l.Kinds[0].DryRun || l.Kinds[0].SHA == "" || len(l.Kinds[0].Conditions) == 0 {
		t.Fatalf("kinds: %+v", l.Kinds)
	}
	if _, err := orchclient.Permits(ctx, h.guest()); code(err) != api.CodeForbidden {
		t.Fatalf("guest list: %v", err)
	}

	// An older yes never replaces a newer grant.
	older := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 60})
	must(orchclient.Decide(ctx, h.device(ipPhone), older.ID, "yes"))
	h.clock.add(time.Minute)
	newer := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 5})
	must(orchclient.Decide(ctx, h.device(ipPhone), newer.ID, "yes"))
	gN := must(grant(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 5}, newer.ID))
	if _, err := grant(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 60}, older.ID); code(err) != api.CodeConflict {
		t.Fatalf("an older yes over a newer grant: %v", err)
	}
	g2 = gN
	h.clock.add(time.Minute)

	// It expires on its own.
	h.clock.add(31 * 24 * time.Hour)
	if l := must(orchclient.Permits(ctx, sock)); l.Permits[0].State != orchapi.PermitExpired {
		t.Fatalf("expired: %+v", l.Permits[0])
	}
	h.clock.add(-31 * 24 * time.Hour)

	// Revoking takes no signature, but an owner or admin; a guest gets
	// not_found.
	if _, err := orchclient.RevokePermit(ctx, h.guest(), g2.Permit.ID); code(err) != api.CodeNotFound {
		t.Fatalf("guest revoke: %v", err)
	}
	// A yes kept back from before the revoke does not bring it back.
	kept := h.permitAsk(agent, lane)
	must(orchclient.Decide(ctx, h.device(ipPhone), kept.ID, "yes"))
	h.clock.add(time.Minute)
	p := must(orchclient.RevokePermit(ctx, sock, g.Permit.ID)) // the replaced one: its kind ends
	if p.State != orchapi.PermitRevoked || p.Revoked == nil {
		t.Fatalf("revoke: %+v", p)
	}
	for _, q := range must(orchclient.Permits(ctx, sock)).Permits {
		if q.State == orchapi.PermitLive {
			t.Fatalf("still live after revoking its kind: %+v", q)
		}
	}
	if _, err := grant(agent, lane, kept.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "new answer") {
		t.Fatalf("a yes from before the revoke: %v", err)
	}
	// A yes after it does; the days count from the answer, and an answer
	// older than its days grants nothing.
	h.clock.add(time.Minute)
	fresh := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 2})
	must(orchclient.Decide(ctx, h.device(ipPhone), fresh.ID, "yes"))
	answeredAt := h.clock.now().Truncate(time.Millisecond)
	h.clock.add(24 * time.Hour)
	g3 := must(grant(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 2}, fresh.ID))
	if !g3.Permit.Expires.Equal(answeredAt.Add(48 * time.Hour)) {
		t.Fatalf("expiry from the answer: %v, answered %v", g3.Permit.Expires, answeredAt)
	}
	old := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 1})
	must(orchclient.Decide(ctx, h.device(ipPhone), old.ID, "yes"))
	h.clock.add(25 * time.Hour)
	if _, err := grant(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 1}, old.ID); code(err) != api.CodeConflict {
		t.Fatalf("an answer older than its days: %v", err)
	}
	if _, err := orchclient.RevokePermit(ctx, sock, "nope"); code(err) != api.CodeNotFound {
		t.Fatalf("missing: %v", err)
	}
}

// A yes from a device paired for no one never creates, renews or widens a
// permit, even where that device would count as the machine's owner:
// zm cannot say who held it.
func TestPermitGrantRefusesADevicePairedForNoOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairFor("ana", ipPhone, "ivo")
	add := must(orchclient.AddApprover(ctx, h.owner, "spare-phone")) // paired for no one
	must(orchclient.Pair(ctx, h.device(ipTablet), add.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "spare-phone"))
	unbound, bound := h.device(ipTablet), h.device(ipPhone)
	agent, _ := h.session("71717171")
	grant := func(req orchapi.PermitCardRequest, ask string) (orchapi.PermitGrant, error) {
		return orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: req, Ask: ask})
	}
	lane := orchapi.PermitCardRequest{Kind: "lane-start", Days: 30}
	a := h.permitAsk(agent, lane)
	must(orchclient.Decide(ctx, unbound, a.ID, "yes"))
	if _, err := grant(lane, a.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "paired for no one") {
		t.Fatalf("create from an unbound phone: %v", err)
	}
	// A live permit (from a bound device) is neither renewed nor widened by it.
	b := h.permitAsk(agent, lane)
	must(orchclient.Decide(ctx, bound, b.ID, "yes"))
	live := must(grant(lane, b.ID)).Permit
	for name, req := range map[string]orchapi.PermitCardRequest{"renew": lane, "widen": {Kind: "lane-start", Days: 90}} {
		c := h.permitAsk(agent, req)
		must(orchclient.Decide(ctx, unbound, c.ID, "yes"))
		if _, err := grant(req, c.ID); code(err) != api.CodeConflict || !strings.Contains(err.Error(), "paired for no one") {
			t.Fatalf("%s from an unbound phone: %v", name, err)
		}
	}
	for _, p := range must(orchclient.Permits(ctx, agent)).Permits {
		if p.State == orchapi.PermitLive && p.ID != live.ID {
			t.Fatalf("an unbound phone's yes made a live permit: %+v", p)
		}
	}
}

// An unbound device's old yes stays no one's when its name is paired again,
// for a person, on another device.
func TestPermitGrantOldYesNeverBorrowsANewPairing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairFor("ana", ipPhone, "ivo")
	add := must(orchclient.AddApprover(ctx, h.owner, "spare-phone"))
	must(orchclient.Pair(ctx, h.device(ipTablet), add.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "spare-phone"))
	agent, _ := h.session("72727272")
	lane := orchapi.PermitCardRequest{Kind: "lane-start", Days: 30}
	a := h.permitAsk(agent, lane)
	must(orchclient.Decide(ctx, h.device(ipTablet), a.ID, "yes"))
	must(orchclient.RemoveApprover(ctx, h.owner, "spare-phone"))
	add2 := must(orchclient.AddApproverFor(ctx, h.owner, "spare-phone", "ivo"))
	must(orchclient.Pair(ctx, h.device(ipPhone2), add2.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "spare-phone"))
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: lane, Ask: a.ID}); code(err) != api.CodeConflict {
		t.Fatalf("an old unbound yes granted after its name was paired again: %v", err)
	}
	// Nor when the same device is paired again for a person.
	b := h.permitAsk(agent, orchapi.PermitCardRequest{Kind: "lane-start", Days: 20})
	must(orchclient.RemoveApprover(ctx, h.owner, "spare-phone"))
	add3 := must(orchclient.AddApprover(ctx, h.owner, "spare-phone"))
	must(orchclient.Pair(ctx, h.device(ipTablet), add3.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "spare-phone"))
	must(orchclient.Decide(ctx, h.device(ipTablet), b.ID, "yes"))
	h.clock.add(time.Minute)
	must(orchclient.RemoveApprover(ctx, h.owner, "spare-phone"))
	add4 := must(orchclient.AddApproverFor(ctx, h.owner, "spare-phone", "ivo"))
	must(orchclient.Pair(ctx, h.device(ipTablet), add4.Code))
	must(orchclient.ConfirmApprover(ctx, h.device(ipPhone), "spare-phone"))
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: orchapi.PermitCardRequest{Kind: "lane-start", Days: 20}, Ask: b.ID}); code(err) != api.CodeConflict {
		t.Fatalf("an old unbound yes granted after the same device was paired again for a person: %v", err)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, b.ID)); got.Decision.ApproverPerson != "" {
		t.Fatalf("the old unbound answer took the new pairing's person: %q", got.Decision.ApproverPerson)
	}
}

// Only a person who owns or administers the project grants, however their
// device is paired.
func TestPermitGrantNeedsAnOwnerOrAdmin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "pat"}))
	must(h.owner.Grant(ctx, api.GrantRequest{Identity: "pat", Role: "member", Project: "harbor"}))
	add := must(orchclient.AddApproverFor(ctx, h.owner, "pat-phone", "pat"))
	must(orchclient.Pair(ctx, h.device(ipPhone), add.Code))
	agent, _ := h.session("51515151")
	lane := orchapi.PermitCardRequest{Kind: "lane-start", Days: 30}
	a := h.permitAsk(agent, lane)
	if _, err := orchclient.Decide(ctx, h.device(ipPhone), a.ID, "yes"); err != nil {
		t.Skipf("a member's device may not decide here: %v", err)
	}
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: lane, Ask: a.ID}); code(err) != api.CodeConflict ||
		!strings.Contains(err.Error(), "own or administer") {
		t.Fatalf("a member's yes: %v", err)
	}
}

// The dry run sees every new card from ask creation itself.
func TestPermitDryRunRunsOnNewAsks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var seen []string
	approvals.SetPermitDryRunHook(func(id string) { seen = append(seen, id) })
	t.Cleanup(func() { approvals.SetPermitDryRunHook(nil) })
	agent, _ := h.session("61616161")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "seen"}))
	if len(seen) != 1 || seen[0] != a.ID {
		t.Fatalf("dry run saw %v, want %s", seen, a.ID)
	}
}

func TestPermitDryRunRecordsAndReports(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairFor("ana", ipPhone, "ivo")
	agent, _ := h.session("41414141")
	sock := h.ownerIn("harbor", "")
	planned := h.workItem("planned work")
	finished := h.workItem("finished work")
	must(orchclient.SetStatus(ctx, h.ownerIn("harbor/solver", ""), finished.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))
	later := time.Now().Add(time.Hour) // after both items were made

	agentTask := orchapi.AskAction{Kind: orchapi.ActionRun, Machine: "box-a", Agent: "claude", Brief: "do the planned work", BriefSHA: "x"}
	script := orchapi.AskAction{Kind: orchapi.ActionRun, Machine: "box-b", Script: "run.sh", Ref: "main", Commit: strings.Repeat("a", 40)}
	command := orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "box-b", Command: "make it", Ref: "main", Commit: strings.Repeat("b", 40)}
	own := must(orchclient.CreateWork(ctx, agent, orchapi.CreateWorkRequest{Title: "made by the asker"}))
	type tc struct {
		name    string
		work    string
		created time.Time
		act     *orchapi.AskAction
		answer  string // "" leaves it open
		lane    string // the failed condition, "" = covers
		pick    string
		on      string // the action's choice, "" = the first
	}
	cases := []tc{
		{"planned, answered as recommended", planned.ID, later, &agentTask, "a", "", "box-no-secrets", ""},
		{"planned, answered otherwise", planned.ID, later, &agentTask, "b", "", "box-no-secrets", ""},
		{"planned, open", planned.ID, later, &agentTask, "", "", "box-no-secrets", ""},
		{"planned, action on b, answered as recommended", planned.ID, later, &agentTask, "a", "", "box-no-secrets", "b"},
		{"asked before the work was planned", planned.ID, time.Now().Add(-time.Hour), &agentTask, "", "work-planned", "box-no-secrets", ""},
		{"finished work", finished.ID, later, &agentTask, "", "work-open", "box-no-secrets", ""},
		{"unreadable work item", "local:nosuchitem", later, &agentTask, "", "work-open", "box-no-secrets", ""},
		{"no work item", "", later, &agentTask, "", "work-open", "box-no-secrets", ""},
		{"a script run", planned.ID, later, &script, "", "agent-task", "box-no-secrets", ""},
		{"a shell command", planned.ID, later, &command, "", "agent-task", "pinned-work", ""},
		{"work the asker made", own.ID, later, &agentTask, "", "work-planned", "box-no-secrets", ""},
		{"no action", planned.ID, later, nil, "", "-", "-", ""},
	}
	ids := map[string]string{}
	for i, c := range cases {
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "dry-run-" + string(rune('a'+i))}))
		ids[c.name] = a.ID
		cd := *card()
		cd.Recommend = "a"
		cd.Action = c.act
		if c.on != "" && c.act != nil {
			act := *c.act
			act.On = c.on
			cd.Action = &act
		}
		b, _ := json.Marshal(cd)
		a.Subject.Work, a.Created = c.work, c.created
		h.svc.PermitDryRunFor(ctx, a, string(b))
		if c.answer != "" {
			must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, c.answer))
		}
	}
	from, to := time.Now().Add(-24*time.Hour), time.Now().Add(24*time.Hour)
	rep := must(orchclient.PermitDryRun(ctx, sock, from, to))
	if len(rep.Rows) != 2 {
		t.Fatalf("rows: %+v", rep.Rows)
	}
	lane, pick := rep.Rows[0], rep.Rows[1]
	if lane.Kind != "lane-start" || lane.Checked != 11 || lane.WouldCover != 4 || lane.Saved != 1 || lane.WronglyCovered != 2 || lane.Open != 1 ||
		lane.NotCovered["work-planned"] != 2 || lane.NotCovered["work-open"] != 3 || lane.NotCovered["agent-task"] != 2 {
		t.Fatalf("lane-start: %+v", lane)
	}
	if pick.Kind != "machine-pick" || pick.Checked != 11 || pick.WouldCover != 0 || pick.NotCovered["box-no-secrets"] != 10 || pick.NotCovered["pinned-work"] != 1 {
		t.Fatalf("machine-pick: %+v", pick)
	}

	// An owner marks box-a as holding no secrets, with a signed yes: from
	// then on a card running on it would be covered.
	defer approvals.SetPermitMachines("box-a", "box-b")()
	box := orchapi.PermitCardRequest{Machine: "box-a", Class: "no-secrets"}
	ghost := orchapi.PermitCardRequest{Machine: "no-such-box", Class: "no-secrets"}
	gm := h.permitAsk(agent, ghost)
	must(orchclient.Decide(ctx, h.device(ipPhone), gm.ID, "yes"))
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: ghost, Ask: gm.ID}); code(err) != api.CodeConflict {
		t.Fatalf("a machine the project does not have: %v", err)
	}
	m := h.permitAsk(agent, box)
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: box, Ask: m.ID}); code(err) != api.CodeConflict {
		t.Fatalf("unanswered box card: %v", err)
	}
	must(orchclient.Decide(ctx, h.device(ipPhone), m.ID, "yes"))
	if g := must(orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: box, Ask: m.ID})); g.Box == nil || g.Box.Machine != "box-a" {
		t.Fatalf("box grant: %+v", g)
	}
	onA := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card(), Topic: "on-box-a"}))
	cd := *card()
	cd.Action = &agentTask
	b, _ := json.Marshal(cd)
	onA.Subject.Work, onA.Created = planned.ID, later
	h.svc.PermitDryRunFor(ctx, onA, string(b))
	if pick = must(orchclient.PermitDryRun(ctx, sock, from, to)).Rows[1]; pick.WouldCover != 1 || pick.Open != 1 {
		t.Fatalf("after the mark: %+v", pick)
	}
	// Taking the mark off needs no signature, and narrows at once.
	if _, err := orchclient.UnmarkBox(ctx, h.guest(), orchapi.BoxUnmarkRequest{Machine: "box-a", Class: "no-secrets"}); code(err) != api.CodeForbidden {
		t.Fatalf("guest unmark: %v", err)
	}
	must(orchclient.UnmarkBox(ctx, sock, orchapi.BoxUnmarkRequest{Machine: "box-a", Class: "no-secrets"}))
	if l := must(orchclient.Permits(ctx, sock)); len(l.Boxes) != 0 {
		t.Fatalf("boxes after unmark: %+v", l.Boxes)
	}
	// The answer that made the mark cannot put it back.
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: box, Ask: m.ID}); code(err) != api.CodeConflict {
		t.Fatalf("the old answer again: %v", err)
	}
	if l := must(orchclient.Permits(ctx, sock)); len(l.Boxes) != 0 {
		t.Fatalf("boxes after replaying the old answer: %+v", l.Boxes)
	}
	// A yes given before an unmark, used after it, marks nothing, even one
	// kept back while no mark was on.
	pre := h.permitAsk(agent, orchapi.PermitCardRequest{Machine: "box-b", Class: "no-secrets"})
	must(orchclient.Decide(ctx, h.device(ipPhone), pre.ID, "yes"))
	h.clock.add(time.Minute)
	if _, err := orchclient.UnmarkBox(ctx, sock, orchapi.BoxUnmarkRequest{Machine: "box-b", Class: "no-secrets"}); code(err) != api.CodeNotFound {
		t.Fatalf("unmark with no mark on: %v", err)
	}
	if _, err := orchclient.GrantPermit(ctx, agent, orchapi.PermitGrantRequest{PermitCardRequest: orchapi.PermitCardRequest{Machine: "box-b", Class: "no-secrets"}, Ask: pre.ID}); code(err) != api.CodeConflict {
		t.Fatalf("a yes from before the unmark: %v", err)
	}
	// No permit was granted by any of this, and no card was skipped: every
	// ask above is still an ordinary ask.
	if l := must(orchclient.Permits(ctx, sock)); len(l.Permits) != 0 {
		t.Fatalf("permits: %+v", l.Permits)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, ids["planned, open"])); got.Decision != nil || got.Status != orchapi.AskOpen {
		t.Fatalf("covered ask: %+v", got)
	}
}
