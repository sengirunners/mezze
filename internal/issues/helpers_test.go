package issues_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

func init() {
	for _, k := range []string{"BEADS_DIR", "BEADS_DOLT_SERVER_MODE", "BEADS_DOLT_PASSWORD", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER", "BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_AUTO_START", "BD_REAL", "BD_ACTOR", "BEADS_ACTOR"} {
		os.Unsetenv(k)
	}
}

func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ZM_KEEP_HOME") != "" {
		t.Logf("keeping ZM_HOME %s", dir)
		return dir
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startDaemon serves a throwaway daemon on its Unix socket and returns it
// with an owner client on that socket.
func startDaemon(t *testing.T) (*server.Daemon, *client.Client) {
	t.Helper()
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return dm, c
}
