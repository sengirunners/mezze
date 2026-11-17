package fulltest

import (
	"context"
	"github.com/sengirunners/mezze/internal/wiring"
	. "github.com/sengirunners/potluck/kit/server"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

func TestTaskLocalMailAndNativeComment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	home := shortHome(t)
	bin := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"task done\"}'\nsleep 0.2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "tester", "mini"
	cfg.File.Agents = map[string][]string{"claude": {bin}}
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	freezeProbe(dm)
	for _, m := range dm.Modules() {
		if s, ok := m.(*session.Service); ok {
			if err := s.Start(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := client.WithHandler(dm.SocketHandler())
	c.Cwd = home
	p, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "task-proj", RepoRules: api.RepoRules{Paths: []string{home}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Project, c.ProjectFrom = p.Slug, "flag"
	if _, err := orchclient.SetupIssues(ctx, c, "task"); err != nil {
		t.Fatal(err)
	}
	it, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "task bead"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := orchclient.Spawn(ctx, c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: home})
	if err != nil {
		t.Fatal(err)
	}
	c.As = parent.Address
	no := false
	task, err := orchclient.TaskStart(ctx, c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", Work: it.ID, Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err = orchclient.TaskStatus(ctx, c, task.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == "ended" && task.MailDelivery == "sent" && task.CommentDelivery == "sent" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if task.Result == nil || task.Result.Outcome.Kind != orchapi.OutcomeOK || task.MailDelivery != "sent" || task.CommentDelivery != "sent" {
		t.Fatalf("task: %+v", task)
	}
	comments, err := orchclient.IssueComments(ctx, c, it.ID)
	if err != nil || len(comments) != 1 || !strings.Contains(comments[0].Text, "task done") {
		t.Fatalf("comments: %+v %v", comments, err)
	}
	mail, err := orchclient.Inbox(ctx, c, orchapi.InboxQuery{Project: p.Slug})
	if err != nil || len(mail) != 1 || !strings.Contains(mail[0].Body, "task done") {
		t.Fatalf("mail: %+v %v", mail, err)
	}
}
