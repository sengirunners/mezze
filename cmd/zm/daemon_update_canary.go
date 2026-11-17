package main

// The canary rollout's dry run (SPEC 2, "update-rollout.json"). At each release update
// check zm records what a canary rollout would have done on this box: its
// role, the release it would take, whether the box is idle and why not, the
// stage it is in, the health check it would run and the rollback it would
// make. It does none of it: the tick's own result is unchanged, and any
// failure here is a line on stderr, never an error. Roles and the process
// list are settings (update-rollout.json in ZM_HOME); no machine name, no
// process name is in the code.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/swap"
)

const (
	rolloutSettingsFile = "update-rollout.json"
	rolloutDryRunFile   = "update-rollout-dryrun.jsonl"
	// rolloutDryRunMax bounds the log: past it the file is kept as .1 (one
	// older file) and a new one starts.
	rolloutDryRunMax = 2 << 20
)

// Roles a box has in the rollout.
const (
	roleCanary    = "canary"
	roleMember    = "member"
	roleAuthority = "authority"
)

// rolloutSettings says which machines take which part of the rollout and
// which processes mean a box is busy. Everything is data: an owner writes it.
type rolloutSettings struct {
	// Canary boxes take a release first. Members follow once a canary lists
	// the release as stable. Authority boxes (the machine the project's
	// card judge and permit checks run on) go last, once every reachable,
	// unheld member lists it stable. A machine in no list is not part of the
	// rollout (an owner's Mac stays a yes each time).
	Canary    []string `json:"canary"`
	Members   []string `json:"members"`
	Authority []string `json:"authority"`
	// IdleProcesses are executable names (a process's command, its base
	// name, exactly) that mean the box is busy: agents, test runners, the
	// farm's job runner.
	IdleProcesses []string `json:"idle_processes"`
}

// loadRolloutSettings reads home's settings. ok is false when there is no
// file; an unreadable or contradictory one is an error (nothing is judged
// from it).
func loadRolloutSettings(home string) (s rolloutSettings, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(home, rolloutSettingsFile))
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return rolloutSettings{}, false, fmt.Errorf("%s: %w", rolloutSettingsFile, err)
	}
	seen := map[string]string{}
	for role, names := range map[string][]string{roleCanary: s.Canary, roleMember: s.Members, roleAuthority: s.Authority} {
		for _, n := range names {
			if n == "" {
				return rolloutSettings{}, false, fmt.Errorf("%s: an empty machine name in %s", rolloutSettingsFile, role)
			}
			if prev, dup := seen[n]; dup {
				return rolloutSettings{}, false, fmt.Errorf("%s: machine %q is listed as %s and as %s", rolloutSettingsFile, n, prev, role)
			}
			seen[n] = role
		}
	}
	return s, true, nil
}

// roleOf is machine's role, "" when it takes no part.
func (s rolloutSettings) roleOf(machine string) string {
	switch {
	case slices.Contains(s.Authority, machine):
		return roleAuthority
	case slices.Contains(s.Canary, machine):
		return roleCanary
	case slices.Contains(s.Members, machine):
		return roleMember
	}
	return ""
}

// idleFacts are what the box shows about itself.
type idleFacts struct {
	SessionsKnown bool
	Sessions      int      // live managed sessions (zm tasks run in them)
	ProcessesErr  string   // set when the process list could not be read
	Running       []string // configured process names found running
}

// judgeIdle: idle means no zm session, no process from the configured list,
// and everything known. What cannot be read is not idle.
func judgeIdle(f idleFacts) (bool, []string) {
	var why []string
	switch {
	case !f.SessionsKnown:
		why = append(why, "the box's session count is unknown")
	case f.Sessions > 0:
		why = append(why, fmt.Sprintf("%d managed zm sessions are live", f.Sessions))
	}
	if f.ProcessesErr != "" {
		why = append(why, "the process list could not be read: "+f.ProcessesErr)
	}
	for _, p := range f.Running {
		why = append(why, "process "+p+" is running")
	}
	return len(why) == 0, why
}

// runningProcesses reads the box's processes (tests replace it): each one's
// command line, as ps prints it. The command line, not ps's short command
// name: Linux cuts that to 15 characters, and a program run by an
// interpreter shows only as the interpreter.
var runningProcesses = func() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "args=").Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(string(out), "\n"), nil
}

// configuredRunning are the names in want that run on the box. A process
// matches a name when the base name of any word of its command line (the
// program, an interpreter's script, a flag's path) equals the name exactly:
// `test-runner-helper` is not `test-runner`. Over-matching (an editor open on
// a file of that name) only makes the box look busy, which is the safe way to
// be wrong; missing one would not be.
func configuredRunning(want []string) (found []string, errText string) {
	if len(want) == 0 {
		return nil, ""
	}
	procs, err := runningProcesses()
	if err != nil {
		return nil, err.Error()
	}
	have := map[string]bool{}
	for _, p := range procs {
		for _, w := range strings.Fields(p) {
			have[filepath.Base(w)] = true
		}
	}
	for _, w := range want {
		if have[w] {
			found = append(found, w)
		}
	}
	return found, ""
}

// rolloutStage says whether the rollout has reached role's box for version,
// from the machine numbers: a canary is always first; a member waits for a
// canary's stable; the authority waits for every reachable, unheld member.
// Any machine that lists the release as failed stops it for all.
func rolloutStage(s rolloutSettings, role, version string, ms []api.Machine) (bool, string) {
	by := map[string]*api.MachineZm{}
	for _, m := range ms {
		if z := m.Stats.Build(); z != nil {
			by[m.Machine] = z
			if slices.Contains(z.Failed, version) {
				return false, fmt.Sprintf("release %s failed on %s", version, m.Machine)
			}
		}
	}
	stable := func(name string) bool { z := by[name]; return z != nil && slices.Contains(z.Stable, version) }
	if role == roleCanary {
		return true, "canaries go first"
	}
	canaryOK := false
	for _, c := range s.Canary {
		if stable(c) {
			canaryOK = true
		}
	}
	if !canaryOK {
		return false, fmt.Sprintf("no canary lists %s as stable yet", version)
	}
	if role == roleMember {
		return true, "a canary lists it as stable"
	}
	present := map[string]bool{}
	for _, m := range ms {
		present[m.Machine] = true
	}
	var waiting []string
	for _, n := range s.Members {
		z := by[n]
		if !present[n] || (z != nil && z.Held != "") {
			continue // unreachable or held: it does not stop the authority
		}
		if !stable(n) { // a reachable member that reports no build (an older zm) cannot vouch: it waits
			waiting = append(waiting, n)
		}
	}
	if len(waiting) > 0 {
		return false, fmt.Sprintf("members not yet stable on %s: %s", version, strings.Join(waiting, ", "))
	}
	return true, "a canary and every reachable member list it as stable"
}

// dryInputs is everything a record is made from.
type dryInputs struct {
	At         time.Time
	Machine    string
	Dir        string // the install directory (where the .prev would be)
	Settings   *rolloutSettings
	SettingErr string
	Running    api.Health
	// Tick is what the real update check did.
	TickResult, TickReason, TickErr string
	Listing                         []listed
	ListErr                         string
	State                           release.State
	HaveState                       bool
	Machines                        []api.Machine
	MachinesErr                     string
	Idle                            idleFacts
	PrevExists                      bool
	Held                            string // why this box's updates are held (the hold switch, a hold file), or ""
}

// dryRecord is one line of the log.
type dryRecord struct {
	At        time.Time `json:"at"`
	Machine   string    `json:"machine"`
	Role      string    `json:"role,omitempty"`
	Running   string    `json:"running"`
	Tick      dryTick   `json:"tick"`
	Candidate string    `json:"candidate,omitempty"`
	Idle      *dryIdle  `json:"idle,omitempty"`
	Stage     *dryStage `json:"stage,omitempty"`
	Would     string    `json:"would"` // install, wait, skip or nothing
	Why       string    `json:"why"`
	Health    *dryCheck `json:"health_check,omitempty"`
	Rollback  *dryRoll  `json:"rollback,omitempty"`
	Order     []dryBox  `json:"order,omitempty"`
}

type dryTick struct {
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

type dryIdle struct {
	Idle    bool     `json:"idle"`
	Reasons []string `json:"reasons,omitempty"`
}

type dryStage struct {
	Open bool   `json:"open"`
	Why  string `json:"why"`
}

// dryCheck is the health check the install would be held to, and what it
// reads on the running daemon now.
type dryCheck struct {
	Would []string `json:"would"`
	Now   struct {
		OK       bool     `json:"ok"`
		Problems []string `json:"problems,omitempty"`
	} `json:"now"`
}

type dryRoll struct {
	Would string `json:"would"`
	Prev  bool   `json:"prev_file"`
}

// dryBox is one machine as the machine numbers show it, in rollout order.
type dryBox struct {
	Machine string `json:"machine"`
	Role    string `json:"role"`
	Version string `json:"version,omitempty"`
	Stable  bool   `json:"stable,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
	Held    string `json:"held,omitempty"`
	// Missing: the settings list this machine, but the machine numbers do not
	// (offline, or the name is wrong).
	Missing bool `json:"missing,omitempty"`
}

var roleOrder = map[string]int{roleCanary: 0, roleMember: 1, roleAuthority: 2}

// makeDryRecord decides what the canary rollout would have done. It reads
// its inputs and nothing else: it changes nothing.
func makeDryRecord(in dryInputs) dryRecord {
	rec := dryRecord{At: in.At.UTC(), Machine: in.Machine, Running: in.Running.Version + " (" + in.Running.Commit + ")",
		Tick: dryTick{Result: in.TickResult, Reason: in.TickReason, Error: in.TickErr}}
	if in.Settings == nil {
		rec.Would, rec.Why = "nothing", "no rollout settings ("+rolloutSettingsFile+"): roles are unknown"
		if in.SettingErr != "" {
			rec.Why = "rollout settings unusable: " + in.SettingErr
		}
		return rec
	}
	s := *in.Settings
	rec.Role = s.roleOf(in.Machine)
	if rec.Role == "" {
		rec.Would, rec.Why = "skip", "this machine is in no rollout list: it is not part of the farm rollout and stays as it is"
		return rec
	}
	if in.ListErr != "" {
		rec.Would, rec.Why = "nothing", "the release listing failed: "+in.ListErr
		return rec
	}
	// The candidate: the newest release this home would take if no age ring
	// or vouch applied (the rollout's own stage is the gate in its place).
	c := chooseRelease(in.Listing, in.Running.Commit, in.State, in.HaveState, 0, in.At, nil)
	if !c.install {
		rec.Would, rec.Why = "nothing", firstOf(c.reason, "the running build is the newest release")
		return rec
	}
	rec.Candidate = c.pick.Version
	idle, why := judgeIdle(in.Idle)
	rec.Idle = &dryIdle{Idle: idle, Reasons: why}
	switch {
	case in.MachinesErr != "":
		rec.Stage = &dryStage{Why: "machine numbers unreadable: " + in.MachinesErr}
	default:
		open, w := rolloutStage(s, rec.Role, c.pick.Version, in.Machines)
		rec.Stage = &dryStage{Open: open, Why: w}
	}
	switch {
	case !rec.Stage.Open:
		rec.Would, rec.Why = "wait", "the rollout has not reached this box: "+rec.Stage.Why
	case in.Held != "":
		rec.Would, rec.Why = "wait", "this box's updates are held: "+in.Held
	case !idle:
		rec.Would, rec.Why = "wait", "the box is busy: "+strings.Join(why, "; ")
	default:
		rec.Would, rec.Why = "install", "the stage is open and the box is idle"
	}
	rec.Health = &dryCheck{Would: []string{
		"the daemon answers and runs the release's commit",
		"its self-check reports no problem",
		"the last swap record is an upgrade that ended ok",
		fmt.Sprintf("no daemon restart for %s (the soak), health read again at its end", release.Soak),
	}}
	rec.Health.Now.OK = in.Running.OK && (in.Running.SelfCheck == nil || in.Running.SelfCheck.OK)
	if in.Running.SelfCheck != nil {
		rec.Health.Now.Problems = in.Running.SelfCheck.Problems
	}
	rec.Rollback = &dryRoll{Prev: in.PrevExists, Would: "the guarded swap puts the old build back if the verify fails; if the soak check fails, the updater runs the same guarded swap to the previous build and records the release failed here (a veto for every other box)"}
	if !in.PrevExists {
		rec.Rollback.Would = "no .prev file beside the installed zm: a rollback would first fetch the previous release; " + rec.Rollback.Would
	}
	for _, m := range in.Machines {
		role := s.roleOf(m.Machine)
		if role == "" {
			continue
		}
		b := dryBox{Machine: m.Machine, Role: role}
		if z := m.Stats.Build(); z != nil {
			b.Version, b.Held = z.Version, z.Held
			b.Stable, b.Failed = slices.Contains(z.Stable, c.pick.Version), slices.Contains(z.Failed, c.pick.Version)
		}
		rec.Order = append(rec.Order, b)
	}
	listed := map[string]bool{}
	for _, m := range in.Machines {
		listed[m.Machine] = true
	}
	for _, names := range [][]string{s.Canary, s.Members, s.Authority} {
		for _, n := range names {
			if !listed[n] {
				rec.Order = append(rec.Order, dryBox{Machine: n, Role: s.roleOf(n), Missing: true})
			}
		}
	}
	slices.SortStableFunc(rec.Order, func(a, b dryBox) int {
		if d := roleOrder[a.Role] - roleOrder[b.Role]; d != 0 {
			return d
		}
		return strings.Compare(a.Machine, b.Machine)
	})
	return rec
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// appendDryRecord writes rec as one line, keeping the file under
// rolloutDryRunMax.
func appendDryRecord(home string, rec dryRecord) error {
	path := filepath.Join(home, rolloutDryRunFile)
	if fi, err := os.Stat(path); err == nil && fi.Size() > rolloutDryRunMax {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// listRecorder is a release source that remembers what its listing said, so
// the dry run reads the listing the tick read and asks GitHub nothing more.
type listRecorder struct {
	releaseSource
	seen []listed
	err  error
}

func (l *listRecorder) list() ([]listed, error) {
	rs, err := l.releaseSource.list()
	l.seen, l.err = rs, err
	return rs, err
}

// usableMachines is the machine numbers the stage may be judged from: none
// when they could not be read or are incomplete (a machine the authority could
// not merge might hold a failure), with why.
func usableMachines(ml api.MachineList, err error) ([]api.Machine, string) {
	switch {
	case err != nil:
		return nil, err.Error()
	case len(ml.Skipped) > 0:
		return ml.Machines, "machine numbers incomplete: " + ml.Skipped[0].Error
	}
	return ml.Machines, ""
}

// dryRunRollout records what the canary rollout would have done at this
// tick. Best effort and read-only: a problem is one line on w.
func dryRunRollout(ctx context.Context, w io.Writer, c *client.Client, home, dir string, health api.Health, rl *listRecorder, st release.State, have bool, result, reason string, tickErr error, now time.Time) {
	// It must never end the update check: a panic here is one stderr line.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(w, "zm: canary dry run:", r)
		}
	}()
	in := dryInputs{At: now, Machine: health.Machine, Dir: dir, Running: health, TickResult: result, TickReason: reason,
		Listing: rl.seen, State: st, HaveState: have}
	if tickErr != nil {
		in.TickResult, in.TickErr = "failed", tickErr.Error()
	}
	if rl.err != nil {
		in.ListErr = rl.err.Error()
	}
	s, ok, err := loadRolloutSettings(home)
	if err == nil && !ok {
		return // no settings, no rollout: nothing to say, and no line per check on every home
	}
	switch {
	case err != nil:
		in.SettingErr = err.Error()
	case ok:
		in.Settings = &s
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ml, e := c.Machines(mctx)
		in.Machines, in.MachinesErr = usableMachines(ml, e)
		for _, m := range in.Machines {
			if m.Machine == health.Machine {
				in.Idle.SessionsKnown, in.Idle.Sessions = m.Stats.Sessions >= 0, m.Stats.Sessions
			}
		}
		in.Idle.Running, in.Idle.ProcessesErr = configuredRunning(s.IdleProcesses)
		in.Held, _ = updateHeld(now, release.HoldPath(home), "")
		if _, e := os.Stat(filepath.Join(dir, "zm"+swap.PrevSuffix)); e == nil {
			in.PrevExists = true
		}
	}
	if err := appendDryRecord(home, makeDryRecord(in)); err != nil {
		fmt.Fprintln(w, "zm: canary dry-run log:", err)
	}
}
