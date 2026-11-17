package team

// Asks that carry an action (SPEC 16): at a team project's authority, the
// approvals module checks an action ask's machine here when it is made, and
// starts its frozen action here once an owner approved it, as that owner
// (orchcore.AskActions). A run is the remote task start machine choice
// already has (client_request_id ask-<id>, the asking session its parent);
// authorize-key is the authorize_key op, carried by the same queue: the
// target appends exactly the frozen key line to its own user's
// authorized_keys.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// opAuthorizeKey adds one public key line to the target's own user's
// authorized_keys. Payload authorizeKeyOp; report authorizeKeyResult.
const opAuthorizeKey = "authorize_key"

type authorizeKeyOp struct {
	User string `json:"user"`
	Key  string `json:"key"`
}

type authorizeKeyResult struct {
	Added   bool `json:"added,omitempty"`
	Already bool `json:"already,omitempty"`
}

// sshHome is the directory whose .ssh an authorize_key op writes: the
// user's own home (tests point it at a temporary directory).
var sshHome = func() (string, error) { return os.UserHomeDir() }

var _ orchcore.AskActions = (*Service)(nil)

// AskActionMachine implements orchcore.AskActions.
func (s *Service) AskActionMachine(ctx context.Context, project, machine string, scripts bool) (api.MachineStats, error) {
	if machine == "" || machine == "auto" {
		return api.MachineStats{}, api.Errorf(api.CodeBadRequest, "an action names its machine, never auto: the approver approves a place")
	}
	if !s.d.Store.IsTeam(project) {
		return api.MachineStats{}, api.Errorf(api.CodeBadRequest, "an action runs on a machine of a team project; this project has no machines").
			WithHint("ask in a team project, or file the step as work")
	}
	rt, err := s.runner()
	if err != nil {
		return api.MachineStats{}, err
	}
	trusted, public, err := s.TrustedMachines(ctx, project, rt.ProjectMachines(ctx, project))
	if err != nil {
		return api.MachineStats{}, err
	}
	if public[machine] {
		return api.MachineStats{}, teamlink.PublicRefused(machine, "runs approved actions")
	}
	for _, m := range trusted {
		if m.Machine != machine {
			continue
		}
		if !m.Stats.RemoteTasks {
			return api.MachineStats{}, api.Errorf(api.CodeForbidden, "%s takes no tasks from other machines", machine).
				WithHint("its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon")
		}
		if scripts && !m.Stats.ScriptRuns {
			return api.MachineStats{}, api.Errorf(api.CodeUnavailable, "%s's zm runs no scripts", machine).
				WithHint("update zm there (zm daemon update)")
		}
		return m.Stats, nil
	}
	return api.MachineStats{}, api.NotFound("machine", machine)
}

// StartAskAction implements orchcore.AskActions.
func (s *Service) StartAskAction(ctx context.Context, sub *authz.Subject, st orchcore.AskActionStart) (string, error) {
	switch st.Kind {
	case orchapi.ActionRun, orchapi.ActionCommand:
		pa, err := api.ParseAddress(st.Parent)
		if err != nil || pa.Kind != api.KindSession {
			return "", api.Errorf(api.CodeBadRequest, "a run action's asker is a session")
		}
		req := st.Task
		req.ClientRequestID = st.ID
		t, err := s.createOp(ctx, sub, pa.Machine, orchapi.RemoteTaskRequest{Project: st.Project, Machine: st.Machine, Parent: pa.String(), Depth: 1, Request: req}, true, st.AsApprover)
		if err != nil {
			// This very start may be in the line although its answer failed
			// (a read after the insert): answer it, so its state is followed
			// and it is never taken for one that did not start. Only an
			// approved action's own identical request: never another start
			// under its id.
			key := req
			key.Machine, key.Lookup = "", false
			body, _ := json.Marshal(key)
			if !api.IsCode(err, api.CodeConflict) {
				if o, lerr := s.lineByKey(context.WithoutCancel(ctx), st.Project, pa.Machine, pa.String(), st.ID); lerr == nil && o.payload.Ask && o.keyBody == string(body) {
					return o.id, nil
				}
			}
			return "", err
		}
		return t.ID, nil
	case orchapi.ActionAuthorizeKey:
		return s.queueAuthorizeKey(ctx, sub, st)
	}
	return "", api.Errorf(api.CodeBadRequest, "action kind %q starts nothing here", st.Kind)
}

// queueAuthorizeKey queues (or runs here) the authorize_key op of st, once
// per st.ID, as sub: a project owner or admin, onto a trusted machine that
// takes remote tasks.
func (s *Service) queueAuthorizeKey(ctx context.Context, sub *authz.Subject, st orchcore.AskActionStart) (string, error) {
	project, err := s.OwnerProject(ctx, sub, st.Project, "authorizes keys on the project's machines")
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(authorizeKeyOp{User: st.User, Key: st.Key})
	self := s.d.Config.Machine
	if o, err := s.FindOp(ctx, project, self, "", st.ID, opAuthorizeKey); err == nil {
		if o.KeyBody != string(payload) || o.Machine != st.Machine {
			return "", api.Errorf(api.CodeConflict, "request %s was used for a different key", st.ID)
		}
		return o.ID, nil
	} else if !teamlink.IsNoOp(err) {
		return "", err
	}
	if _, err := s.AskActionMachine(ctx, project, st.Machine, false); err != nil {
		return "", err
	}
	op := api.MachineOp{ID: ids.New(), Kind: opAuthorizeKey, From: sub.Identity.Name, Payload: payload}
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: op, Project: project, Machine: st.Machine, FromMachine: self,
		Key: st.ID, KeyBody: string(payload)}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return s.queueAuthorizeKey(ctx, sub, st) // a racing retry: answer the first
		}
		return "", err
	}
	if st.Machine == self {
		s.runHereOffCaller(ctx, project, op) // off the answer request, as createOp's ask starts
	}
	return op.ID, nil
}

// askRequestPrefix starts an approved action's request ids (ask-<id>,
// ask-<id>-<n>), laneRequestPrefix a fresh lane's (relane-<id>): no other
// start may take one.
const (
	askRequestPrefix  = "ask-"
	laneRequestPrefix = "relane-"
)

// AskActionStatus implements orchcore.AskActions.
func (s *Service) AskActionStatus(ctx context.Context, project, id string) (orchcore.AskActionStatus, error) {
	s.ExpireOps(ctx)
	// A run's state is its task's in the waiting line: the parent sees the
	// same. Its first placement's op never learns a refusal or a later
	// placement (a refused action read queued for good).
	if strings.HasPrefix(id, orchapi.RemoteTaskPrefix) {
		l, err := s.loadLine(ctx, id)
		if err == nil {
			if l.project != project {
				return orchcore.AskActionStatus{}, api.NotFound("action", id)
			}
			if l.payload.Ask && s.expireAskStart(ctx, id) {
				if l, err = s.loadLine(ctx, id); err != nil {
					return orchcore.AskActionStatus{}, err
				}
			}
			st := taskActionStatus(s.lineView(ctx, l, false), store.FromMillis(l.updated))
			st.NeverStarted = st.State == orchapi.ActionFailed && s.neverHanded(ctx, l)
			return st, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return orchcore.AskActionStatus{}, err
		}
	}
	o, err := s.LoadOp(ctx, id)
	if err != nil {
		if teamlink.IsNoOp(err) {
			return orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: "its record at the authority is gone"}, nil
		}
		return orchcore.AskActionStatus{}, err
	}
	if o.Project != project {
		return orchcore.AskActionStatus{}, api.NotFound("action", id)
	}
	if o.Kind == opTaskStart {
		var t orchapi.Task
		if err := json.Unmarshal([]byte(o.Status), &t); err != nil {
			return orchcore.AskActionStatus{}, err
		}
		return taskActionStatus(t, o.Updated), nil
	}
	switch {
	case o.State == teamlink.OpPending && !o.Sent:
		return orchcore.AskActionStatus{State: orchapi.ActionQueued}, nil
	case o.State != teamlink.OpDone:
		return orchcore.AskActionStatus{State: orchapi.ActionRunning}, nil
	case o.Error != "":
		at := o.Updated
		return orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: o.Error, Ended: &at}, nil
	}
	at := o.Updated
	return orchcore.AskActionStatus{State: orchapi.ActionDone, Ended: &at}, nil
}

// askStartWithin is one window for an approved run's machine to take it;
// the line places it again each window (opTakeWithin), and it fails after
// askStartTries windows (SPEC 16): a machine busy for a few minutes still
// runs the owner's yes, which never waits for hours and then runs, or
// fails, when nobody expects it.
var askStartWithin = 10 * time.Minute

const askStartTries = 3

// neverHanded reports whether line task l ended refused without any
// machine ever being handed it: no session reported, no cancel, no copy
// recorded anywhere (every placement that may have reached a machine
// leaves one), never sent back after a lost machine, and either never
// placed or placed once, unsent, on a machine other than this one (the
// authority runs its own placements without sending them). Anything else
// may have started, so it is false.
func (s *Service) neverHanded(ctx context.Context, l lineRow) bool {
	if l.state != lineDone || l.task().State != orchapi.TaskRefused || l.session != "" || l.cancelled || l.requeues > 0 || l.lostOn != "" || l.acted != "" {
		return false
	}
	var copies int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM team_task_copies WHERE task = ?`, l.id).Scan(&copies); err != nil || copies > 0 {
		return false
	}
	if l.op == "" {
		return l.places == 0
	}
	if l.machine == "" || l.machine == s.d.Config.Machine || l.places != 1 {
		return false
	}
	o, err := s.LoadOp(ctx, l.op)
	return err == nil && !o.Sent && o.Machine == l.machine
}

// AskActionPin implements orchcore.AskActions: the session module's live
// check of a run's ref here; not checked when it has none.
func (s *Service) AskActionPin(ctx context.Context, project, ref, commit string) (bool, error) {
	rt, err := s.runner()
	if err != nil {
		return false, nil
	}
	pc, ok := rt.(orchcore.RunPinChecker)
	if !ok {
		return false, nil
	}
	return pc.CheckRunPin(ctx, project, ref, commit)
}

// expireAskStart ends approved run l refused when its machine has not
// taken it within askStartWithin of entering the line (a requeue counts
// from then): still waiting there, or placed on another machine and not
// carried to it yet. Never a start the machine may have begun (carried, or
// on the authority's own machine): the line's lost-machine rules decide
// those. Under lineMu, on a fresh read; the reason is the row's, so every
// later read says the same. False when it ended nothing.
func (s *Service) expireAskStart(ctx context.Context, id string) bool {
	ctx = context.WithoutCancel(ctx)
	ended, project := false, ""
	func() {
		s.lineMu.Lock()
		defer s.lineMu.Unlock()
		l, err := s.loadLine(ctx, id)
		if err != nil || !l.payload.Ask || l.session != "" || l.task().State != orchapi.TaskPending {
			return
		}
		since := store.FromMillis(l.created)
		if l.requeues > 0 {
			since = store.FromMillis(l.updated)
		}
		total := askStartWithin * askStartTries
		if time.Since(since) < total {
			return
		}
		why := l.pin + " did not take it within " + total.String() + " (busy, at its limit, or offline): zm cancelled it before it ran"
		switch l.state {
		case lineQueued:
		case linePlaced:
			if l.machine == s.d.Config.Machine {
				return
			}
			// Only while no answer carried it: the machine never saw it.
			if ok, err := s.UpdateOp(ctx, l.op, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: &why, Unsent: true}); err != nil || !ok {
				return
			}
		default:
			return
		}
		ended, project = s.endLine(ctx, l, refused(l.task(), why)), l.project
	}()
	if ended {
		s.placeLine(ctx, project, false) // its place there is free again
	}
	return ended
}

// taskActionStatus is a remote task's status as an action's state: done
// only for an ok outcome, else failed with how it ended and its last lines.
func taskActionStatus(t orchapi.Task, updated time.Time) orchcore.AskActionStatus {
	switch t.State {
	case orchapi.TaskPending:
		return orchcore.AskActionStatus{State: orchapi.ActionQueued}
	case orchapi.TaskRefused:
		return orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: first(t.Error, "refused"), Ended: &updated}
	case "ended":
	default:
		return orchcore.AskActionStatus{State: orchapi.ActionRunning}
	}
	if t.Result != nil && t.Result.Outcome.Kind == orchapi.OutcomeOK && (t.Result.Outcome.ExitCode == nil || *t.Result.Outcome.ExitCode == 0) {
		return orchcore.AskActionStatus{State: orchapi.ActionDone, Ended: &updated}
	}
	why := "it ended without a result"
	if r := t.Result; r != nil {
		why = "it ended " + string(r.Outcome.Kind)
		if c := r.Outcome.ExitCode; c != nil {
			why = "it exited " + itoa(*c)
		}
		if r.Outcome.Signal != "" {
			why = "it was killed (" + r.Outcome.Signal + ")"
		}
		if tail := lastLines(r.Text, 3, 300); tail != "" {
			why += ": " + tail
		}
	}
	if t.Lost != "" {
		why = "its machine was lost: " + t.Lost
	}
	return orchcore.AskActionStatus{State: orchapi.ActionFailed, Reason: why, Ended: &updated}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// lastLines is text's last n non-empty lines joined by " / ", at most max
// bytes (cut from the front).
func lastLines(text string, n, max int) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	out := strings.Join(keep, " / ")
	if len(out) > max {
		out = "…" + strings.ToValidUTF8(out[len(out)-max:], "")
	}
	return out
}

func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// registerAskActionOps registers authorize_key with teamlink's queue.
func (s *Service) registerAskActionOps() {
	s.RegisterOp(opAuthorizeKey, teamlink.OpHandler{
		Run:       s.runAuthorizeKey,
		Expiry:    opTakeWithin,
		ReportNow: true,
		Expired: func(ctx context.Context, o teamlink.Op, _ string) {
			why := o.Machine + " never took it within " + opTakeWithin.String()
			_, _ = s.UpdateOp(ctx, o.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: &why, NotDone: true})
		},
	})
}

// Key types an authorize_key op adds (the approvals module checks the key
// in full when the ask is made; this machine checks its shape again).
var keyTypes = map[string]bool{"ssh-ed25519": true, "ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true, "ssh-rsa": true}

// runAuthorizeKey runs an authorize_key op here: only when this machine
// takes remote tasks, only for the user zm runs as (zm never uses sudo).
// It creates .ssh (0700) and authorized_keys (0600) when missing, and adds
// the line once.
func (s *Service) runAuthorizeKey(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error) {
	if !s.d.Config.File.RemoteTasks {
		return nil, api.Errorf(api.CodeForbidden, "%s takes no tasks from other machines", s.d.Config.Machine)
	}
	var p authorizeKeyOp
	if json.Unmarshal(op.Payload, &p) != nil {
		return nil, api.Errorf(api.CodeBadRequest, "authorize_key without a request")
	}
	f := strings.Fields(p.Key)
	if strings.ContainsAny(p.Key, "\r\n") || len(f) < 2 || !keyTypes[f[0]] {
		return nil, api.Errorf(api.CodeBadRequest, "not one public key line")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) < 4+len(f[0]) || string(blob[4:4+len(f[0])]) != f[0] {
		return nil, api.Errorf(api.CodeBadRequest, "not one public key line")
	}
	me, err := user.Current()
	if err != nil {
		return nil, err
	}
	if p.User != me.Username {
		return nil, api.Errorf(api.CodeForbidden, "zm runs as %s on %s, not %s: ask that machine's owner to run zm as that user, or pick its user", me.Username, s.d.Config.Machine, p.User)
	}
	home, err := sshHome()
	if err != nil {
		return nil, err
	}
	already, err := appendAuthorizedKey(home, p.Key, f[0], f[1])
	if err != nil {
		return nil, err
	}
	if already {
		b, _ := json.Marshal(authorizeKeyResult{Already: true})
		return b, nil
	}
	b, _ := json.Marshal(authorizeKeyResult{Added: true})
	return b, nil
}

// appendAuthorizedKey adds line to home's .ssh/authorized_keys unless a line
// there already carries the key (typ, b64), past any options. It creates
// .ssh (0700) and the file (0600) when missing.
func appendAuthorizedKey(home, line, typ, b64 string) (already bool, err error) {
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	path := filepath.Join(dir, "authorized_keys")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(old))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		have := strings.Fields(sc.Text())
		for i := 0; i+1 < len(have); i++ {
			if have[i] == typ && have[i+1] == b64 {
				return true, nil
			}
		}
	}
	add := strings.TrimSpace(line) + "\n"
	if len(old) > 0 && old[len(old)-1] != '\n' {
		add = "\n" + add
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := fh.WriteString(add); err != nil {
		fh.Close()
		return false, err
	}
	return false, fh.Close()
}
