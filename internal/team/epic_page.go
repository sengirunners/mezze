package team

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// forwardEpicPage forwards GET /v0/work/page?epic= and refuses an answer
// that does not echo the epic: an authority that predates epic= ignores it
// and pages the whole project, which must never pass for the epic's items.
// Such an answer becomes an empty page naming the authority in skipped.
func (s *Service) forwardEpicPage(w http.ResponseWriter, r *http.Request, x *remote, area string) {
	resp, err := s.ForwardResp(r, x, area, true, nil)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.relay(w, r, resp)
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "reading the authority's answer: %v", err))
		return
	}
	var p orchapi.WorkPage
	if json.Unmarshal(b, &p) == nil && p.Epic != "" && p.Epic == strings.TrimSpace(r.URL.Query().Get("epic")) {
		if v := resp.Header.Get("Content-Type"); v != "" {
			w.Header().Set("Content-Type", v)
		}
		_, _ = w.Write(b)
		return
	}
	snap := x.Snapshot()
	api.WriteJSON(w, http.StatusOK, orchapi.WorkPage{Items: []orchapi.WorkItem{}, Cursors: []string{}, Skipped: []api.WorkSkipped{{Project: snap.Project.ID,
		Source: "authority", Error: snap.Project.Slug + ": the authority " + snap.Authority + " ignores epic= (its zm predates it): update zm there"}}})
}
