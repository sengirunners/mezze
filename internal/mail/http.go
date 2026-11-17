package mail

import (
	"net/http"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Routes registers /v0/mail/*. See API.md.
func (s *Service) Routes(r *core.Router) {
	r = r.For("mail")
	r.HandleFunc("POST /v0/mail/imports", core.Authed, "local owner or aware node-bound canonical human self-import; explicit project, WriteTarget and immutable bounded personal mapping", s.handleImport)
	r.HandleFunc("POST /v0/mail", core.Authed, "write on the resolved project/area (WriteTarget); recipient and ref must be visible to the sender", s.handleSend)
	r.HandleFunc("GET /v0/mail", core.Authed, "read: ReadScope(?project=), rows by readWhere (guest areas: restricted mail only to its parties)", s.handleQuery)
	r.HandleFunc("GET /v0/mail/inbox", core.Authed, "read: the caller's mail across ReadScope", s.handleInbox)
	r.HandleFunc("GET /v0/mail/inbox/peek", core.Authed, "read: the caller's mail across ReadScope; marks nothing seen", s.handlePeek)
	r.HandleFunc("GET /v0/mail/sent", core.Authed, "read: mail the caller sent, across ReadScope", s.handleSent)
	r.HandleFunc("GET /v0/mail/read-marks", core.Authed, "read: ReadScope(?project=); only the caller's own marks (identity from the subject, never the request); a session has none", s.handleReadMarks)
	r.HandleFunc("PUT /v0/mail/read-marks", core.Authed, "read on ?project= (required), else not_found; only the caller's own marks; a session is forbidden", s.handlePutReadMarks)
	r.HandleFunc("GET /v0/mail/notice", core.Authed, "read; never an error, \"\" when nothing resolves", s.handleNotice)
	s.triageRoutes(r)
	r.HandleFunc("GET /v0/mail/{id}", core.Authed, "read on the mail's project/area, else not_found", s.handleGet)
	r.HandleFunc("POST /v0/mail/{id}/ack", core.Authed, "write on the mail's area and party to it; needs=human: human callers only; never an open ask's mail, never a for-you item (only its person's Got it)", s.handleAck)
	r.HandleFunc("POST /v0/mail/{id}/got-it", core.Authed, "read on the mail; only the person it is addressed to (human:<name>, never a session) and only a for-you item; else not_found or forbidden", s.handleGotIt)
	r.HandleFunc("POST /v0/mail/{id}/reply", core.Authed, "write on the mail's area and party to it; never an open ask's mail", s.handleReply)
	r.HandleFunc("GET /v0/mail/{id}/thread", core.Authed, "read; mails the caller cannot read are omitted", s.handleThread)
}

func qint(r *http.Request, k string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(k))
	return n
}

func qbool(r *http.Request, k string) bool {
	v := r.URL.Query().Get(k)
	return v == "1" || v == "true"
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, v)
}

func (s *Service) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.SendMailRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	req.ID = core.WriteID(r)
	sub := core.SubjectOf(ctx)
	var (
		t   core.Target
		err error
	)
	if p, moved := s.followMoved(ctx, sub, &req); moved {
		// The issue's project and area answer for it: an area named for the
		// project it left means nothing here.
		t.Project, req.Area = p, ""
	} else if t, err = s.d.WriteTarget(ctx, req.Area, authz.Write); err != nil {
		api.WriteError(w, err)
		return
	}
	m, err := s.send(ctx, sub, t.Project, t.Area, sendOpts{areaExplicit: req.Area != "", direct: true}, req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	s.nudge(ctx, m)
	api.WriteJSON(w, http.StatusOK, s.sentAnswer(ctx, sub, m))
}

func (s *Service) handleQuery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	q := r.URL.Query()
	sc, err := s.d.ReadScope(ctx, q.Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	where, args := "1=1", []any{}
	for _, f := range []struct{ val, sql string }{{q.Get("to"), "(recipient = ? OR to_addr = ?)"}, {q.Get("from"), "sender = ?"}} {
		if f.val == "" {
			continue
		}
		a := canon(f.val)
		if !s.visibleAddr(ctx, sub, a) { // a hidden session's mail is not a thing you can probe for
			respond(w, orchapi.MailList{Mail: []orchapi.Mail{}}, nil)
			return
		}
		where += " AND " + f.sql
		args = append(args, a)
		if f.sql[0] == '(' {
			args = append(args, a)
		}
	}
	if v := q.Get("ref"); v != "" {
		where += " AND ref = ?"
		args = append(args, canon(v))
	}
	if v := q.Get("needs"); v != "" {
		where += " AND needs = ?"
		args = append(args, v)
	}
	if qbool(r, "open") {
		where += " AND acked_at IS NULL"
	}
	for _, f := range []struct{ key, sql string }{{"since", " AND created >= ?"}, {"until", " AND created <= ?"}} {
		if v := q.Get(f.key); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				api.WriteError(w, api.Errorf(api.CodeBadRequest, "%s is a time like 2026-11-14T00:00:00Z", f.key))
				return
			}
			where += f.sql
			args = append(args, store.Millis(t))
		}
	}
	rs, err := s.query(ctx, sc, sub.Address, limitOf(qint(r, "limit")), func(scoped) (string, []any, bool) { return where, args, true })
	if err != nil {
		api.WriteError(w, err)
		return
	}
	// A ref filter must not reveal mail about an item in an area the caller cannot see.
	out := make([]orchapi.Mail, 0, len(rs))
	for _, m := range rs {
		if mm := s.redact(ctx, sub, m); q.Get("ref") == "" || mm.Ref != "" {
			out = append(out, mm)
		}
	}
	respond(w, orchapi.MailList{Mail: out}, nil)
}

func (s *Service) handleInbox(w http.ResponseWriter, r *http.Request) { s.serveInbox(w, r, true) }

// handlePeek is the inbox for a poll (a hook's wake): the same rows, and the
// announcements in them stay unseen. Its own path, so a daemon too old to
// know it answers not_found rather than marking them.
func (s *Service) handlePeek(w http.ResponseWriter, r *http.Request) { s.serveInbox(w, r, false) }

func (s *Service) serveInbox(w http.ResponseWriter, r *http.Request, mark bool) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	c := s.callerOf(ctx, sub)
	box := r.URL.Query().Get("box")
	if box != "" && box != orchapi.InboxBoxReports {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "box %q: want reports, or none for the inbox", box))
		return
	}
	rs, err := s.box(ctx, c, sc, qbool(r, "all"), box == orchapi.InboxBoxReports, limitOf(qint(r, "limit")))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if mark {
		s.markSeen(ctx, c, rs)
	}
	s.groupByProject(rs)
	respond(w, orchapi.MailList{Mail: s.redactAll(ctx, sub, rs)}, nil)
}

func (s *Service) handleSent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	rs, err := s.query(ctx, sc, sub.Address, limitOf(qint(r, "limit")), func(scoped) (string, []any, bool) {
		return "sender = ?", []any{sub.Address}, sub.Address != ""
	})
	respond(w, orchapi.MailList{Mail: s.redactAll(ctx, sub, rs)}, err)
}

// handleNotice never fails: hooks print whatever line comes back.
func (s *Service) handleNotice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n := orchapi.Notice{}
	if sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project")); err == nil {
		read := s.notice
		if r.URL.Query().Get("once") == "1" {
			read = s.hookNotice
		}
		if got, err := read(ctx, core.SubjectOf(ctx), sc); err == nil {
			n = got
		} else {
			s.d.Log.Warn("mail notice", "err", err)
		}
	}
	api.WriteJSON(w, http.StatusOK, n)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	m, _, err := s.find(ctx, sc, sub.Address, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	respond(w, s.redact(ctx, sub, m), nil)
}

func (s *Service) handleAck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	m, db, err := s.find(ctx, sc, sub.Address, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	c := s.callerOf(ctx, sub)
	if err := s.mayAck(ctx, c, m); err != nil {
		api.WriteError(w, err)
		return
	}
	if err := s.refuseOpenAsk(ctx, m); err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.ack(ctx, c, db, m)
	respond(w, a, err)
}

func (s *Service) handleReply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	var req orchapi.ReplyRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	req.ID = core.WriteID(r)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	parent, _, err := s.find(ctx, sc, sub.Address, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	c := s.callerOf(ctx, sub)
	if err := sub.Check(authz.Write, authz.Object{Project: parent.Project, Area: parent.Area}, "mail", parent.ID); err != nil {
		api.WriteError(w, err)
		return
	}
	if !s.party(c, parent) {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "you are not a party to mail %s", short(parent.ID)))
		return
	}
	if err := s.refuseOpenAsk(ctx, parent); err != nil {
		api.WriteError(w, err)
		return
	}
	m, err := s.reply(ctx, c, parent, req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	respond(w, s.redact(ctx, sub, m), nil)
}

func (s *Service) handleThread(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	m, _, err := s.find(ctx, sc, sub.Address, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	rs, err := s.thread(ctx, sc, sub.Address, m)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	respond(w, orchapi.MailList{Mail: s.redactAll(ctx, sub, rs)}, nil)
}

// canon canonicalises an address for filtering; unparsable input is kept.
func canon(v string) string {
	if a, err := api.ParseAddress(v); err == nil {
		return a.String()
	}
	return v
}
