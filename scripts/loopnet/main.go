// Command loopnet runs several zm daemons on this one machine, each in its
// own ZM_HOME with its own Unix socket, joined to one fake tailnet on
// loopback (potluck/kit/tailnet/tailnettest): the same transport the team tests
// use. It exists so a person can rehearse a team (authority, members, guests)
// with the real `zm` CLI before any real Tailscale node exists:
//
//	go run ./scripts/loopnet mini:ivo:/tmp/zm-mini bea-mac:bea:/tmp/zm-bea
//	ZM_HOME=/tmp/zm-mini zm project team harbor   # and so on, per machine
//
// Each argument is machine:human:home[:tagged]. Node identities are faked
// (node ID n-<machine>, login <human>@loop.example); nothing joins a real
// tailnet and no auth key is used. Every daemon's node is brought up, so
// `zm invite` and `zm join` work at once. It is a development tool: not part
// of zm, never in a release.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loopnet:", err)
		os.Exit(1)
	}
}

type machine struct {
	name, human, home string
	tagged            bool
}

func parse(args []string) ([]machine, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: loopnet machine:human:home[:tagged] ...")
	}
	var ms []machine
	seen := map[string]bool{}
	for _, a := range args {
		p := strings.Split(a, ":")
		if len(p) < 3 || len(p) > 4 || p[0] == "" || p[1] == "" || p[2] == "" {
			return nil, fmt.Errorf("%q: want machine:human:home[:tagged]", a)
		}
		m := machine{name: config.Sanitize(p[0]), human: config.Sanitize(p[1]), home: p[2]}
		if len(p) == 4 {
			if p[3] != "tagged" {
				return nil, fmt.Errorf("%q: the fourth field can only be \"tagged\"", a)
			}
			m.tagged = true
		}
		if seen[m.name] {
			return nil, fmt.Errorf("machine %s given twice", m.name)
		}
		seen[m.name] = true
		ms = append(ms, m)
	}
	return ms, nil
}

func run(args []string) error {
	ms, err := parse(args)
	if err != nil {
		return err
	}
	lvl := slog.LevelInfo
	if os.Getenv("LOOPNET_VERBOSE") != "" {
		lvl = slog.LevelDebug
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tn := tailnettest.New()
	var stops []func()
	defer func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}()
	for _, m := range ms {
		stop, err := start(ctx, tn, m, lvl)
		if err != nil {
			return fmt.Errorf("%s: %w", m.name, err)
		}
		stops = append(stops, stop)
		fmt.Printf("loopnet: %s (human %s) serving %s/zm.sock, tailnet node %s%s\n", m.name, m.human, m.home, api.NodePrefixNew, m.name)
	}
	fmt.Println("loopnet: ready; ctrl-c stops every daemon")
	<-ctx.Done()
	fmt.Println("loopnet: stopping")
	return nil
}

func start(ctx context.Context, tn *tailnettest.Net, m machine, lvl slog.Level) (func(), error) {
	cfg, err := config.LoadFrom(m.home)
	if err != nil {
		return nil, err
	}
	cfg.Human, cfg.Machine = m.human, m.name
	if err := cfg.EnsureHome(); err != nil {
		return nil, err
	}
	release, err := server.LockHome(cfg.Home)
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("machine", m.name)
	dctx, cancel := context.WithCancel(ctx)
	dm, err := server.New(dctx, cfg, log, wiring.Full)
	if err != nil {
		cancel()
		release()
		return nil, err
	}
	id := tailnettest.Identity{NodeID: "n-" + m.name, NodeKey: "nodekey:" + m.name, Host: m.name}
	if m.tagged {
		id.Tagged = true
	} else {
		id.Login = m.human + "@loop.example"
	}
	for _, mod := range dm.Modules() {
		if s, ok := mod.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(id)
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(dctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			fmt.Fprintf(os.Stderr, "loopnet: %s did not stop\n", m.name)
		}
		release()
	}
	sock := client.NewUnix(cfg.SocketPath())
	for i := 0; ; i++ {
		if _, err := sock.Health(ctx); err == nil {
			break
		}
		if i > 400 {
			stop()
			return nil, fmt.Errorf("socket never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	owner := client.WithHandler(dm.SocketHandler())
	st, err := owner.TailnetUp(ctx, api.TailnetUpRequest{})
	if err != nil || !st.Running {
		stop()
		return nil, fmt.Errorf("tailnet up: %+v %v", st, err)
	}
	return stop, nil
}
