package approvals

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

func (s *Service) handleAsks(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if r.URL.Query().Has("q") {
		s.handleSimilar(w, r, sub, sc, r.URL.Query().Get("q"))
		return
	}
	where, args := sc.SQL("a.project", "a.area", authz.Read)
	if v := r.URL.Query().Get("open"); v == "1" || v == "true" {
		where += " AND d.ask IS NULL AND a.withdrawn IS NULL"
	}
	// A held ask is its asker's, and the project's owners' while the judge has
	// not checked it (marked not_checked), until the judge passes it.
	hv, hargs := heldVisible(sc, sub.Address, false)
	where += hv
	args = append(args, hargs...)
	// Paging (zm measures reads every card): before_id is the last page's
	// Next; limit defaults to 200 and tops at maxAskPage.
	page := askPage(qintOf(r, "limit"))
	if v := r.URL.Query().Get("before_id"); v != "" {
		where += " AND a.id < ?"
		args = append(args, strings.ToUpper(strings.TrimSpace(v)))
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE `+where+` ORDER BY a.id DESC LIMIT `+strconv.Itoa(page), args...)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer rows.Close()
	var asks []orchapi.Ask
	var last string
	var scanned int
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		last, scanned = a.ID, scanned+1
		if mayRead(sc, sub.Address, a) {
			asks = append(asks, a)
		}
	}
	if err := rows.Err(); err != nil {
		api.WriteError(w, err)
		return
	}
	rows.Close() // before view, which reads the registry again (see inbox)
	out := orchapi.AskList{Asks: []orchapi.Ask{}}
	if scanned == page {
		out.Next = last // the page was full: older cards may follow, even if none of these were readable
	}
	for _, a := range asks {
		out.Asks = append(out.Asks, s.withMeasures(ctx, s.view(ctx, sub, a)))
	}
	api.WriteJSON(w, http.StatusOK, out)
}

// OpenAsk reports whether mail id of project is an ask no approver has
// decided yet (mail.AskChecker: the mail module refuses ack and reply on it
// over HTTP; only the decision answers it).
func (s *Service) OpenAsk(ctx context.Context, project, id string) (bool, error) {
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*)`+askFrom+` WHERE a.id = ? AND a.project = ? AND d.ask IS NULL AND a.withdrawn IS NULL`,
		id, project).Scan(&n)
	return n > 0, err
}

// HeldAsks maps each open ask of project held for an outside party (ask.for)
// to its party's name (mail.HeldChecker): its mail waits on that party, not
// on a person here, so it neither holds its asker at needs_human, pushes,
// nor counts as anyone's unread mail.
func (s *Service) HeldAsks(ctx context.Context, project string) (map[string]string, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT a.id, json_extract(a.for_group, '$.group')`+askFrom+
		` WHERE a.project = ? AND a.for_group != '' AND d.ask IS NULL AND a.withdrawn IS NULL`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var group sql.NullString
		if err := rows.Scan(&id, &group); err != nil {
			return nil, err
		}
		out[id] = first(group.String, "an outside party")
	}
	return out, rows.Err()
}

// IsAsk reports whether mail id of project is an ask, open or decided: its
// mail is a decision, never a for-you item (SPEC 9 "For you").
func (s *Service) IsAsk(ctx context.Context, project, id string) (bool, error) {
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM asks WHERE id = ? AND project = ?`, id, project).Scan(&n)
	return n > 0, err
}

func (s *Service) ask(ctx context.Context, id string) (orchapi.Ask, error) {
	a, err := scanAsk(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+askCols+askFrom+` WHERE a.id = ?`, strings.ToUpper(strings.TrimSpace(id))))
	if errors.Is(err, sql.ErrNoRows) {
		return a, api.NotFound("ask", id)
	}
	return a, err
}

func (s *Service) handleGetAsk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	id := r.PathValue("id")
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	reason, held := "", false
	if err == nil {
		reason, held = s.heldReason(ctx, a.ID)
	}
	if held && a.AskedBy != sub.Address { // a held ask is its asker's, and an owner's while unchecked
		if _, unchecked := s.heldUnchecked(ctx, a.ID); !unchecked || !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}) {
			err = api.NotFound("ask", id)
		}
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	v := s.withMeasures(ctx, s.view(ctx, sub, a))
	if held {
		v.Note = heldNote(reason)
	}
	api.WriteJSON(w, http.StatusOK, v)
}

// maxAskPage bounds one GET /v0/asks page; the default stays 200.
const maxAskPage = 1000

func askPage(n int) int {
	switch {
	case n <= 0:
		return 200
	case n > maxAskPage:
		return maxAskPage
	}
	return n
}

func qintOf(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return n
}
