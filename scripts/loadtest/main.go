// Command loadtest is SPEC 17.7's load test, over the loopback tailnet
// (potluck/kit/tailnet/tailnettest, the transport scripts/loopnet and the team
// tests use): one authority and N people's replicas (default 5), each with M
// agent sessions (default 8) doing constant claims (racing on the same
// items), comments, status changes, mail and acks, presence changes and
// reads, for -duration (default 10 min). Mid-run one replica is cut off
// from the authority (its writes queue and drain when it heals) and the
// authority restarts once (every replica queues, then drains).
//
// Afterwards it checks: every write an agent was told succeeded (or was
// queued) is at the authority exactly once; no two holders ever held one
// key at once (the authority's event log); every outbox drained; every
// replica's copy converges to the authority (items, owners, leases,
// comments, mail); no steady-state operation's p95 is over its ceiling in
// scripts/ceilings/ceilings.json (the ones in the table; set on Sengi for
// this default shape). With -budgets=false (including CI), latency only warns;
// missing samples and correctness still fail. It prints
// p50/p95 latencies (markdown with -out).
//
// Every daemon runs in its own throwaway ZM_HOME under a temp directory,
// with BEADS_*/BD_*/DOLT_* unset; nothing joins a real tailnet, no tracker
// adapter runs (native local items). It is a development tool, never part
// of zm.
//
//	go run ./scripts/loadtest -duration 10m -out /tmp/load.md
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/mail"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/mezze/scripts/ceilings"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

var (
	flagDuration = flag.Duration("duration", 10*time.Minute, "how long agents run")
	flagPeople   = flag.Int("people", 5, "replicas (people)")
	flagAgents   = flag.Int("agents", 8, "agent sessions per replica")
	flagGuests   = flag.Int("guests", 1, "guest replicas (demo/solver) besides the people")
	flagGAgents  = flag.Int("guest-agents", 4, "agent sessions per guest replica")
	flagItems    = flag.Int("items", 40, "work items agents race on")
	flagPause    = flag.Duration("pause", 150*time.Millisecond, "mean pause between an agent's operations")
	flagOut      = flag.String("out", "", "write the results as markdown here")
	flagNoChaos  = flag.Bool("no-chaos", false, "skip the partition and the authority restart")
	flagVerbose  = flag.Bool("v", false, "daemon logs to stderr")
	flagCPU      = flag.String("cpuprofile", "", "write a CPU profile of the run (every daemon is in this process)")
	flagBudgets  = flag.Bool("budgets", true, "fail when a p95 is over its ceiling (scripts/ceilings/ceilings.json, set on Sengi for the default shape; false warns, missing samples still fail)")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

// ---- daemons ----

type node struct {
	name, human, home string
	id                tailnettest.Identity
	tn                *tailnettest.Net
	dm                *server.Daemon
	cancel            context.CancelFunc
	done              chan error
	release           func()
	owner             *client.Client // in-process, as the socket's owner
	sock              string
	area              string // a guest's area ("" for a member)
}

func (n *node) start(ctx context.Context, log *slog.Logger) error {
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
	dm, err := server.New(dctx, cfg, log.With("machine", n.name), wiring.Full)
	if err != nil {
		cancel()
		n.release()
		return err
	}
	for _, mod := range dm.Modules() {
		if s, ok := mod.(*tailnet.Service); ok {
			s.NewNode = n.tn.NodeFunc(n.id)
		}
	}
	n.dm, n.cancel, n.done, n.sock = dm, cancel, make(chan error, 1), cfg.SocketPath()
	go func() { n.done <- dm.Serve(dctx) }()
	sock := client.NewUnix(n.sock)
	for i := 0; ; i++ {
		if _, err := sock.Health(ctx); err == nil {
			break
		}
		if i > 1000 {
			n.stop()
			return errors.New("socket never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	n.owner = client.WithHandler(dm.SocketHandler())
	st, err := n.owner.TailnetUp(ctx, api.TailnetUpRequest{})
	if err != nil || !st.Running {
		n.stop()
		return fmt.Errorf("tailnet up: %+v %v", st, err)
	}
	return nil
}

func (n *node) stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(15 * time.Second):
		fmt.Fprintf(os.Stderr, "loadtest: %s did not stop\n", n.name)
		if os.Getenv("LOADTEST_DUMP") != "" {
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
		}
	}
	n.release()
	n.cancel = nil
}

// at is a client on n's real Unix socket, narrowed to project and acting as as.
func (n *node) at(project, as string) *client.Client {
	c := client.NewUnix(n.sock)
	c.Project, c.ProjectFrom, c.As = project, "flag", as
	return c
}

// ---- measurements ----

type stats struct {
	mu sync.Mutex
	d  map[string][]time.Duration
}

func (s *stats) add(op string, d time.Duration) {
	s.mu.Lock()
	if s.d == nil {
		s.d = map[string][]time.Duration{}
	}
	s.d[op] = append(s.d[op], d)
	s.mu.Unlock()
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	i := int(float64(len(ds)-1) * p)
	return ds[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }

// ---- what agents were told ----

type ledger struct {
	mu        sync.Mutex
	comments  map[string]string // body token -> comment id the agent got
	mails     map[string]string // body token -> mail id
	claims    int64
	conflicts int64
	queued    int64
	errs      map[string]int
}

func (l *ledger) err(op string, err error) {
	l.mu.Lock()
	if l.errs == nil {
		l.errs = map[string]int{}
	}
	msg := err.Error()
	if len(msg) > 90 {
		msg = msg[:90]
	}
	l.errs[op+": "+msg]++
	l.mu.Unlock()
}

// ---- the run ----

func run() error {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "DOLT_") {
			os.Unsetenv(k)
		}
	}
	base, err := os.MkdirTemp("", "zmload")
	if err != nil {
		return err
	}
	defer os.RemoveAll(base)
	var lw io.Writer = io.Discard
	if *flagVerbose {
		lw = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(lw, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Presence every 15 s as in production; the claim wait as in production.
	tn := tailnettest.New()

	auth := &node{name: "mini", human: "ana", home: filepath.Join(base, "mini"), tn: tn,
		id: tailnettest.Identity{NodeID: "n-mini", NodeKey: "nodekey:mini", Login: "ana@loop.example", Host: "mini"}}
	if err := auth.start(ctx, log); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	defer func() { auth.stop() }()
	demo, err := auth.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "demo"})
	if err != nil {
		return err
	}
	// Every agent mails a few dozen peers far faster than real agents do; the
	// project's send limits (SPEC 9) would refuse pairs at random, so they are
	// raised here: this test measures load and sync, not the loop breaker.
	if _, err := auth.dm.Deps.Projects.SetSection(ctx, demo.ID, "mail_limits", func(any) (any, error) {
		return mail.Limits{PerSender: 1_000_000, PerPair: 1_000_000}, nil
	}); err != nil {
		return err
	}
	areas := []string{"solver", "billing"}
	for _, a := range areas {
		if _, err := auth.owner.AddArea(ctx, "demo", api.Area{Name: a}); err != nil {
			return err
		}
	}
	if _, err := auth.owner.ProjectTeam(ctx, "demo"); err != nil {
		return err
	}
	ao := auth.at("demo", "")
	var items []string
	for i := 0; i < *flagItems; i++ {
		it, err := orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprintf("load item %d", i), Area: areas[i%2]})
		if err != nil {
			return err
		}
		items = append(items, it.ID)
	}
	probe, err := orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "propagation probe", Area: "solver"})
	if err != nil {
		return err
	}

	// People and their agents.
	var people []*node
	type agent struct {
		n         *node
		addr, rid string
		area      string
		c         *client.Client
		pi        int
		pool      []string // the items it works on (a guest: its area's)
		to        []int    // agents it mails (indexes into agents)
	}
	var agents []*agent
	for i := 1; i <= *flagPeople; i++ {
		name := fmt.Sprintf("p%d-mac", i)
		p := &node{name: name, human: fmt.Sprintf("person%d", i), home: filepath.Join(base, name), tn: tn,
			id: tailnettest.Identity{NodeID: "n-" + name, NodeKey: "nodekey:" + name, Login: fmt.Sprintf("person%d@loop.example", i), Host: name}}
		if err := p.start(ctx, log); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		defer p.stop()
		inv, err := auth.owner.Invite(ctx, api.InviteRequest{Project: "demo", As: p.human, Role: "member"})
		if err != nil {
			return err
		}
		j, err := p.owner.Join(ctx, api.JoinRequest{Code: inv.Code})
		if err != nil || j.Remote == nil {
			return fmt.Errorf("%s join: %+v %v", name, j, err)
		}
		people = append(people, p)
		for k := 0; k < *flagAgents; k++ {
			rid := fmt.Sprintf("%s-agent-%d", name, k)
			ss, err := orchclient.Register(ctx, p.at("demo/"+areas[k%2], ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Title: rid, State: orchapi.StateRunning})
			if err != nil {
				return fmt.Errorf("%s register: %w", name, err)
			}
			agents = append(agents, &agent{n: p, addr: ss.Address, rid: rid, area: areas[k%2], c: p.at("demo", ss.Address), pi: i - 1, pool: items})
		}
	}
	// Guests of demo/solver: they work, mail and read in their area only,
	// and their copy must never hold a row outside it (SPEC 17.7).
	var solverItems []string
	for i, id := range items {
		if areas[i%2] == "solver" {
			solverItems = append(solverItems, id)
		}
	}
	var guests []*node
	for i := 1; i <= *flagGuests; i++ {
		name := fmt.Sprintf("g%d-mac", i)
		g := &node{name: name, human: fmt.Sprintf("guest%d", i), home: filepath.Join(base, name), tn: tn, area: "solver",
			id: tailnettest.Identity{NodeID: "n-" + name, NodeKey: "nodekey:" + name, Login: fmt.Sprintf("guest%d@loop.example", i), Host: name}}
		if err := g.start(ctx, log); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		defer g.stop()
		inv, err := auth.owner.Invite(ctx, api.InviteRequest{Project: "demo/solver", As: g.human, Role: "guest"})
		if err != nil {
			return err
		}
		if j, err := g.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil || j.Remote == nil {
			return fmt.Errorf("%s join: %+v %v", name, j, err)
		}
		guests = append(guests, g)
		for k := 0; k < *flagGAgents; k++ {
			rid := fmt.Sprintf("%s-agent-%d", name, k)
			ss, err := orchclient.Register(ctx, g.at("demo/solver", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Title: rid, State: orchapi.StateRunning})
			if err != nil {
				return fmt.Errorf("%s register: %w", name, err)
			}
			agents = append(agents, &agent{n: g, addr: ss.Address, rid: rid, area: "solver", c: g.at("demo", ss.Address), pi: *flagPeople + i - 1, pool: solverItems})
		}
	}
	for i, ag := range agents {
		for j, o := range agents {
			// Mail goes to another machine; a guest mails sessions in its
			// area only (mail to a session is filed in the session's area).
			if o.pi != ag.pi && (ag.n.area == "" || o.area == ag.n.area) {
				agents[i].to = append(agents[i].to, j)
			}
		}
	}
	replicas := append(append([]*node{}, people...), guests...)
	for _, p := range people {
		if err := waitCopy(ctx, p, len(items)+1); err != nil {
			return err
		}
	}
	for _, g := range guests {
		if err := waitCopy(ctx, g, len(solverItems)+1); err != nil {
			return err
		}
	}
	for _, p := range replicas {
		teamOf(p).Heartbeat(ctx) // the authority knows every agent before mail flows
	}
	fmt.Printf("loadtest: authority + %d replicas x %d agents + %d guest replicas x %d agents, %d items, %s\n", len(people), *flagAgents, len(guests), *flagGAgents, len(items), *flagDuration)

	if *flagCPU != "" {
		f, err := os.Create(*flagCPU)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	st := &stats{}
	led := &ledger{comments: map[string]string{}, mails: map[string]string{}}
	var down atomic.Int32 // >0 while a disruption runs (probes are not counted then)
	loadCtx, stopLoad := context.WithTimeout(ctx, *flagDuration)
	defer stopLoad()
	var wg sync.WaitGroup
	var seq atomic.Int64

	// The guests' copies are checked for rows outside their area every 2 s
	// while load runs, and once more at the end.
	hidden := map[string]bool{}
	for i, id := range items {
		if areas[i%2] != "solver" {
			hidden[id] = true
		}
	}
	var leakMu sync.Mutex
	leakN, leakFirst, leakChecks := 0, "", 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for loadCtx.Err() == nil {
			time.Sleep(2 * time.Second)
			for _, g := range guests {
				n, first := leaks(context.Background(), g, hidden)
				leakMu.Lock()
				leakChecks++
				leakN += n
				if n > 0 && leakFirst == "" {
					leakFirst = g.name + ": " + first
				}
				leakMu.Unlock()
			}
		}
	}()

	// Agents.
	for ai, ag := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(ai)+1, 7))
			timed := func(op string, f func() error) error {
				t0 := time.Now()
				d0 := down.Load()
				err := f()
				if err == nil || api.IsCode(err, api.CodeQueued) {
					// Steady state and the disruption windows (a replica cut off,
					// the authority restarting, and 10 s after each) apart: the
					// budgets are for the first.
					if d0 > 0 || down.Load() > 0 {
						op = "during disruption: " + op
					}
					st.add(op, time.Since(t0))
				}
				return err
			}
			for loadCtx.Err() == nil {
				time.Sleep(time.Duration(rng.Int64N(int64(2 * *flagPause))))
				if loadCtx.Err() != nil {
					return
				}
				ctx := context.WithoutCancel(loadCtx)
				switch x := rng.IntN(100); {
				case x < 30: // a claim cycle: claim, comment, maybe status, release
					key := ag.pool[rng.IntN(len(ag.pool))]
					var cr orchapi.ClaimResponse
					err := timed("claim", func() error {
						var err error
						cr, err = orchclient.Claim(ctx, ag.c, key, orchapi.ClaimRequest{TTLSeconds: 30})
						return err
					})
					atomic.AddInt64(&led.claims, 1)
					switch {
					case api.IsCode(err, api.CodeQueued):
						atomic.AddInt64(&led.queued, 1)
						continue
					case api.IsCode(err, api.CodeConflict):
						atomic.AddInt64(&led.conflicts, 1)
						continue
					case err != nil:
						led.err("claim", err)
						continue
					}
					if cr.Lease.Holder != ag.addr {
						led.err("claim", fmt.Errorf("holder %s", cr.Lease.Holder))
					}
					time.Sleep(time.Duration(50+rng.IntN(200)) * time.Millisecond)
					tok := fmt.Sprintf("c-%d", seq.Add(1))
					var c orchapi.Comment
					if err := timed("comment", func() error {
						var err error
						c, err = orchclient.Comment(ctx, ag.c, key, orchapi.CommentRequest{Body: "progress " + tok})
						return err
					}); err != nil {
						led.err("comment", err)
					} else {
						led.mu.Lock()
						led.comments[tok] = c.ID
						led.mu.Unlock()
					}
					if rng.IntN(3) == 0 {
						stt := []orchapi.WorkStatus{orchapi.WorkInReview, orchapi.WorkOpen}[rng.IntN(2)]
						if err := timed("status", func() error {
							_, err := orchclient.SetStatus(ctx, ag.c, key, orchapi.StatusRequest{Status: stt})
							return err
						}); err != nil {
							led.err("status", err)
						}
					}
					if err := timed("release", func() error {
						_, err := orchclient.Release(ctx, ag.c, key, orchapi.ReleaseRequest{})
						return err
					}); err != nil && !api.IsCode(err, api.CodeQueued) {
						led.err("release", err)
					}
				case x < 50: // a comment on any item
					key := ag.pool[rng.IntN(len(ag.pool))]
					tok := fmt.Sprintf("c-%d", seq.Add(1))
					var c orchapi.Comment
					if err := timed("comment", func() error {
						var err error
						c, err = orchclient.Comment(ctx, ag.c, key, orchapi.CommentRequest{Body: "note " + tok})
						return err
					}); err != nil {
						led.err("comment", err)
						continue
					}
					led.mu.Lock()
					led.comments[tok] = c.ID
					led.mu.Unlock()
				case x < 68: // mail to an agent on another machine
					to := agents[ag.to[rng.IntN(len(ag.to))]]
					tok := fmt.Sprintf("m-%d", seq.Add(1))
					var m orchapi.Mail
					if err := timed("send", func() error {
						var err error
						m, err = orchclient.Send(ctx, ag.c, orchapi.SendMailRequest{To: to.addr, Body: "hello " + tok})
						return err
					}); err != nil {
						led.err("send", err)
						continue
					}
					led.mu.Lock()
					led.mails[tok] = m.ID
					led.mu.Unlock()
				case x < 78: // inbox, ack the oldest
					var in []orchapi.Mail
					if err := timed("inbox", func() error {
						var err error
						in, err = orchclient.Inbox(ctx, ag.n.at("", ag.addr), orchapi.InboxQuery{})
						return err
					}); err != nil {
						led.err("inbox", err)
						continue
					}
					if len(in) > 0 {
						if err := timed("ack", func() error {
							_, err := orchclient.Ack(ctx, ag.c, in[len(in)-1].ID)
							return err
						}); err != nil && !api.IsCode(err, api.CodeNotFound) {
							led.err("ack", err)
						}
					}
				case x < 86: // reads
					if err := timed("work ls", func() error {
						_, err := orchclient.WorkList(ctx, ag.n.at("", ag.addr), orchapi.WorkQuery{Limit: 500})
						return err
					}); err != nil {
						led.err("work ls", err)
					}
					if err := timed("work show", func() error {
						_, err := orchclient.GetWork(ctx, ag.c, ag.pool[rng.IntN(len(ag.pool))])
						return err
					}); err != nil {
						led.err("work show", err)
					}
				case x < 92: // the prompt hook's notice
					if err := timed("notice", func() error {
						_, err := orchclient.Notice(ctx, ag.n.at("", ag.addr))
						return err
					}); err != nil {
						led.err("notice", err)
					}
				default: // presence: the session's state moves (a heartbeat follows)
					s := []orchapi.SessionState{orchapi.StateRunning, orchapi.StateIdle}[rng.IntN(2)]
					if err := timed("presence", func() error {
						_, err := orchclient.Register(ctx, ag.n.at("demo/"+ag.area, ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: ag.rid, Title: ag.rid, State: s})
						return err
					}); err != nil {
						led.err("presence", err)
					}
				}
			}
		}()
	}

	// Propagation probe: a comment on one replica, timed until every other
	// replica's copy shows it (not counted while a disruption runs).
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewPCG(99, 7))
		for loadCtx.Err() == nil {
			time.Sleep(time.Second)
			if down.Load() > 0 {
				continue
			}
			w := replicas[rng.IntN(len(replicas))]
			c, err := orchclient.Comment(loadCtx, w.at("demo", ""), probe.ID, orchapi.CommentRequest{Body: "probe"})
			if err != nil {
				continue
			}
			t0 := time.Now()
			var pw sync.WaitGroup
			for _, o := range replicas {
				if o == w {
					continue
				}
				pw.Add(1)
				go func() {
					defer pw.Done()
					oc := o.at("demo", "")
					for time.Since(t0) < 10*time.Second {
						it, err := orchclient.GetWork(context.Background(), oc, probe.ID)
						if err == nil {
							for _, x := range it.Comments {
								if x.ID == c.ID {
									if down.Load() == 0 {
										st.add("propagation (replica to replica)", time.Since(t0))
									}
									return
								}
							}
						}
						time.Sleep(2 * time.Millisecond)
					}
				}()
			}
			pw.Wait()
		}
	}()

	// Chaos: one replica cut off for a while, then the authority restarts.
	if !*flagNoChaos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := *flagDuration
			sleep := func(x time.Duration) bool {
				select {
				case <-loadCtx.Done():
					return false
				case <-time.After(x):
					return true
				}
			}
			if !sleep(d * 3 / 10) {
				return
			}
			cut := people[len(people)-1]
			gap := min(30*time.Second, d/10)
			fmt.Printf("loadtest: %s cut off from the authority for %s\n", cut.name, gap)
			down.Add(1)
			tn.Cut(api.NodePrefixNew+cut.name, api.NodePrefixNew+"mini", true)
			sleep(gap)
			tn.Cut(api.NodePrefixNew+cut.name, api.NodePrefixNew+"mini", false)
			sleep(10 * time.Second)
			down.Add(-1)
			if !sleep(d * 3 / 10) {
				return
			}
			fmt.Println("loadtest: the authority restarts")
			down.Add(1)
			auth.stop()
			time.Sleep(2 * time.Second)
			if err := auth.start(ctx, log); err != nil {
				fmt.Fprintln(os.Stderr, "loadtest: authority restart:", err)
				os.Exit(1)
			}
			sleep(10 * time.Second)
			down.Add(-1)
		}()
	}

	<-loadCtx.Done()
	wg.Wait()
	loadEnd := time.Now()
	fmt.Println("loadtest: load stopped; checking")

	// 1. Every outbox drains.
	var fails []string
	for _, p := range replicas {
		if err := waitDrained(ctx, p, 60*time.Second); err != nil {
			fails = append(fails, err.Error())
		}
	}
	drained := time.Since(loadEnd)
	// 2. Every replica converges to the authority.
	conv, err := converge(ctx, auth, replicas, 30*time.Second)
	if err != nil {
		fails = append(fails, err.Error())
	}
	convAfter := time.Since(loadEnd)
	// 3. No lost or duplicated write.
	lost, dups, err := checkWrites(ctx, auth, led)
	if err != nil {
		fails = append(fails, err.Error())
	}
	// 4. No double claim, by the authority's own log.
	claims, doubles, err := checkClaims(ctx, auth)
	if err != nil {
		fails = append(fails, err.Error())
	}
	leakMu.Lock()
	for _, g := range guests {
		if n, first := leaks(ctx, g, hidden); n > 0 {
			leakN += n
			if leakFirst == "" {
				leakFirst = g.name + ": " + first
			}
		}
	}
	if leakN > 0 {
		fails = append(fails, fmt.Sprintf("%d rows outside a guest's area (first: %s)", leakN, leakFirst))
	}
	leakMu.Unlock()
	refused := map[string]int{}
	for _, p := range replicas {
		es, _ := p.owner.Outbox(ctx, "", false)
		for _, e := range es {
			refused[e.Kind+" "+e.State]++
		}
	}
	if lost > 0 {
		fails = append(fails, fmt.Sprintf("%d writes lost", lost))
	}
	if dups > 0 {
		fails = append(fails, fmt.Sprintf("%d writes duplicated", dups))
	}
	if doubles > 0 {
		fails = append(fails, fmt.Sprintf("%d double claims", doubles))
	}
	for k, n := range refused {
		if !strings.HasPrefix(k, "claim conflict") && !strings.HasPrefix(k, "ack conflict") && !strings.HasPrefix(k, "status conflict") && !strings.HasPrefix(k, "release failed") && !strings.HasPrefix(k, "lease conflict") {
			fails = append(fails, fmt.Sprintf("outbox: %d %s", n, k))
		}
	}

	limits, err := ceilings.Load()
	if err != nil {
		return err
	}
	for _, ds := range st.d {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	}
	budgetFails, warnings := checkCeilings(st.d, limits.Loadtest, *flagBudgets)
	fails = append(fails, budgetFails...)

	// Report.
	var b strings.Builder
	fmt.Fprintf(&b, "Load test (scripts/loadtest): authority + %d replicas x %d agents + %d guest replicas x %d agents over the loopback tailnet, %d items, %s",
		len(people), *flagAgents, len(guests), *flagGAgents, len(items), *flagDuration)
	if !*flagNoChaos {
		b.WriteString(", one replica cut off mid-run, one authority restart")
	}
	b.WriteString(".\n\n| operation (on a replica's socket) | n | p50 ms | p95 ms | p99 ms | max ms | ceiling ms |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	var ops []string
	for op := range st.d {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		ds := st.d[op]
		ceiling := "-"
		if c, ok := limits.Loadtest[op]; ok {
			ceiling = fmt.Sprintf("%g", c.P95)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s |\n", op, len(ds), ms(pct(ds, .5)), ms(pct(ds, .95)), ms(pct(ds, .99)), ms(ds[len(ds)-1]), ceiling)
	}
	led.mu.Lock()
	fmt.Fprintf(&b, "\nWrites agents were told succeeded or queued: %d comments, %d mails; lost %d, duplicated %d.\n", len(led.comments), len(led.mails), lost, dups)
	fmt.Fprintf(&b, "Claims tried %d: %d conflicts answered at once, %d queued; the authority's log has %d claims and %d double claims.\n",
		led.claims, led.conflicts, led.queued, claims, doubles)
	if len(guests) > 0 {
		fmt.Fprintf(&b, "Guest replicas (demo/solver, %d x %d agents): %d checks of their copies while load ran and once after; %d rows outside their area.\n",
			len(guests), *flagGAgents, leakChecks, leakN)
	}
	fmt.Fprintf(&b, "Outboxes drained %.1f s after load stopped; every copy matched the authority %.1f s after (%s).\n", drained.Seconds(), convAfter.Seconds(), conv)
	if len(refused) > 0 {
		var ks []string
		for k, n := range refused {
			ks = append(ks, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(ks)
		fmt.Fprintf(&b, "Refused queued writes kept in outboxes: %s.\n", strings.Join(ks, ", "))
	}
	if len(led.errs) > 0 {
		var ks []string
		for k, n := range led.errs {
			ks = append(ks, fmt.Sprintf("%dx %s", n, k))
		}
		sort.Strings(ks)
		fmt.Fprintf(&b, "Errors agents saw: %s.\n", strings.Join(ks, "; "))
	}
	led.mu.Unlock()
	result := writeResult(&b, fails, warnings)
	fmt.Print(b.String())
	if *flagOut != "" {
		_ = os.WriteFile(*flagOut, []byte(b.String()), 0o644)
	}
	return result
}

func waitCopy(ctx context.Context, p *node, n int) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if wl, err := orchclient.WorkList(ctx, p.at("demo", ""), orchapi.WorkQuery{Limit: 5000}); err == nil && len(wl.Items) >= n {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("%s: copy never filled", p.name)
}

func waitDrained(ctx context.Context, p *node, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		rs, err := p.owner.Remotes(ctx)
		if err == nil && len(rs) == 1 && rs[0].Replica != nil && rs[0].Replica.Pending == 0 && rs[0].Replica.Reachable {
			return nil
		}
		if time.Now().After(deadline) {
			dumpGoroutines()
			return fmt.Errorf("%s: outbox not drained: %s %v", p.name, replicaState(rs), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// view is what convergence compares: items (status, owner), leases,
// comment ids per item, mail ids.
type view struct {
	items    map[string]string
	leases   map[string]string
	comments map[string]int
	mail     map[string]bool
}

func viewOf(ctx context.Context, c *client.Client, area string) (view, error) {
	v := view{items: map[string]string{}, leases: map[string]string{}, comments: map[string]int{}, mail: map[string]bool{}}
	for _, s := range []orchapi.WorkStatus{orchapi.WorkOpen, orchapi.WorkClaimed, orchapi.WorkInReview, orchapi.WorkDone} {
		wl, err := orchclient.WorkList(ctx, c, orchapi.WorkQuery{Status: s, Area: area, Limit: 5000})
		if err != nil {
			return v, err
		}
		for _, it := range wl.Items {
			v.items[it.ID] = string(it.Status) + " " + it.Owner
			full, err := orchclient.GetWork(ctx, c, it.ID)
			if err != nil {
				return v, err
			}
			v.comments[it.ID] = len(full.Comments)
		}
	}
	ls, err := orchclient.Leases(ctx, c, orchapi.LeaseQuery{Project: "demo"})
	if err != nil {
		return v, err
	}
	for _, l := range ls {
		if _, isItem := v.items[l.Key]; isItem {
			v.leases[l.Key] = l.Holder
		}
	}
	return v, nil
}

// mailOf reads (id, acked) of every mail a member replica sees (not the
// owners mailbox nor needs=human mail, which a member's replica is not sent)
// straight from a file: the authority's project.db or a replica's copy.
func mailOf(ctx context.Context, db *sql.DB, q string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var acked bool
		if err := rows.Scan(&id, &acked); err != nil {
			return nil, err
		}
		out[id+" "+fmt.Sprint(acked)] = true
	}
	return out, rows.Err()
}

// redactedEq: a guest's copy shows a holder it may not see as "someone"
// (redacted for it, SPEC 6), which matches any real holder at the authority.
func redactedEq(auth, rep string, guest bool) bool {
	if auth == rep || !guest {
		return auth == rep
	}
	as, ah, isItem := strings.Cut(auth, " ") // items: "status owner"; leases: "holder"
	if !isItem {
		return rep == "someone" && auth != ""
	}
	rs, rh, _ := strings.Cut(rep, " ")
	return as == rs && rh == "someone" && ah != ""
}

func diff(a, b view, guest bool) string {
	for k, x := range a.items {
		if !redactedEq(x, b.items[k], guest) {
			return fmt.Sprintf("item %s: authority %q, replica %q", k, x, b.items[k])
		}
	}
	if len(a.items) != len(b.items) {
		return fmt.Sprintf("%d items vs %d", len(a.items), len(b.items))
	}
	for k, x := range a.leases {
		if !redactedEq(x, b.leases[k], guest) {
			return fmt.Sprintf("lease %s: %q vs %q", k, x, b.leases[k])
		}
	}
	if len(a.leases) != len(b.leases) {
		return fmt.Sprintf("%d leases vs %d", len(a.leases), len(b.leases))
	}
	for k, n := range a.comments {
		if b.comments[k] != n {
			return fmt.Sprintf("item %s: %d comments vs %d", k, n, b.comments[k])
		}
	}
	for k := range a.mail {
		if !b.mail[k] {
			return "mail " + k + " missing or differs"
		}
	}
	if len(a.mail) != len(b.mail) {
		return fmt.Sprintf("%d mails vs %d", len(a.mail), len(b.mail))
	}
	return ""
}

func converge(ctx context.Context, auth *node, people []*node, d time.Duration) (string, error) {
	deadline := time.Now().Add(d)
	summary := ""
	for {
		adb, err := projectDB(auth)
		if err != nil {
			return "", err
		}
		wants := map[string]view{} // by area ("" = the whole project)
		bad := ""
		for _, p := range people {
			want, ok := wants[p.area]
			if !ok {
				if want, err = viewOf(ctx, auth.at("demo", ""), p.area); err != nil {
					return "", err
				}
				q := `SELECT id, acked_at IS NOT NULL FROM mail WHERE recipient != 'owners' AND needs != 'human'`
				if p.area != "" {
					q += ` AND area = '` + p.area + `'`
				}
				if want.mail, err = mailOf(ctx, adb, q); err != nil {
					return "", err
				}
				wants[p.area] = want
				if p.area == "" {
					summary = fmt.Sprintf("%d items, %d leases, %d comments, %d mails", len(want.items), len(want.leases), sum(want.comments), len(want.mail))
				}
			}
			got, err := viewOf(ctx, p.at("demo", ""), "")
			if err != nil {
				bad = p.name + ": " + err.Error()
				break
			}
			rdb, err := replicaDB(ctx, p)
			if err != nil {
				bad = p.name + ": " + err.Error()
				break
			}
			got.mail, err = mailOf(ctx, rdb, `SELECT id, acked = 1 FROM mail WHERE recipient != 'owners' AND needs != 'human'`)
			rdb.Close()
			if err != nil {
				bad = p.name + ": " + err.Error()
				break
			}
			if x := diff(want, got, p.area != ""); x != "" {
				bad = p.name + ": " + x
				break
			}
		}
		if bad == "" {
			return summary, nil
		}
		if time.Now().After(deadline) {
			explain(ctx, auth, people, bad)
			return "", errors.New("no convergence: " + bad)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// leaks counts the rows in a guest's copy outside its area: items in
// another area, and comments, leases and mail of one (SPEC 17.7: "no guest
// replica ever held a row outside its area").
func leaks(ctx context.Context, g *node, hidden map[string]bool) (int, string) {
	db, err := replicaDB(ctx, g)
	if err != nil {
		return 0, ""
	}
	defer db.Close()
	n, first := 0, ""
	for _, q := range []string{
		`SELECT id FROM items WHERE area != '` + g.area + `'`,
		`SELECT 'comment on ' || item FROM comments`,
		`SELECT 'lease ' || key FROM leases`,
		`SELECT 'mail ' || id FROM mail WHERE coalesce(json_extract(json, '$.area'), '') != '` + g.area + `'`,
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		for rows.Next() {
			var x string
			_ = rows.Scan(&x)
			f := strings.Fields(x)
			if strings.HasPrefix(q, "SELECT 'comment") || strings.HasPrefix(q, "SELECT 'lease") {
				if !hidden[f[len(f)-1]] {
					continue
				}
			}
			n++
			if first == "" {
				first = x
			}
		}
		rows.Close()
	}
	return n, first
}

// replicaDB opens p's copy of demo (a second handle; WAL readers share)
// through p's store, so it has team's replica migrations (d.AddMigrations).
func replicaDB(ctx context.Context, p *node) (*sql.DB, error) {
	rs, err := p.owner.Remotes(ctx)
	if err != nil || len(rs) != 1 {
		return nil, fmt.Errorf("remotes %v", err)
	}
	return p.dm.Deps.Store.OpenReplica(ctx, filepath.Join(p.home, team.DirReplicas, rs[0].Project.ID+".db"), 1)
}

// explain prints, for a diverging item, the authority's event trail and each
// copy's row and outbox entries for it.
func explain(ctx context.Context, auth *node, people []*node, bad string) {
	f := strings.Fields(bad)
	if len(f) < 3 || f[1] != "item" {
		return
	}
	id := strings.TrimSuffix(f[2], ":")
	fmt.Println("explain", id)
	_ = auth.owner.Events(ctx, api.EventQuery{Project: "demo", Subject: id, Since: "0", NoLive: true}, func(e api.Event) error {
		var it struct {
			Status, Owner, Holder string
		}
		_ = json.Unmarshal(e.Data, &it)
		fmt.Printf("  authority %s %-28s status=%s owner=%s holder=%s\n", e.ID, e.Type, it.Status, it.Owner, it.Holder)
		return nil
	})
	for _, p := range people {
		db, err := replicaDB(ctx, p)
		if err != nil {
			continue
		}
		var base, js, cursor string
		_ = db.QueryRowContext(ctx, `SELECT base, json FROM items WHERE id = ?`, id).Scan(&base, &js)
		_ = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'cursor'`).Scan(&cursor)
		var it orchapi.WorkItem
		_ = json.Unmarshal([]byte(js), &it)
		fmt.Printf("  %s base=%s cursor=%s status=%s\n", p.name, base, cursor, it.Status)
		rows, err := db.QueryContext(ctx, `SELECT id, kind, state, seen, body FROM outbox WHERE item = ? ORDER BY seq`, id)
		if err == nil {
			for rows.Next() {
				var wid, kind, state, seen, body string
				_ = rows.Scan(&wid, &kind, &state, &seen, &body)
				fmt.Printf("    outbox %s %s %s seen=%s %s\n", wid, kind, state, seen, body)
			}
			rows.Close()
		}
		db.Close()
	}
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// checkWrites: every comment and mail an agent got an id for is at the
// authority exactly once (by its unique body token), under that id. It reads
// the authority's file directly (tens of thousands of mails).
func checkWrites(ctx context.Context, auth *node, led *ledger) (lost, dups int, err error) {
	db, err := projectDB(auth)
	if err != nil {
		return 0, 0, err
	}
	byTok := map[string][]string{}
	for _, q := range []string{`SELECT id, body FROM work_comments`, `SELECT id, body FROM mail`} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var id, body string
			if err := rows.Scan(&id, &body); err != nil {
				rows.Close()
				return 0, 0, err
			}
			f := strings.Fields(body)
			if len(f) > 0 && (strings.HasPrefix(f[len(f)-1], "c-") || strings.HasPrefix(f[len(f)-1], "m-")) {
				byTok[f[len(f)-1]] = append(byTok[f[len(f)-1]], id)
			}
		}
		rows.Close()
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	check := func(m map[string]string) {
		for tok, id := range m {
			got := byTok[tok]
			switch {
			case len(got) == 0:
				lost++
			case got[0] != id:
				lost++ // stored under another id than the agent was given
			}
		}
	}
	check(led.comments)
	check(led.mails)
	for _, ids := range byTok {
		if len(ids) > 1 {
			dups++
		}
	}
	return lost, dups, nil
}

// projectDB is the authority's file for demo.
func projectDB(auth *node) (*sql.DB, error) {
	ps, err := auth.owner.Projects(context.Background())
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.Slug == "demo" {
			return auth.dm.Deps.Store.Project(authz.All(), authz.Read, p.ID)
		}
	}
	return nil, errors.New("no project demo")
}

func teamOf(n *node) *team.Service {
	for _, m := range n.dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			return s
		}
	}
	panic("no team module")
}

// checkClaims walks the authority's log: a key claimed by a new holder
// while another still held it (no release or expiry between) is a double
// claim.
func checkClaims(ctx context.Context, auth *node) (claims, doubles int, err error) {
	holder := map[string]string{}
	err = auth.owner.Events(ctx, api.EventQuery{Project: "demo", Types: "dev.zm.work.", Since: "0", NoLive: true}, func(e api.Event) error {
		var l orchapi.Lease
		switch e.Type {
		case orchapi.EventWorkClaimed:
			claims++
			if json.Unmarshal(e.Data, &l) != nil {
				return nil
			}
			if h := holder[l.Key]; h != "" && h != l.Holder {
				doubles++
			}
			holder[l.Key] = l.Holder
		case orchapi.EventWorkReleased, orchapi.EventWorkLeaseExpired:
			if json.Unmarshal(e.Data, &l) == nil && holder[l.Key] == l.Holder {
				holder[l.Key] = ""
			}
		}
		return nil
	})
	return claims, doubles, err
}
