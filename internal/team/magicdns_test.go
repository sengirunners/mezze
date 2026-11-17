package team_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An authority node shared in from another tailnet resolves only by
// its full MagicDNS name. The invite carries that name, so the join works;
// an older code with the short name still joins (the replica falls back to
// the peer's full name).
func TestJoinASharedNodeByItsFullName(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	tn.Shared("potluck-mini")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	if !strings.HasPrefix(inv.Code, "potluck-mini."+tailnettest.Suffix+":") {
		t.Fatalf("the code %q does not carry the full MagicDNS name", inv.Code)
	}
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if j.Remote == nil || !strings.Contains(j.Remote.URL, "potluck-mini."+tailnettest.Suffix) {
		t.Fatalf("join with the full name: %+v", j)
	}
	if _, err := orchclient.CreateWork(ctx, at(r.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "through the shared node"}); err != nil {
		t.Fatalf("work through the shared node: %v", err)
	}
	// An older code naming potluck-mini: the short name does not resolve, the
	// replica dials the full one.
	inv2 := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "dana", Role: "member"}))(t)
	short := "potluck-mini:" + strings.SplitN(inv2.Code, ":", 2)[1]
	r2 := startDaemon(t, tn, "dana-mac", tailnettest.Identity{NodeID: "nDANA", NodeKey: "nodekey:dana-mac", Login: "dana@example.com", Host: "dana-mac"})
	if j2 := mustf(r2.owner.Join(ctx, api.JoinRequest{Code: short}))(t); j2.Remote == nil {
		t.Fatalf("join with an older short code: %+v", j2)
	}
}

// GitHub-join discovery reaches a shared authority by its full name too,
// and records the name it dialled.
func TestGitHubDiscoveryFindsASharedNode(t *testing.T) {
	ctx := context.Background()
	gh := githubfake.New(t)
	tn := tailnettest.New()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, quiet,
		func(c *config.Config) {
			c.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
		})
	tn.Shared("potluck-mini")
	c := startDaemon(t, tn, "beas-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea", Login: "bea@example.com", Host: "beas-mac"})
	j := mustf(c.owner.Join(ctx, api.JoinRequest{Project: "harbor"}))(t)
	if len(j.Authorities) != 1 || j.Authorities[0].Machine != "mini" || j.Authorities[0].Host != "potluck-mini."+tailnettest.Suffix {
		t.Fatalf("discovery of a shared node: %+v", j)
	}
}

// Host names are self-reported: an outsider's node shared in under the
// same potluck-mini name must never become the fallback's target. The pinned
// join dials the peer whose node is the pin; a pin matching neither node
// fails with the name's own error; a refused connection is not hidden
// behind a pin mismatch; and discovery asks both nodes. The fake lists
// peers in node ID order, so the outsider (nEVIL) comes first: a fallback
// that ignored the pin would dial it every time.
func TestSameHostNameFallsBackByPin(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	tailnetNode(t, tn, tailnettest.Identity{NodeID: "nEVIL", NodeKey: "nodekey:evil", Login: "eve@example.com", Host: "evil"}, "potluck-mini")
	tn.Shared("potluck-mini")
	tn.Shared("potluck-mini-1") // the outsider's node, which the tailnet named potluck-mini-1
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	short := "potluck-mini:" + strings.SplitN(inv.Code, ":", 2)[1]
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	if j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: short}))(t); j.Remote == nil {
		t.Fatalf("a short code with two potluck-mini peers: %+v", j)
	}
	// A pin that is neither node: the fallback dials nothing.
	parts := strings.Split(short, ":")
	other := "potluck-mini:" + project.NodeFingerprint("nNOBODY") + ":" + parts[2]
	r2 := startDaemon(t, tn, "dana-mac", tailnettest.Identity{NodeID: "nDANA", NodeKey: "nodekey:dana-mac", Login: "dana@example.com", Host: "dana-mac"})
	if _, err := r2.owner.Join(ctx, api.JoinRequest{Code: other}); err == nil || api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a pin matching no potluck-mini: %v, want unreachable (not a pin mismatch)", err)
	}
	// The outsider's node by its full name answers nothing (not listening):
	// a refused connection, never retried elsewhere, never a pin mismatch.
	full := "potluck-mini-1." + tailnettest.Suffix + ":" + project.NodeFingerprint("nEVIL") + ":" + parts[2]
	if _, err := r2.owner.Join(ctx, api.JoinRequest{Code: full}); err == nil || api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refused connection: %v", err)
	}
}

// Discovery asks every peer, two with one host name included.
func TestDiscoveryAsksPeersWithTheSameHostName(t *testing.T) {
	ctx := context.Background()
	gh := githubfake.New(t)
	tn := tailnettest.New()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, quiet,
		func(c *config.Config) {
			c.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
		})
	tailnetNode(t, tn, tailnettest.Identity{NodeID: "nEVIL", NodeKey: "nodekey:evil", Login: "eve@example.com", Host: "evil"}, "potluck-mini")
	c := startDaemon(t, tn, "beas-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea", Login: "bea@example.com", Host: "beas-mac"})
	j := mustf(c.owner.Join(ctx, api.JoinRequest{Project: "harbor"}))(t)
	if len(j.Authorities) != 1 || j.Authorities[0].Node != "nMINI" || len(j.Skipped) != 1 || j.Skipped[0].Machine != "mini" {
		t.Fatalf("two potluck-mini peers: found %+v, skipped %+v", j.Authorities, j.Skipped)
	}
}
