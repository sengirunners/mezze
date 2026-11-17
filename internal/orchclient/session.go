package orchclient

// Owned by the session builder. One method per /v0/sessions endpoint in API.md.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func sessionPath(id, suffix string) string { return "/v0/sessions/" + client.Seg(id) + suffix }

// Spawn is POST /v0/sessions.
func Spawn(ctx context.Context, c *client.Client, req orchapi.SpawnRequest) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodPost, "/v0/sessions", req, &s)
}

// Sessions is GET /v0/sessions, rows only. Presentation callers should use
// SessionPage to retain pagination and incomplete/stale-share information.
func Sessions(ctx context.Context, c *client.Client, q orchapi.SessionQuery) ([]orchapi.Session, error) {
	l, err := SessionPage(ctx, c, q)
	return l.Sessions, err
}

// SessionPage is one page of GET /v0/sessions (q.Before = the previous
// page's Next; Next is empty on the last page). Skipped and Behind describe
// unavailable or non-current shares and must not be treated as a complete view.
func SessionPage(ctx context.Context, c *client.Client, q orchapi.SessionQuery) (orchapi.SessionList, error) {
	var l orchapi.SessionList
	limit := ""
	if q.Limit > 0 {
		limit = client.Itoa(int64(q.Limit))
	}
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/sessions", "project", q.Project, "state", string(q.State),
		"agent", q.Agent, "all", client.Bool(q.All), "before", q.Before, "limit", limit), nil, &l)
	return l, err
}

// Register is POST /v0/sessions/register.
func Register(ctx context.Context, c *client.Client, req orchapi.RegisterRequest) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodPost, "/v0/sessions/register", req, &s)
}

// Hook is POST /v0/sessions/hook: register-or-find, state and notice in one request.
func Hook(ctx context.Context, c *client.Client, req orchapi.HookRequest) (orchapi.HookResponse, error) {
	var r orchapi.HookResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/sessions/hook", req, &r)
}

// Admission is GET /v0/sessions/admission.
func Admission(ctx context.Context, c *client.Client) (orchapi.Admission, error) {
	var a orchapi.Admission
	return a, c.Do(ctx, http.MethodGet, "/v0/sessions/admission", nil, &a)
}

// SessionGC is GET /v0/sessions/gc (lists; deletes nothing).
func SessionGC(ctx context.Context, c *client.Client) ([]orchapi.GCCandidate, error) {
	var l orchapi.GCList
	return l.Candidates, c.Do(ctx, http.MethodGet, "/v0/sessions/gc", nil, &l)
}

// Session is GET /v0/sessions/{id}.
func Session(ctx context.Context, c *client.Client, id string) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodGet, sessionPath(id, ""), nil, &s)
}

// SetSessionName is POST /v0/sessions/{id}/name ("" clears the name).
func SetSessionName(ctx context.Context, c *client.Client, id, name string) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodPost, sessionPath(id, "/name"), orchapi.NameRequest{Name: name}, &s)
}

// SetState is POST /v0/sessions/{id}/state.
func SetState(ctx context.Context, c *client.Client, id string, req orchapi.StateRequest) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodPost, sessionPath(id, "/state"), req, &s)
}

// MoveSession is POST /v0/sessions/{id}/project.
func MoveSession(ctx context.Context, c *client.Client, id string, req orchapi.MoveRequest) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodPost, sessionPath(id, "/project"), req, &s)
}

// Input is POST /v0/sessions/{id}/input.
func Input(ctx context.Context, c *client.Client, id string, req orchapi.InputRequest) error {
	return c.Do(ctx, http.MethodPost, sessionPath(id, "/input"), req, nil)
}

// Resize is POST /v0/sessions/{id}/resize.
func Resize(ctx context.Context, c *client.Client, id string, req orchapi.ResizeRequest) error {
	return c.Do(ctx, http.MethodPost, sessionPath(id, "/resize"), req, nil)
}

// Stop is POST /v0/sessions/{id}/stop.
func Stop(ctx context.Context, c *client.Client, id string, req orchapi.StopRequest) (orchapi.Session, error) {
	var s orchapi.Session
	return s, c.Do(ctx, http.MethodPost, sessionPath(id, "/stop"), req, &s)
}

// Output is GET /v0/sessions/{id}/output.
func Output(ctx context.Context, c *client.Client, id string, since int64) (orchapi.Output, error) {
	var o orchapi.Output
	return o, c.Do(ctx, http.MethodGet, client.Query(sessionPath(id, "/output"), "since", client.Itoa(since)), nil, &o)
}

// Attach is GET /v0/sessions/{id}/attach (HTTP Upgrade zm-pty): raw PTY bytes both ways.
func Attach(ctx context.Context, c *client.Client, id string) (io.ReadWriteCloser, error) {
	return c.Upgrade(ctx, sessionPath(id, "/attach"), orchapi.AttachProtocol)
}

// AttachAll is Attach with ?replay=all: every byte of scrollback the daemon
// holds comes first, for a host that keeps its own history (zm attach --embed).
func AttachAll(ctx context.Context, c *client.Client, id string) (io.ReadWriteCloser, error) {
	return c.Upgrade(ctx, client.Query(sessionPath(id, "/attach"), "replay", "all"), orchapi.AttachProtocol)
}

// AttachReadOnly is GET /v0/sessions/{id}/attach?input=0: PTY output only.
// The daemon drops whatever is written to it, never counts it as a person
// typing, and keeps answering the session's cursor queries itself.
func AttachReadOnly(ctx context.Context, c *client.Client, id string) (io.ReadWriteCloser, error) {
	return c.Upgrade(ctx, client.Query(sessionPath(id, "/attach"), "input", "0"), orchapi.AttachProtocol)
}

// SessionActivity is GET /v0/sessions/{id}/activity: the session's recent
// activity lines, newest first (limit 0 = the default; subagent nil = all).
func SessionActivity(ctx context.Context, c *client.Client, id string, limit int, subagent *string) (orchapi.ActivityList, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if subagent != nil {
		q.Set("subagent", *subagent)
	}
	path := sessionPath(id, "/activity")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out orchapi.ActivityList
	return out, c.Do(ctx, http.MethodGet, path, nil, &out)
}

// SessionSubagents is GET /v0/sessions/{id}/subagents.
func SessionSubagents(ctx context.Context, c *client.Client, id string) (orchapi.SubagentList, error) {
	var out orchapi.SubagentList
	return out, c.Do(ctx, http.MethodGet, sessionPath(id, "/subagents"), nil, &out)
}

// Turn is POST /v0/sessions/{id}/turn: a structured session's turn, or with
// req.Steer more input for the running one.
func Turn(ctx context.Context, c *client.Client, id string, req orchapi.SessionTurnRequest) (orchapi.SessionOpResult, error) {
	var r orchapi.SessionOpResult
	return r, c.Do(ctx, http.MethodPost, sessionPath(id, "/turn"), req, &r)
}

// Interrupt is POST /v0/sessions/{id}/interrupt.
func Interrupt(ctx context.Context, c *client.Client, id string, req orchapi.SessionInterruptRequest) (orchapi.SessionOpResult, error) {
	var r orchapi.SessionOpResult
	return r, c.Do(ctx, http.MethodPost, sessionPath(id, "/interrupt"), req, &r)
}

// Answer is POST /v0/sessions/{id}/answer.
func Answer(ctx context.Context, c *client.Client, id string, req orchapi.SessionAnswerRequest) (orchapi.SessionOpResult, error) {
	var r orchapi.SessionOpResult
	return r, c.Do(ctx, http.MethodPost, sessionPath(id, "/answer"), req, &r)
}

// SessionEvents is GET /v0/sessions/{id}/events: a structured session's events
// after seq after, waiting up to waitS seconds (at most 45) for the first.
func SessionEvents(ctx context.Context, c *client.Client, id string, after int64, limit, waitS int) (orchapi.SessionEvents, error) {
	var r orchapi.SessionEvents
	return r, c.Do(ctx, http.MethodGet, client.Query(sessionPath(id, "/events"), "after", client.Itoa(after), "limit", client.Itoa(int64(limit)), "wait_s", client.Itoa(int64(waitS))), nil, &r)
}
