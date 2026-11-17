package team_test

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestTaskDeliveryQueuesReplicaComment(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "task work", Area: "solver"}))(t)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	child := register(t, r, "harbor/solver", "task-child")
	eventually(t, 5*time.Second, func() error {
		_, err := orchclient.GetWork(ctx, at(r.owner, "harbor", child.Address), it.ID)
		return err
	})
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	relay, ok := orchcore.Team(r.dm.Deps).(orchcore.TaskRelay)
	if !ok {
		t.Fatal("no task relay")
	}
	handled, err := relay.DeliverTask(ctx, r.dm.Deps.Projects.Local(child.Address), j.Remote.Project.ID, "solver", "comment", "", it.ID, "task result")
	if !handled || !api.IsCode(err, api.CodeQueued) {
		t.Fatalf("delivery handled=%v err=%v", handled, err)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitReachable(t, r, true)
	waitDrained(t, r)
	comments, err := orchclient.Comments(ctx, at(a.owner, "harbor", ""), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "task result" {
		t.Fatalf("comments: %+v", comments)
	}
}

func TestTaskDeliveryQueuesReplicaMail(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	parent := register(t, r, "harbor/solver", "task-parent")
	child := register(t, r, "harbor/solver", "task-child")
	heartbeat(t, r)
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	relay := orchcore.Team(r.dm.Deps).(orchcore.TaskRelay)
	handled, err := relay.DeliverTask(ctx, r.dm.Deps.Projects.Local(child.Address), j.Remote.Project.ID, "solver", "mail", parent.Address, "", "task finished")
	if !handled || !api.IsCode(err, api.CodeQueued) {
		t.Fatalf("delivery handled=%v err=%v", handled, err)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitReachable(t, r, true)
	waitDrained(t, r)
	ms, err := orchclient.Inbox(ctx, at(r.owner, "harbor", parent.Address), orchapi.InboxQuery{Project: "harbor"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range ms {
		if m.Body == "task finished" && m.From == child.Address {
			found = true
		}
	}
	if !found {
		t.Fatalf("inbox: %+v", ms)
	}
}
