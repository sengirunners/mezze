package team

// The waiting line (SPEC 8 "Waiting line"): the farm's task
// queue, inside machine choice rather than beside it. Every machine-choice start is a row of team_task_line
// (the authority's registry, so it survives a restart). While no suitable
// machine is free it waits there, ordered by front then line. placeLine
// gives the oldest waiting start the roomiest free machine that suits it:
// each placement is one ordinary task_start op in teamlink's queue (the
// first has the task's own id, a later one id.N), answered at once on the
// machine's held ops poll. A report counts only for the row's current
// placement, so a machine that lost the task can never write over where it
// runs now. lineLoop places again when a slot frees, finds machines whose
// heartbeats stopped, and sends their tasks back to the line once
// (lostStart) unless they took an outward action zm saw, are script runs,
// or their time budget is spent. The copy a lost machine may still run is
// a team_task_copies row: its session's writes are refused, and it is
// cancelled when its machine reports it again.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

var taskLineMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "team/0020_task_line", Script: `
CREATE TABLE team_task_line (
	id            TEXT PRIMARY KEY,           -- rt-<ulid>: the task
	project       TEXT NOT NULL,
	from_machine  TEXT NOT NULL,              -- the parent's machine
	from_identity TEXT NOT NULL,
	parent        TEXT NOT NULL,
	request_id    TEXT NOT NULL DEFAULT '',
	request       TEXT NOT NULL DEFAULT '',   -- the request JSON a repeated request_id must match
	payload       TEXT NOT NULL,              -- JSON TaskStartOp (timeout_s: what is left after a requeue)
	pin           TEXT NOT NULL DEFAULT '',   -- the machine it was named onto ('' auto)
	front         INTEGER NOT NULL DEFAULT 0, -- before every non-front start
	line          INTEGER NOT NULL,           -- its order (smaller first)
	state         TEXT NOT NULL,              -- queued | placed | done
	machine       TEXT NOT NULL DEFAULT '',   -- where it is placed
	op            TEXT NOT NULL DEFAULT '',   -- that placement's op id
	places        INTEGER NOT NULL DEFAULT 0, -- placements so far
	placed        INTEGER NOT NULL DEFAULT 0,
	first_placed  INTEGER NOT NULL DEFAULT 0, -- its time budget runs from here
	requeues      INTEGER NOT NULL DEFAULT 0, -- times it went back after its machine was lost
	lost_on       TEXT NOT NULL DEFAULT '',   -- machines it was lost on (comma-separated)
	acted         TEXT NOT NULL DEFAULT '',   -- outward actions zm saw (comma-separated)
	session       TEXT NOT NULL DEFAULT '',   -- its current copy's session, once reported
	cancelled     INTEGER NOT NULL DEFAULT 0, -- a cancel was asked
	status        TEXT NOT NULL,              -- JSON Task as the parent sees it
	created       INTEGER NOT NULL,
	updated       INTEGER NOT NULL
);
CREATE UNIQUE INDEX team_task_line_request ON team_task_line (project, from_machine, parent, request_id) WHERE request_id != '';
CREATE INDEX team_task_line_wait ON team_task_line (project, state, front, line);
CREATE INDEX team_task_line_state ON team_task_line (state, machine);
CREATE INDEX team_task_line_op ON team_task_line (op);
CREATE INDEX team_task_line_session ON team_task_line (session);
CREATE INDEX team_task_line_parent ON team_task_line (parent, state);
CREATE INDEX team_task_line_order ON team_task_line (project, state, line);
CREATE INDEX team_task_line_recent ON team_task_line (project, state, created);
CREATE TABLE team_task_copies (
	op      TEXT PRIMARY KEY,           -- the lost placement's op id (its machine knows the task by it)
	task    TEXT NOT NULL,
	project TEXT NOT NULL,
	machine TEXT NOT NULL,
	session TEXT NOT NULL DEFAULT '',   -- its session there, once known
	live    INTEGER NOT NULL DEFAULT 1, -- it may still run there
	stopped INTEGER NOT NULL DEFAULT 0, -- when a cancel for it was last queued
	created INTEGER NOT NULL
);
CREATE INDEX team_task_copies_session ON team_task_copies (session);
CREATE INDEX team_task_copies_machine ON team_task_copies (machine, project, live);
CREATE INDEX team_task_copies_task ON team_task_copies (task);
CREATE INDEX team_task_copies_created ON team_task_copies (created);
-- the parent's machine reads a parent's refs on every start (liveTasks)
CREATE INDEX IF NOT EXISTS team_task_refs_parent ON team_task_refs (parent, created);`},
}

// Tunables (tests shorten them).
var (
	// opLineWait: a start nothing could take this long is refused.
	opLineWait = 24 * time.Hour
	// opLostAfter: a machine whose heartbeat stopped this long has lost its
	// tasks.
	opLostAfter = 3 * time.Minute
	// opLineEvery: how often lineLoop looks for lost machines and places.
	opLineEvery = 5 * time.Second
	// opRequeueMin: a lost task with less budget left ends instead.
	opRequeueMin = time.Minute
	// opTaskBudget: a task's time budget without timeout_s (SPEC 8: the
	// task deadline's default).
	opTaskBudget = 4 * time.Hour
)

// recordHook, when set (tests), runs in recordTask between its read and
// its write.
var recordHook func()

// opExpireSweeps and opPruneSweeps: how many full sweeps between expiry
// passes and between prunes.
const opExpireSweeps, opPruneSweeps = 6, 720

// opWaitingPerParent bounds one parent's waiting starts (tests lower it).
var opWaitingPerParent = 100

// Line states.
const (
	lineQueued = "queued"
	linePlaced = "placed"
	lineDone   = "done"
)

// lineRow is one task of the line.
type lineRow struct {
	id, project, from, fromIdentity, parent, key, keyBody string
	payload                                               orchapi.TaskStartOp
	pin                                                   string
	front                                                 bool
	line                                                  int64
	state, machine, op                                    string
	places                                                int
	placed, firstPlaced                                   int64
	requeues                                              int
	lostOn, acted, session                                string
	cancelled                                             bool
	status                                                string
	created, updated                                      int64
}

const lineCols = `id, project, from_machine, from_identity, parent, request_id, request, payload, pin, front, line, state, machine, op, places,
	placed, first_placed, requeues, lost_on, acted, session, cancelled, status, created, updated`

func scanLine(sc interface{ Scan(...any) error }) (lineRow, error) {
	var o lineRow
	var payload string
	err := sc.Scan(&o.id, &o.project, &o.from, &o.fromIdentity, &o.parent, &o.key, &o.keyBody, &payload, &o.pin, &o.front, &o.line,
		&o.state, &o.machine, &o.op, &o.places, &o.placed, &o.firstPlaced, &o.requeues, &o.lostOn, &o.acted, &o.session, &o.cancelled,
		&o.status, &o.created, &o.updated)
	if err == nil {
		err = json.Unmarshal([]byte(payload), &o.payload)
	}
	return o, err
}

// budget is o's time budget, run from its first placement.
func (o lineRow) budget() time.Duration {
	if o.payload.Request.TimeoutSeconds > 0 {
		return time.Duration(o.payload.Request.TimeoutSeconds) * time.Second
	}
	return opTaskBudget
}

// task is o's status as stored.
func (o lineRow) task() orchapi.Task {
	var t orchapi.Task
	_ = json.Unmarshal([]byte(o.status), &t)
	return t
}

func (o lineRow) isRun() bool { return o.payload.Request.Agent == orchapi.TaskAgentRun }

// isSandbox: a sandbox task, run by a machine's task driver in a pool box
// (SPEC 8 "Waiting line"), counted against its sandbox slots.
func (o lineRow) isSandbox() bool { return o.payload.Request.Sandbox != nil }

func (s *Service) lineRows(ctx context.Context, where string, args ...any) ([]lineRow, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+lineCols+` FROM team_task_line WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lineRow
	for rows.Next() {
		o, err := scanLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// loadLine is task id's row (sql.ErrNoRows when none). A start queued by a
// zm from before the line (its op has the task's id) is taken in first.
func (s *Service) loadLine(ctx context.Context, id string) (lineRow, error) {
	o, err := scanLine(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+lineCols+` FROM team_task_line WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) && s.adopt(ctx, id) {
		return scanLine(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+lineCols+` FROM team_task_line WHERE id = ?`, id))
	}
	return o, err
}

// lineByOp is the row whose placement (current or earlier) is op id: the
// task id is the op id up to its first dot.
func (s *Service) lineByOp(ctx context.Context, op string) (lineRow, error) {
	id, _, _ := strings.Cut(op, ".")
	return s.loadLine(ctx, id)
}

// adopt takes a start op queued before the line (its id is the task's)
// into the line as placed there, or done; false when there is none. Its
// request no longer says whether it was named or auto: it counts as pinned
// there, so it never runs elsewhere.
func (s *Service) adopt(ctx context.Context, id string) bool {
	if !strings.HasPrefix(id, orchapi.RemoteTaskPrefix) || strings.Contains(id, ".") {
		return false
	}
	o, err := s.LoadOp(ctx, id)
	if err != nil || o.Kind != opTaskStart {
		return false
	}
	var t orchapi.Task
	_ = json.Unmarshal([]byte(o.Status), &t)
	state := linePlaced
	if o.State == teamlink.OpDone || settled(t) {
		state = lineDone
	}
	session := ""
	if a, err := api.ParseAddress(t.Session); err == nil && a.Kind == api.KindSession {
		session = a.String()
	}
	created := store.Millis(o.Created)
	_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_task_line (id, project, from_machine, from_identity, parent, request_id, request, payload,
		pin, line, state, machine, op, places, placed, first_placed, session, status, created, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, id, o.Project, o.FromMachine, o.From, o.Parent, o.Key, o.KeyBody, string(o.Payload), o.Machine, created, state, o.Machine, o.ID,
		created, created, session, o.Status, created, store.Millis(time.Now()))
	return err == nil
}

// lineCheck refuses now a start no machine of ms could ever take; any other
// waits in the line. A sandbox start needs a machine with a task driver.
func lineCheck(ms []api.Machine, want, from string, run, sandbox bool) error {
	if want != "auto" {
		for _, m := range ms {
			if m.Machine != want {
				continue
			}
			if !m.Stats.RemoteTasks {
				return api.Errorf(api.CodeForbidden, "%s takes no tasks from other machines", want).
					WithHint("its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon")
			}
			if sandbox && !m.Stats.SandboxTasks {
				return api.Errorf(api.CodeForbidden, "%s takes no sandbox tasks", want).
					WithHint("its owner sets \"sandbox_task_cmd\" (a task driver) in its zm config.json and restarts its daemon")
			}
			if run && !m.Stats.ScriptRuns {
				return api.Errorf(api.CodeUnavailable, "%s's zm runs no scripts", want).WithHint("update zm there (zm daemon update)")
			}
			return nil
		}
		return api.NotFound("machine", want)
	}
	for _, m := range ms {
		if m.Stats.RemoteTasks && (!run || m.Stats.ScriptRuns) && (!sandbox || m.Stats.SandboxTasks) && m.Machine != from {
			return nil
		}
	}
	if sandbox {
		return api.Errorf(api.CodeUnavailable, "no machine of the project takes sandbox tasks now").
			WithHint("a machine takes them once its owner sets \"remote_tasks\": true and \"sandbox_task_cmd\"; GET /v0/machines shows which do")
	}
	return api.Errorf(api.CodeUnavailable, "no machine of the project takes remote tasks now").
		WithHint("a machine takes them once its owner sets \"remote_tasks\": true; GET /v0/machines shows which do")
}

// taskSlots is how many placed tasks a machine runs at once: its sandbox
// slots for sandbox tasks, apart from its plain ones.
func taskSlots(st api.MachineStats, sandbox bool) int {
	if sandbox {
		if !st.SandboxTasks {
			return 0
		}
		return max(1, st.SandboxSlots)
	}
	if st.TaskSlots > 0 {
		return st.TaskSlots
	}
	return 1 // a zm from before the line reports none
}

// nextLine is a new start's place in project's line: after every start
// queued so far (front ones sort first by their own column). Under lineMu.
func (s *Service) nextLine(ctx context.Context, project string) int64 {
	var hi int64
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT COALESCE(MAX(line), 0) FROM team_task_line WHERE project = ? AND state = 'queued'`, project).Scan(&hi)
	return max(time.Now().UnixMilli(), hi+1)
}

// waitingFor counts parent's waiting starts across this authority's
// projects.
func (s *Service) waitingFor(ctx context.Context, parent string) int {
	var n int
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT COUNT(*) FROM team_task_line WHERE parent = ? AND state = 'queued'`, parent).Scan(&n)
	return n
}

// heard is when machine last beat for project here (zero: not since this
// daemon started); this machine always is.
func (s *Service) heard(project, machine string) time.Time {
	if machine == s.d.Config.Machine {
		return time.Now()
	}
	return s.LastBeat(project, machine)
}

// placeLine places project's waiting starts, front ones first, then oldest
// first, each on the roomiest free machine that suits it (pickMachine over
// the free ones); one with none free waits, without holding up the starts
// behind it. A start placed on this machine runs here (now when wait, else
// off the caller).
func (s *Service) placeLine(ctx context.Context, project string, wait bool) {
	ctx = context.WithoutCancel(ctx)
	rt, err := s.runner()
	if err != nil {
		return
	}
	db := s.d.Store.Registry()
	type here struct {
		op      api.MachineOp
		project string
	}
	var mine []here
	func() {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		waiting, err := s.lineRows(ctx, `project = ? AND state = 'queued' ORDER BY front DESC, line, created, id`, project)
		if err != nil || len(waiting) == 0 {
			return
		}
		trusted, _, err := s.TrustedMachines(ctx, project, rt.ProjectMachines(ctx, project))
		if err != nil {
			return
		}
		// Placed, unsettled tasks: per machine (any project: a machine's
		// slots are its own) and kind (sandbox tasks have their own slots),
		// the plain ones not started yet there, and per parent across this
		// authority's projects (SPEC 8).
		type slot struct {
			machine string
			sandbox bool
		}
		busy, unstarted, parents := map[slot]int{}, map[string]int{}, map[string]int{}
		placed, err := s.lineRows(ctx, `state = 'placed'`)
		if err != nil {
			return
		}
		for _, o := range placed {
			busy[slot{o.machine, o.isSandbox()}]++
			if o.session == "" && !o.isSandbox() {
				unstarted[o.machine]++
			}
			parents[o.parent]++
		}
		now := time.Now()
		for _, o := range waiting {
			if parents[o.parent] >= opTaskLimit {
				continue // SPEC 8: at most 3 of a parent's tasks placed at once
			}
			var free []api.Machine
			sandbox := o.isSandbox()
			for _, m := range trusted {
				st := m.Stats
				if busy[slot{m.Machine, sandbox}] >= taskSlots(st, sandbox) || (o.pin == "" && inCSV(o.lostOn, m.Machine)) {
					continue
				}
				at := s.heard(project, m.Machine)
				if now.Sub(at) > min(opSeenWithin, opLostAfter) {
					continue // quiet: a machine whose heartbeat stopped gets nothing new
				}
				if st.DiskFreeMB >= 0 && st.DiskFreeMB < 2048 {
					continue // pickMachine skips these for auto only; a pinned start waits too
				}
				// A sandbox task's driver is small and its agent runs in a box:
				// the session limit is not its to wait for.
				if !sandbox && st.MaxSessions > 0 && st.Sessions+unstarted[m.Machine] >= st.MaxSessions {
					continue
				}
				m.Stats.Sessions += unstarted[m.Machine] // placed here, not in its numbers yet
				m.Seen = &at
				free = append(free, m)
			}
			want := o.pin
			if want == "" {
				want = "auto"
			}
			pick := func() (string, error) { return pickMachine(free, want, o.from, s.d.Config.Machine, o.isRun()) }
			if sandbox {
				pick = func() (string, error) { return pickSandboxMachine(free, want, o.from, s.d.Config.Machine) }
			}
			machine, err := pick()
			if err != nil {
				continue // nothing suitable is free: it waits
			}
			opID := o.id
			if o.places > 0 {
				opID = o.id + "." + strconv.Itoa(o.places+1)
			}
			t := o.task()
			t.Machine, t.State, t.Position = machine, orchapi.TaskPending, 0
			js, _ := json.Marshal(t)
			ms := store.Millis(now)
			res, err := db.ExecContext(ctx, `UPDATE team_task_line SET state = 'placed', machine = ?, op = ?, places = places + 1, placed = ?, status = ?, updated = ?,
				first_placed = CASE WHEN first_placed = 0 THEN ? ELSE first_placed END WHERE id = ? AND state = 'queued' AND cancelled = 0`,
				machine, opID, ms, string(js), ms, ms, o.id)
			if err != nil {
				continue
			}
			if n, _ := res.RowsAffected(); n != 1 {
				continue
			}
			payload, _ := json.Marshal(o.payload)
			op := api.MachineOp{ID: opID, Kind: opTaskStart, From: o.fromIdentity, Payload: payload}
			if err := s.QueueOp(ctx, teamlink.Op{MachineOp: op, Project: project, Machine: machine, FromMachine: o.from, Parent: o.parent, Status: string(js)}); err != nil {
				// Not queued: it waits again, in its place.
				t.Machine, t.State = o.pin, orchapi.TaskPending
				back, _ := json.Marshal(t)
				_, _ = db.ExecContext(ctx, `UPDATE team_task_line SET state = 'queued', machine = '', op = '', placed = ?, first_placed = ?, status = ?, updated = ?
					WHERE id = ? AND op = ?`, o.placed, o.firstPlaced, string(back), ms, o.id, opID)
				s.d.Log.Warn("team: place a waiting task", "task", o.id, "err", err)
				continue
			}
			busy[slot{machine, sandbox}]++
			if !sandbox {
				unstarted[machine]++
			}
			parents[o.parent]++
			if machine == s.d.Config.Machine {
				mine = append(mine, here{op, project})
			}
		}
	}()
	for _, h := range mine {
		if wait {
			if testHookRunHere != nil {
				testHookRunHere()
			}
			s.RunHere(ctx, h.project, h.op)
			continue
		}
		s.runHereOffCaller(ctx, h.project, h.op)
	}
}

// testHookRunHere, when set, runs just before a start placed on this machine runs here (a test holds it).
var testHookRunHere func()

// runHereOffCaller runs op here on its own goroutine, past its caller's deadline and cancel.
func (s *Service) runHereOffCaller(ctx context.Context, project string, op api.MachineOp) {
	ctx = context.WithoutCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if testHookRunHere != nil {
			testHookRunHere()
		}
		s.RunHere(ctx, project, op)
	}()
}

// recordTask stores t, the status op's machine reported, on the task when
// op is still its current placement (a report from a machine that lost it
// changes nothing there: lostReport). A settled one is done and frees its
// place for the waiting line.
func (s *Service) recordTask(ctx context.Context, op string, t orchapi.Task) {
	o, err := s.lineByOp(ctx, op)
	if err != nil {
		return
	}
	freed := false
	func() {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		// A write the session forwards (noteOutward, outside lineMu) may
		// add to acted meanwhile: the update holds only on the acted it
		// read, and is tried again on the fresh row.
		for try := 0; try < 5; try++ {
			if o, err = s.loadLine(ctx, o.id); err != nil {
				return
			}
			if recordHook != nil {
				recordHook()
			}
			if o.op != op {
				s.lostReport(ctx, o, op, t)
				return
			}
			rt := asRemote(t, o.id, o.machine)
			acted := addCSV(o.acted, rt.Acted...)
			rt.Acted, rt.Requeued, rt.Lost, rt.Position = nil, 0, "", 0 // the line's own, added by lineView
			js, _ := json.Marshal(rt)
			if o.state != linePlaced {
				// Ended here: later reports still carry its mail and comment
				// delivery, its redaction; a refused, cancelled or lost task
				// takes none, and none reopens it.
				if prior := o.task(); o.state != lineDone || prior.State != "ended" || prior.Lost != "" || !settled(rt) {
					s.lostReport(ctx, o, op, t)
					return
				}
				_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE team_task_line SET status = ?, acted = ?, updated = ? WHERE id = ? AND op = ? AND state = 'done' AND acted = ?`,
					string(js), acted, store.Millis(time.Now()), o.id, op, o.acted)
				_, _ = s.UpdateOp(ctx, op, teamlink.OpUpdate{Status: teamlink.Str(string(js))})
				return
			}
			state := linePlaced
			if settled(rt) {
				state = lineDone
			}
			session := ""
			if a, err := api.ParseAddress(rt.Session); err == nil && a.Kind == api.KindSession {
				session = a.String()
			}
			if state == linePlaced && string(js) == o.status && acted == o.acted && session == o.session {
				return // nothing new (the authority's own tasks are read on every session event)
			}
			// Only the row as read: a requeue, a cancel, a placement or an
			// outward action since wins.
			res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_task_line SET status = ?, state = ?, session = ?, acted = ?, updated = ?
				WHERE id = ? AND op = ? AND state = 'placed' AND machine = ? AND acted = ?`, string(js), state, session, acted, store.Millis(time.Now()), o.id, op, o.machine, o.acted)
			if err != nil {
				return
			}
			if n, _ := res.RowsAffected(); n != 1 {
				continue
			}
			opState := teamlink.OpTaken
			if state == lineDone {
				opState, freed = teamlink.OpDone, true
			}
			_, _ = s.UpdateOp(ctx, op, teamlink.OpUpdate{Status: teamlink.Str(string(js)), State: &opState})
			return
		}
	}()
	if freed {
		s.placeLine(ctx, o.project, false) // a slot freed
	}
}

// lostReport takes a report on op, a placement of task o that is not its
// current one (or o is no longer placed): the copy a lost machine ran. One
// still running there is stopped; one that ended no longer counts. Under
// lineMu.
func (s *Service) lostReport(ctx context.Context, o lineRow, op string, t orchapi.Task) {
	db := s.d.Store.Registry()
	var machine string
	var live bool
	if db.QueryRowContext(ctx, `SELECT machine, live FROM team_task_copies WHERE op = ?`, op).Scan(&machine, &live) != nil {
		return // a settled task's late report: nothing runs elsewhere
	}
	session := ""
	if a, err := api.ParseAddress(t.Session); err == nil && a.Kind == api.KindSession {
		session = a.String()
	}
	if settled(t) {
		_, _ = db.ExecContext(ctx, `UPDATE team_task_copies SET live = 0, session = CASE WHEN ? != '' THEN ? ELSE session END WHERE op = ?`, session, session, op)
		return
	}
	_, _ = db.ExecContext(ctx, `UPDATE team_task_copies SET live = 1, session = CASE WHEN ? != '' THEN ? ELSE session END WHERE op = ?`, session, session, op)
	s.stopCopy(ctx, o, op, machine)
}

// stopCopy queues a cancel of op, task o's copy on machine, unless one went
// out within opTakeWithin.
func (s *Service) stopCopy(ctx context.Context, o lineRow, op, machine string) {
	if machine == s.d.Config.Machine {
		return // never recorded here (this machine is never lost); RunHere under lineMu would deadlock
	}
	db := s.d.Store.Registry()
	now := time.Now()
	res, err := db.ExecContext(ctx, `UPDATE team_task_copies SET stopped = ? WHERE op = ? AND stopped < ?`,
		store.Millis(now), op, store.Millis(now.Add(-opTakeWithin)))
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	if _, err := s.followOp(ctx, o.project, machine, o.from, o.parent, opTaskCancel, taskRef{Task: op}); err == nil {
		s.d.Log.Info("team: a lost task's copy runs on: cancelled there", "task", o.id, "op", op, "machine", machine)
	}
}

// stopCopies stops every copy of task o that may still run (a cancel).
// Under lineMu.
func (s *Service) stopCopies(ctx context.Context, o lineRow) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT op, machine FROM team_task_copies WHERE task = ? AND live = 1`, o.id)
	if err != nil {
		return
	}
	var cs [][2]string
	for rows.Next() {
		var c [2]string
		if rows.Scan(&c[0], &c[1]) == nil {
			cs = append(cs, c)
		}
	}
	rows.Close()
	for _, c := range cs {
		s.stopCopy(ctx, o, c[0], c[1])
	}
}

// addCopy records task o's current placement as a copy on a machine that
// lost it. Under lineMu.
func (s *Service) addCopy(ctx context.Context, o lineRow) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_task_copies (op, task, project, machine, session, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (op) DO NOTHING`, o.op, o.id, o.project, o.machine, o.session, store.Millis(time.Now()))
}

// endLine ends task o for good with t (state done). Under lineMu; false
// when the row changed since it was read.
func (s *Service) endLine(ctx context.Context, o lineRow, t orchapi.Task) bool {
	js, _ := json.Marshal(t)
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_task_line SET state = 'done', status = ?, updated = ? WHERE id = ? AND state = ? AND op = ? AND updated = ?`,
		string(js), store.Millis(time.Now()), o.id, o.state, o.op, o.updated)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// refused is t refused for why.
func refused(t orchapi.Task, why string) orchapi.Task {
	t.State, t.Error = orchapi.TaskRefused, why
	return t
}

// lostStart: task o's machine was lost with o placed there (its heartbeat
// stopped for opLostAfter, or it never reported a start it took). Not
// carried yet, o just goes back to the line. A cancelled one ends. A pinned
// one waits for its machine (until its budget is spent). Else it goes back
// once, in its old place, never onto that machine again and with what is
// left of its time budget, unless it was requeued already, zm saw it act
// outward, it is a script run (carried: it may have run), or its budget is
// spent: then it ends lost, saying why. Either way its copy there is
// recorded: its session's writes are refused, and it is stopped when that
// machine reports it again.
func (s *Service) lostStart(ctx context.Context, id, why string) {
	project := ""
	func() {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		o, err := s.loadLine(ctx, id)
		if err != nil || o.state != linePlaced {
			return
		}
		if s.lostStartLocked(ctx, o, why) {
			project = o.project
		}
	}()
	if project != "" {
		s.placeLine(ctx, project, false)
	}
}

// lostStartLocked is lostStart on o as read (under lineMu); true when o
// went back or ended.
func (s *Service) lostStartLocked(ctx context.Context, o lineRow, why string) bool {
	db := s.d.Store.Registry()
	now := time.Now()
	t := o.task()
	if settled(t) {
		return false
	}
	// Not carried yet: ended here, it never runs there.
	if ok, _ := s.UpdateOp(ctx, o.op, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: &why, Unsent: true}); ok {
		if o.cancelled {
			return s.endLine(ctx, o, refused(t, startCancelled))
		}
		t.Machine, t.State = o.pin, orchapi.TaskPending
		js, _ := json.Marshal(t)
		res, err := db.ExecContext(ctx, `UPDATE team_task_line SET state = 'queued', machine = '', op = '', status = ?, updated = ? WHERE id = ? AND op = ? AND state = 'placed'`,
			string(js), store.Millis(now), o.id, o.op)
		if err != nil {
			return false
		}
		n, _ := res.RowsAffected()
		return n == 1
	}
	// Its budget runs from its first placement. (A requeued payload holds
	// only what was left, but a requeued task never goes back again.)
	left := o.budget() - now.Sub(store.FromMillis(o.firstPlaced))
	why += " (" + o.machine + " lost it)"
	reason := ""
	switch {
	case o.cancelled:
		reason = why + "; it was cancelled"
	case o.pin != "" && left >= opRequeueMin:
		// Pinned: it never runs elsewhere, nor again there (a machine runs
		// an op once). It waits for its machine, whose copy is the task:
		// running on, or reporting how it ended.
		return false
	case o.pin != "":
		reason = why + "; its time budget is spent"
	case o.requeues >= 1:
		reason = why + "; it went back to the line once already"
	case o.acted != "":
		reason = why + " after it took an outward action (" + o.acted + "), so it does not run again"
	case o.isRun():
		reason = why + "; a script run its machine took does not run again (zm cannot see what it did)"
	case left < opRequeueMin:
		reason = why + "; its time budget is spent"
	case o.isSandbox() && !s.otherSandboxMachine(ctx, o):
		reason = why + "; no other machine of the project takes sandbox tasks"
	}
	if reason != "" {
		t.State, t.Lost, t.Error = "ended", reason, reason
		if t.Result == nil {
			t.Result = &orchapi.TaskResult{Finished: "not_done", Outcome: orchapi.Outcome{Kind: orchapi.OutcomeInfra, Evidence: reason, Note: "its machine was lost"}}
		}
		for _, d := range []*string{&t.MailDelivery, &t.CommentDelivery} {
			if *d == "pending" || *d == "sending" {
				*d = "skipped" // its machine sends them, and it is gone
			}
		}
		if !s.endLine(ctx, o, t) {
			return false // a report came in since: the next sweep decides with it
		}
		s.addCopy(ctx, o)
		s.FinishOp(ctx, o.op, "", "its machine was lost")
		s.d.Log.Info("team: a remote task ended with its machine lost", "task", o.id, "machine", o.machine, "why", reason)
		return true
	}
	p := o.payload
	p.Request.TimeoutSeconds = int(left / time.Second) // what is left of its budget
	payload, _ := json.Marshal(p)
	t = orchapi.Task{ID: o.id, Parent: t.Parent, Agent: t.Agent, Model: t.Model, Work: t.Work, State: orchapi.TaskPending, MailDelivery: "pending", CommentDelivery: "pending",
		Sandbox: o.payload.Request.Sandbox}
	js, _ := json.Marshal(t)
	res, err := db.ExecContext(ctx, `UPDATE team_task_line SET state = 'queued', machine = '', op = '', session = '', placed = 0, payload = ?, status = ?,
		requeues = requeues + 1, lost_on = ?, updated = ? WHERE id = ? AND state = 'placed' AND op = ? AND updated = ?`,
		string(payload), string(js), addCSV(o.lostOn, o.machine), store.Millis(now), o.id, o.op, o.updated)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false // a report came in since: the next sweep decides with it
	}
	s.addCopy(ctx, o)
	s.FinishOp(ctx, o.op, "", "its machine was lost")
	s.d.Log.Info("team: a remote task went back to the line, its machine lost", "task", o.id, "machine", o.machine, "left", left.Round(time.Second))
	return true
}

// otherSandboxMachine: a trusted machine of o's project other than the
// parent's and those o was lost on (now including its current one) takes
// sandbox tasks, so a requeued o could run somewhere instead of waiting out
// opLineWait for nothing. Unknown (no runner, a failed read) counts as yes.
func (s *Service) otherSandboxMachine(ctx context.Context, o lineRow) bool {
	rt, err := s.runner()
	if err != nil {
		return true
	}
	trusted, _, err := s.TrustedMachines(ctx, o.project, rt.ProjectMachines(ctx, o.project))
	if err != nil {
		return true
	}
	lost := addCSV(o.lostOn, o.machine)
	for _, m := range trusted {
		if m.Stats.RemoteTasks && m.Stats.SandboxTasks && m.Machine != o.from && !inCSV(lost, m.Machine) {
			return true
		}
	}
	return false
}

// startExpired is a placement's op its machine did not take (or report) in
// time (teamlink's ExpireOps). One for this machine is starting here
// (RunHere: nothing carries it) unless no start of it runs in this process
// (a restart between placing and running it): then it never will, and is
// refused. Any other is as if its machine were lost.
func (s *Service) startExpired(ctx context.Context, op teamlink.Op, why string) {
	o, err := s.lineByOp(ctx, op.ID)
	if err != nil || o.op != op.ID || o.state != linePlaced {
		s.FinishOp(ctx, op.ID, "", why) // no longer where the task runs
		return
	}
	if op.Machine == s.d.Config.Machine {
		if s.gates.running(op.ID) {
			return
		}
		func() {
			s.lineMu.Lock()
			defer s.lineMu.Unlock()
			if o, err := s.loadLine(ctx, o.id); err == nil && o.op == op.ID && o.state == linePlaced {
				if s.endLine(ctx, o, refused(o.task(), "it did not start here within "+opTakeWithin.String())) {
					s.FinishOp(ctx, op.ID, "", why)
				}
			}
		}()
		s.placeLine(ctx, o.project, false) // its slot is free again
		return
	}
	if o.pin != "" && time.Since(s.heard(o.project, op.Machine)) <= opLostAfter {
		// Pinned to a machine that beats but never took it: the op will not
		// go out again (too old), so it goes back to wait for that machine
		// as a new placement, its budget kept.
		s.retryPlacement(ctx, o.id, op.ID, why)
		return
	}
	s.lostStart(ctx, o.id, op.Machine+" did not take it within "+opTakeWithin.String())
}

// retryPlacement sends task id back to the line when placement op is still
// its current one, without counting a requeue (it never ran: its machine
// never reported it). Should the machine have it after all, it is a copy:
// stopped when it reports it.
func (s *Service) retryPlacement(ctx context.Context, id, op, why string) {
	project := ""
	func() {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		o, err := s.loadLine(ctx, id)
		if err != nil || o.op != op || o.state != linePlaced {
			return
		}
		t := o.task()
		// Cancelled, or its budget spent (a pinned task its machine never
		// reports would otherwise go back every take window for ever): it
		// ends instead; a copy its machine has after all is stopped when it
		// reports it.
		end := ""
		switch {
		case o.cancelled:
			end = startCancelled
		case o.firstPlaced > 0 && o.budget()-time.Since(store.FromMillis(o.firstPlaced)) < opRequeueMin:
			end = "its machine never reported it and its time budget is spent"
		}
		if end != "" {
			if s.endLine(ctx, o, refused(t, end)) {
				s.addCopy(ctx, o)
				s.FinishOp(ctx, op, "", why)
				project = o.project
			}
			return
		}
		t.Machine, t.State, t.Session = o.pin, orchapi.TaskPending, ""
		js, _ := json.Marshal(t)
		res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_task_line SET state = 'queued', machine = '', op = '', session = '', placed = 0, status = ?, updated = ?
			WHERE id = ? AND op = ? AND state = 'placed' AND updated = ? AND cancelled = 0`, string(js), store.Millis(time.Now()), o.id, op, o.updated)
		if err != nil {
			return
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return
		}
		s.addCopy(ctx, o)
		s.FinishOp(ctx, op, "", why)
		project = o.project
	}()
	if project != "" {
		s.placeLine(ctx, project, false)
	}
}

// expireLine refuses starts nothing could take within opLineWait.
func (s *Service) expireLine(ctx context.Context) {
	cut := store.Millis(time.Now().Add(-opLineWait))
	old, err := s.lineRows(ctx, `state = 'queued' AND created < ? AND (requeues = 0 OR updated < ?)`, cut, cut) // a requeued one: from when it went back
	if err != nil {
		return
	}
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	for _, o := range old {
		s.endLine(ctx, o, refused(o.task(), "no suitable machine was free within "+opLineWait.String()))
	}
}

// refusePinned refuses the starts waiting in project's line pinned to
// machine.
func (s *Service) refusePinned(ctx context.Context, project, machine, why string) {
	waiting, err := s.lineRows(ctx, `project = ? AND state = 'queued' AND pin = ?`, project, machine)
	if err != nil {
		return
	}
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	for _, o := range waiting {
		s.endLine(ctx, o, refused(o.task(), why))
	}
}

// pruneLine drops settled tasks and copies older than teamlink keeps ops.
func (s *Service) pruneLine(ctx context.Context) {
	db := s.d.Store.Registry()
	cut := store.Millis(time.Now().Add(-teamlink.OpsKeep))
	_, _ = db.ExecContext(ctx, `DELETE FROM team_task_line WHERE state = 'done' AND updated < ?`, cut)
	_, _ = db.ExecContext(ctx, `DELETE FROM team_task_copies WHERE created < ?`, cut)
}

// lineLoop keeps every line moving: every opLineEvery it finds machines
// gone quiet and places; a session event here (a task of this machine
// ending) refreshes this machine's own tasks at once.
func (s *Service) lineLoop(ctx context.Context) {
	// Only a session starting, changing state or ending can move a task
	// (activity and title updates are most session events and move none).
	filter := events.Filter{Types: []string{"dev.zm.session.started", "dev.zm.session.state_changed", "dev.zm.session.ended"}}
	evs, cancel := s.d.Events.Subscribe(s.d.System(), filter, 64)
	defer func() { cancel() }()
	tick := time.NewTicker(opLineEvery)
	defer tick.Stop()
	s.sweepLine(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.sweepLine(ctx, true)
		case _, ok := <-evs:
			if !ok {
				evs, cancel = s.d.Events.Subscribe(s.d.System(), filter, 64)
			}
			s.sweepLine(ctx, false)
		}
	}
}

// sweepLine is one pass: this machine's own placed tasks refreshed; with
// full, expiry, lost machines and placing every project with a line.
func (s *Service) sweepLine(ctx context.Context, full bool) {
	rt, err := s.runner()
	if err != nil {
		return
	}
	// This machine's own: its task's status lives here, read now (as opTask
	// does), so its slot frees as it ends.
	own, _ := s.lineRows(ctx, `state = 'placed' AND machine = ?`, s.d.Config.Machine)
	for _, o := range own {
		t := o.task()
		if t.Session == "" || settled(t) {
			continue
		}
		if a, err := api.ParseAddress(t.Session); err == nil {
			if cur, err := rt.RemoteTaskStatus(ctx, a.Session); err == nil {
				s.recordTask(ctx, o.op, cur)
			}
		}
	}
	if !full {
		return
	}
	// Expiry scans teamlink's ops and the line's old rows: every
	// opExpireSweeps sweeps (30 s at 5 s), pruning hourly; placing and lost
	// machines every sweep.
	n := s.lineSweeps.Add(1)
	if n%opExpireSweeps == 1 {
		s.ExpireOps(ctx) // a placement not taken or reported in time: startExpired
		s.expireLine(ctx)
	}
	if n%opPruneSweeps == 1 {
		s.pruneLine(ctx)
	}
	// A placement whose op is gone (a crash between placing and queuing
	// it) never runs: back to the line.
	if all, err := s.lineRows(ctx, `state = 'placed'`); err == nil {
		for _, o := range all {
			if _, err := s.LoadOp(ctx, o.op); teamlink.IsNoOp(err) {
				s.retryPlacement(ctx, o.id, o.op, "its placement was never queued")
			}
		}
	}
	s.loseQuiet(ctx, time.Now())
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT DISTINCT project FROM team_task_line WHERE state = 'queued'`)
	if err != nil {
		return
	}
	var projects []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			projects = append(projects, p)
		}
	}
	rows.Close()
	for _, p := range projects {
		s.placeLine(ctx, p, false)
	}
}

// loseQuiet loses the tasks placed on machines quiet as of now: no heartbeat
// here for opLostAfter, once this authority has been up that long (to have
// heard from the live ones). Beat times and now are this daemon's own clock
// (teamlink), so no failed lookup or late machine number reads as "lost",
// and the window is many beats (3 min against a 15 s beat, 2 s while a task
// runs): one late beat never loses a live machine. Answers how
// many placed tasks it judged.
func (s *Service) loseQuiet(ctx context.Context, now time.Time) int {
	if now.Sub(time.Unix(0, s.started.Load())) < opLostAfter {
		return 0
	}
	placed, err := s.lineRows(ctx, `state = 'placed' AND machine != ?`, s.d.Config.Machine)
	if err != nil {
		return 0
	}
	for _, o := range placed {
		if at := s.heard(o.project, o.machine); now.Sub(at) > opLostAfter {
			s.lostStart(ctx, o.id, o.machine+" was not heard from in "+opLostAfter.String())
		}
	}
	return len(placed)
}

// lineView is task o's status with the line's own fields.
func (s *Service) lineView(ctx context.Context, o lineRow, position bool) orchapi.Task {
	t := o.task()
	t.ID = o.id
	t.Requeued = o.requeues
	t.Sandbox = o.payload.Request.Sandbox // SPEC 8: set on a sandbox task only
	t.Acted = splitCSV(addCSV(o.acted, t.Acted...))
	if o.state == lineQueued && position {
		var n int
		_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT COUNT(*) FROM team_task_line WHERE project = ? AND state = 'queued'
			AND (front > ? OR front = ? AND (line < ? OR line = ? AND (created < ? OR created = ? AND id < ?)))`,
			o.project, o.front, o.front, o.line, o.line, o.created, o.created, o.id).Scan(&n)
		t.Position = n + 1
	}
	return t
}

// TaskParent implements orchcore.TaskParents: the line's parent for its
// current copy's session, or for a lost copy's. A session is the task's only
// when it is on the machine the task was placed on: a machine's report
// names any address, and must not lend another machine's session a parent.
func (s *Service) TaskParent(ctx context.Context, session string) string {
	a, err := api.ParseAddress(session)
	if err != nil || a.Kind != api.KindSession {
		return ""
	}
	var parent string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT parent FROM team_task_line WHERE session = ? AND machine = ?
		UNION ALL SELECT l.parent FROM team_task_copies c JOIN team_task_line l ON l.id = c.task WHERE c.session = ? AND c.machine = ? LIMIT 1`,
		a.String(), a.Machine, a.String(), a.Machine).Scan(&parent)
	return parent
}
