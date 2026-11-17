package team

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// PlayOlderAuthority makes this package's replicas send machine numbers to
// a route no authority has, as if every authority were older than it (the
// returned func undoes it).
func PlayOlderAuthority() func() {
	old := teamlink.MachinePath
	teamlink.MachinePath = "/v0/team/machine-from-the-future"
	return func() { teamlink.MachinePath = old }
}

// BeatByHand stops every daemon's own heartbeat loop: the test's heartbeat
// calls are the only beats, so none runs beside it (the returned func undoes
// it).
func BeatByHand() func() {
	teamlink.BeatsOff.Store(true)
	return func() { teamlink.BeatsOff.Store(false) }
}

// PlayAuthorityBeforeZm makes this package's authorities refuse a machine
// report carrying the zm block, under either name, as an authority older
// than it does (api.DecodeJSON's unknown field), so replicas must send it
// again without it (the returned func undoes it).
func PlayAuthorityBeforeZm() func() { return PlayAuthorityBefore("potluck", "zm") }

// PlayAuthorityBefore makes this package's authorities refuse a machine
// report carrying any of fields, as an authority older than them does (the
// returned func undoes it).
func PlayAuthorityBefore(fields ...string) func() {
	old := teamlink.DecodeMachineReport
	teamlink.DecodeMachineReport = func(r *http.Request, v any) error {
		b, _ := io.ReadAll(r.Body)
		for _, f := range fields {
			if bytes.Contains(b, []byte(`"`+f+`":`)) {
				return api.Errorf(api.CodeBadRequest, "invalid JSON body: json: unknown field %q", f)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		return old(r, v)
	}
	return func() { teamlink.DecodeMachineReport = old }
}

// ReportEveryBeat makes replicas send machine numbers after every heartbeat
// (the returned func undoes it).
func ReportEveryBeat() func() {
	old := teamlink.MachineEvery
	teamlink.MachineEvery = 0
	return func() { teamlink.MachineEvery = old }
}

// routesFor is a teamlink with every row a Full daemon on machine
// registers (teamlink's own, team's, run targets').
func routesFor(machine string) *teamlink.Service {
	d := &core.Deps{Config: &config.Config{Machine: machine}}
	link := teamlink.New(d)
	(&Service{Service: link, d: d}).hook()
	link.RegisterRoutes("runtarget", runtarget.RelayRoutes()...)
	return link
}

// ForwardedKind is how a member on machine's proxy treats method and path
// ("" when it is not forwarded).
func ForwardedKind(method, path, machine string) string {
	row, ok := routesFor(machine).Route(httptest.NewRequest(method, "http://zm"+path, nil))
	if !ok || row.Kind == teamlink.KindLocal {
		return ""
	}
	return row.Kind.String()
}

// BeatProject sends one heartbeat to the authority of the joined project
// slug only (tests order heartbeats to different projects).
func (s *Service) BeatProject(ctx context.Context, slug string) {
	x := s.ByName(slug)
	if x == nil {
		panic("BeatProject: no project " + slug)
	}
	s.BeatRemote(ctx, x)
}

// CachedPresenceUnread reads heartbeat notice evidence independently of mail sync.
func (s *Service) CachedPresenceUnread(project, address string) int {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	n := 0
	for _, part := range s.cache.notices[project][address] {
		n += part.Unread
	}
	return n
}

// PresenceInventory exercises inventory selection without starting a daemon.
func PresenceInventory(ctx context.Context, sessions orchcore.SessionService) ([]orchapi.Session, error) {
	return (&Service{d: withSessions(&core.Deps{}, sessions)}).presenceSessions(ctx)
}

// PresenceAckBatch exposes the bounded live/ended acknowledgment allocation.
func PresenceAckBatch(unacked map[string][]string, live map[string]orchapi.Session) (map[string][]string, []orchapi.PresenceSession) {
	sent := boundedPresenceAcks(unacked, live)
	return sent, endedAcks(sent, live)
}

// SlowCopyOpen makes opening a replica's copy take d, as on a slow disk (the
// returned func undoes it).
func SlowCopyOpen(d time.Duration) func() {
	old := copyOpenDelay
	copyOpenDelay = d
	return func() { copyOpenDelay = old }
}

// DelayBeatApply holds every heartbeat's answer for d before it is applied,
// as an answer that arrives late (the returned func undoes it).
func DelayBeatApply(d time.Duration) func() {
	beatApplyDelay.Store(int64(d))
	return func() { beatApplyDelay.Store(0) }
}

// PowerCutAfterAnswer makes the replica act as if a refused write's caller
// was told but its row was not deleted (the delete, which now commits before
// the answer, failed) and what writeApply commits is lost (the returned func
// undoes it).
func PowerCutAfterAnswer() func() {
	settleStopsAfterAnswer.Store(true)
	applyCommitsLost.Store(true)
	return func() { settleStopsAfterAnswer.Store(false); applyCommitsLost.Store(false) }
}

// DeliverAnswer runs what this replica does with an answer from slug's
// authority that carries team header raw, to a request that sent scope
// version sent: one sent after every other call (later), or before them all.
func (s *Service) DeliverAnswer(ctx context.Context, slug, raw string, status int, sent int64, later bool) {
	x := s.ByName(slug)
	var seq uint64
	if later {
		seq = x.Sent.Add(1)
	}
	s.Observe(ctx, x, &http.Response{StatusCode: status, Header: http.Header{api.HeaderTeam: {raw}}}, seq, sent)
}

// HeldHeader is the team header this replica last took from slug's
// authority, and the scope version its calls send.
func (s *Service) HeldHeader(slug string) (string, int64) {
	x := s.ByName(slug)
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return x.HeaderB64, x.R.ScopeVersion
}

// ReportsLeaseHeld exposes the replica's actual heartbeat-held cache to tests.
func (s *Service) ReportsLeaseHeld(address, key string) bool {
	return s.cache.holds(address, key)
}

// CopyHolds reports whether any project copy on this replica holds work item id.
func (s *Service) CopyHolds(ctx context.Context, id string) bool {
	for _, x := range s.List() {
		if rep(x).hasItem(ctx, id) {
			return true
		}
	}
	return false
}

// CopyCursor reports the applied cursor of a ready project copy.
func (s *Service) CopyCursor(slug string) string {
	x := s.ByName(slug)
	if x == nil {
		return ""
	}
	rp := rep(x)
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if !rp.ready {
		return ""
	}
	return rp.cursor
}

// CopyRestore reports the restore generation a ready project copy was
// taken under.
func (s *Service) CopyRestore(slug string) string {
	x := s.ByName(slug)
	if x == nil {
		return ""
	}
	rp := rep(x)
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if !rp.ready {
		return ""
	}
	return rp.restore
}

// HoldRefresh installs a per-copy test hook after a refresh answer arrives.
func (s *Service) HoldRefresh(slug string, hook func(string) func()) func() {
	rp := rep(s.ByName(slug))
	rp.mu.Lock()
	rp.testRefreshHook = hook
	rp.mu.Unlock()
	return func() { rp.mu.Lock(); rp.testRefreshHook = nil; rp.mu.Unlock() }
}

func (s *Service) WantRefresh(slug, id string) { rep(s.ByName(slug)).want(id) }

// FakeT3 replaces this package's Mezze app runtime on every machine (the returned
// func undoes it).
func FakeT3(ensure func(ctx context.Context, c config.T3, op orchapi.T3EnsureOp) (orchapi.T3Environment, error), stats func(c config.T3) *api.MachineT3) func() {
	oe, ost := t3EnsureHook, t3StatsHook
	t3EnsureHook, t3StatsHook = ensure, stats
	return func() { t3EnsureHook, t3StatsHook = oe, ost }
}

// ShortT3Wait shortens how long the authority waits for a t3_ensure answer.
func ShortT3Wait(d time.Duration) func() {
	old := t3Wait
	t3Wait = d
	return func() { t3Wait = old }
}

// T3Row is one t3_ensure op as the authority keeps it, or a machine's
// record of running one.
type T3Row struct{ ID, State, Result string }

// T3Ops are s's t3_ensure ops (the authority's side).
func T3Ops(s *Service) []T3Row {
	return t3Rows(s, `SELECT id, state, result FROM team_ops WHERE kind = ? ORDER BY created`)
}

// T3Runs are s's records of t3_ensure ops it ran (a machine's side).
func T3Runs(s *Service) []T3Row {
	return t3Rows(s, `SELECT id, CASE acked WHEN 1 THEN 'acked' ELSE state END, report FROM team_ops_run WHERE kind = ? ORDER BY created`)
}

func t3Rows(s *Service, q string) []T3Row {
	rows, err := s.d.Store.Registry().Query(q, opT3Ensure)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []T3Row
	for rows.Next() {
		var r T3Row
		if rows.Scan(&r.ID, &r.State, &r.Result) == nil {
			out = append(out, r)
		}
	}
	return out
}

// ApplyReport is the authority taking a machine's report (a resent one).
func ApplyReport(s *Service, project, machine string, rep api.MachineOpReport) {
	s.ApplyOpReports(context.Background(), project, machine, []api.MachineOpReport{rep})
}

// OpsPollOff stops replicas' GET /v0/team/ops poll and their direct op
// reports: machine ops travel with heartbeats alone (the returned func
// undoes it).
func OpsPollOff() func() {
	teamlink.OpsPollOff.Store(true)
	return func() { teamlink.OpsPollOff.Store(false) }
}

// OpsPolls is how many GET /v0/team/ops this process's authorities answered.
func OpsPolls() int64 { return teamlink.OpsPolls.Load() }

// AskAnswerWithin is how long a forwarded ask waits for its answer.
func AskAnswerWithin() time.Duration { return askAnswerWithin }

// SetAskAnswerWithin sets it (the returned func undoes it).
func SetAskAnswerWithin(d time.Duration) func() {
	old := askAnswerWithin
	askAnswerWithin = d
	return func() { askAnswerWithin = old }
}

// SetLine sets when a machine whose heartbeat stopped has lost its tasks
// and how often the waiting line is swept (set before the daemons start;
// the returned func undoes it).
func SetLine(lostAfter, every time.Duration) func() {
	oldLost, oldEvery := opLostAfter, opLineEvery
	opLostAfter, opLineEvery = lostAfter, every
	return func() { opLostAfter, opLineEvery = oldLost, oldEvery }
}

// LoseQuietAfterBeat runs the sweep's lost-machine check as of exactly
// opLostAfter after machine's last heartbeat for project here (the last
// instant it still counts as heard from) and answers how many placed tasks
// it judged: the rule at its edge, on no wall-clock race.
func LoseQuietAfterBeat(s *Service, project, machine string) int {
	return s.loseQuiet(context.Background(), s.LastBeat(project, machine).Add(opLostAfter))
}

// LoseQuietPast runs the sweep's lost-machine check as of the first instant
// after opLostAfter from machine's last heartbeat for project here (the first
// it no longer counts as heard from) and answers how many placed tasks it
// judged: a machine lost on no wall-clock race.
func LoseQuietPast(s *Service, project, machine string) int {
	return s.loseQuiet(context.Background(), s.LastBeat(project, machine).Add(opLostAfter+time.Nanosecond))
}

// SetWaitingPerParent sets how many of a parent's starts may wait (the
// returned func undoes it).
func SetWaitingPerParent(n int) func() {
	old := opWaitingPerParent
	opWaitingPerParent = n
	return func() { opWaitingPerParent = old }
}

// WriteClasses are the write routes a member forwards (every pattern of a
// forwarded row that is not a read) and how the waiting line classes them
// (outward with its kind, or harmless).
func WriteClasses() ([]string, map[string]string, map[string]bool) {
	var out []string
	for _, r := range routesFor("m").Rows() {
		if r.Kind == teamlink.KindLocal || strings.HasPrefix(r.Pattern, "GET ") || strings.HasPrefix(r.Pattern, "HEAD ") || !strings.Contains(r.Pattern, " ") {
			continue
		}
		out = append(out, r.Pattern)
	}
	return out, outwardWrites, harmlessWrites
}

// Plant is a placed task PlantStart writes as placeLine would leave it.
type Plant struct {
	Project, Machine, Parent string
	Run, Sent, Pin           bool
	Session                  string        // its copy's session ("" not reported)
	Acted                    string        // outward actions seen
	PlacedAgo                time.Duration // when it was placed
}

// PlantStart writes a placed task and its op (no machine is told) and
// answers the task's id.
func PlantStart(s *Service, p Plant) string {
	ctx := context.Background()
	id := orchapi.RemoteTaskPrefix + strings.ToLower(ids.New())
	req := orchapi.TaskStartRequest{Agent: "codex", Prompt: "planted"}
	if p.Run {
		req = orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Run: &orchapi.TaskRun{Ref: "main", Script: "x.sh"}}
	}
	payload, _ := json.Marshal(orchapi.TaskStartOp{Parent: p.Parent, Depth: 1, Request: req})
	state := orchapi.TaskPending
	if p.Session != "" {
		state = "running"
	}
	pin := ""
	if p.Pin {
		pin = p.Machine
	}
	task, _ := json.Marshal(orchapi.Task{ID: id, Machine: p.Machine, Parent: p.Parent, Agent: req.Agent, State: state, Session: p.Session, MailDelivery: "pending", CommentDelivery: "pending"})
	now := time.Now()
	placed := store.Millis(now.Add(-p.PlacedAgo))
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_task_line (id, project, from_machine, from_identity, parent, payload, pin, line, state, machine, op, places,
		placed, first_placed, acted, session, status, created, updated) VALUES (?, ?, 'mac', 'ivo', ?, ?, ?, ?, 'placed', ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.Project, p.Parent, string(payload), pin, placed, p.Machine, id, placed, placed, p.Acted, p.Session, string(task), placed, store.Millis(now)); err != nil {
		panic(err)
	}
	op := api.MachineOp{ID: id, Kind: opTaskStart, From: "ivo", Payload: payload}
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: op, Project: p.Project, Machine: p.Machine, FromMachine: "mac", Parent: p.Parent, Status: string(task)}); err != nil {
		panic(err)
	}
	if p.Sent {
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_ops SET sent = 1 WHERE id = ?`, id); err != nil {
			panic(err)
		}
	}
	if p.PlacedAgo > 0 {
		if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_ops SET created = ? WHERE id = ?`, placed, id); err != nil {
			panic(err)
		}
	}
	return id
}

// StartRow is a task's line state, its current placement's op and its
// status as its parent sees it.
func StartRow(s *Service, id string) (string, string, orchapi.Task) {
	o, err := s.loadLine(context.Background(), id)
	if err != nil {
		return "", "", orchapi.Task{}
	}
	return o.state, o.op, s.lineView(context.Background(), o, true)
}

// LostStart is lostStart on the task as read before between runs (a report
// landing between the sweep's read and its write).
func LostStart(s *Service, id, why string, between func()) {
	ctx := context.Background()
	o, err := s.loadLine(ctx, id)
	if err != nil {
		panic(err)
	}
	if between != nil {
		between()
	}
	s.lineMu.Lock()
	s.lostStartLocked(ctx, o, why)
	s.lineMu.Unlock()
}

// CancelTask cancels task id as the machine that started it ("mac" for a
// planted one) would.
func CancelTask(s *Service, id string) (orchapi.Task, error) {
	o, err := s.loadLine(context.Background(), id)
	if err != nil {
		return orchapi.Task{}, err
	}
	return s.cancelOp(context.Background(), authz.System(s.d.Config.Machine), o.from, id)
}

// CopyCancels are the task_cancel ops queued for machine naming placement
// op (a lost copy being stopped).
func CopyCancels(s *Service, project, machine, op string) int {
	ops, err := s.Ops(context.Background(), teamlink.OpFilter{Project: project, Machine: machine, Kind: opTaskCancel})
	if err != nil {
		panic(err)
	}
	n := 0
	for _, o := range ops {
		var ref taskRef
		if json.Unmarshal(o.Payload, &ref) == nil && ref.Task == op {
			n++
		}
	}
	return n
}

// ExpireOps is teamlink's ExpireOps (the line's startExpired included).
func ExpireOps(s *Service) { s.ExpireOps(context.Background()) }

// LinePlans are SQLite's plans for the line's per-write lookups and its
// sweep's queries.
func LinePlans(s *Service) map[string][]string {
	out := map[string][]string{}
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"lostBySession":  {lostBySession, []any{"m/0a1b2c3d"}},
		"lostByMachine":  {lostByMachine, []any{"m", "p"}},
		"actedBySession": {actedBySession, []any{"m/0a1b2c3d", "m"}},
		"placedOn":       {`SELECT ` + lineCols + ` FROM team_task_line WHERE state = 'placed' AND machine = ?`, []any{"m"}},
		"placedElse":     {`SELECT ` + lineCols + ` FROM team_task_line WHERE state = 'placed' AND machine != ?`, []any{"m"}},
		"waiting":        {`SELECT ` + lineCols + ` FROM team_task_line WHERE project = ? AND state = 'queued' ORDER BY front DESC, line, created, id`, []any{"p"}},
		"oldWaiting":     {`SELECT ` + lineCols + ` FROM team_task_line WHERE state = 'queued' AND created < ?`, []any{1}},
		"byOp":           {`SELECT ` + lineCols + ` FROM team_task_line WHERE op = ?`, []any{"x"}},
		"nextLine":       {`SELECT COALESCE(MAX(line), 0) FROM team_task_line WHERE project = ? AND state = 'queued'`, []any{"p"}},
		"recentDone":     {`SELECT ` + lineCols + ` FROM team_task_line WHERE project = ? AND state = 'done' ORDER BY created DESC LIMIT ?`, []any{"p", 20}},
	} {
		rows, err := s.d.Store.Registry().Query("EXPLAIN QUERY PLAN "+q.sql, q.args...)
		if err != nil {
			panic(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if rows.Scan(&id, &parent, &unused, &detail) == nil {
				out[name] = append(out[name], detail)
			}
		}
		rows.Close()
	}
	return out
}

// PlantLostCopy records session on machine as a lost copy (placement op)
// of task.
func PlantLostCopy(s *Service, session, op, project, machine string) {
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	s.addCopy(context.Background(), lineRow{id: strings.SplitN(op, ".", 2)[0], op: op, project: project, machine: machine, session: session})
}

// RecordBetween is recordTask on placement op with between run once
// between its read and its write (a forwarded write landing then).
func RecordBetween(s *Service, op string, t orchapi.Task, between func()) {
	once := false
	recordHook = func() {
		if !once {
			once = true
			between()
		}
	}
	defer func() { recordHook = nil }()
	s.recordTask(context.Background(), op, t)
}

// SetActed sets task id's outward actions as noteOutward would.
func SetActed(s *Service, id, acted string) {
	if _, err := s.d.Store.Registry().Exec(`UPDATE team_task_line SET acted = ? WHERE id = ?`, acted, id); err != nil {
		panic(err)
	}
}

// DropOp deletes op id from teamlink's queue (a crash before it was queued).
func DropOp(s *Service, id string) {
	if _, err := s.d.Store.Registry().Exec(`DELETE FROM team_ops WHERE id = ?`, id); err != nil {
		panic(err)
	}
}

// SetSSHHome points authorize_key ops at dir instead of the user's home
// (the returned func undoes it).
func SetSSHHome(dir string) func() {
	old := sshHome
	sshHome = func() (string, error) { return dir, nil }
	return func() { sshHome = old }
}

// withSessions is d with ss as its session module.
func withSessions(d *core.Deps, ss orchcore.SessionService) *core.Deps {
	orchcore.Wire(d).Sessions = ss
	return d
}

// SetAskStartWithin sets how long an approved run may wait for its machine
// to start it (the returned func undoes it).
func SetAskStartWithin(d time.Duration) func() {
	old := askStartWithin
	askStartWithin = d
	return func() { askStartWithin = old }
}

// SetRunHereHook runs f just before each start placed on this machine runs here; the returned func
// undoes it. Undo only once every start the test caused has run.
func SetRunHereHook(f func()) func() {
	old := testHookRunHere
	testHookRunHere = f
	return func() { testHookRunHere = old }
}
