package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

const taskDepthLimit = 2
const taskParentLimit = 3
const taskTranscriptLimit = 32 << 20

var taskMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0008_tasks", Script: `CREATE TABLE session_tasks (
 id TEXT PRIMARY KEY, parent TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', request_json TEXT NOT NULL,
 depth INTEGER NOT NULL, work TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'running',
 transcript BLOB NOT NULL DEFAULT X'', output_offset INTEGER NOT NULL DEFAULT 0,
 last_output INTEGER NOT NULL, failure TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '',
 mail_done TEXT NOT NULL DEFAULT 'pending', comment_done TEXT NOT NULL DEFAULT 'pending',
 mail_error TEXT NOT NULL DEFAULT '', comment_error TEXT NOT NULL DEFAULT ''
 );
 CREATE UNIQUE INDEX session_tasks_request ON session_tasks(parent, request_id) WHERE request_id != '';
 CREATE INDEX session_tasks_parent ON session_tasks(parent, state);`},
	{Kind: store.KindRegistry, Name: "session/0014_handoff_group_stop", Script: `ALTER TABLE session_tasks ADD COLUMN group_stopped INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindRegistry, Name: "session/0015_child_never_started", Script: `ALTER TABLE session_tasks ADD COLUMN child_never_started INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindRegistry, Name: "session/0013_handoffs", Script: `CREATE TABLE session_handoffs (source TEXT PRIMARY KEY, continuation TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '', policy TEXT NOT NULL DEFAULT ''); CREATE TABLE session_task_stopping (parent TEXT PRIMARY KEY); CREATE TABLE session_handoff_requests (parent TEXT NOT NULL, request_id TEXT NOT NULL, request_hash TEXT NOT NULL, source TEXT NOT NULL, PRIMARY KEY(parent,request_id)); ALTER TABLE session_tasks ADD COLUMN request_verified INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindRegistry, Name: "session/0009_task_review", Script: `ALTER TABLE session_tasks ADD COLUMN text_withheld INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE session_tasks ADD COLUMN deadline_at INTEGER NOT NULL DEFAULT 0;
	 CREATE TABLE session_lineage (id TEXT PRIMARY KEY, parent TEXT NOT NULL, depth INTEGER NOT NULL);
	 INSERT INTO session_lineage(id,parent,depth) SELECT id,parent,depth FROM session_tasks;`},
	{Kind: store.KindRegistry, Name: "session/0010_task_holds", Script: `CREATE TABLE session_task_holds (task_id TEXT NOT NULL, hold_key TEXT NOT NULL, project TEXT NOT NULL, subjects TEXT NOT NULL, texts TEXT NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(task_id,hold_key));
 CREATE TABLE session_task_compactions (project TEXT PRIMARY KEY);
 ALTER TABLE session_tasks ADD COLUMN mail_attempts INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE session_tasks ADD COLUMN comment_attempts INTEGER NOT NULL DEFAULT 0;`},
	{Kind: store.KindRegistry, Name: "session/0011_task_groups", Script: `ALTER TABLE session_tasks ADD COLUMN pgid INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE session_tasks ADD COLUMN group_start INTEGER NOT NULL DEFAULT 0;
 UPDATE session_tasks SET pgid=(SELECT pid FROM sessions WHERE sessions.id=session_tasks.id),group_start=(SELECT pid_start FROM sessions WHERE sessions.id=session_tasks.id) WHERE EXISTS(SELECT 1 FROM sessions WHERE sessions.id=session_tasks.id);`},
	{Kind: store.KindRegistry, Name: "session/0012_holder_home", Script: `CREATE TABLE session_holders (id TEXT PRIMARY KEY, holder_pid INTEGER NOT NULL, holder_start INTEGER NOT NULL, home_id TEXT NOT NULL, home_path TEXT NOT NULL, home_dev TEXT NOT NULL, home_ino TEXT NOT NULL);
 ALTER TABLE session_tasks ADD COLUMN home_id TEXT NOT NULL DEFAULT '';
 ALTER TABLE session_tasks ADD COLUMN home_path TEXT NOT NULL DEFAULT '';
 ALTER TABLE session_tasks ADD COLUMN home_dev TEXT NOT NULL DEFAULT '';
 ALTER TABLE session_tasks ADD COLUMN home_ino TEXT NOT NULL DEFAULT '';`},
	// remote: an authority asked for the task (SPEC 8 "Machine choice"). Its
	// parent's end here never stops it, as for a parent on another machine:
	// an approved ask's action may have its asker on this machine.
	{Kind: store.KindRegistry, Name: "session/0024_task_remote", Script: `ALTER TABLE session_tasks ADD COLUMN remote INTEGER NOT NULL DEFAULT 0;`},
	// A refusal that names a wait (a send limit's rate_limited with retry_after) keeps a notice pending until then
	// without spending an attempt; limited_since bounds the waiting (taskNoticeWaitMax).
	{Kind: store.KindRegistry, Name: "session/0025_task_notice_wait", Script: `ALTER TABLE session_tasks ADD COLUMN mail_retry_at INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE session_tasks ADD COLUMN mail_limited_since INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE session_tasks ADD COLUMN comment_retry_at INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE session_tasks ADD COLUMN comment_limited_since INTEGER NOT NULL DEFAULT 0;`},
}

type taskStart struct {
	parent, requestID, requestJSON, work, source, prompt, baseBranch, project, area string
	depth                                                                           int
	// remote: an authority asked for it (SPEC 8 "Machine choice"); parent is
	// a session on another machine and project is pinned.
	remote bool
	// run: a script run (SPEC 8 "Script runs"): no prompt on stdin and only
	// the allowlisted environment.
	run bool
	// cloud: the runtime of a cloud task (task_cloud.go): spawn fills its
	// project's environment into the watcher's argv and sends the prompt as is.
	cloud string
	// sandbox: a sandbox task (task_sandbox.go): the task JSON on stdin as
	// is, the allowlisted environment and the driver's own variables.
	sandbox *sandboxStart
	// checkoutRefused: spawn could not make the task's worktree from cwd.
	checkoutRefused bool
	// asApprover: an approved ask's script run on its approver's own machine
	// (orchapi.TaskStartOp.AsApprover): it acts as this machine's human.
	asApprover bool
}

// refusedCheckout marks t's start as refused for its worktree (t may be
// nil: not a task) and returns err.
func (t *taskStart) refusedCheckout(err error) error {
	if t != nil {
		t.checkoutRefused = true
	}
	return err
}

// remoteStart pins a task an authority asked for: its project here and the
// depth the parent's machine counted.
type remoteStart struct {
	project string
	depth   int
	// command is a script run's (SPEC 8 "Script runs") or a sandbox task's
	// driver; empty for agents.
	command []string
	// sandbox: a sandbox task (task_sandbox.go); command is its driver.
	sandbox *sandboxStart
	// checkoutRefused: the start failed making its worktree from cwd (a
	// submodule, a filter, a partial clone): set by startTaskLocked so
	// StartRemoteTask tries another checkout.
	checkoutRefused bool
	// asApprover: taskStart's, for a script run only.
	asApprover bool
}

func (s *Service) insertTask(ctx context.Context, ss orchapi.Session, t *taskStart) error {
	var req orchapi.TaskStartRequest
	_ = json.Unmarshal([]byte(t.requestJSON), &req)
	timeout := s.taskTimeout(req)
	now := time.Now()
	h, err := s.currentTaskHome()
	if err != nil {
		return err
	}
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stopping int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM session_task_stopping WHERE parent=?`, t.parent).Scan(&stopping); err != nil {
		return err
	}
	if stopping != 0 && !t.remote {
		return api.Errorf(api.CodeUnavailable, "parent session is stopping")
	}
	// A remote parent (SPEC 8 "Machine choice") lives on another machine,
	// whose daemon checked it was live.
	var parentState string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE address=?`, t.parent).Scan(&parentState); !t.remote && (err != nil || parentState == string(orchapi.StateEnded)) {
		return api.Errorf(api.CodeUnavailable, "parent session is not live")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_tasks(id,parent,request_id,request_json,depth,work,last_output,deadline_at,home_id,home_path,home_dev,home_ino,remote) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, ss.ID, t.parent, t.requestID, t.requestJSON, t.depth, t.work, now.UnixMilli(), now.Add(time.Duration(timeout)*time.Second).UnixMilli(), h.id, h.path, h.dev, h.ino, t.remote); err != nil {
		return err
	}
	if t.source != "" {
		result, err := tx.ExecContext(ctx, `UPDATE session_handoffs SET status='started',continuation=? WHERE source=? AND status='pending'`, ss.ID, t.source)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return api.Errorf(api.CodeConflict, "continuation already decided")
		}
	}
	return tx.Commit()
}
func (s *Service) loadTaskIDs(ctx context.Context) error {
	if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM session_task_stopping`); err != nil {
		return err
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET last_output=? WHERE state='running'`, time.Now().UnixMilli()); err != nil {
		return err
	}
	defaultSeconds := s.d.Config.File.TaskDeadlineSeconds
	if defaultSeconds <= 0 {
		defaultSeconds = 4 * 60 * 60
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET deadline_at=? WHERE state='running' AND deadline_at=0`, time.Now().Add(time.Duration(defaultSeconds)*time.Second).UnixMilli()); err != nil {
		return err
	}
	// A crash during an external write leaves its outcome unknown; do not replay it.
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET mail_done='unknown',mail_error='delivery interrupted; check mail before retrying' WHERE mail_done='sending'`)
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET comment_done='unknown',comment_error='delivery interrupted; check work history before retrying' WHERE comment_done='sending'`)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id FROM session_tasks WHERE state='running'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		s.taskIDs.Store(id, true)
	}
	return rows.Err()
}

// captureTask stores only unseen output, including replay after adoption. A
// gap means the holder's bounded ring lost data while the daemon was away.
func (s *Service) captureTask(id string, off int64, data []byte) {
	s.taskIO.Lock()
	defer s.taskIO.Unlock()
	db := s.d.Store.Registry()
	ctx := context.Background()
	var next int64
	var transcript []byte
	if err := db.QueryRowContext(ctx, `SELECT output_offset,transcript FROM session_tasks WHERE id=? AND state='running'`, id).Scan(&next, &transcript); err != nil {
		return
	}
	spoolPath := holder.TaskOutputPath(s.d.Config.Home, id)
	_, statErr := os.Stat(spoolPath)
	spooled := statErr == nil
	if off > next {
		// The holder may have overwritten its ring during a burst. Its spool
		// is written first and contains the missing bytes for one-shot tasks.
		if !spooled || off-next > taskTranscriptLimit {
			_, _ = db.ExecContext(ctx, `UPDATE session_tasks SET failure='task output overflow' WHERE id=? AND failure=''`, id)
			return
		}
		f, err := os.Open(spoolPath)
		if err != nil {
			return
		}
		missing := make([]byte, off-next)
		_, err = f.ReadAt(missing, next)
		_ = f.Close()
		if err != nil {
			return
		}
		data = append(missing, data...)
		off = next
	}
	skip := next - off
	if skip >= int64(len(data)) {
		return
	}
	data = data[skip:]
	combined := []byte{}
	progress := false
	r, ok := s.load(ctx, id)
	if spooled {
		if next+int64(len(data)) > taskTranscriptLimit {
			_, _ = db.ExecContext(ctx, `UPDATE session_tasks SET failure='task output exceeded 32 MiB' WHERE id=? AND failure=''`, id)
			return
		}
		// The durable spool already holds the partial line. Persist only the
		// byte cursor, not a growing copy of it on every 32 KiB holder frame.
		// Recover its prefix once a newline actually completes that event.
		if ok && bytes.IndexByte(data, '\n') >= 0 {
			var err error
			progress, err = taskSpoolProgress(spoolPath, r.Agent, next, data)
			if err != nil {
				return
			}
		}
	} else {
		combined = append(transcript, data...)
		if len(combined) > taskTranscriptLimit {
			_, _ = db.ExecContext(ctx, `UPDATE session_tasks SET failure='task output exceeded 32 MiB' WHERE id=? AND failure=''`, id)
			return
		}
		// Legacy non-spooled holders still need the full captured transcript.
		start := bytes.LastIndexByte(transcript, '\n') + 1
		if ok {
			progress = taskCompleteProgress(r.Agent, combined[start:])
		}
	}
	if progress {
		_, err := db.ExecContext(ctx, `UPDATE session_tasks SET transcript=?,output_offset=?,last_output=? WHERE id=? AND output_offset=? AND state='running'`, combined, next+int64(len(data)), time.Now().UnixMilli(), id, next)
		if err != nil {
			s.d.Log.Warn("capture task output", "session", id, "err", err)
		}
	} else {
		_, err := db.ExecContext(ctx, `UPDATE session_tasks SET transcript=?,output_offset=? WHERE id=? AND output_offset=? AND state='running'`, combined, next+int64(len(data)), id, next)
		if err != nil {
			s.d.Log.Warn("capture task output", "session", id, "err", err)
		}
	}
}

// taskSpoolProgress reads an unfinished prefix only when new data completes a
// line. Its backward scan is bounded by the spool cap; no daemon-local state is
// needed, so an adopted holder or replay gap resumes from the persisted cursor.
func taskSpoolProgress(path, agent string, next int64, data []byte) (bool, error) {
	end := bytes.LastIndexByte(data, '\n') + 1
	if end == 0 {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	start := next
	var previous [1]byte
	if next > 0 {
		if _, err := f.ReadAt(previous[:], next-1); err != nil {
			return false, err
		}
	}
	var block [32 << 10]byte
	for start > 0 && previous[0] != '\n' {
		n := min(start, int64(len(block)))
		start -= n
		if _, err := f.ReadAt(block[:n], start); err != nil {
			return false, err
		}
		if i := bytes.LastIndexByte(block[:n], '\n'); i >= 0 {
			start += int64(i + 1)
			break
		}
	}
	prefix := int(next - start)
	complete := make([]byte, prefix+end)
	if prefix > 0 {
		if _, err := f.ReadAt(complete[:prefix], start); err != nil {
			return false, err
		}
	}
	copy(complete[prefix:], data[:end])
	return taskCompleteProgress(agent, complete), nil
}

// Only newline-complete runtime events advance the task's progress clock.
func taskCompleteProgress(agent string, data []byte) bool {
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return false
		}
		if taskProgress(agent, data[:i+1]) {
			return true
		}
		data = data[i+1:]
	}
}

// startTask starts req as the caller's task; project, with machine, names
// the team project to place it through (else the parent's); where "cloud"
// runs it on the provider's cloud computer (task_cloud.go).
func (s *Service) startTask(ctx context.Context, req orchapi.TaskStartRequest, project, where string) (orchapi.Task, error) {
	sub, _, err := s.processCaller(ctx, core.SubjectOf(ctx))
	if err != nil {
		return orchapi.Task{}, err
	}
	ctx = core.WithSubject(ctx, sub)
	if sub.Session == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "task_start needs a parent session")
	}
	if strings.HasPrefix(req.ClientRequestID, orchapi.RemoteTaskPrefix) {
		// The authority's ops use their ID as the request id of a remote start;
		// a local start must not take one: the two share (parent, request_id).
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "client_request_id may not start with %q", orchapi.RemoteTaskPrefix)
	}
	if err := orchapi.CheckTaskSandbox(req); err != nil {
		return orchapi.Task{}, err
	}
	if req.Sandbox != nil && (where != "" || req.Machine == "" || req.Machine == s.d.Config.Machine) {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a sandbox task runs through the waiting line on another machine: pass machine (auto or a name)")
	}
	if where != "" {
		return s.startCloudTask(ctx, sub, req, project, where)
	}
	if req.OnLimit != "" && req.OnLimit != "next_agent" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "on_limit must be next_agent")
	}
	if req.Machine == s.d.Config.Machine {
		req.Machine = "" // here
	}
	if req.Machine != "" {
		return s.startTaskOn(ctx, sub, req, project)
	}
	if project != "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "project is for a start with machine: another zm machine of that team project, or auto").
			WithHint("a task on this machine stays in its parent's project")
	}
	if req.Lookup {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "lookup is for a start with machine: another zm machine of the team project, or auto")
	}
	if req.Front {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "front is for a start with machine: it jumps the authority's waiting line")
	}
	if isRun(req) {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run needs machine: another zm machine of the team project, or auto")
	}
	if req.ContinueFrom != "" {
		return s.continueTask(ctx, sub, req)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt is required")
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 7*24*60*60 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "timeout_s must be between 0 and 604800")
	}
	if len(req.Prompt) > 256<<10 || len(req.ClientRequestID) > 200 || len(req.Model) > 200 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt, model or client_request_id is too long")
	}
	if req.Agent != "claude" && req.Agent != "codex" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "agent must be claude or codex")
	}
	parent, ok := s.Lookup(ctx, sub.Session)
	if !ok || parent.State == orchapi.StateEnded {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "parent session is not live")
	}
	if req.Cwd == "" {
		req.Cwd = core.ResolveInputOf(ctx).Cwd
	}
	if req.Cwd == "" {
		req.Cwd = parent.Cwd
	}
	if req.Worktree == nil {
		yes := true
		req.Worktree = &yes
	}
	b, _ := json.Marshal(req)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return s.startTaskLocked(ctx, sub, parent, req, string(b), "", "", nil)
}

func (s *Service) startTaskLocked(ctx context.Context, sub *authz.Subject, parent orchapi.Session, req orchapi.TaskStartRequest, requestJSON, launchPrompt, baseBranch string, rs *remoteStart) (orchapi.Task, error) {
	if s.ctx.Err() != nil {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "session service is stopping")
	}
	db := s.d.Store.Registry()
	if req.ClientRequestID != "" {
		if req.ContinueFrom == "" && rs == nil { // a remote start's key is the authority's op id: never a handoff's
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session_handoff_requests WHERE parent=? AND request_id=?`, sub.Session, req.ClientRequestID).Scan(&n); err != nil {
				return orchapi.Task{}, err
			}
			if n != 0 {
				return orchapi.Task{}, api.Errorf(api.CodeConflict, "client_request_id was used for a different request")
			}
		}
		var id, prior string
		err := db.QueryRowContext(ctx, `SELECT id,request_json FROM session_tasks WHERE parent=? AND request_id=? AND remote=?`, sub.Session, req.ClientRequestID, rs != nil).Scan(&id, &prior)
		if err == nil {
			if prior != requestJSON {
				return orchapi.Task{}, api.Errorf(api.CodeConflict, "client_request_id was used for a different request")
			}
			return s.getTask(ctx, sub, id)
		}
		if err != sql.ErrNoRows {
			return orchapi.Task{}, err
		}
	}
	var stopping int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session_task_stopping WHERE parent=?`, sub.Session).Scan(&stopping); err != nil {
		return orchapi.Task{}, err
	}
	if stopping != 0 && rs == nil { // a remote start's parent's end here does not stop it
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "parent session is stopping")
	}
	depth := 1
	var pd int
	err := db.QueryRowContext(ctx, `SELECT depth FROM session_lineage WHERE id=?`, parent.ID).Scan(&pd)
	if err == sql.ErrNoRows {
		err = db.QueryRowContext(ctx, `SELECT depth FROM session_tasks WHERE id=?`, parent.ID).Scan(&pd)
	}
	if rs != nil {
		depth, err = rs.depth, nil
	} else if err == nil {
		depth = pd + 1
	} else if err != sql.ErrNoRows {
		return orchapi.Task{}, err
	}
	if depth > taskDepthLimit {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "task depth limit %d reached", taskDepthLimit)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session_tasks WHERE parent=? AND state='running' AND remote=?`, sub.Session, rs != nil).Scan(&n); err != nil {
		return orchapi.Task{}, err
	}
	if n >= taskParentLimit {
		return orchapi.Task{}, api.Errorf(api.CodeUnavailable, "parent task concurrency limit %d reached", taskParentLimit)
	}
	env, err := envMap(nil)
	if err != nil {
		return orchapi.Task{}, err
	}
	var cmd []string
	agent, cloud := cloudTaskAgent(req, requestJSON)
	var sandbox *sandboxStart
	if isRun(req) {
		if rs == nil || len(rs.command) == 0 {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run starts only on another machine")
		}
		cmd = rs.command
	} else if req.Sandbox != nil {
		if rs == nil || rs.sandbox == nil || len(rs.command) == 0 {
			return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a sandbox task starts only through the waiting line, on a machine with a task driver")
		}
		cmd, agent, sandbox = rs.command, taskAgentSandbox, rs.sandbox
	} else if cloud != "" {
		if cmd, err = s.cloudCommand(cloud, env); err != nil {
			return orchapi.Task{}, err
		}
	} else {
		if cmd, err = s.taskCommand(req.Agent, env); err != nil {
			return orchapi.Task{}, err
		}
		if req.Model != "" {
			cmd = append(cmd, "--model", req.Model)
		}
		if req.Agent == "codex" {
			cmd = append(cmd, "-")
		}
	}
	var sourceProject, sourceArea string
	if req.ContinueFrom != "" {
		source, ok := s.load(ctx, req.ContinueFrom)
		if !ok {
			return orchapi.Task{}, api.NotFound("task", req.ContinueFrom)
		}
		sourceProject, sourceArea = source.Project, source.Area
	}
	if rs != nil {
		sourceProject = rs.project
	}
	ts := &taskStart{parent: sub.Session, requestID: req.ClientRequestID, requestJSON: requestJSON, work: req.Work, depth: depth, source: req.ContinueFrom, prompt: launchPrompt, baseBranch: baseBranch, project: sourceProject, area: sourceArea, remote: rs != nil, run: isRun(req), cloud: cloud, sandbox: sandbox, asApprover: rs != nil && rs.asApprover && isRun(req)}
	ss, err := s.spawn(ctx, orchapi.SpawnRequest{Agent: agent, Command: cmd, Model: spawnModel(req, sandbox), Cwd: req.Cwd, Area: sourceArea, Worktree: *req.Worktree, Title: "task",
		For: or(req.For, parent.For), JobKind: or(req.JobKind, parent.JobKind)}, ts)
	if err != nil {
		if rs != nil {
			rs.checkoutRefused = ts.checkoutRefused
		}
		return orchapi.Task{}, err
	}
	return s.getTask(ctx, sub, ss.ID)
}

func (s *Service) getTask(ctx context.Context, sub *authz.Subject, id string) (orchapi.Task, error) {
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	origin, err := s.taskOrigin(ctx, sub)
	if err != nil {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	var parent, work, state, raw, mailErr, commentErr, mailState, commentState string
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT parent,work,state,result,mail_error,comment_error,mail_done,comment_done FROM session_tasks WHERE id=?`, r.ID).Scan(&parent, &work, &state, &raw, &mailErr, &commentErr, &mailState, &commentState)
	if err == sql.ErrNoRows {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	if err != nil {
		return orchapi.Task{}, err
	}
	if origin != "" && origin != parent {
		return orchapi.Task{}, api.NotFound("task", id)
	}
	var continuedFrom string
	var reqJSON string
	var requestVerified bool
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT request_json,request_verified FROM session_tasks WHERE id=?`, r.ID).Scan(&reqJSON, &requestVerified)
	var req orchapi.TaskStartRequest
	_ = json.Unmarshal([]byte(reqJSON), &req)
	continuedFrom = req.ContinueFrom
	var continuation, continuationErr, handoffStatus string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT continuation,error,status FROM session_handoffs WHERE source=?`, r.ID).Scan(&continuation, &continuationErr, &handoffStatus)
	if handoffStatus == "pending" {
		continuation = "pending"
	}
	t := orchapi.Task{ContinuedFrom: continuedFrom, Continuation: continuation, ContinuationError: continuationErr, ID: r.ID, Session: r.Address, Parent: parent, Agent: r.Agent, Model: r.Model, Work: work, State: state, MailError: mailErr, CommentError: commentErr, MailDelivery: mailState, CommentDelivery: commentState}
	if r.Agent == taskAgentSandbox {
		t.Agent, t.Sandbox = req.Agent, req.Sandbox // the agent the driver runs in its box
	}
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_task_holds WHERE task_id=?)`, r.ID).Scan(&t.RedactionPending); err != nil {
		return orchapi.Task{}, err
	}
	if raw != "" {
		var result orchapi.TaskResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return orchapi.Task{}, err
		}
		t.Result = &result
		var held bool
		if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT text_withheld FROM session_tasks WHERE id=?`, r.ID).Scan(&held); err != nil {
			return orchapi.Task{}, err
		}
		if held || result.TextWithheld || t.RedactionPending {
			t.Result.Text = ""
			t.Result.TextWithheld = true
			// Evidence can contain runtime output too. A pending scrub has
			// not verified it, including when completion delivery ages out.
			t.Result.Outcome.Evidence = "withheld pending redaction"
		}
		if (held || result.TextWithheld) && !requestVerified && t.ContinuationError != "" {
			t.ContinuationError = "withheld pending redaction"
		}
	}
	return t, nil
}

func (s *Service) completeTask(id string) {
	s.taskIO.Lock()
	locked := true
	defer func() {
		if locked {
			s.taskIO.Unlock()
		}
	}()
	if _, ok := s.taskIDs.Load(id); !ok {
		return
	}
	ctx := context.Background()
	db := s.d.Store.Registry()
	if db == nil { // the store closed under a late run
		return
	}
	r, ok := s.load(ctx, id)
	if !ok || r.State != orchapi.StateEnded {
		return
	}
	var transcript []byte
	var failure, state string
	var captured int64
	var withheld bool
	if err := db.QueryRowContext(ctx, `SELECT transcript,failure,state,output_offset,text_withheld FROM session_tasks WHERE id=?`, id).Scan(&transcript, &failure, &state, &captured, &withheld); err != nil || state != "running" {
		return
	}
	if b, err := os.ReadFile(holder.TaskOutputPath(s.d.Config.Home, id)); err == nil {
		transcript = b
		captured = r.outBytes
		if _, err := os.Stat(holder.TaskOverflowPath(s.d.Config.Home, id)); err == nil {
			failure = "task output exceeded 32 MiB or could not be spooled"
		}
	}
	// Recover a quick exit or the tail left by a holder while the daemon was away.
	if captured < r.outBytes {
		if data, err := s.outputRow(r, captured); err == nil {
			if data.Offset > captured {
				failure = "task output overflow"
			} else {
				transcript = append(transcript, []byte(data.Data)...)
				captured = data.Next
			}
		}
		if captured < r.outBytes && failure == "" {
			failure = "task output was incomplete"
		}
	}
	var parsed taskEventResult
	var outcome orchapi.Outcome
	if r.Agent == orchapi.TaskAgentRun {
		outcome = runOutcome(failure, r.Outcome)
		parsed = runResult(transcript, outcome)
	} else if r.Agent == taskAgentSandbox {
		outcome = sandboxOutcome(failure, r.Outcome)
		parsed = sandboxResult(r.Cwd, transcript, outcome)
	} else if isCloudAgent(r.Agent) {
		outcome, parsed = cloudEnd(failure, r.Outcome, transcript)
	} else {
		parsed = parseTaskEvents(r.Agent, transcript)
		if failure != "" {
			parsed.Finished = "not_done"
		}
		outcome = taskOutcome(parsed, failure, r.Outcome)
	}
	result := orchapi.TaskResult{Finished: parsed.Finished, Text: parsed.Text, Outcome: outcome, Usage: orchapi.TaskUsage{InputTokens: parsed.InputTokens, OutputTokens: parsed.OutputTokens, CachedInputTokens: parsed.CachedInputTokens}, Branch: r.Branch}
	if transcript == nil {
		transcript = []byte{}
	} // a held, unlinked empty spool is still a non-NULL blob
	b, _ := json.Marshal(result)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE session_tasks SET state='ended',result=?,transcript=? WHERE id=? AND state='running'`, string(b), transcript, id); err != nil {
		s.d.Log.Warn("complete task", "id", id, "err", err)
		return
	}
	var request string
	if err = tx.QueryRowContext(ctx, `SELECT request_json FROM session_tasks WHERE id=?`, id).Scan(&request); err != nil {
		return
	}
	var req orchapi.TaskStartRequest
	if json.Unmarshal([]byte(request), &req) != nil {
		return
	}
	if req.ContinueFrom != "" && r.PID == 0 && outcome.Kind == orchapi.OutcomeInfra {
		if _, err = tx.ExecContext(ctx, `UPDATE session_handoffs SET status='failed',error='continuation start interrupted; inspect its result' WHERE source=? AND continuation=?`, req.ContinueFrom, id); err != nil {
			return
		}
	}
	if req.OnLimit == "next_agent" && (outcome.Kind == orchapi.OutcomeQuota || outcome.Kind == orchapi.OutcomeAuth) && failure == "" {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_handoffs(source) VALUES(?)`, id); err != nil {
			return
		}
	}
	if err = tx.Commit(); err != nil {
		return
	}
	s.taskIDs.Delete(id)
	if r.Agent == orchapi.TaskAgentRun && !strings.Contains(failure, "may still be running") {
		removeRunCheckout(ctx, s.runsDir(), req.Cwd) // else kept for inspection
	}
	if r.Agent == taskAgentSandbox && !strings.Contains(failure, "may still be running") {
		removeSandboxDir(s.sandboxDir(), req.Cwd)
	}
	// An ask action's scratch directory, only by the task made for it (a
	// child task standing in it never removes it), unless its process may
	// still run there or a next agent may carry on in it.
	if isRemoteTask(req.ClientRequestID) && req.Cwd == s.askScratch(req.ClientRequestID) && req.OnLimit == "" && !strings.Contains(failure, "may still be running") {
		removeSandboxDir(s.askScratchDir(), req.Cwd)
	}
	_, _ = s.update(ctx, id, "", func(rr *row) (bool, error) {
		rr.Outcome = &result.Outcome
		rr.Note = string(result.Outcome.Kind)
		return true, nil
	})
}

func (s *Service) taskExitOutcome(ctx context.Context, id, agent string, process orchapi.Outcome, end int64) orchapi.Outcome {
	if s.d == nil || s.d.Store == nil {
		return process
	}
	db := s.d.Store.Registry()
	if db == nil {
		return process // a late holder exit after store shutdown has no task evidence
	}
	var transcript []byte
	var failure string
	var captured int64
	if err := db.QueryRowContext(ctx, `SELECT transcript,failure,output_offset FROM session_tasks WHERE id=?`, id).Scan(&transcript, &failure, &captured); err != nil {
		return process
	}
	if b, err := os.ReadFile(holder.TaskOutputPath(s.d.Config.Home, id)); err == nil {
		transcript = b
		if _, e := os.Stat(holder.TaskOverflowPath(s.d.Config.Home, id)); e == nil {
			failure = "task output exceeded 32 MiB or could not be spooled"
		}
	} else if captured < end {
		if r, ok := s.load(ctx, id); ok {
			r.outBytes = end
			if data, e := s.outputRow(r, captured); e == nil {
				if data.Offset > captured {
					failure = "task output overflow"
				} else {
					transcript = append(transcript, []byte(data.Data)...)
					captured = data.Next
				}
			}
		}
		if captured < end && failure == "" {
			failure = "task output was incomplete"
		}
	}
	if agent == orchapi.TaskAgentRun {
		return runOutcome(failure, &process)
	}
	if agent == taskAgentSandbox {
		return sandboxOutcome(failure, &process)
	}
	if isCloudAgent(agent) {
		return cloudOutcome(failure, &process, transcript)
	}
	return taskOutcome(parseTaskEvents(agent, transcript), failure, &process)
}

func (s *Service) taskLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.handoffTick()
			s.taskTick()
		}
	}
}
func (s *Service) taskTick() {
	ctx := s.ctx
	s.compactTaskScrubs(ctx)
	s.sweepRunCheckouts(ctx)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id,state,last_output,failure,deadline_at FROM session_tasks WHERE state='running' OR (state='ended' AND ((mail_done='pending' AND mail_retry_at<=?1) OR (comment_done='pending' AND comment_retry_at<=?1) OR EXISTS(SELECT 1 FROM session_task_holds h WHERE h.task_id=session_tasks.id)))`, time.Now().UnixMilli())
	if err != nil {
		return
	}
	type pending struct {
		id, state, failure string
		last, deadline     int64
	}
	var ps []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.state, &p.last, &p.failure, &p.deadline) == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	for _, p := range ps {
		if p.state == "running" {
			r, ok := s.load(ctx, p.id)
			if !ok {
				continue
			}
			if r.State == orchapi.StateEnded {
				s.completeTask(p.id)
				continue
			}
			if s.unreachableWhy(r.ID) != "" {
				if _, exited := holder.ReadExit(s.d.Config.Home, r.ID); exited {
					s.endFromFile(ctx, r)
					continue
				}
			}
			actionableFailure := p.failure != "" && !strings.Contains(p.failure, "may still be running")
			if actionableFailure || (p.deadline > 0 && time.Now().UnixMilli() >= p.deadline) || (s.TaskNoOutput > 0 && !outputAgent(r.Agent) && time.Since(time.UnixMilli(p.last)) >= s.TaskNoOutput) {
				if p.failure == "" {
					reason := "no output timeout"
					if p.deadline > 0 && time.Now().UnixMilli() >= p.deadline {
						reason = "overall deadline exceeded"
					}
					_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET failure=? WHERE id=? AND failure=''`, reason, p.id)
				}
				if r.Agent == taskAgentSandbox {
					s.stopSandbox(p.id) // its driver has SandboxTermWait: never hold up the loop
					continue
				}
				_, _ = s.Stop(ctx, s.d.System(), p.id, orchapi.StopRequest{Escalate: true, Force: true})
			}
		} else {
			s.recoverTaskHolds(ctx, p.id)
			s.wg.Add(1)
			go func(id string) { defer s.wg.Done(); s.deliverTask(id) }(p.id)
		}
	}
}

func (s *Service) hTaskAgents(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, s.taskAgents())
}
func (s *Service) hTaskTranscriptDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.getTask(ctx, core.SubjectOf(ctx), r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if r.URL.Query().Get("discard_redaction") == "1" {
		if err := s.discardTaskRedaction(ctx, t); err != nil {
			api.WriteError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if info, e := os.Lstat(s.scrollbackPath(t.ID)); e == nil && info.IsDir() {
		returnErr := api.Errorf(api.CodeConflict, "discarded output barriers are retained")
		api.WriteError(w, returnErr)
		return
	}
	if t.State != "ended" || t.Result == nil || t.Result.TextWithheld || t.MailDelivery == "pending" || t.MailDelivery == "sending" || t.CommentDelivery == "pending" || t.CommentDelivery == "sending" {
		api.WriteError(w, api.Errorf(api.CodeConflict, "task must end, finish delivery, and clear redaction holds before cleanup"))
		return
	}
	s.taskIO.Lock()
	defer s.taskIO.Unlock()
	active, e := s.handoffInUse(ctx, t.ID)
	if e != nil || active {
		api.WriteError(w, api.Errorf(api.CodeConflict, "task continuation is pending or running"))
		return
	}
	var held bool
	if e = s.d.Store.Registry().QueryRowContext(ctx, `SELECT text_withheld FROM session_tasks WHERE id=?`, t.ID).Scan(&held); e != nil || held {
		api.WriteError(w, api.Errorf(api.CodeConflict, "task output withheld pending redaction"))
		return
	}
	if _, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET transcript=X'' WHERE id=?`, t.ID); err == nil {
		err = os.Remove(s.scrollbackPath(t.ID))
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err == nil {
		for _, path := range []string{holder.TaskOutputPath(s.d.Config.Home, t.ID), holder.TaskErrorPath(s.d.Config.Home, t.ID), holder.TaskOverflowPath(s.d.Config.Home, t.ID)} {
			if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
				err = e
				break
			}
		}
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// discardTaskRedaction is an explicit local escape hatch for an ended task
// whose writers cannot be identified. It seals output paths before scrubbing.
func (s *Service) discardTaskRedaction(ctx context.Context, t orchapi.Task) error {
	if t.State != "ended" || s.proc(t.ID) != nil {
		return api.Errorf(api.CodeConflict, "task must be ended and disconnected")
	}
	var prior taskHomeIdentity
	var holderStart int64
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT home_id,home_path,home_dev,home_ino,holder_start FROM session_holders WHERE id=?`, t.ID).Scan(&prior.id, &prior.path, &prior.dev, &prior.ino, &holderStart); err != nil {
		return api.Errorf(api.CodeConflict, "holder home is not recorded; cannot seal all output paths")
	}
	home, err := s.currentTaskHome()
	if err != nil {
		return err
	}
	if prior.path != home.path {
		if gone, _ := s.recordedHolderGone(ctx, t.ID); holderStart <= 0 || !gone || prior.id != home.id || prior.dev != home.dev || prior.ino != home.ino {
			return api.Errorf(api.CodeConflict, "holder may still write in its former home; cannot seal all output paths")
		}
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project,subjects,texts,created FROM session_task_holds WHERE task_id=?`, t.ID)
	if err != nil {
		return err
	}
	var holds []core.Scrub
	for rows.Next() {
		var sc core.Scrub
		var subjects, texts string
		var created int64
		if err = rows.Scan(&sc.Project, &subjects, &texts, &created); err != nil {
			rows.Close()
			return err
		}
		if time.Since(time.UnixMilli(created)) < 30*time.Second {
			rows.Close()
			return api.Errorf(api.CodeConflict, "redaction hold is less than 30 seconds old")
		}
		if json.Unmarshal([]byte(subjects), &sc.Subjects) != nil || json.Unmarshal([]byte(texts), &sc.Texts) != nil {
			rows.Close()
			return api.Errorf(api.CodeConflict, "redaction hold cannot be decoded")
		}
		holds = append(holds, sc)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(holds) == 0 {
		return api.Errorf(api.CodeConflict, "task has no pending redaction")
	}
	for _, sc := range holds {
		if err = s.scrubTasksMode(ctx, sc, t.ID, false); err != nil && !api.IsCode(err, api.CodeUnavailable) {
			return err
		}
	}
	var pending bool
	if err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_task_holds WHERE task_id=?)`, t.ID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return api.Errorf(api.CodeConflict, "redaction remains pending")
	}
	return nil
}
func (s *Service) hTaskStart(w http.ResponseWriter, r *http.Request) {
	var req orchapi.TaskStartBody
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	t, err := s.startTask(r.Context(), req.TaskStartRequest, strings.TrimSpace(req.Project), strings.TrimSpace(req.Where))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.taskView(r.Context(), t))
}
func (s *Service) hTaskStatus(w http.ResponseWriter, r *http.Request) {
	wait := 0
	if v := r.URL.Query().Get("wait_s"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "wait_s must be nonnegative"))
			return
		}
		wait = min(n, 45)
	}
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	if id := r.PathValue("id"); isRemoteTask(id) {
		t, err := s.remoteTaskStatus(r.Context(), id, wait)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, t)
		return
	}
	for {
		t, err := s.getTask(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		if t.State == "ended" && t.Result != nil && t.Result.TextWithheld {
			if p := s.proc(t.ID); p != nil {
				select {
				case <-p.exited:
				case <-time.After(time.Second):
				case <-r.Context().Done():
					return
				}
			}
			s.recoverTaskHolds(r.Context(), t.ID)
			if refreshed, e := s.getTask(r.Context(), core.SubjectOf(r.Context()), t.ID); e == nil {
				t = refreshed
			}
		}
		if t.State == "ended" || wait == 0 {
			api.WriteJSON(w, http.StatusOK, s.taskView(r.Context(), t))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			api.WriteJSON(w, http.StatusOK, s.taskView(r.Context(), t))
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) hTaskCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var t orchapi.Task
	var err error
	if id := r.PathValue("id"); isRemoteTask(id) {
		t, err = s.remoteTaskCancel(ctx, id)
	} else {
		t, err = s.cancelTask(ctx, core.SubjectOf(ctx), id)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.taskView(ctx, t))
}

// cancelTask stops a running task as sub (a live continuation of an ended
// one instead) and answers its status.
func (s *Service) cancelTask(ctx context.Context, sub *authz.Subject, id string) (orchapi.Task, error) {
	t, err := s.getTask(ctx, sub, id)
	if err != nil {
		return orchapi.Task{}, err
	}
	if t.State == "ended" {
		next, e := s.cancelHandoffTarget(ctx, t.ID)
		if e != nil {
			return orchapi.Task{}, e
		}
		if next == "" {
			return s.getTask(ctx, sub, t.ID)
		}
		t, e = s.getTask(ctx, sub, next)
		if e != nil {
			return orchapi.Task{}, e
		}
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET failure='cancelled' WHERE id=? AND state='running' AND failure=''`, t.ID)
	if err == nil && t.Sandbox != nil {
		// Its driver gets SandboxTermWait to release its box: the stop goes
		// on off the caller, and the task ends when it exits.
		if r, ok := s.load(ctx, t.ID); ok && r.Agent == taskAgentSandbox {
			s.stopSandbox(t.ID)
			return s.getTask(ctx, sub, t.ID)
		}
	}
	if err == nil {
		_, err = s.Stop(ctx, sub, t.ID, orchapi.StopRequest{Escalate: true, Force: true})
	}
	if err != nil {
		// It may have exited naturally between lookup and stop.
		s.completeTask(t.ID)
		current, getErr := s.getTask(ctx, sub, t.ID)
		if getErr == nil && current.State == "ended" {
			return current, nil
		}
		return orchapi.Task{}, err
	}
	s.completeTask(t.ID)
	return s.getTask(ctx, sub, t.ID)
}

// waitRemote polls get until its task settles or wait seconds pass (at most
// 45, as task_status).
func waitRemote(ctx context.Context, wait int, get func() (orchapi.Task, error)) (orchapi.Task, error) {
	deadline := time.Now().Add(time.Duration(min(max(wait, 0), 45)) * time.Second)
	for {
		t, err := get()
		if err != nil || remoteSettled(t) || !time.Now().Before(deadline) {
			return t, err
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
