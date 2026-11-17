package wiring

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/swap"
)

// fakeDaemon answers the probe's reads like a daemon serving the joined
// project harbor (authority project 01JHARBOR), or like an older
// build that could not: its lists drop harbor and reads of it say
// `no project "harbor"`, while GET /v0/team/remotes still lists it.
func fakeDaemon(serves bool) http.Handler {
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) { api.WriteJSON(w, http.StatusOK, v) }
	noProject := func(w http.ResponseWriter, r *http.Request) bool {
		if !serves && r.URL.Query().Get("project") == "harbor" {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "no project %q", "harbor"))
			return true
		}
		return false
	}
	mux.HandleFunc("GET /v0/health", func(w http.ResponseWriter, _ *http.Request) { j(w, api.Health{OK: true, Version: "v", Commit: "c"}) })
	mux.HandleFunc("GET /v0/projects", func(w http.ResponseWriter, _ *http.Request) {
		ps := []api.Project{{ID: "01JSOLO", Slug: "solo"}}
		if serves {
			ps = append(ps, api.Project{ID: "01JHARBOR", Slug: "harbor"})
		}
		j(w, api.ProjectList{Projects: ps})
	})
	mux.HandleFunc("GET /v0/team/remotes", func(w http.ResponseWriter, _ *http.Request) {
		j(w, api.RemoteList{Remotes: []api.Remote{{Project: api.ProjectRef{ID: "01JHARBOR", Slug: "harbor"}, State: "ok"}}})
	})
	mux.HandleFunc("GET /v0/issues/projects", func(w http.ResponseWriter, _ *http.Request) {
		ps := []orchapi.IssueProjectInfo{}
		if serves {
			ps = append(ps, orchapi.IssueProjectInfo{Project: "01JHARBOR", Slug: "harbor", Prefix: "harbor"})
		}
		j(w, orchapi.IssueProjects{Projects: ps})
	})
	mux.HandleFunc("GET /v0/issues", func(w http.ResponseWriter, r *http.Request) {
		if !noProject(w, r) {
			j(w, orchapi.IssueList{Issues: []orchapi.Issue{}})
		}
	})
	mux.HandleFunc("GET /v0/work", func(w http.ResponseWriter, r *http.Request) {
		if !noProject(w, r) {
			j(w, orchapi.WorkList{Items: []orchapi.WorkItem{}})
		}
	})
	mux.HandleFunc("GET /v0/work/sources", func(w http.ResponseWriter, _ *http.Request) {
		j(w, orchapi.WorkSourceList{Sources: []orchapi.WorkSource{}})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		mux.ServeHTTP(w, r)
	})
}

// TestProbeCatchesNoProjectHarbor replays that older build through Probe: the
// new build lists harbor as joined but serves none of it, and drops it
// from /v0/projects; the swap must see that as a regression.
func TestProbeCatchesNoProjectHarbor(t *testing.T) {
	ctx := context.Background()
	before := swap.Probe(ctx, client.WithHandler(fakeDaemon(true)), Probes...)
	if f := swap.Failures(before); len(f) != 0 {
		t.Fatalf("the good daemon fails: %v", f)
	}
	after := swap.Probe(ctx, client.WithHandler(fakeDaemon(false)), Probes...)
	got := strings.Join(swap.Regressions(before, after), "\n")
	for _, want := range []string{"issues harbor: served before, missing now", `work harbor: served before, now bad_request: no project "harbor"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %q among the regressions:\n%s", want, got)
		}
	}
	if strings.Contains(got, "solo") {
		t.Fatalf("solo is still served:\n%s", got)
	}
	// No wait fixes it: the bad_request is local, so a swap refuses at once.
	if local, _ := swap.SplitRegressions(before, after); !strings.Contains(strings.Join(local, "\n"), `work harbor`) {
		t.Fatalf("the older build must refuse without the tailnet grace: %v", local)
	}
}

// TestAgentItem: an agent whose binary resolves is
// present even when its auth probe fails; only a missing binary is not.
func TestAgentItem(t *testing.T) {
	no := false
	present := agentItem(orchapi.TaskAgent{Agent: "claude", Authenticated: &no, Reason: "authentication status unavailable or not logged in"})
	if !present.OK || present.Code != "" || !strings.Contains(present.Error, "not logged in") {
		t.Fatalf("auth failing: %+v", present)
	}
	custom := agentItem(orchapi.TaskAgent{Agent: "codex", Available: true, Reason: "custom command: authentication not probed"})
	if !custom.OK || custom.Error != "" {
		t.Fatalf("custom command: %+v", custom)
	}
	missing := agentItem(orchapi.TaskAgent{Agent: "claude", Reason: `exec: "claude": executable file not found in $PATH`})
	if missing.OK || missing.Code != api.CodeUnavailable {
		t.Fatalf("missing: %+v", missing)
	}
	before := api.ServeCheck{Items: []api.ServeItem{{Kind: swap.ItemAgent, Name: "claude", OK: true}}}
	if regs := swap.Regressions(before, api.ServeCheck{Items: []api.ServeItem{present}}); len(regs) > 0 {
		t.Fatalf("auth flap is a regression: %v", regs)
	}
	if regs := swap.Regressions(before, api.ServeCheck{Items: []api.ServeItem{missing}}); len(regs) != 1 {
		t.Fatalf("lost binary: %v", regs)
	}
}
