package summaries

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

func (s *Service) Routes(r *core.Router) {
	r = r.For("summaries")
	r.HandleFunc("GET /v0/summaries", core.Authed, "read: explicit whole-project read; area-only readers see missing", s.list)
	r.HandleFunc("POST /v0/summaries", core.Authed, "operate: explicit whole-project operate, bounded work digest", s.create)
	r.HandleFunc("GET /v0/summaries/config", core.Authed, "read: explicit whole-project read", s.getConfig)
	r.HandleFunc("PUT /v0/summaries/config", core.Authed, "admin: authenticated human owner, explicit whole project", s.putConfig)
}

func (s *Service) project(ctx context.Context, raw string, action authz.Action) (api.Project, error) {
	if raw == "" {
		return api.Project{}, api.Errorf(api.CodeBadRequest, "project is required")
	}
	sc, err := s.d.ReadScope(ctx, raw)
	if err != nil {
		return api.Project{}, err
	}
	p, err := s.d.Projects.Get(sc, raw)
	if err != nil {
		return api.Project{}, err
	}
	sub := core.SubjectOf(ctx)
	if !sc.WholeProject(p.ID) || sub.Check(action, authz.Object{Project: p.ID}, "project", raw) != nil {
		return api.Project{}, api.NotFound("project", raw)
	}
	if action != authz.Read {
		writeCtx := core.WithResolveInput(ctx, project.ResolveInput{Explicit: raw, ExplicitFrom: project.RuleFlag})
		target, e := s.d.WriteTarget(writeCtx, "", action)
		if e != nil {
			return api.Project{}, e
		}
		if target.Project.ID != p.ID || target.Area != "" {
			return api.Project{}, api.NotFound("project", raw)
		}
	}
	return p, nil
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		api.WriteError(w, err)
	} else {
		api.WriteJSON(w, http.StatusOK, v)
	}
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	p, err := s.project(r.Context(), r.URL.Query().Get("project"), authz.Read)
	if err != nil {
		reply(w, nil, err)
		return
	}
	q := r.URL.Query().Get("q")
	if len(q) > 512 || strings.ContainsAny(q, "\x00\r\n") {
		reply(w, nil, api.Errorf(api.CodeBadRequest, "q must be one line of at most 512 bytes"))
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 100 {
			reply(w, nil, api.Errorf(api.CodeBadRequest, "limit must be 1–100"))
			return
		}
	}
	rows, err := s.d.Store.Registry().QueryContext(r.Context(), `SELECT data FROM project_summaries WHERE project=? AND (?='' OR instr(lower(data),lower(?))>0) ORDER BY day DESC,id DESC LIMIT ?`, p.ID, q, q, limit)
	if err != nil {
		reply(w, nil, err)
		return
	}
	out := orchapi.SummaryList{Summaries: []orchapi.ProjectSummary{}}
	for rows.Next() {
		var raw string
		var v orchapi.ProjectSummary
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			break
		}
		out.Summaries = append(out.Summaries, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	reply(w, out, err)
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	p, err := s.project(r.Context(), r.URL.Query().Get("project"), authz.Operate)
	if err != nil {
		reply(w, nil, err)
		return
	}
	var req orchapi.SummaryRequest
	if err = api.DecodeJSON(r, &req); err != nil {
		reply(w, nil, err)
		return
	}
	c, err := s.config(r.Context(), p.ID)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if req.Day == "" {
		loc, _ := time.LoadLocation(c.Timezone)
		req.Day = s.now().In(loc).Format(time.DateOnly)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := s.generate(ctx, core.SubjectOf(ctx), c, req.Day)
	reply(w, out, err)
}

func (s *Service) getConfig(w http.ResponseWriter, r *http.Request) {
	p, err := s.project(r.Context(), r.URL.Query().Get("project"), authz.Read)
	if err != nil {
		reply(w, nil, err)
		return
	}
	c, err := s.config(r.Context(), p.ID)
	reply(w, c, err)
}

func (s *Service) putConfig(w http.ResponseWriter, r *http.Request) {
	sub := core.SubjectOf(r.Context())
	if sub.Session != "" || sub.Identity.Kind != api.IdentityHuman {
		reply(w, nil, api.Errorf(api.CodeForbidden, "a human project owner configures summaries"))
		return
	}
	p, err := s.project(r.Context(), r.URL.Query().Get("project"), authz.Admin)
	if err != nil {
		reply(w, nil, err)
		return
	}
	var c orchapi.SummaryConfig
	if err = api.DecodeJSON(r, &c); err != nil {
		reply(w, nil, err)
		return
	}
	c.Project = p.ID
	c.LastAttempt, c.LastGenerated, c.LastError = nil, nil, ""
	if err = validateConfig(&c); err != nil {
		reply(w, nil, err)
		return
	}
	raw, _ := json.Marshal(c)
	_, err = s.d.Store.Registry().ExecContext(r.Context(), `INSERT INTO summary_configs(project,config) VALUES(?,?) ON CONFLICT(project) DO UPDATE SET config=excluded.config`, p.ID, string(raw))
	if err == nil && s.d.Events != nil {
		// Status and config edits use the same content-free refresh hint;
		// a disabled schedule must update other open readers too.
		if _, hintErr := s.d.Events.Publish(r.Context(), events.Input{Type: orchapi.EventSummaryScheduleChanged, Project: p.ID, Data: orchapi.ProjectHint{Project: p.ID}}); hintErr != nil && s.d.Log != nil {
			s.d.Log.Warn("summary schedule refresh hint unavailable", "project", p.ID)
		}
	}
	reply(w, c, err)
}
