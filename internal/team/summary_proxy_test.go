package team_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestSummaryArchiveAndScheduleStayAtAuthority(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	owner := at(a.owner, "harbor", "")
	mustf(orchclient.CreateWork(ctx, owner, orchapi.CreateWorkRequest{Title: "Authority work digest", Area: "solver"}))(t)
	r, j := replica(t, tn, a, "summary-mac", "nSUMMARY", "harbor", "bea", "member")
	c := at(r.owner, "harbor", "")
	if _, err := orchclient.SetSummaryConfig(ctx, c, "harbor", orchapi.SummaryConfig{Enabled: true, Hour: 7, Timezone: "UTC"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("member configured owner schedule: %v", err)
	}
	config, err := orchclient.SetSummaryConfig(ctx, owner, "harbor", orchapi.SummaryConfig{Enabled: false, Hour: 7, Timezone: "UTC"})
	if err != nil || config.Project != j.Remote.Project.ID {
		t.Fatalf("remote admin schedule: %+v %v", config, err)
	}
	first, err := orchclient.GenerateSummary(ctx, c, "harbor", "")
	if err != nil || !strings.Contains(first.Body, "Authority work digest") {
		t.Fatalf("authority digest: %+v %v", first, err)
	}
	second, err := orchclient.GenerateSummary(ctx, owner, "harbor", first.Day)
	if err != nil || first.ID != second.ID {
		t.Fatalf("forked archive: %+v %v", second, err)
	}
	list, err := orchclient.Summaries(ctx, c, "harbor", "", 30)
	if err != nil || len(list.Summaries) != 1 || list.Summaries[0].ID != first.ID {
		t.Fatalf("remote history: %+v %v", list, err)
	}
	var localRows int
	if err = r.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM project_summaries`).Scan(&localRows); err != nil || localRows != 0 {
		t.Fatalf("replica forked summaries: %d %v", localRows, err)
	}
	g, _ := replica(t, tn, a, "summary-guest", "nSUMMARYG", "harbor/solver", "reader", "guest")
	if _, err = orchclient.Summaries(ctx, g.owner, "harbor", "", 30); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("area guest read projectwide summary: %v", err)
	}
}
