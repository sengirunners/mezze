package team

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// An authority older than read marks answers 404: a member's merged list
// keeps its own marks and names that authority in skipped, never an error
// on the whole list.
func TestReadMarksFromAnOldAuthorityAreSkipped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/mail/read-marks" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	pid := ids.New()
	x := &remote{R: api.Remote{Project: api.ProjectRef{ID: pid, Slug: "harbor"}, Authority: "mini", URL: server.URL, State: api.RemoteOK}, HC: server.Client()}
	d := orderingDeps(t, "replica")
	link := teamlink.New(d)
	link.Add(x)
	s := New(d, link)
	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteJSON(w, http.StatusOK, orchapi.ReadMarkList{Marks: []orchapi.ReadMark{{Project: "home", Conv: "c", UptoMs: 7}}})
	})
	req := httptest.NewRequest(http.MethodGet, "/v0/mail/read-marks", nil)
	req = req.WithContext(core.WithSubject(core.WithLocal(req.Context()), &authz.Subject{Via: authz.ViaLocal, Address: "human:owner", Scope: authz.All()}))
	rec := httptest.NewRecorder()
	if !s.Proxy(rec, req, local) {
		t.Fatal("GET /v0/mail/read-marks was not proxied to the joined project")
	}
	var got struct {
		Marks   []orchapi.ReadMark `json:"marks"`
		Skipped []api.WorkSkipped  `json:"skipped"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(got.Marks) != 1 || got.Marks[0].Conv != "c" || len(got.Skipped) != 1 || got.Skipped[0].Project != pid {
		t.Fatalf("merged %s", rec.Body)
	}
}
