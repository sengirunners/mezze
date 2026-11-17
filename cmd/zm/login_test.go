package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
	"github.com/sengirunners/potluck/kit/server"
)

// The CLI starts the login at the authority, shows the code and URL, and
// waits; it never sees a GitHub token.
func TestLoginGitHub(t *testing.T) {
	gh := githubfake.New(t)
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	cfg.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	u, _ := gh.AddUser("ana", "User")
	gh.Approver(u.ID, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var msg bytes.Buffer
	out, err := githubLogin(ctx, client.WithHandler(dm.SocketHandler()), api.GitHubLoginRequest{Purpose: "link"}, &msg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Link == nil || out.Link.Login != "ana" || out.Link.Identity != "ivo" {
		t.Fatalf("outcome %+v", out)
	}
	if !strings.Contains(msg.String(), gh.URL+"/login/device") || !strings.Contains(msg.String(), "WDJB-MJHT") {
		t.Fatalf("prompt %q must show the URL and the code", msg.String())
	}
	if issued := gh.Issued(); len(issued) != 1 || strings.Contains(msg.String(), issued[0]) {
		t.Fatalf("issued %v; prompt %q", issued, msg.String())
	}
}
