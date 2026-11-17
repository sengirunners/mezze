package team_test

import (
	"context"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// redeemer posts join codes to the authority from one fake node.
type redeemer struct {
	c *client.Client
}

func newRedeemer(t *testing.T, tn *tailnettest.Net, id tailnettest.Identity) redeemer {
	n := tailnetNode(t, tn, id, "zm-"+strings.ToLower(id.NodeID))
	return redeemer{c: client.NewRemote("http://potluck-mini:7433", n.HTTPClient())}
}

func (r redeemer) redeem(code, machine string) (api.RedeemResponse, error) {
	var out api.RedeemResponse
	err := r.c.Do(context.Background(), http.MethodPost, "/v0/invites/redeem", api.RedeemRequest{Code: code, Machine: machine}, &out)
	return out, err
}

func sameRefusal(t *testing.T, what string, err error) {
	t.Helper()
	var e *api.Error
	if !strings.Contains(errString(err), project.ErrBadCode.Message) || !errorsAs(err, &e) || e.Code != api.CodeForbidden {
		t.Fatalf("%s: want the uniform refusal, got %v", what, err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func errorsAs(err error, e **api.Error) bool {
	x, ok := err.(*api.Error)
	if ok {
		*e = x
	}
	return ok
}

func TestRedeemRules(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	invite := func(as string, tagged bool, ttl time.Duration) string {
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor/solver", As: as, Role: "guest", AllowTagged: tagged, TTLSeconds: int64(ttl / time.Second)}))(t)
		if !strings.HasPrefix(inv.Code, "potluck-mini."+tailnettest.Suffix+":"+project.NodeFingerprint("nMINI")+":") { // the full MagicDNS name
			t.Fatalf("code %q", inv.Code)
		}
		return inv.Code
	}
	n1 := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nONE", NodeKey: "k1", Login: "one@example.com"})
	n2 := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nTWO", NodeKey: "k2", Login: "two@example.com"})

	// Wrong code.
	_, err := n1.redeem("potluck-mini:"+project.NodeFingerprint("nMINI")+":AAAA-AAAA", "one")
	sameRefusal(t, "wrong code", err)

	// A lost answer: the same code from the same node returns the same
	// token until that token's first call.
	code := invite("vida", false, time.Hour)
	r1 := mustf(n1.redeem(code, "one"))(t)
	r2 := mustf(n1.redeem(code, "one"))(t)
	if r1.Token == "" || r1.Token != r2.Token || r1.Cap.Area != "solver" || r1.Cap.Role != "guest" || r1.Machine != "one" {
		t.Fatalf("retry: %+v %+v", r1, r2)
	}
	// Retries from the redeeming node are not attempts: more of them than
	// MaxInviteAttempts still get the same token before its first use.
	for i := 0; i < project.MaxInviteAttempts+2; i++ {
		if r3, err := n1.redeem(code, "one"); err != nil || r3.Token != r1.Token {
			t.Fatalf("retry %d: %+v %v", i, r3, err)
		}
	}
	// From another node: refused, like a wrong code.
	_, err = n2.redeem(code, "two")
	sameRefusal(t, "used code from another node", err)
	// The token's first call ends the retry window.
	tc := client.NewRemote("http://potluck-mini:7433", tn.Node("zm-none").HTTPClient())
	tc.Token = r1.Token
	if _, err := tc.Projects(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = n1.redeem(code, "one")
	sameRefusal(t, "used code after first use", err)

	// Expired.
	exp := invite("ex", false, time.Second)
	time.Sleep(1100 * time.Millisecond)
	_, err = n2.redeem(exp, "two")
	sameRefusal(t, "expired code", err)

	// Tagged nodes only with --allow-tagged.
	tg := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nTAG", NodeKey: "kt", Tagged: true})
	_, err = tg.redeem(invite("ci", false, time.Hour), "ci")
	sameRefusal(t, "tagged without allow-tagged", err)
	if _, err := tg.redeem(invite("ci2", true, time.Hour), "ci"); err != nil {
		t.Fatalf("tagged with allow-tagged: %v", err)
	}

	// The authority's own machine is refused before the code is read.
	self := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nSELF", NodeKey: "ks", Login: "ivo@example.com", Host: "mini"})
	if _, err := self.redeem(invite("me", false, time.Hour), "me"); !api.IsCode(err, api.CodeForbidden) || strings.Contains(err.Error(), "invalid") {
		t.Fatalf("own machine: %v", err)
	}
	// The socket and an unidentified network caller.
	if err := a.owner.Do(ctx, http.MethodPost, "/v0/invites/redeem", api.RedeemRequest{Code: code}, nil); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("socket: %v", err)
	}
	if err := client.WithHandler(a.dm.Handler).Do(ctx, http.MethodPost, "/v0/invites/redeem", api.RedeemRequest{Code: code}, nil); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("unmarked: %v", err)
	}

	// One machine name per node at this authority.
	n3 := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nTHREE", NodeKey: "k3", Login: "three@example.com"})
	if _, err := n3.redeem(invite("three", false, time.Hour), "one"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("taken machine name: %v", err)
	}

	// Rate limit: after RedeemPerNode wrong codes, even a right one is refused.
	n4 := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nFOUR", NodeKey: "k4", Login: "four@example.com"})
	good := invite("four", false, time.Hour)
	for i := 0; i < teamlink.RedeemPerNode; i++ {
		_, err := n4.redeem("potluck-mini:"+project.NodeFingerprint("nMINI")+":BBBB-BBB"+string(rune('0'+i)), "four")
		sameRefusal(t, "wrong", err)
	}
	_, err = n4.redeem(good, "four")
	sameRefusal(t, "rate-limited right code", err)

	// At most 10 live invites per project.
	for i := 0; ; i++ {
		_, err := a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bulk", Role: "member"})
		if err != nil {
			if !api.IsCode(err, api.CodeConflict) || i > project.MaxLiveInvites {
				t.Fatalf("invite %d: %v", i, err)
			}
			break
		}
	}
	// Removed invites leave nothing behind: no identity was created.
	ids := mustf(a.owner.Identities(ctx))(t)
	for _, id := range ids {
		if id.Name == "bulk" || id.Name == "ex" {
			t.Fatalf("an unredeemed invite made identity %s", id.Name)
		}
	}
}

func TestJoinRefusals(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)

	// A mismatched authority fingerprint: the node answering as potluck-mini is
	// not the one the code pinned.
	c, _ := project.ParseJoinCode(inv.Code)
	bad := strings.Replace(inv.Code, c.Fingerprint, project.NodeFingerprint("nIMPOSTOR"), 1)
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "kc", Login: "bea@example.com", Host: "bea-mac"})
	if _, err := r.owner.Join(ctx, api.JoinRequest{Code: bad}); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("fingerprint mismatch: %v", err)
	}
	// A taken zm-<machine> name on the tailnet: a second bea-mac.
	r2 := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA2", NodeKey: "kc2", Login: "bea@example.com", Host: "bea-mac-2"})
	if _, err := r2.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("taken name: %v", err)
	}
	// --machine must be this daemon's machine name.
	if _, err := r.owner.Join(ctx, api.JoinRequest{Code: inv.Code, Machine: "other"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("--machine: %v", err)
	}
	// The real one joins with the same code (the refusals spent nothing).
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if j.Remote == nil || j.Remote.Cap.Role != "member" {
		t.Fatalf("join %+v", j)
	}
	// Joining again with a fresh code: refused, and the new token revoked.
	inv2 := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	if _, err := r.owner.Join(ctx, api.JoinRequest{Code: inv2.Code}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("second join: %v", err)
	}
	live := 0
	for _, tk := range mustf(a.owner.Tokens(ctx))(t) {
		if tk.Node != "" && tk.Revoked == nil {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("%d live node tokens after a refused second join", live)
	}
	// solo is refused while a replica is joined.
	if _, err := a.owner.ProjectSolo(ctx, "harbor"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("solo while joined: %v", err)
	}
}
