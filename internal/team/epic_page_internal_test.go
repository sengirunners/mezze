package team

import (
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// Review finding: an authority that predates epic= ignores it and pages
// the whole project. A member's socket refuses that page (empty, the
// authority named in skipped) and passes an answer that echoes the epic.
func TestEpicPageFromAnOldAuthorityIsRefused(t *testing.T) {
	var echo atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/work/page" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		p := orchapi.WorkPage{Items: []orchapi.WorkItem{{ID: "bd:solver-x"}}, Cursors: []string{"c"}}
		if echo.Load() {
			p.Epic = r.URL.Query().Get("epic")
		}
		api.WriteJSON(w, http.StatusOK, p)
	}))
	t.Cleanup(server.Close)
	pid := ids.New()
	x := &remote{R: api.Remote{Project: api.ProjectRef{ID: pid, Slug: "harbor"}, Authority: "mini", URL: server.URL, State: api.RemoteOK}, HC: server.Client()}
	d := orderingDeps(t, "replica")
	s := New(d, teamlink.New(d))
	get := func() orchapi.WorkPage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v0/work/page?project=harbor&epic=bd:solver-ep", nil)
		req = req.WithContext(core.WithSubject(req.Context(), &authz.Subject{}))
		rec := httptest.NewRecorder()
		s.forwardEpicPage(rec, req, x, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var p orchapi.WorkPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := get(); len(p.Items) != 0 || len(p.Skipped) != 1 || p.Skipped[0].Project != pid || p.Epic != "" {
		t.Fatalf("an old authority's unfiltered page passed: %+v", p)
	}
	echo.Store(true)
	if p := get(); len(p.Items) != 1 || len(p.Skipped) != 0 || p.Epic != "bd:solver-ep" {
		t.Fatalf("an echoed page refused: %+v", p)
	}
}
