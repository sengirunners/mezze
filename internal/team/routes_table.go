package team

// The orchestration routes' rows for teamlink's proxy (SPEC 17.3): how a
// member treats each work, lease, mail, session, ask, issue, summary and
// door call to a joined project, and whether a forwarded one may be sent
// twice (teamlink routes.go). Compute registers its own rows.
// The mail notice is never forwarded (hooks stay local; the team share
// comes from the presence cache), nor any session route but reading a list
// or another machine's session (sessions on a replica are local), nor
// decide on an ask (the socket holds no decide right, here or there).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	tl "github.com/sengirunners/potluck/kit/teamlink"
)

// routeRows are team's rows (registered in New).
func (s *Service) routeRows() []tl.RouteRule {
	machine := s.d.Config.Machine
	hedged := func(p string, k tl.Kind, h *tl.RouteHook) tl.RouteRule {
		return tl.RouteRule{Pattern: p, Kind: k, Hedge: tl.Hedged, Hook: h}
	}
	write := func(p string, k tl.Kind, h *tl.RouteHook) tl.RouteRule {
		return tl.RouteRule{Pattern: p, Kind: k, Hedge: tl.Write, Hook: h}
	}
	once := func(p string, k tl.Kind, why string, h *tl.RouteHook) tl.RouteRule {
		return tl.RouteRule{Pattern: p, Kind: k, Hedge: tl.Once, Why: why, Hook: h}
	}
	list := func(key string) *tl.RouteHook { return &tl.RouteHook{List: &tl.ListMerge{Key: key}} }
	const (
		L, I, W = tl.KindList, tl.KindItem, tl.KindWrite
	)

	auto := &tl.RouteHook{NoCopy: true}
	// Native issues have no copy: every call asks the authority. One that
	// the named project answers 404 may have moved (forwardIssue).
	issue := &tl.RouteHook{NoCopy: true}
	issueItem := &tl.RouteHook{NoCopy: true, Named: func(w http.ResponseWriter, r *http.Request, x *remote, area string, _ bool, _ []byte) bool {
		s.forwardIssue(w, r, x, area)
		return true
	}}
	// A work read follows an id that moved (moved_work.go).
	wread := &tl.RouteHook{
		Named: func(w http.ResponseWriter, r *http.Request, x *remote, area string, _ bool, _ []byte) bool {
			if _, _, ok := workRead(r); !ok {
				return false
			}
			s.forwardWorkRead(w, r, x, area)
			return true
		},
		Missing: func(w http.ResponseWriter, r *http.Request) bool { return s.followMovedRead(w, r, nil) },
	}
	// A mail id names its own project: $ZM_PROJECT narrows neither ack,
	// reply nor thread (ReadScope ignores it here too), -P does. A farm job
	// runs with ZM_PROJECT=acme and is mailed in zm.
	mailItem := &tl.RouteHook{IDNamesProject: true}
	// release --force is the authority's: it needs operate there, from its
	// own socket.
	release := &tl.RouteHook{Refuse: func(r *http.Request, body []byte, snap api.Remote) error {
		if isRelease(r) && forced(body) {
			return api.Errorf(api.CodeForbidden, "release --force is the authority's: it needs operate there, from its own socket").
				WithHint("ask an owner on " + snap.Authority + " to run zm release --force")
		}
		return refuseNoQueueForward(r, body, snap)
	}}
	// A no-queue claim or lease is never forwarded (cancel.go).
	noQueueOnly := &tl.RouteHook{Refuse: refuseNoQueueForward}
	// Another machine's session (sessions on a replica are local); its
	// structured session's calls go through its authority.
	otherSession := &tl.RouteHook{When: s.otherSession}
	relayed := &tl.RouteHook{When: func(r *http.Request) bool { return relayPath(r.Method, r.URL.Path, machine) }, Unnamed: s.relayRoute}
	// Shared sessions (SPEC 17.10): the authority logs and routes them.
	prompt := &tl.RouteHook{Unnamed: s.promptRoute}

	sessions := list("sessions")
	sessions.List.Share = func(_, theirs map[string]json.RawMessage) {
		// This machine's own sessions are answered here (they are local);
		// the authority's copy of them is presence.
		theirs["sessions"] = dropMachine(theirs["sessions"], machine)
	}
	machines := list("machines")
	machines.List.Share = func(_, theirs map[string]json.RawMessage) {
		theirs["machines"] = dropMachine(theirs["machines"], machine) // this machine's numbers are answered here
	}
	machines.List.Done = func(merged map[string]json.RawMessage, sk []api.WorkSkipped) []api.WorkSkipped {
		merged["machines"] = dedupMachines(merged["machines"])
		return sk
	}
	issues := list("issues")
	issues.NoCopy = true
	issues.List.Share = func(merged, theirs map[string]json.RawMessage) { mergeIssuePage(merged, theirs) }
	issueProjects := list("projects")
	issueProjects.NoCopy = true
	// Read marks have no event, so no copy holds them: the authority answers.
	marks := list("marks")
	marks.NoCopy = true
	sources := list("sources")
	sources.List.Done = func(merged map[string]json.RawMessage, sk []api.WorkSkipped) []api.WorkSkipped {
		// An authority that did not answer is a source in error.
		var add []orchapi.WorkSource
		for _, x := range sk {
			add = append(add, orchapi.WorkSource{Project: x.Project, Name: "authority", Prefix: "authority", Verbs: []string{}, Error: x.Error})
		}
		if len(add) > 0 {
			js, _ := json.Marshal(add)
			merged["sources"] = tl.AppendArrays(merged["sources"], js)
		}
		return nil
	}

	return []tl.RouteRule{
		// Work and leases.
		hedged("GET /v0/work", L, list("items")),
		hedged("GET /v0/work/sources", L, sources),
		hedged("GET /v0/work/inflight", L, list("inflight")),
		hedged("GET /v0/work/rollup", L, list("rollups")),
		hedged("GET /v0/leases", L, list("leases")),
		write("POST /v0/work", W, &tl.RouteHook{Named: s.createNamed}),
		write("POST /v0/leases", W, noQueueOnly),
		hedged("GET /v0/work/{id}", I, wread),
		hedged("GET /v0/work/{id}/holder", I, wread),
		hedged("GET /v0/work/{id}/comments", I, wread),
		hedged("GET /v0/work/labels", I, nil),
		hedged("GET /v0/work/page", I, &tl.RouteHook{Named: func(w http.ResponseWriter, r *http.Request, x *remote, area string, _ bool, _ []byte) bool {
			if r.URL.Query().Get("epic") == "" {
				return false
			}
			s.forwardEpicPage(w, r, x, area)
			return true
		}}),
		write("POST /v0/work/{id}/claim", I, noQueueOnly),
		write("POST /v0/work/{id}/comment", I, nil),
		write("POST /v0/work/{id}/link", I, nil),
		write("POST /v0/work/{id}/release", I, release),
		write("POST /v0/work/{id}/status", I, nil),
		write("DELETE /v0/leases/{key}", I, release),

		// Mail.
		hedged("GET /v0/mail", L, list("mail")),
		once("GET /v0/mail/inbox", L, "marks the mail it answers seen: a second attempt would answer without it", list("mail")),
		hedged("GET /v0/mail/inbox/peek", L, list("mail")), // marks nothing: safe to hedge
		hedged("GET /v0/mail/sent", L, list("mail")),
		{Pattern: "GET /v0/mail/notice", Kind: tl.KindLocal},
		// What a session owes reads this machine's sessions, leases and cards; a
		// joined project's authority is named in its unknown list, never merged.
		{Pattern: "GET /v0/owed", Kind: tl.KindLocal},
		write("POST /v0/mail", W, &tl.RouteHook{Before: func(r *http.Request) (*http.Request, error) {
			if err := s.mailNames(r); err != nil { // names.go
				return nil, err
			}
			return r, s.followMovedMail(r) // moved_work.go
		}}),
		// Imports have no replica sink, cwd or local fallback (import.go).
		write("POST /v0/mail/imports", tl.KindOwn, &tl.RouteHook{Own: func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool { return s.importRoute(w, r) }}),
		hedged("GET /v0/mail/{id}", I, mailItem),
		hedged("GET /v0/mail/{id}/thread", I, mailItem),
		write("POST /v0/mail/{id}/ack", I, mailItem),
		write("POST /v0/mail/{id}/got-it", I, mailItem),
		write("POST /v0/mail/{id}/reply", I, mailItem),
		hedged("GET /v0/mail/read-marks", L, marks),
		write("PUT /v0/mail/read-marks", W, nil), // ?project= names it: the proxy never routes by a body

		// Sessions and machines.
		hedged("GET /v0/sessions", L, sessions),
		hedged("GET /v0/machines", L, machines),
		hedged("GET /v0/sessions/{id}", I, otherSession),
		once("GET /v0/sessions/{id}/prompts", I, "a shared session's prompt log (SPEC 17.10): not a bulk read, left out", prompt),
		write("POST /v0/sessions/{id}/prompt", I, prompt),
		once("GET /v0/sessions/{id}/events", I, "a long poll (wait_s) or a stream of another machine's session through its authority", relayed),
		write("POST /v0/sessions/{id}/turn", I, relayed), // run at most once
		write("POST /v0/sessions/{id}/interrupt", I, relayed),
		write("POST /v0/sessions/{id}/answer", I, relayed),
		write("POST /v0/sessions/{id}/stop", I, relayed), // a session started from another machine, run at most once

		// Asks, decisions, approvals, receipts and aliases. A decision on an
		// ask lives at the project's authority; decide has no row.
		hedged("GET /v0/asks", L, list("asks")),
		hedged("GET /v0/decisions", L, list("decisions")),
		write("POST /v0/asks", W, s.askHook()), // waits out the card judge; a cut answer may have stored it (ask_forward.go)
		hedged("GET /v0/asks/{id}", I, nil),
		write("POST /v0/asks/{id}/answer", I, nil),
		write("POST /v0/asks/{id}/withdraw", I, nil),
		write("POST /v0/asks/{id}/action/confirm", I, nil),                                        // an owner saw how an approved action went (SPEC 16)
		write("POST /v0/asks/{id}/action/retry", I, nil),                                          // the identical approved action again, after it never started (SPEC 16)
		write("POST /v0/asks/{id}/step", I, nil),                                                  // the person did their step: the asker runs the check (SPEC 16)
		write("POST /v0/asks/{id}/check", I, nil),                                                 // the asker's verify check of approved work (SPEC 16)
		write("POST /v0/asks/{id}/choose", I, nil),                                                // the owner's decision: one per ask, never sent twice
		write("POST /v0/asks/{id}/undo", I, nil), write("POST /v0/asks/{id}/device-undo", I, nil), // take back an applied default
		// Person steps (SPEC 16): they wait at the project's authority for its daily steps card.
		hedged("GET /v0/steps", L, list("steps")),
		write("POST /v0/steps", W, nil),
		hedged("GET /v0/steps/{id}", I, nil),
		write("POST /v0/steps/{id}/drop", I, nil),
		write("POST /v0/steps/{id}/check", I, nil),
		hedged("GET /v0/bundles/{id}", I, nil),
		write("POST /v0/bundles/{id}/steps/{n}/report", I, nil), // the person's agent, from their own machine
		hedged("GET /v0/run-targets", W, nil),                   // the project's machines and whose they are, at its authority
		hedged("GET /v0/approvals/key", I, nil),                 // a key paired from a member asks the authority whether it is confirmed
		write("POST /v0/approvals/pair-key", W, nil),
		write("DELETE /v0/approvals/key", W, nil), // a key ends itself at the authority that bound it (?project=)
		write("POST /v0/approvers/self", W, nil),
		hedged("GET /v0/approvers/self", W, nil), // the caller's own approvers at the authority, which decides the project's asks
		hedged("GET /v0/receipts/key", W, nil),   // project-scoped reads must use the signing authority
		hedged("GET /v0/receipts/verify", W, &tl.RouteHook{Before: receiptProject}),
		hedged("GET /v0/aliases", W, nil), // inbox aliases live with the identity, at the project's authority
		write("POST /v0/aliases", W, nil),
		write("DELETE /v0/aliases/{alias}", W, nil),
		hedged("GET /v0/person-links", W, nil), // one person's accounts, at the project's authority
		write("POST /v0/person-links", W, nil),
		write("DELETE /v0/person-links/{identity}", W, nil),
		hedged("GET /v0/rulings", W, nil), // owner rulings live in project.toml at the project's authority
		write("POST /v0/rulings", W, nil),
		write("DELETE /v0/rulings/{name}", W, nil),

		// Summaries: the authority owns the archive and schedule.
		hedged("GET /v0/summaries", W, nil),
		hedged("GET /v0/summaries/config", W, nil),
		write("POST /v0/summaries", W, nil),
		write("PUT /v0/summaries/config", W, nil),

		// A project's GitHub repo list is project config at its authority:
		// every member reads and an admin sets the same list.
		hedged("GET /v0/github/repo-list", W, nil),
		write("PUT /v0/github/repo-list", W, nil),

		// The Mezze app door lives at the team project's authority (SPEC 18.4).
		hedged("GET /v0/door", W, nil),
		hedged("GET /v0/door/tasks", W, nil),
		hedged("GET /v0/door/tasks/{task}/deliveries", W, nil),
		write("PUT /v0/door", W, nil),
		write("PUT /v0/door/tasks/{task}", W, nil),
		write("DELETE /v0/door/tasks/{task}", W, nil),
		write("POST /v0/door/tasks/{task}/rotate", W, nil), // mints a new URL: twice would leave the caller holding a dead one

		// Automation records live at the project's authority, with no copy
		// (SPEC 18.6): every call asks it. An id names no project, so an item
		// is asked here, then at each joined project while it answers 404.
		hedged("GET /v0/automations", W, auto),
		write("POST /v0/automations", W, &tl.RouteHook{NoCopy: true, Before: automationProject}),
		hedged("GET /v0/automations/{id}", I, auto),
		write("PATCH /v0/automations/{id}", I, auto),
		write("DELETE /v0/automations/{id}", I, auto),
		hedged("GET /v0/automations/{id}/history", I, auto),
		write("PUT /v0/automations/{id}/health", I, auto),
		hedged("GET /v0/automation-homes", W, auto),
		write("PUT /v0/automation-homes/{home}", W, auto),

		// Native issues.
		hedged("GET /v0/issues", L, issues),
		hedged("GET /v0/issues/projects", L, issueProjects),
		write("POST /v0/issues", W, issue),
		hedged("GET /v0/issues/stats", W, issue),
		hedged("GET /v0/issues/labels", W, issue),
		hedged("GET /v0/issues/comments", W, issue),
		hedged("GET /v0/issues/memories", W, issue),
		write("PUT /v0/issues/memories/{key}", W, issue),
		write("DELETE /v0/issues/memories/{key}", W, issue),
		// One read-only SELECT over in-memory copies; it may think before it
		// answers (a copy of the events table takes 1.7 s at the authority:
		// a hedge then only doubles the work).
		hedged("POST /v0/issues/sql", W, &tl.RouteHook{NoCopy: true, Thinks: 4}),
		write("POST /v0/issues/setup", W, issue),
		write("POST /v0/issues/move", W, issue),
		write("POST /v0/issues/restore", I, issueItem),
		hedged("GET /v0/issues/{id}", I, issueItem),
		write("PATCH /v0/issues/{id}", I, issueItem),
		hedged("GET /v0/issues/{id}/comments", I, issueItem),
		hedged("GET /v0/issues/{id}/history", I, issueItem),
		hedged("GET /v0/issues/{id}/attachments", I, issueItem),
		hedged("GET /v0/issues/{id}/attachments/{aid}", I, issueItem), // one image's bytes (at most 1 MiB)
		write("POST /v0/issues/{id}/attachments", I, issueItem),
		write("DELETE /v0/issues/{id}/attachments/{aid}", I, issueItem),
		write("POST /v0/issues/{id}/claim", I, issueItem),
		write("POST /v0/issues/{id}/close", I, issueItem),
		write("POST /v0/issues/{id}/reopen", I, issueItem),
		write("POST /v0/issues/{id}/comments", I, issueItem),
		write("PATCH /v0/issues/{id}/comments/{cid}", I, issueItem),
		write("DELETE /v0/issues/{id}/comments/{cid}", I, issueItem),
		write("POST /v0/issues/{id}/comments/{cid}/redact", I, issueItem),
		write("POST /v0/issues/{id}/comments/{cid}/ack-gap", I, issueItem),
		write("POST /v0/issues/{id}/deps", I, issueItem),
		write("DELETE /v0/issues/{id}/deps/{target}", I, issueItem),
		write("POST /v0/issues/{id}/labels", I, issueItem),
		write("DELETE /v0/issues/{id}/labels/{label}", I, issueItem),
	}
}

// otherSession reports whether r names (by its path's {id}) a session on
// another machine.
func (s *Service) otherSession(r *http.Request) bool {
	id, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/v0/sessions/"))
	a, err := api.ParseAddress(id)
	return err == nil && a.Kind == api.KindSession && a.Machine != s.d.Config.Machine
}

// receiptProject names the project a receipt was signed for, when the
// verify call names none: project-scoped reads must use the signing
// authority.
func receiptProject(r *http.Request) (*http.Request, error) {
	if r.URL.Query().Get("project") != "" {
		return r, nil
	}
	raw := r.URL.Query().Get("receipt")
	b := []byte(raw)
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		b, _ = base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	}
	var rc orchapi.Receipt
	if json.Unmarshal(b, &rc) != nil || !ids.Valid(rc.Project) {
		return r, nil
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	clone.URL = &u
	q := u.Query()
	q.Set("project", rc.Project)
	clone.URL.RawQuery = q.Encode()
	return clone, nil
}

// automationProject names the project a record's create names in its body
// (the Mezze app's client sends it there), when the call names none: the proxy routes
// by the query and headers only. r keeps its body too: the proxy serves r
// here when the project is this machine's.
func automationProject(r *http.Request) (*http.Request, error) {
	if r.URL.Query().Get("project") != "" || api.GetHeader(r.Header, api.HeaderProject) != "" || r.Body == nil {
		return r, nil
	}
	body, err := tl.ReadBody(r)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	var in struct {
		Project string `json:"project"`
	}
	if json.Unmarshal(body, &in) != nil || in.Project == "" {
		return clone, nil
	}
	u := *r.URL
	q := u.Query()
	q.Set("project", in.Project)
	u.RawQuery = q.Encode()
	clone.URL = &u
	return clone, nil
}
