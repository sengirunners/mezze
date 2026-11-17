package team_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Bea joins (SPEC 22): Ivo runs zm grant
// @beagh owner harbor; Bea, on the tailnet, runs zm join harbor,
// approves in the browser, and bd (native issues), mail and work reach the
// authority from his machine. No join code exists anywhere. GitHub is a
// fake on httptest; the tailnet is tailnettest's.
func TestBeaJoins(t *testing.T) {
	ctx := context.Background()
	gh := githubfake.New(t)
	tn := tailnettest.New()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	withGitHub := func(c *config.Config) {
		c.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
	}
	// Ivo's authority: harbor, a team project with native issues.
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, quiet, withGitHub)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	ao := at(a.owner, "harbor", "")
	if res, err := orchclient.AddTracker(ctx, ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("native tracker: %+v %v", res, err)
	}
	// zm grant @beagh owner harbor
	bea, _ := gh.AddUser("beagh", "User")
	if g, err := a.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@beagh", Role: "owner", Project: "harbor"}); err != nil || g.Login != "beagh" {
		t.Fatalf("grant: %+v %v", g, err)
	}
	// Two more zm nodes: another authority that runs GitHub logins, and one
	// that runs none (github:false).
	startDaemonWith(t, tn, "lab", tailnettest.Identity{NodeID: "nLAB", NodeKey: "nodekey:lab", Login: "lab@example.com", Host: "lab"}, quiet, withGitHub)
	startDaemon(t, tn, "plain", tailnettest.Identity{NodeID: "nPLAIN", NodeKey: "nodekey:plain", Login: "plain@example.com", Host: "plain"})
	// Bea's Mac, on the tailnet; he approves in the browser (the fake).
	c := startDaemon(t, tn, "beas-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea", Login: "bea@example.com", Host: "beas-mac"})
	// zm join harbor: the authorities found, with their tailnet owners, and
	// the skipped peer; nothing starts until one is chosen.
	j := mustf(c.owner.Join(ctx, api.JoinRequest{Project: "harbor"}))(t)
	if len(j.Authorities) != 2 || j.Authorities[0].Machine != "lab" || j.Authorities[1].Machine != "mini" ||
		j.Authorities[1].Owner != "ivo@example.com" || j.GitHub != nil || gh.Starts() != 0 {
		t.Fatalf("discovery: %+v (GitHub asked %d times)", j, gh.Starts())
	}
	if len(j.Skipped) != 1 || j.Skipped[0].Machine != "plain" || !strings.Contains(j.Skipped[0].Reason, "github:false") {
		t.Fatalf("skipped: %+v", j.Skipped)
	}
	// A refusal and an expiry come back through the daemon.
	poll := func(project string) (api.JoinResponse, error) {
		j, err := c.owner.Join(ctx, api.JoinRequest{Project: project, From: "mini"})
		for err == nil && j.GitHub != nil && j.Remote == nil {
			j, err = c.owner.Join(ctx, api.JoinRequest{Project: project, Login: j.GitHub.Login, WaitMS: 5000})
		}
		return j, err
	}
	gh.Approver(bea.ID, 0)
	if _, err := poll("nope"); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "no grant") {
		t.Fatalf("a project with no grant: %v, want the refusal", err)
	}
	gh.Approver(-1, 0)
	if _, err := poll("harbor"); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired code: %v", err)
	}
	// --from (any case) starts it; the code comes with the owner.
	gh.Approver(bea.ID, 1)
	j = mustf(c.owner.Join(ctx, api.JoinRequest{Project: "harbor", From: "MINI"}))(t)
	g := j.GitHub
	if g == nil || g.UserCode == "" || !strings.HasPrefix(g.VerificationURI, gh.URL) || g.Authority != "mini" || g.Owner != "ivo@example.com" ||
		!strings.Contains(g.For, "nBEA") || !strings.Contains(g.For, "beas-mac") || j.Remote != nil {
		t.Fatalf("first answer: %+v", j)
	}
	deadline := time.Now().Add(30 * time.Second)
	for j.Remote == nil {
		if time.Now().After(deadline) {
			t.Fatal("the join never ended")
		}
		j = mustf(c.owner.Join(ctx, api.JoinRequest{Project: "harbor", Login: g.Login, WaitMS: 5000}))(t)
	}
	if fi, err := os.Stat(j.Remote.TokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v, want 0600", fi.Mode(), err)
	}
	if j.Remote.AuthorityOwner != "ivo@example.com" {
		t.Fatalf("the remote records owner %q", j.Remote.AuthorityOwner)
	}
	if rm := j.Remote; rm.Project.Slug != "harbor" || rm.Cap.Role != "owner" || rm.Identity != "beagh" || rm.Authority != "mini" {
		t.Fatalf("remote: %+v", rm)
	}
	// No join code was ever made.
	if invs := mustf(a.owner.Invites(ctx))(t); len(invs) != 0 {
		t.Fatalf("invites: %+v", invs)
	}
	cc := at(c.owner, "harbor", "")
	// A verified human owner configures a summary at the authority, never
	// a second archive or schedule in the replica's registry.
	summaryConfig, err := orchclient.SetSummaryConfig(ctx, cc, "harbor", orchapi.SummaryConfig{Enabled: false, Hour: 7, Timezone: "America/Los_Angeles"})
	if err != nil || summaryConfig.Project != j.Remote.Project.ID {
		t.Fatalf("owner summary schedule: %+v %v", summaryConfig, err)
	}
	authorityConfig, err := orchclient.SummaryConfig(ctx, ao, "harbor")
	if err != nil || authorityConfig.Version != summaryConfig.Version {
		t.Fatalf("summary schedule fork: %+v %v", authorityConfig, err)
	}
	// bd: native issues, through the replica, at the authority.
	is := mustf(orchclient.CreateIssue(ctx, cc, orchapi.NewIssueRequest{Title: "filed from Bea's Mac"}))(t)
	if got := mustf(orchclient.Issue(ctx, a.owner, is.ID))(t); got.Title != "filed from Bea's Mac" {
		t.Fatalf("issue at the authority: %+v", got)
	}
	// Mail: Bea to Ivo, read at the authority.
	m := mustf(orchclient.Send(ctx, cc, orchapi.SendMailRequest{To: "human:mini-human", Body: "joined with GitHub", Needs: orchapi.NeedsAny}))(t)
	eventually(t, 10*time.Second, func() error {
		got, err := orchclient.Mail(ctx, a.owner, orchapi.MailQuery{Project: "harbor", To: "human:mini-human"})
		if err != nil {
			return err
		}
		for _, x := range got {
			if x.ID == m.ID || x.Body == "joined with GitHub" {
				return nil
			}
		}
		return errors.New("not yet")
	})
	// Work: an item made at the authority, claimed from Bea's Mac.
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "for Bea"}))(t)
	eventually(t, 10*time.Second, func() error {
		_, err := orchclient.Claim(ctx, cc, it.ID, orchapi.ClaimRequest{})
		return err
	})
	// The owners heard about the new node.
	ms := mustf(orchclient.Mail(ctx, a.owner, orchapi.MailQuery{Project: "harbor", To: "owners"}))(t)
	found := false
	for _, x := range ms {
		found = found || (strings.Contains(x.Body, "@beagh") && strings.Contains(x.Body, "nBEA"))
	}
	if !found {
		t.Fatalf("no owner mail about Bea's node: %+v", ms)
	}
}

// A rogue zm node that is the only one to answer is shown with its
// tailnet owner, and nothing starts until it is chosen: its claim to be an
// authority is never taken on trust.
func TestRogueSoleAuthorityNeedsAChoice(t *testing.T) {
	ctx := context.Background()
	gh := githubfake.New(t)
	tn := tailnettest.New()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	startDaemonWith(t, tn, "evil", tailnettest.Identity{NodeID: "nEVIL", NodeKey: "nodekey:evil", Login: "mallory@example.com", Host: "evil"}, quiet,
		func(c *config.Config) {
			c.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
		})
	c := startDaemon(t, tn, "beas-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea", Login: "bea@example.com", Host: "beas-mac"})
	j := mustf(c.owner.Join(ctx, api.JoinRequest{Project: "harbor"}))(t)
	if len(j.Authorities) != 1 || j.Authorities[0].Machine != "evil" || j.Authorities[0].Owner != "mallory@example.com" || j.GitHub != nil {
		t.Fatalf("the sole responder: %+v", j)
	}
	if gh.Starts() != 0 {
		t.Fatalf("a GitHub login started at an authority nobody chose (%d)", gh.Starts())
	}
}

// A join by name that finds no authority is refused promptly, saying how
// many zm peers it asked and why each was skipped; the whole bring-up and
// discovery is bounded (a short injected deadline here).
func TestJoinByNameFailsFast(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	c := startDaemon(t, tn, "beas-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea", Login: "bea@example.com", Host: "beas-mac"})
	_, err := c.owner.Join(ctx, api.JoinRequest{Project: "nonsense"})
	if !api.IsCode(err, api.CodeNotFound) || !strings.Contains(err.Error(), "checked 0 zm-* peers") || !strings.Contains(err.Error(), "is Tailscale up?") {
		t.Fatalf("no zm peers: %v", err)
	}
	startDaemon(t, tn, "plain", tailnettest.Identity{NodeID: "nPLAIN", NodeKey: "nodekey:plain", Login: "plain@example.com", Host: "plain"})
	_, err = c.owner.Join(ctx, api.JoinRequest{Project: "nonsense"})
	if !strings.Contains(err.Error(), "checked 1 zm-* peers; zm-plain: runs no GitHub logins") {
		t.Fatalf("a github:false peer: %v", err)
	}
	old := teamlink.NameJoinWait
	teamlink.NameJoinWait = time.Millisecond
	t.Cleanup(func() { teamlink.NameJoinWait = old })
	start := time.Now()
	_, err = c.owner.Join(ctx, api.JoinRequest{Project: "nonsense"})
	if took := time.Since(start); took > 5*time.Second || !strings.Contains(err.Error(), "is Tailscale up?") {
		t.Fatalf("past the deadline: %v after %s", err, took)
	}
}

// A stranger's machine, with no tailnet, joins one area of harbor as a
// guest over the home's https URL, through a fake GitHub: the grant is the
// project-level member grant of its account, and the join takes less.
func TestGitHubJoinOverTunnelIsOneAreaAsGuest(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	gh := githubfake.New(t)
	a, p := tunnelHomeWith(t, nil, "mini", tailnettest.Identity{}, func(c *config.Config) {
		c.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
	})
	teamHarbor(t, a)
	fp, _ := tunnelOf(t, a).Fingerprint()
	ao := at(a.owner, "harbor", "")
	solver := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "solver work", Area: "solver"}))(t)
	billing := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "billing work", Area: "billing"}))(t)
	host, _ := gh.AddUser("hostuser", "User")
	if _, err := a.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@hostuser", Role: "member", Project: "harbor"}); err != nil {
		t.Fatal(err)
	}
	m := startDaemon(t, nil, "host-box", tailnettest.Identity{})
	home := p.URL() + "#" + fp
	// No area, or no fingerprint: refused before any call.
	if _, err := m.owner.Join(ctx, api.JoinRequest{Project: "harbor", From: home}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no area: %v", err)
	}
	if _, err := m.owner.Join(ctx, api.JoinRequest{Project: "harbor", Area: "solver", From: p.URL()}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no fingerprint: %v", err)
	}
	gh.Approver(host.ID, 0)
	// A tunnel join is a guest's: a higher role is refused before any call.
	if _, err := m.owner.Join(ctx, api.JoinRequest{Project: "harbor", Area: "solver", Role: "owner", From: home}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "guest") {
		t.Fatalf("asking owner over https: %v", err)
	}
	if _, err := m.owner.Join(ctx, api.JoinRequest{Project: "harbor", Area: "solver", From: "http-box"}); err == nil || strings.Contains(err.Error(), "https://<host>") {
		t.Fatalf("a machine named http-box is a tailnet join: %v", err)
	}
	// The join: the code and who is asking come first; no tailnet anywhere.
	j := mustf(m.owner.Join(ctx, api.JoinRequest{Project: "harbor", Area: "solver", From: home}))(t)
	g := j.GitHub
	if g == nil || g.UserCode == "" || g.Owner != "" || !strings.Contains(g.For, "zmkey:") || !strings.Contains(g.For, "solver") || !strings.Contains(g.For, "guest") {
		t.Fatalf("first answer: %+v", j)
	}
	deadline := time.Now().Add(30 * time.Second)
	for j.Remote == nil {
		if time.Now().After(deadline) {
			t.Fatal("the join never ended")
		}
		j = mustf(m.owner.Join(ctx, api.JoinRequest{Project: "harbor", Area: "solver", From: home, Login: g.Login, WaitMS: 5000}))(t)
	}
	x := j.Remote
	if x.Via != api.ViaTunnel || x.URL != p.URL() || x.Fingerprint != fp || x.Cap.Role != api.RoleGuest || x.Cap.Area != "solver" || x.Identity != "hostuser" {
		t.Fatalf("remote: %+v", x)
	}
	if invs := mustf(a.owner.Invites(ctx))(t); len(invs) != 0 {
		t.Fatalf("invites: %+v", invs)
	}
	// The copy holds the area, not the rest of the project.
	eventually(t, 10*time.Second, func() error {
		wl := mustf(orchclient.WorkList(ctx, m.owner, orchapi.WorkQuery{}))(t)
		if !hasItem(wl.Items, solver.ID) {
			return fmt.Errorf("copy: %+v", wl.Items)
		}
		if hasItem(wl.Items, billing.ID) {
			t.Fatalf("a solver guest sees billing work: %+v", wl.Items)
		}
		return nil
	})
}
