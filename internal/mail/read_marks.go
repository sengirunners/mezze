package mail

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Read marks (SPEC 9 "Read marks"): a person's "read up to here" per
// conversation, in each project's database. The person is the caller
// (callerOf), never the request, so a person reads and moves only their own.
// A mark takes no mail: it is not an ack, and only clients read it.

// handleReadMarks is GET /v0/mail/read-marks[?project=]: the caller's marks
// across ReadScope. A session has none.
func (s *Service) handleReadMarks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out := orchapi.ReadMarkList{Marks: []orchapi.ReadMark{}}
	c := s.callerOf(ctx, core.SubjectOf(ctx))
	if !c.human() {
		respond(w, out, nil)
		return
	}
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		ms, err := s.readMarks(ctx, p.DB, p.ID, c.addr.String())
		if err != nil {
			api.WriteError(w, err)
			return
		}
		out.Marks = append(out.Marks, ms...)
	}
	respond(w, out, nil)
}

// handlePutReadMarks is PUT /v0/mail/read-marks?project=: the caller's marks
// in that project move up (never back); it answers all of them there.
func (s *Service) handlePutReadMarks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.URL.Query().Get("project")
	if name == "" {
		// A team member's proxy routes by ?project= (or Zm-Project), never by a body.
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "read marks are set in one project: name it with ?project=<slug>"))
		return
	}
	sc, err := s.d.ReadScope(ctx, name)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	ps := s.d.Store.Projects(sc, authz.Read)
	if len(ps) != 1 {
		api.WriteError(w, api.NotFound("project", name))
		return
	}
	c := s.callerOf(ctx, core.SubjectOf(ctx))
	if !c.human() {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "read marks are a person's (human:<name>); a session keeps none"))
		return
	}
	var req orchapi.ReadMarksRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if err := checkReadMarks(req.Marks); err != nil {
		api.WriteError(w, err)
		return
	}
	me, now := c.addr.String(), store.Millis(s.now())
	err = store.Tx(ctx, ps[0].DB, func(tx *sql.Tx) error {
		for _, m := range req.Marks {
			// A mark past now would hide mail not yet sent.
			upto := min(m.UptoMs, now)
			if _, err := tx.ExecContext(ctx, `INSERT INTO mail_read_marks (identity, conv, upto_ms, updated) VALUES (?, ?, ?, ?)
				ON CONFLICT (identity, conv) DO UPDATE SET upto_ms = excluded.upto_ms, updated = excluded.updated
				WHERE excluded.upto_ms > mail_read_marks.upto_ms`, me, m.Conv, upto, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, err)
		return
	}
	ms, err := s.readMarks(ctx, ps[0].DB, ps[0].ID, me)
	respond(w, orchapi.ReadMarkList{Marks: ms}, err)
}

// checkReadMarks refuses an empty or oversize PUT, an empty or oversize
// conversation key and a negative mark.
func checkReadMarks(ms []orchapi.ReadMarkSet) error {
	if len(ms) == 0 || len(ms) > orchapi.MaxReadMarksPut {
		return api.Errorf(api.CodeBadRequest, "%d marks: a PUT carries 1 to %d", len(ms), orchapi.MaxReadMarksPut)
	}
	for _, m := range ms {
		if m.Conv == "" || len(m.Conv) > orchapi.MaxReadMarkConv {
			return api.Errorf(api.CodeBadRequest, "conv of %d bytes: want 1 to %d", len(m.Conv), orchapi.MaxReadMarkConv)
		}
		if m.UptoMs < 0 {
			return api.Errorf(api.CodeBadRequest, "upto_ms %d: want a time in ms", m.UptoMs)
		}
	}
	return nil
}

// readMarks lists me's marks in one project's database.
func (s *Service) readMarks(ctx context.Context, db *sql.DB, project, me string) ([]orchapi.ReadMark, error) {
	rows, err := db.QueryContext(ctx, `SELECT conv, upto_ms FROM mail_read_marks WHERE identity = ? ORDER BY conv`, me)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slug := s.d.Projects.Slug(project)
	out := []orchapi.ReadMark{}
	for rows.Next() {
		m := orchapi.ReadMark{Project: slug}
		if err := rows.Scan(&m.Conv, &m.UptoMs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
