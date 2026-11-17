package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/store"
)

var storeMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0001_init", Script: `
CREATE TABLE sessions (
	id         TEXT PRIMARY KEY,          -- session8
	address    TEXT NOT NULL UNIQUE,      -- <machine>/<session8>
	machine    TEXT NOT NULL,
	project    TEXT NOT NULL,             -- ULID or 'none'
	area       TEXT NOT NULL DEFAULT '',
	launcher   TEXT NOT NULL DEFAULT '',
	agent      TEXT NOT NULL,
	command    TEXT NOT NULL DEFAULT '',  -- JSON argv
	version    TEXT NOT NULL DEFAULT '',
	model      TEXT NOT NULL DEFAULT '',
	cwd        TEXT NOT NULL DEFAULT '',
	worktree   TEXT NOT NULL DEFAULT '',
	branch     TEXT NOT NULL DEFAULT '',
	scratch    TEXT NOT NULL DEFAULT '',
	sandbox    TEXT NOT NULL DEFAULT '',
	title      TEXT NOT NULL DEFAULT '',
	state      TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	work       TEXT NOT NULL DEFAULT '',
	runtime_id TEXT NOT NULL DEFAULT '',
	managed    INTEGER NOT NULL,
	pid        INTEGER NOT NULL DEFAULT 0,
	outcome    TEXT NOT NULL DEFAULT '',  -- JSON api.Outcome when ended
	started    INTEGER NOT NULL,
	updated    INTEGER NOT NULL,
	ended      INTEGER,
	out_bytes  INTEGER NOT NULL DEFAULT 0 -- total PTY output, set when it ends
);
CREATE UNIQUE INDEX sessions_runtime ON sessions (agent, runtime_id) WHERE runtime_id != '';
CREATE INDEX sessions_scope ON sessions (project, area, state);`},
	// What the agent is doing (hooks) and its terminal title (PTY output);
	// presence carries both, and the replica's token cap (reach).
	{Kind: store.KindRegistry, Name: "session/0003_activity", Script: `
ALTER TABLE sessions ADD COLUMN activity TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN activity_at INTEGER;
ALTER TABLE sessions ADD COLUMN term_title TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN activity TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN activity_at INTEGER;
ALTER TABLE presence ADD COLUMN term_title TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN reach TEXT NOT NULL DEFAULT '';`},
	// Shared sessions (SPEC 17.10): the flag, a replica session's in_use in
	// presence, and the prompt log the authority keeps.
	{Kind: store.KindRegistry, Name: "session/0005_shared", Script: `
ALTER TABLE sessions ADD COLUMN shared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE presence ADD COLUMN shared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE presence ADD COLUMN in_use TEXT NOT NULL DEFAULT '';
CREATE TABLE session_prompts (
	id           TEXT PRIMARY KEY,
	session      TEXT NOT NULL,         -- the session's address
	project      TEXT NOT NULL,
	area         TEXT NOT NULL DEFAULT '',
	sender       TEXT NOT NULL,         -- the sender's address
	who          TEXT NOT NULL,         -- the sender's identity
	text         TEXT NOT NULL,
	at           INTEGER NOT NULL,
	state        TEXT NOT NULL,         -- pending, delivered, expired
	delivered_at INTEGER
);
CREATE INDEX session_prompts_session ON session_prompts (session, at);
CREATE INDEX session_prompts_pending ON session_prompts (state, session) WHERE state = 'pending';`},
	// Session names (SPEC 8 "Names"): unique among this machine's live
	// sessions; an ended session keeps its name, which no longer resolves.
	{Kind: store.KindRegistry, Name: "session/0006_name", Script: `
ALTER TABLE sessions ADD COLUMN name TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX sessions_live_name ON sessions (name) WHERE name != '' AND state != 'ended';`},
	// The start time of a hook session's pid (procStart), so a pid the
	// kernel gave to another process does not keep a gone agent's session
	// alive (reapUnmanaged).
	{Kind: store.KindRegistry, Name: "session/0007_pid_start", Script: `
ALTER TABLE sessions ADD COLUMN pid_start INTEGER NOT NULL DEFAULT 0;`},
	// The agent bound to a managed shell, subagents and why a session waits
	// (JSON, '' = none), and the activity log (sidebar.go).
	{Kind: store.KindRegistry, Name: "session/0017_sidebar", Script: `
ALTER TABLE sessions ADD COLUMN runtime_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN subagents TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN needs TEXT NOT NULL DEFAULT '';
CREATE TABLE session_activity (
	seq      INTEGER PRIMARY KEY,
	session  TEXT NOT NULL,             -- session8
	subagent TEXT NOT NULL DEFAULT '',  -- a subagent's id; '' = the session's own
	at       INTEGER NOT NULL,
	text     TEXT NOT NULL
);
CREATE INDEX session_activity_session ON session_activity (session, seq);`},
	// Structured sessions (SPEC 8): the mode ('' = PTY, 'pipe') and the
	// capabilities its adapter declared (JSON).
	{Kind: store.KindRegistry, Name: "session/0018_mode", Script: `
ALTER TABLE sessions ADD COLUMN mode TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN caps TEXT NOT NULL DEFAULT '';`},
	// Usage attribution (SPEC 8 "Usage"): the Claude account, who it works
	// for, the job kind, the runtime's home ('' = the default) and the
	// tokens (JSON api.SessionTokens, '' = none yet); presence carries all but
	// the home.
	{Kind: store.KindRegistry, Name: "session/0019_usage", Script: `
ALTER TABLE sessions ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN for_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN job_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN agent_home TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN tokens TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN for_id TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN job_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN tokens TEXT NOT NULL DEFAULT '';`},
}

const cols = `id, address, machine, project, area, launcher, agent, command, version, model, cwd, worktree, branch,
	scratch, sandbox, title, state, note, work, runtime_id, managed, pid, outcome, started, updated, ended, out_bytes,
	activity, activity_at, term_title, shared, name, pid_start, runtime_agent, subagents, needs, mode, caps,
	account, for_id, job_kind, agent_home, tokens`

type scanner interface{ Scan(...any) error }

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// row is a stored session plus the columns api.Session does not carry.
type row struct {
	orchapi.Session
	outBytes int64
	pidStart int64 // procStart of PID when it was recorded (0: unknown)
	// rtPIDStart is procStart of RuntimeAgent.PID; subs are every subagent
	// kept (Session.Subagents is the live ones: sync), pending spawns included.
	rtPIDStart int64
	subs       []orchapi.Subagent
	agentHome  string // the runtime's CLAUDE_CONFIG_DIR or CODEX_HOME; "" = the default
}

// storedRuntimeAgent is the runtime_agent column.
type storedRuntimeAgent struct {
	orchapi.RuntimeAgent
	PIDStart int64 `json:"pid_start,omitempty"`
}

func scanRow(sc scanner) (row, error) {
	var (
		r                row
		command, outcome string
		caps             string
		managed, shared  int
		endedAt, actAt   sql.NullInt64
		started, updated int64
		state            string
		rt, subs, needs  string
		tokens           string
	)
	err := sc.Scan(&r.ID, &r.Address, &r.Machine, &r.Project, &r.Area, &r.Launcher, &r.Agent, &command, &r.Version,
		&r.Model, &r.Cwd, &r.Worktree, &r.Branch, &r.Scratch, &r.Sandbox, &r.Title, &state, &r.Note, &r.Work,
		&r.RuntimeID, &managed, &r.PID, &outcome, &started, &updated, &endedAt, &r.outBytes, &r.Activity, &actAt, &r.TermTitle, &shared, &r.Name, &r.pidStart, &rt, &subs, &needs, &r.Mode, &caps,
		&r.Account, &r.For, &r.JobKind, &r.agentHome, &tokens)
	if err != nil {
		return r, err
	}
	if rt != "" {
		var x storedRuntimeAgent
		if json.Unmarshal([]byte(rt), &x) == nil && x.Agent != "" {
			r.RuntimeAgent, r.rtPIDStart = &x.RuntimeAgent, x.PIDStart
		}
	}
	if subs != "" {
		_ = json.Unmarshal([]byte(subs), &r.subs)
	}
	if needs != "" {
		var n orchapi.SessionNeeds
		if json.Unmarshal([]byte(needs), &n) == nil && n.Kind != "" {
			r.Needs = &n
		}
	}
	r.State = orchapi.SessionState(state)
	if command != "" {
		_ = json.Unmarshal([]byte(command), &r.Command)
	}
	if outcome != "" {
		var o orchapi.Outcome
		if json.Unmarshal([]byte(outcome), &o) == nil {
			r.Outcome = &o
		}
	}
	r.Tokens = parseTokens(tokens)
	r.Managed, r.Shared = managed != 0, shared != 0
	if caps != "" {
		var c orchapi.AgentCapabilities
		if json.Unmarshal([]byte(caps), &c) == nil {
			r.Capabilities = &c
		}
	}
	r.Started, r.Updated, r.Ended = store.FromMillis(started), store.FromMillis(updated), store.NullMillis(endedAt)
	r.ActivityAt = store.NullMillis(actAt)
	r.sync()
	return r, nil
}

func args(r row) []any {
	cmd := ""
	if r.Command != nil {
		b, _ := json.Marshal(r.Command)
		cmd = string(b)
	}
	outcome := ""
	if r.Outcome != nil {
		b, _ := json.Marshal(r.Outcome)
		outcome = string(b)
	}
	var ended any
	if r.Ended != nil {
		ended = store.Millis(*r.Ended)
	}
	var actAt any
	if r.ActivityAt != nil {
		actAt = store.Millis(*r.ActivityAt)
	}
	caps := ""
	if r.Capabilities != nil {
		b, _ := json.Marshal(r.Capabilities)
		caps = string(b)
	}
	managed, shared := 0, 0
	if r.Managed {
		managed = 1
	}
	if r.Shared {
		shared = 1
	}
	rt, subs, needs := "", "", ""
	if r.RuntimeAgent != nil {
		b, _ := json.Marshal(storedRuntimeAgent{RuntimeAgent: *r.RuntimeAgent, PIDStart: r.rtPIDStart})
		rt = string(b)
	}
	if len(r.subs) > 0 {
		b, _ := json.Marshal(r.subs)
		subs = string(b)
	}
	if r.Needs != nil {
		b, _ := json.Marshal(r.Needs)
		needs = string(b)
	}
	return []any{r.ID, r.Address, r.Machine, r.Project, r.Area, r.Launcher, r.Agent, cmd, r.Version, r.Model, r.Cwd,
		r.Worktree, r.Branch, r.Scratch, r.Sandbox, r.Title, string(r.State), r.Note, r.Work, r.RuntimeID, managed,
		r.PID, outcome, store.Millis(r.Started), store.Millis(r.Updated), ended, r.outBytes, r.Activity, actAt, r.TermTitle, shared, r.Name, r.pidStart,
		rt, subs, needs, r.Mode, caps, r.Account, r.For, r.JobKind, r.agentHome, tokensJSON(r.Tokens)}
}

func parseTokens(js string) *orchapi.SessionTokens {
	if js == "" {
		return nil
	}
	var t orchapi.SessionTokens
	if json.Unmarshal([]byte(js), &t) != nil {
		return nil
	}
	return &t
}

func tokensJSON(t *orchapi.SessionTokens) string {
	if t == nil {
		return ""
	}
	b, _ := json.Marshal(t)
	return string(b)
}

// storedTime is t as the sessions and presence tables keep it:
// milliseconds, UTC. Every Session time is cut to it before it is written
// and before any event or answer carries it, so a session read back from
// the table equals the one its events carried (clients upsert by updated).
func storedTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return store.FromMillis(store.Millis(t))
}

func storedPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	x := storedTime(*t)
	return &x
}

// stored cuts every time of r to storedTime.
func (r *row) stored() {
	r.Started, r.Updated = storedTime(r.Started), storedTime(r.Updated)
	r.Ended, r.ActivityAt, r.Seen = storedPtr(r.Ended), storedPtr(r.ActivityAt), storedPtr(r.Seen)
}

func insertRow(ctx context.Context, db execer, r row) error {
	r.sync()
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (`+cols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args(r)...)
	return err
}

func deleteRow(ctx context.Context, db *sql.DB, id8 string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id8)
	return err
}

// errNoRow is internal; handlers turn it into api.NotFound with the caller's id.
var errNoRow = errors.New("no such session")

// errRegistryClosed: the store closed (Store.Close clears Registry).
var errRegistryClosed = errors.New("session registry is closed")

func loadRow(ctx context.Context, q queryer, id8 string) (row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+cols+` FROM sessions WHERE id = ?`, id8))
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNoRow
	}
	return r, err
}

func idExists(ctx context.Context, db *sql.DB, id8 string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE id = ?`, id8).Scan(&n)
	return n > 0, err
}

func byRuntime(ctx context.Context, db *sql.DB, agent, runtimeID string) (row, bool, error) {
	r, err := scanRow(db.QueryRowContext(ctx, `SELECT `+cols+` FROM sessions WHERE agent = ? AND runtime_id = ?`, agent, runtimeID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// updateRow runs fn on the stored row inside one transaction and writes it
// back when fn reports a change. It returns the row after fn.
func updateRow(ctx context.Context, db *sql.DB, id8 string, fn func(*row) (bool, error)) (row, bool, error) {
	var (
		r       row
		changed bool
	)
	err := store.Tx(ctx, db, func(tx *sql.Tx) error {
		var err error
		if r, err = loadRow(ctx, tx, id8); err != nil {
			return err
		}
		if changed, err = fn(&r); err != nil || !changed {
			return err
		}
		r.Updated = time.Now()
		r.sync()
		r.stored()
		a := args(r)
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET project=?, area=?, launcher=?, agent=?, command=?, version=?,
			model=?, cwd=?, worktree=?, branch=?, scratch=?, sandbox=?, title=?, state=?, note=?, work=?, runtime_id=?,
			managed=?, pid=?, outcome=?, started=?, updated=?, ended=?, out_bytes=?, activity=?, activity_at=?, term_title=?,
			shared=?, name=?, pid_start=?, runtime_agent=?, subagents=?, needs=?, mode=?, caps=?, account=?, for_id=?,
			job_kind=?, agent_home=?, tokens=? WHERE id=?`,
			append(a[3:], r.ID)...)
		return err
	})
	return r, changed, err
}

// byName returns this machine's live session named name.
func byName(ctx context.Context, q queryer, name string) (row, bool, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+cols+` FROM sessions WHERE name = ? AND state != ?`, name, string(orchapi.StateEnded)))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// nameTaken reports whether err is the live-name index refusing a write.
func nameTaken(err error) bool {
	return store.IsUnique(err) && strings.Contains(err.Error(), "sessions.name")
}

func listRows(ctx context.Context, db *sql.DB, where string, a ...any) ([]row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+cols+` FROM sessions `+where, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
