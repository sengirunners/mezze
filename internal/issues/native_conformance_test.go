package issues_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/mezze/internal/work/conformance"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// The native adapter (zm's own issues behind zm work) meets the same zm/v0
// adapter contract as an executable adapter: the conformance suite runs
// against it in process, reads and writes.
func TestNativeAdapterConformance(t *testing.T) {
	ctx := context.Background()
	svc, c, pid := issuesService(t)
	c.Project, c.ProjectFrom = "bx", "flag"
	it, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "conformance target"})
	if err != nil {
		t.Fatal(err)
	}
	rt := func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
		var req orchapi.AdapterRequest
		if err := json.Unmarshal(in, &req); err != nil {
			return orchapi.AdapterResponse{OK: false, Error: api.Errorf(api.CodeBadRequest, "bad request: %v", err)}, nil
		}
		var out json.RawMessage
		if err := svc.NativeCall(ctx, pid, req, &out); err != nil {
			var e *api.Error
			if !errors.As(err, &e) {
				e = api.Errorf(api.CodeInternal, "%v", err)
			}
			return orchapi.AdapterResponse{Error: e}, nil
		}
		return orchapi.AdapterResponse{OK: true, Result: out}, nil
	}
	for _, write := range []bool{false, true} {
		res := conformance.Run(ctx, rt, conformance.Options{ID: "issue:" + it.ID, Write: write})
		if conformance.Failed(res) > 0 {
			t.Errorf("write=%v:\n%s", write, conformance.Format(res))
		}
	}
}

// issuesService is a full daemon with one project, bx, and its issues module.
func issuesService(t *testing.T) (*issues.Service, *client.Client, string) {
	t.Helper()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	owner := client.WithHandler(dm.SocketHandler())
	p, err := owner.CreateProject(context.Background(), api.NewProjectRequest{Slug: "bx"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*issues.Service); ok {
			return s, owner, p.ID
		}
	}
	t.Fatal("no issues module")
	return nil, nil, ""
}
