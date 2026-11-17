package main

// The replica-* and team-* rows: a member's replica of a team project on
// 11.5k native issues, joined to its authority over the loopback tailnet
// (potluck/kit/tailnet/tailnettest). A fake tailnet node cannot be handed to a
// separate process, so both daemons run in this one, on real Unix sockets,
// from this tree: -zm does not reach them. That is also why ZM=<other build>
// turns them off.
//
// replica-* rows are answered from the replica's copy (SPEC 17.6), with no
// wait on the authority: the 16 ms bar. team-* rows take one round trip by
// design: a claim is a compare-and-set at the authority, and items are made
// there (SPEC 17.7: 50 ms loopback). A native issue has no route of its own
// on a replica yet (SPEC "Replica hook"): replicas reach them as work items.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func teamRow(name string) bool {
	return strings.HasPrefix(name, "replica-") || strings.HasPrefix(name, "team-")
}

// teamNode is a daemon in this process.
type teamNode struct {
	name, human, home string
	id                tailnettest.Identity
	dm                *server.Daemon
	cancel            context.CancelFunc
	done              chan error
	release           func()
	owner             *client.Client
	sock              string
}

// teamEnv is what the team rows need.
type teamEnv struct {
	auth, rep *teamNode
	c         *client.Client // an agent session on the replica
	open      []string       // open work items, for the writes
	all       []string       // every work item
	next      int            // the next item team-claim takes
}

func (n *teamNode) start(ctx context.Context, tn *tailnettest.Net, log *slog.Logger) error {
	cfg, err := config.LoadFrom(n.home)
	if err != nil {
		return err
	}
	cfg.Human, cfg.Machine = n.human, n.name
	if err := cfg.EnsureHome(); err != nil {
		return err
	}
	if n.release, err = server.LockHome(cfg.Home); err != nil {
		return err
	}
	dctx, cancel := context.WithCancel(ctx)
	dm, err := server.New(dctx, cfg, log, wiring.Full)
	if err != nil {
		cancel()
		n.release()
		return err
	}
	for _, mod := range dm.Modules() {
		if s, ok := mod.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(n.id)
		}
	}
	n.dm, n.cancel, n.done, n.sock = dm, cancel, make(chan error, 1), cfg.SocketPath()
	go func() { n.done <- dm.Serve(dctx) }()
	if err := waitHealth(n.sock, 10*time.Second); err != nil {
		n.stop()
		return err
	}
	n.owner = client.NewUnix(n.sock)
	st, err := n.owner.TailnetUp(ctx, api.TailnetUpRequest{})
	if err != nil {
		n.stop()
		return err
	}
	if !st.Running {
		n.stop()
		return errors.New("the loopback tailnet did not come up")
	}
	return nil
}

func (n *teamNode) stop() {
	if n == nil || n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(15 * time.Second):
		fmt.Fprintln(os.Stderr, "bench:", n.name, "did not stop")
	}
	n.release()
	n.cancel = nil
}

// startTeam starts the authority and a member's replica and waits for the
// replica's copy of every issue.
func (e *env) startTeam(ctx context.Context) error {
	// These daemons read this process's environment: nothing real may reach them.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ZM_") {
			os.Unsetenv(k)
		}
	}
	os.Setenv("HOME", filepath.Join(e.tmp, "u"))
	log := slog.New(slog.DiscardHandler)
	tn := tailnettest.New()
	t := &teamEnv{
		auth: &teamNode{name: "mini", human: "ana", home: filepath.Join(e.tmp, "a"),
			id: tailnettest.Identity{NodeID: "n-mini", NodeKey: "nodekey:mini", Login: "ana@loop.example", Host: "mini"}},
		rep: &teamNode{name: "mac", human: "bea", home: filepath.Join(e.tmp, "r"),
			id: tailnettest.Identity{NodeID: "n-mac", NodeKey: "nodekey:mac", Login: "bea@loop.example", Host: "mac"}},
	}
	e.team = t
	if err := t.auth.start(ctx, tn, log); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	t0 := time.Now()
	if _, err := bindNative(ctx, t.auth.owner, t.auth.home, e.native.seed, "mina"); err != nil {
		return fmt.Errorf("authority's store: %w", err)
	}
	if _, err := t.auth.owner.ProjectTeam(ctx, issueProject); err != nil {
		return err
	}
	if err := t.rep.start(ctx, tn, log); err != nil {
		return fmt.Errorf("replica: %w", err)
	}
	inv, err := t.auth.owner.Invite(ctx, api.InviteRequest{Project: issueProject, As: t.rep.human, Role: "member"})
	if err != nil {
		return err
	}
	if j, err := t.rep.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil || j.Remote == nil {
		return fmt.Errorf("join: %+v %v", j, err)
	}
	c := client.NewUnix(t.rep.sock)
	c.Project, c.ProjectFrom = issueProject, "flag"
	if err := t.waitCopy(ctx, c); err != nil {
		return err
	}
	ss, err := orchclient.Register(ctx, c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bench-replica-agent", Title: "bench", State: orchapi.StateRunning})
	if err != nil {
		return err
	}
	c.As = ss.Address
	t.c = c
	for _, id := range e.native.open {
		t.open = append(t.open, workPrefix+":"+id)
	}
	for _, id := range e.native.ids {
		t.all = append(t.all, workPrefix+":"+id)
	}
	// A row must time the copy: the last issue, with its comments, is in it.
	last := t.all[len(t.all)-1]
	if _, err := orchclient.GetWork(ctx, c, last); err != nil {
		return fmt.Errorf("the copy has no %s: %w", last, err)
	}
	fmt.Printf("replica joined and copied %d items in %s\n", len(t.all), time.Since(t0).Round(time.Millisecond))
	return nil
}

// waitCopy waits until the replica's copy is in step with the authority.
func (t *teamEnv) waitCopy(ctx context.Context, c *client.Client) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		rs, err := t.rep.owner.Remotes(ctx)
		if err == nil && len(rs) == 1 && rs[0].Replica != nil && rs[0].Replica.State == "ok" {
			if wl, err := orchclient.WorkList(ctx, c, orchapi.WorkQuery{Status: orchapi.WorkDone, Limit: 5000}); err == nil && len(wl.Items) == 5000 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the replica's copy did not fill after 3m: %+v %v", rs, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *env) stopTeam() {
	if e.team == nil {
		return
	}
	e.team.rep.stop()
	e.team.auth.stop()
}

func (e *env) teamRows() map[string]func() (time.Duration, error) {
	ctx := context.Background()
	t := e.team
	if t == nil || t.c == nil {
		return nil
	}
	rng := rand.New(rand.NewPCG(5, 6))
	anyID := func() string { return t.all[rng.IntN(len(t.all))] }
	openID := func() string { return t.open[rng.IntN(len(t.open))] }
	// nextOpen walks the open items, so a claim never meets its own lease.
	nextOpen := func() string {
		id := t.open[t.next%len(t.open)]
		t.next++
		return id
	}
	list := func(q func(i int) orchapi.WorkQuery) func() (time.Duration, error) {
		return timedN(func(i int) error {
			l, err := orchclient.WorkList(ctx, t.c, q(i))
			if err == nil && len(l.Items) == 0 {
				err = fmt.Errorf("an empty list for %+v", q(i))
			}
			return err
		})
	}
	return map[string]func() (time.Duration, error){
		"replica-ls": list(func(int) orchapi.WorkQuery { return orchapi.WorkQuery{} }),
		"replica-search": list(func(i int) orchapi.WorkQuery {
			return orchapi.WorkQuery{Q: fmt.Sprintf("module %d", i%40)} // in 1 title of 40, so most of the copy is walked
		}),
		"replica-show": timedN(func(int) error {
			it, err := orchclient.GetWork(ctx, t.c, anyID())
			if err == nil && it.Title == "" {
				err = fmt.Errorf("an empty item")
			}
			return err
		}),
		"replica-rollup": timedN(func(int) error {
			l, err := orchclient.WorkRollup(ctx, t.c, t.c.Project, "")
			if err == nil && (len(l.Rollups) == 0 || l.Rollups[0].Totals.Total == 0) {
				err = fmt.Errorf("an empty rollup: %+v", l)
			}
			return err
		}),
		"replica-comment": timedN(func(i int) error {
			_, err := orchclient.Comment(ctx, t.c, anyID(), orchapi.CommentRequest{Body: fmt.Sprintf("bench comment %d", i)})
			return err
		}),
		"replica-status": timedN(func(i int) error {
			_, err := orchclient.SetStatus(ctx, t.c, openID(), orchapi.StatusRequest{Status: []orchapi.WorkStatus{orchapi.WorkInReview, orchapi.WorkOpen}[i%2]})
			return err
		}),
		"replica-link": timedN(func(i int) error {
			_, err := orchclient.Link(ctx, t.c, openID(), orchapi.LinkRequest{Rel: "pr", Target: fmt.Sprintf("https://example.com/%d", i)})
			return err
		}),
		"team-claim": func() (time.Duration, error) {
			id := nextOpen()
			t0 := time.Now()
			_, err := orchclient.Claim(ctx, t.c, id, orchapi.ClaimRequest{TTLSeconds: 30})
			d := time.Since(t0)
			if err != nil {
				return 0, err
			}
			_, err = orchclient.Release(ctx, t.c, id, orchapi.ReleaseRequest{})
			return d, err
		},
		"team-release": func() (time.Duration, error) {
			id := nextOpen()
			if _, err := orchclient.Claim(ctx, t.c, id, orchapi.ClaimRequest{TTLSeconds: 30}); err != nil {
				return 0, err
			}
			t0 := time.Now()
			_, err := orchclient.Release(ctx, t.c, id, orchapi.ReleaseRequest{})
			return time.Since(t0), err
		},
		"team-create": timedN(func(i int) error {
			_, err := orchclient.CreateWork(ctx, t.c, orchapi.CreateWorkRequest{Title: fmt.Sprintf("bench item %d", i)})
			return err
		}),
	}
}
