package team

// The Mezze app on a machine (SPEC 8 "Mezze app on a machine"): the owner of a
// team project starts (or reuses) a Mezze app server on another machine of it and
// gets a one-time pairing credential for it, with no ssh and no pairing URL.
// It rides the machine-ops queue: this machine's owner asks on its socket
// (POST /v0/team/machines/{machine}/t3), a replica sends it on to the
// project's authority, which queues a t3_ensure op the machine takes with
// its next heartbeat answer. A machine runs it only with a "t3" key in its
// config ("t3": {} takes every default; zm t3 enable writes it): from a
// t3code checkout (cloned into <zm home>/t3code when missing) at a pinned
// main commit, or origin/main's tip with no pin, it runs
// `node apps/server/src/bin.ts serve --lan` (fetching, checking out and
// `vp i` first when the pin moved), then mints the credential with that
// server's own `auth pairing create --json`. The credential is the secret
// here: the authority hands it to the requester once and blanks it then (or
// after t3Keep), and the machine blanks its copy once an answer took it.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// Tunables (tests shorten them).
var (
	// t3Wait: how long the authority waits for the machine's answer (under
	// a replica's CallTimeout, which forwards the call).
	t3Wait = 25 * time.Second
	// t3Keep: an answer nobody collected has its credential blanked then
	// (the credential itself dies after 5 minutes).
	t3Keep = 10 * time.Minute
	// t3StartWait: a server zm started must answer on loopback by then.
	t3StartWait = 90 * time.Second
	// t3InstallWait bounds `vp i`.
	t3InstallWait = 15 * time.Minute
	// t3CloneWait bounds the first clone.
	t3CloneWait = 10 * time.Minute
	// t3Repo is what a missing checkout is cloned from, with the machine's
	// own git credentials (tests: a local repository): t3_repo.txt, which
	// the public copy carries blank (internal/deps/publiccopy), so a public
	// build clones nothing and asks for a checkout.
	t3Repo = strings.TrimSpace(t3RepoFile)
	// t3DiskFloorMB: a clone or an install is refused with less free disk
	// on the checkout's volume (a checkout with node_modules takes a few GB).
	t3DiskFloorMB int64 = 8 << 10
	// t3DiskFree is the free MB on path's volume (tests replace it).
	t3DiskFree = func(path string) (int64, error) {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(path, &fs); err != nil {
			return 0, err
		}
		return int64(uint64(fs.Bavail) * uint64(fs.Bsize) >> 20), nil
	}
)

const (
	t3DefaultPort = 3773
	t3TTL         = "5m"
	t3State       = "t3.json"           // in zm's home: what zm started
	t3Log         = "zm-t3.log"         // in the server's home
	t3Installed   = ".zm-installed"     // in node_modules: the commit an install finished at
	t3Checkout    = orchcore.T3Checkout // the default checkout, in zm's home
)

var (
	t3ScopeRe  = regexp.MustCompile(`^[a-z][a-z-]{0,31}:[a-z][a-z-]{0,31}$`)
	t3LabelRe  = regexp.MustCompile(`^zm: [A-Za-z0-9._-]{1,64}$`)
	t3CommitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// tailnet addresses (CGNAT), listed before private LAN ones.
	t3Tailnet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
)

//go:embed t3_repo.txt
var t3RepoFile string

// t3EnsureHook and t3StatsHook replace this machine's Mezze app runtime (tests).
var (
	t3EnsureHook func(ctx context.Context, c config.T3, op orchapi.T3EnsureOp) (orchapi.T3Environment, error)
	t3StatsHook  func(c config.T3) *api.MachineT3
)

var _ orchcore.MachineT3 = (*Service)(nil)

func t3Refused(machine string) error {
	return api.Errorf(api.CodeForbidden, "%s runs no Mezze app for zm", machine).
		WithHint("its owner runs zm t3 enable there (or adds \"t3\": {} to its zm config.json and restarts its daemon)")
}

// T3Block is this machine's t3 block and whether its owner set one: a
// block naming a checkout, or a "t3" key holding an object ("t3": {} is
// every default, which config.T3 alone cannot tell from no key), read once
// per daemon as it is built (a change takes a restart, as zm t3 enable
// does). An empty block is filled with the default checkout in config.json
// then, before any module saves the file and drops it (orchcore.FillT3).
func (s *Service) T3Block() (config.T3, bool) {
	h := &s.t3
	h.once.Do(func() {
		h.block = s.d.Config.File.T3
		path := s.d.Config.Path(config.FileConfig)
		h.on = h.block.Enabled() || t3KeySet(path)
		if h.on && !h.block.Enabled() {
			if _, err := orchcore.FillT3(s.d.Config.Home); err != nil && s.d.Log != nil {
				s.d.Log.Warn("t3: filling the empty t3 block in config.json", "err", err)
			}
		}
	})
	return h.block, h.on
}

// T3Enabled implements orchcore.MachineT3.
func (s *Service) T3Enabled() bool {
	_, on := s.T3Block()
	return on
}

// t3KeySet: config.json at path has a "t3" key holding an object.
func t3KeySet(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return false
	}
	raw := bytes.TrimSpace(doc["t3"])
	return len(raw) > 0 && raw[0] == '{'
}

// t3Scopes checks requested scope names (T3 checks them again): at most 32,
// each a T3 scope's shape, duplicates dropped.
func t3Scopes(in []string) ([]string, error) {
	if len(in) > 32 {
		return nil, api.Errorf(api.CodeBadRequest, "at most 32 scopes")
	}
	var out []string
	seen := map[string]bool{}
	for _, sc := range in {
		if !t3ScopeRe.MatchString(sc) {
			return nil, api.Errorf(api.CodeBadRequest, "%q is not a T3 scope name (such as orchestration:read)", sc)
		}
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	return out, nil
}

// t3Routes registers the route (this machine's socket, and the authority's
// side of it).
func (s *Service) t3Routes(r *core.Router) {
	r.HandleFunc("POST /v0/team/machines/{machine}/t3", core.Authed, "this machine's owner on its socket (sent on to a joined project's authority), or at the authority a replica's node-bound token; its identity owns or administers the team project; the machine has a t3 block; each credential answered once", jsonH(s.handleT3))
}

// handleT3 is POST /v0/team/machines/{machine}/t3.
func (s *Service) handleT3(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	replica := sub != nil && sub.Node != "" && sub.Machine != ""
	if !replica && (sub == nil || sub.Via != authz.ViaLocal) {
		return nil, api.Errorf(api.CodeForbidden, "only this machine's owner, on its socket, starts the Mezze app on a machine").
			WithHint("run it here: zm t3 <machine> -P <project>")
	}
	var req orchapi.T3EnsureRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	machine := r.PathValue("machine")
	if req.Project == "" {
		return nil, api.Errorf(api.CodeBadRequest, "name the team project %s is in", machine).WithHint("zm t3 <machine> -P <project>")
	}
	if replica {
		return s.ensureT3(ctx, sub, sub.Machine, machine, req)
	}
	if x := s.ByName(req.Project); x != nil {
		snap := x.Snapshot()
		if err := teamlink.Ended(snap); err != nil {
			return nil, err
		}
		fwd := req
		fwd.Project = snap.Project.ID
		var out orchapi.T3Environment
		err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/machines/"+url.PathEscape(machine)+"/t3", fwd, &out)
		return out, teamlink.OlderFor(err, x, "the Mezze app on a machine")
	}
	return s.ensureT3(ctx, sub, s.d.Config.Machine, machine, req)
}

// ---- the authority ----

// t3Result is a t3_ensure op's outcome as team_ops keeps it.
type t3Result struct {
	Code string                 `json:"code,omitempty"`
	Hint string                 `json:"hint,omitempty"`
	Env  *orchapi.T3Environment `json:"env,omitempty"`
}

// ensureT3 queues a t3_ensure op on machine for from's owner (run here at
// once when it is this machine) and waits up to t3Wait for its answer.
func (s *Service) ensureT3(ctx context.Context, sub *authz.Subject, from, machine string, req orchapi.T3EnsureRequest) (orchapi.T3Environment, error) {
	rt, err := s.runner()
	if err != nil {
		return orchapi.T3Environment{}, err
	}
	project, err := s.OwnerProject(ctx, sub, req.Project, "starts the Mezze app on other machines")
	if err != nil {
		return orchapi.T3Environment{}, err
	}
	scopes, err := t3Scopes(req.Scopes)
	if err != nil {
		return orchapi.T3Environment{}, err
	}
	found := false
	for _, m := range rt.ProjectMachines(ctx, project) {
		if m.Machine != machine {
			continue
		}
		found = true
		if c, err := s.MachineClass(ctx, project, machine); err != nil {
			return orchapi.T3Environment{}, err
		} else if c.Class != api.MachineTrusted {
			return orchapi.T3Environment{}, teamlink.PublicRefused(machine, "runs the Mezze app")
		}
		if !m.Stats.T3Enabled {
			return orchapi.T3Environment{}, t3Refused(machine)
		}
		if m.Machine != s.d.Config.Machine && (m.Seen == nil || time.Since(*m.Seen) > opSeenWithin) {
			return orchapi.T3Environment{}, api.Errorf(api.CodeUnavailable, "%s has not been heard from in %s", machine, opSeenWithin)
		}
	}
	if !found {
		return orchapi.T3Environment{}, api.NotFound("machine", machine)
	}
	s.ExpireOps(ctx)
	payload, _ := json.Marshal(orchapi.T3EnsureOp{Label: "zm: " + from, Scopes: scopes})
	op := api.MachineOp{ID: ids.New(), Kind: opT3Ensure, From: sub.Identity.Name, Payload: payload}
	s.t3Waiting.Store(op.ID, true)
	defer s.endT3Wait(ctx, op.ID)
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: op, Project: project, Machine: machine, FromMachine: from}); err != nil {
		return orchapi.T3Environment{}, err
	}
	if machine == s.d.Config.Machine {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.RunHere(ctx, project, op)
		}()
	}
	return s.collectT3(ctx, op.ID, machine)
}

// collectT3 waits for op id's answer and hands it out once: the stored
// credential is blanked as it is read.
func (s *Service) collectT3(ctx context.Context, id, machine string) (orchapi.T3Environment, error) {
	deadline := time.Now().Add(t3Wait)
	for {
		o, err := s.LoadOp(ctx, id)
		if err != nil {
			return orchapi.T3Environment{}, err
		}
		if o.State == teamlink.OpDone {
			if _, err := s.UpdateOp(ctx, id, teamlink.OpUpdate{Result: teamlink.Str("")}); err != nil {
				return orchapi.T3Environment{}, err
			}
			var res t3Result
			_ = json.Unmarshal([]byte(o.Result), &res)
			if o.Error != "" {
				code := res.Code
				if code == "" {
					code = api.CodeUnavailable
				}
				return orchapi.T3Environment{}, &api.Error{Code: code, Message: o.Error, Hint: res.Hint}
			}
			if res.Env == nil {
				return orchapi.T3Environment{}, api.Errorf(api.CodeUnavailable, "%s answered no Mezze app server", machine)
			}
			env := *res.Env
			env.Machine = machine
			return env, nil
		}
		if time.Now().After(deadline) {
			return orchapi.T3Environment{}, api.Errorf(api.CodeUnavailable, "%s did not answer within %s", machine, t3Wait).
				WithHint("the Mezze app may still be starting there (a first start installs its dependencies): ask again; a running server only mints a new credential")
		}
		select {
		case <-ctx.Done():
			return orchapi.T3Environment{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// endT3Wait: nobody waits for op id any more (answered, timed out, or the
// requester gone). The op is closed with no credential kept: one stored
// before this is blanked, and applyT3 fills a done op never again.
func (s *Service) endT3Wait(ctx context.Context, id string) {
	s.t3Waiting.Delete(id)
	ctx = context.WithoutCancel(ctx)
	_, _ = s.UpdateOp(ctx, id, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: teamlink.Str("nobody waited for the answer any more"), NotDone: true})
	_, _ = s.UpdateOp(ctx, id, teamlink.OpUpdate{Result: teamlink.Str("")})
}

// applyT3 records a t3_ensure op's report. One that is done already (its
// answer collected, its requester gone, or never taken in time) is not
// filled again: a late report, or one resent because its ack was lost,
// never brings a credential back. Nor is a credential kept for an op no
// requester waits for on this daemon (one that restarted since).
func (s *Service) applyT3(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	var env *orchapi.T3Environment
	if len(rep.Payload) > 0 {
		var e orchapi.T3Environment
		if json.Unmarshal(rep.Payload, &e) == nil {
			env = &e
		}
	}
	if _, waiting := s.t3Waiting.Load(o.ID); !waiting && env != nil {
		env.Credential = ""
	}
	b, _ := json.Marshal(t3Result{Code: rep.Code, Hint: rep.Hint, Env: env})
	_, _ = s.UpdateOp(ctx, o.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Result: teamlink.Str(string(b)), Error: &rep.Error, NotDone: true})
}

// opT3Ensure starts (or finds) the Mezze app on the machine and mints a credential
// (SPEC 8 "Mezze app on a machine"): payload orchapi.T3EnsureOp, report
// orchapi.T3Environment with the credential, blanked once taken.
const opT3Ensure = "t3_ensure"

// registerT3Ops registers t3_ensure with teamlink's queue.
func (s *Service) registerT3Ops() {
	s.RegisterOp(opT3Ensure, teamlink.OpHandler{Run: s.runT3, Apply: s.applyT3, Expiry: relayOpTake, BlankOnAck: true, Keep: t3Keep})
}

// ---- the machine that runs it ----

// T3Stats implements orchcore.MachineT3.
func (s *Service) T3Stats(ctx context.Context) *api.MachineT3 {
	c, on := s.T3Block()
	if !on {
		return nil
	}
	if t3StatsHook != nil {
		return t3StatsHook(c)
	}
	rc, err := resolveT3(c, s.d.Config.Home)
	if err != nil {
		return nil
	}
	if st := readT3State(s.d.Config.Path(t3State)); !t3Alive(st) || st.Port != rc.port || st.Home != rc.home {
		return nil
	}
	d, ok := t3Probe(ctx, rc.port, 300*time.Millisecond)
	if !ok {
		return nil
	}
	return &api.MachineT3{EnvironmentID: d.EnvironmentID, Version: d.ServerVersion, Port: rc.port}
}

// runT3 runs a t3_ensure op here.
func (s *Service) runT3(ctx context.Context, _ string, op api.MachineOp) (json.RawMessage, error) {
	c, on := s.T3Block()
	fail := func(err error) (json.RawMessage, error) {
		var e *api.Error
		if errors.As(err, &e) {
			return nil, e
		}
		return nil, api.Errorf(api.CodeUnavailable, "%s", err.Error())
	}
	if !on {
		return fail(t3Refused(s.d.Config.Machine))
	}
	var in orchapi.T3EnsureOp
	if json.Unmarshal(op.Payload, &in) != nil || !t3LabelRe.MatchString(in.Label) {
		return fail(api.Errorf(api.CodeBadRequest, "a t3_ensure op without a zm label"))
	}
	scopes, err := t3Scopes(in.Scopes)
	if err != nil {
		return fail(err)
	}
	req := orchapi.T3EnsureOp{Label: in.Label, Scopes: scopes}
	var env orchapi.T3Environment
	if t3EnsureHook != nil {
		env, err = t3EnsureHook(ctx, c, req)
	} else {
		env, err = s.t3.ensure(ctx, s.d.Config.Home, c, req)
	}
	if err != nil {
		return fail(err)
	}
	env.Machine = s.d.Config.Machine
	b, _ := json.Marshal(env)
	return b, nil
}

// t3Host runs this machine's Mezze app server, one ensure at a time.
type t3Host struct {
	mu sync.Mutex
	// once reads the block (T3Block).
	once  sync.Once
	block config.T3
	on    bool
}

// t3Conf is a t3 block resolved: commit "" runs origin/main's tip.
type t3Conf struct {
	checkout, commit, home, node, vp string
	port                             int
}

// t3StateFile is what zm started (kept in zm's home, so a block whose home
// or port changed still finds it): the server's process (its own process
// group), the commit, port and home it runs.
type t3StateFile struct {
	PID     int       `json:"pid"`
	Commit  string    `json:"commit"`
	Port    int       `json:"port"`
	Home    string    `json:"home"`
	Started time.Time `json:"started"`
}

// t3Descriptor is the part of /.well-known/t3/environment zm reads.
type t3Descriptor struct {
	EnvironmentID string `json:"environmentId"`
	ServerVersion string `json:"serverVersion"`
}

// resolveT3 fills the block's defaults: checkout <zmHome>/t3code, home
// ~/.z3, port 3773, node and vp on PATH, no commit (origin/main's tip).
func resolveT3(c config.T3, zmHome string) (t3Conf, error) {
	bad := func(format string, a ...any) (t3Conf, error) {
		return t3Conf{}, api.Errorf(api.CodeUnavailable, "the t3 block in zm's config.json: "+format, a...)
	}
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if p == "~" {
			return home
		}
		if rest, ok := strings.CutPrefix(p, "~/"); ok && home != "" {
			return filepath.Join(home, rest)
		}
		return p
	}
	rc := t3Conf{checkout: expand(c.Checkout), commit: c.Commit, home: expand(c.Home), node: c.Node, vp: c.VP, port: c.Port}
	if rc.checkout == "" {
		rc.checkout = filepath.Join(zmHome, t3Checkout)
	}
	if rc.home == "" {
		rc.home = expand("~/.z3")
	}
	if rc.port == 0 {
		rc.port = t3DefaultPort
	}
	if rc.node == "" {
		rc.node = "node"
	}
	if rc.vp == "" {
		rc.vp = "vp"
	}
	switch {
	case !filepath.IsAbs(rc.checkout):
		return bad("checkout %q is not an absolute path", rc.checkout)
	case !filepath.IsAbs(rc.home):
		return bad("home %q is not an absolute path", c.Home)
	case rc.commit != "" && !t3CommitRe.MatchString(rc.commit):
		return bad("commit must be a full 40-character main commit (or left out: origin/main's tip), not %q", c.Commit)
	case rc.port < 1 || rc.port > 65535:
		return bad("port %d", c.Port)
	}
	return rc, nil
}

// t3Tools refuses when git, node or vp is not there: zm never installs
// them.
func (rc t3Conf) t3Tools() error {
	var missing []string
	for _, t := range []struct{ bin, what string }{{"git", "git"}, {rc.node, "Node.js (node)"}, {rc.vp, "Vite+ (vp)"}} {
		if _, err := exec.LookPath(t.bin); err != nil {
			missing = append(missing, t.what)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return api.Errorf(api.CodeUnavailable, "the Mezze app here needs %s, not found on the zm daemon's PATH", strings.Join(missing, " and ")).
		WithHint("install it on this machine (zm installs no system packages), or set \"node\" / \"vp\" in the t3 block to its path, then ask again")
}

// diskFloor refuses a clone or an install with less than t3DiskFloorMB
// free on the volume of path (or its nearest existing parent).
func diskFloor(path, what string) error {
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil || p == filepath.Dir(p) {
			path = p
			break
		}
	}
	free, err := t3DiskFree(path)
	if err != nil || free >= t3DiskFloorMB {
		return nil
	}
	return api.Errorf(api.CodeUnavailable, "the Mezze app here: %d MB free at %s, under the %d MB floor for %s", free, path, t3DiskFloorMB, what).
		WithHint("free disk on this machine (its free disk is in its machine numbers), then ask again")
}

// provision clones the checkout when it is missing (no directory, or an
// empty one), into a sibling first so a failed clone leaves nothing
// behind; a directory that is no t3code checkout is refused.
func (rc t3Conf) provision(ctx context.Context) error {
	bin := filepath.Join(rc.checkout, "apps", "server", "src", "bin.ts")
	if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
		return nil
	}
	if ents, err := os.ReadDir(rc.checkout); err == nil && len(ents) > 0 {
		return api.Errorf(api.CodeUnavailable, "the t3 block in zm's config.json: %s is not a t3code checkout (no apps/server/src/bin.ts)", rc.checkout).
			WithHint("move it away (zm clones one there) or set \"checkout\" in the t3 block")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if t3Repo == "" {
		return api.Errorf(api.CodeUnavailable, "no t3code checkout at %s, and this zm names no repository to clone it from", rc.checkout).
			WithHint("clone the t3code repository there (or set \"checkout\" in the t3 block to a clone), then ask again")
	}
	if err := diskFloor(filepath.Dir(rc.checkout), "a t3code clone"); err != nil {
		return err
	}
	parent := filepath.Dir(rc.checkout)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(rc.checkout)+".clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := rc.run(ctx, t3CloneWait, parent, false, "git", "clone", "--quiet", "--branch", "main", "--", t3Repo, tmp); err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Join(tmp, "apps", "server", "src", "bin.ts")); err != nil || fi.IsDir() {
		return api.Errorf(api.CodeUnavailable, "%s has no apps/server/src/bin.ts on main", t3Repo)
	}
	_ = os.Remove(rc.checkout) // the empty directory, if any
	return os.Rename(tmp, rc.checkout)
}

func readT3State(path string) t3StateFile {
	var st t3StateFile
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// t3Alive: the state names a live process zm started, still running the
// server (not a process that took its pid after a reboot).
func t3Alive(st t3StateFile) bool {
	return st.PID > 0 && syscall.Kill(st.PID, 0) == nil && t3Serving(st.PID)
}

// t3Serving: pid's command line is a Mezze app serve (Linux /proc, else ps).
func t3Serving(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		b, err = exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
	}
	line := strings.ReplaceAll(string(b), "\x00", " ")
	return strings.Contains(line, filepath.Join("apps", "server", "src", "bin.ts")) && strings.Contains(line, " serve ")
}

// t3Probe reads the server's descriptor on loopback.
func t3Probe(ctx context.Context, port int, timeout time.Duration) (t3Descriptor, bool) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/.well-known/t3/environment", nil)
	if err != nil {
		return t3Descriptor{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return t3Descriptor{}, false
	}
	defer resp.Body.Close()
	var d t3Descriptor
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&d) != nil || d.EnvironmentID == "" {
		return t3Descriptor{}, false
	}
	return d, true
}

// ensure starts the server unless zm's own already answers at the pinned
// commit (any, with no pin), port and home, then mints a credential for
// it. zmHome holds the record of the server zm started (and the default
// checkout). A server zm started at another commit, port or home is
// replaced, stopped only once the checkout is ready: a pin that cannot be
// prepared leaves it running.
func (h *t3Host) ensure(ctx context.Context, zmHome string, c config.T3, req orchapi.T3EnsureOp) (orchapi.T3Environment, error) {
	rc, err := resolveT3(c, zmHome)
	if err != nil {
		return orchapi.T3Environment{}, err
	}
	statePath := filepath.Join(zmHome, t3State)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(rc.home, 0o700); err != nil {
		return orchapi.T3Environment{}, err
	}
	st := readT3State(statePath)
	alive := t3Alive(st)
	onPort := alive && st.Port == rc.port // what answers the port is zm's
	same := onPort && st.Home == rc.home && (rc.commit == "" || st.Commit == rc.commit)
	d, up := t3Probe(ctx, rc.port, 2*time.Second)
	if !up && same {
		// Started by an earlier run (or a daemon before this one) and not
		// answering yet: give it its start time before replacing it.
		d, up = t3WaitUp(ctx, rc.port, nil)
	}
	started := false
	switch {
	case up && !onPort:
		return orchapi.T3Environment{}, api.Errorf(api.CodeConflict, "port %d here answers a Mezze app server zm did not start", rc.port).
			WithHint("set \"port\" in the t3 block to a free port, or stop that server")
	case up && same:
		rc.commit = st.Commit
	default:
		if err := rc.t3Tools(); err != nil {
			return orchapi.T3Environment{}, err
		}
		if err := rc.provision(ctx); err != nil {
			return orchapi.T3Environment{}, err
		}
		if err := rc.prepare(ctx); err != nil {
			return orchapi.T3Environment{}, err
		}
		if alive {
			t3Stop(st.PID)
		}
		exited, err := rc.start(statePath)
		if err != nil {
			return orchapi.T3Environment{}, err
		}
		if d, up = t3WaitUp(ctx, rc.port, exited); !up {
			return orchapi.T3Environment{}, api.Errorf(api.CodeUnavailable, "the Mezze app did not answer on port %d within %s: %s", rc.port, t3StartWait, logTail(filepath.Join(rc.home, t3Log)))
		}
		started = true
	}
	cred, err := rc.mint(ctx, req)
	if err != nil {
		return orchapi.T3Environment{}, err
	}
	return orchapi.T3Environment{EnvironmentID: d.EnvironmentID, Version: d.ServerVersion, Commit: rc.commit, Port: rc.port,
		URLs: t3URLs(rc.port), Started: started, Credential: cred.Credential, Scopes: cred.Scopes, ExpiresAt: cred.ExpiresAt}, nil
}

// run runs one command in dir; its error carries the end of its output, or
// of its stderr alone when stdout may hold a secret (secretOut).
func (rc t3Conf) run(ctx context.Context, wait time.Duration, dir string, secretOut bool, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // a missing credential fails, never waits
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	shown := &out
	if secretOut {
		cmd.Stderr, shown = &stderr, &stderr
	}
	err := cmd.Run()
	if err != nil {
		return "", api.Errorf(api.CodeUnavailable, "%s %s: %v: %s", filepath.Base(name), strings.Join(args[:min(len(args), 2)], " "), err, cutOut(shown.String()))
	}
	return out.String(), nil
}

// prepare puts the checkout at the pinned commit (a main commit, the tree
// clean; with no pin, origin/main's tip after a fetch, which it sets as
// rc.commit) and installs its dependencies unless an install finished at
// that commit (a marker in node_modules: an install that failed runs
// again).
func (rc *t3Conf) prepare(ctx context.Context) error {
	git := func(args ...string) (string, error) {
		out, err := rc.run(ctx, 2*time.Minute, rc.checkout, false, "git", args...)
		return strings.TrimSpace(out), err
	}
	if rc.commit == "" {
		if _, err := git("fetch", "--quiet", "origin", "main"); err != nil {
			return err
		}
		tip, err := git("rev-parse", "origin/main")
		if err != nil {
			return err
		}
		rc.commit = tip
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	moved := head != rc.commit
	if moved {
		if dirty, err := git("status", "--porcelain", "--untracked-files=no"); err != nil {
			return err
		} else if dirty != "" {
			return api.Errorf(api.CodeConflict, "the t3 checkout %s has local changes", rc.checkout).WithHint("commit or drop them there")
		}
	}
	if _, err := git("merge-base", "--is-ancestor", rc.commit, "origin/main"); err != nil {
		if _, ferr := git("fetch", "--quiet", "origin", "main"); ferr != nil {
			return ferr
		}
		if _, err := git("merge-base", "--is-ancestor", rc.commit, "origin/main"); err != nil {
			return api.Errorf(api.CodeConflict, "%s is not a commit on the checkout's origin/main", rc.commit).WithHint("pin a main commit in the t3 block")
		}
	}
	if moved {
		if _, err := git("checkout", "--quiet", "--detach", rc.commit); err != nil {
			return err
		}
	}
	marker := filepath.Join(rc.checkout, "node_modules", t3Installed)
	if b, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(b)) != rc.commit {
		if err := diskFloor(rc.checkout, "installing the Mezze app's dependencies"); err != nil {
			return err
		}
		_ = os.Remove(marker)
		if _, err := rc.run(ctx, t3InstallWait, rc.checkout, false, rc.vp, "i"); err != nil {
			return err
		}
		_ = os.WriteFile(marker, []byte(rc.commit+"\n"), 0o600)
	}
	return nil
}

// start starts the server in its own process group (it outlives this
// daemon) and records it; exited closes when it exits while this daemon
// runs.
func (rc t3Conf) start(statePath string) (<-chan struct{}, error) {
	logf, err := os.OpenFile(filepath.Join(rc.home, t3Log), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n== zm starts the Mezze app at %s, %s ==\n", rc.commit, time.Now().UTC().Format(time.RFC3339))
	cmd := exec.Command(rc.node, filepath.Join(rc.checkout, "apps", "server", "src", "bin.ts"),
		"serve", "--lan", "--port", strconv.Itoa(rc.port), "--base-dir", rc.home)
	cmd.Dir = rc.home
	// Provider sessions' default cwd. A HOME that does not exist (make
	// test's throwaway one, a service account's) fails the start with a
	// misleading "fork/exec <node>: no such file", so keep rc.home then.
	if h, err := os.UserHomeDir(); err == nil {
		if fi, err := os.Stat(h); err == nil && fi.IsDir() {
			cmd.Dir = h
		}
	}
	cmd.Env = append(os.Environ(), "T3CODE_NO_BROWSER=1", "T3CODE_AUTO_BOOTSTRAP_PROJECT_FROM_CWD=false")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, api.Errorf(api.CodeUnavailable, "starting the Mezze app: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	b, _ := json.Marshal(t3StateFile{PID: cmd.Process.Pid, Commit: rc.commit, Port: rc.port, Home: rc.home, Started: time.Now().UTC()})
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t3Stop(cmd.Process.Pid)
		return nil, err
	}
	return exited, nil
}

// t3WaitUp waits up to t3StartWait for the descriptor (until exited
// closes, when set).
func t3WaitUp(ctx context.Context, port int, exited <-chan struct{}) (t3Descriptor, bool) {
	deadline := time.Now().Add(t3StartWait)
	for {
		if d, ok := t3Probe(ctx, port, time.Second); ok {
			return d, true
		}
		if time.Now().After(deadline) {
			return t3Descriptor{}, false
		}
		select {
		case <-exited:
			return t3Descriptor{}, false
		case <-ctx.Done():
			return t3Descriptor{}, false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// t3Stop ends the server's process group: TERM, then KILL after 10 s.
func t3Stop(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// t3Pairing is the part of `auth pairing create --json` zm reads.
type t3Pairing struct {
	Credential string    `json:"credential"`
	Scopes     []string  `json:"scopes"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// mint runs the server's own `auth pairing create`.
func (rc t3Conf) mint(ctx context.Context, req orchapi.T3EnsureOp) (t3Pairing, error) {
	args := []string{filepath.Join(rc.checkout, "apps", "server", "src", "bin.ts"), "auth", "pairing", "create",
		"--base-dir", rc.home, "--ttl", t3TTL, "--label", req.Label, "--json"}
	for _, sc := range req.Scopes {
		args = append(args, "--scope", sc)
	}
	out, err := rc.run(ctx, time.Minute, rc.home, true, rc.node, args...) // stdout carries the credential
	if err != nil {
		return t3Pairing{}, err
	}
	var p t3Pairing
	if i := strings.IndexByte(out, '{'); i >= 0 {
		_ = json.NewDecoder(strings.NewReader(out[i:])).Decode(&p)
	}
	if p.Credential == "" {
		return t3Pairing{}, api.Errorf(api.CodeUnavailable, "the Mezze app's auth pairing create printed no credential")
	}
	return p, nil
}

// t3URLs are http://<ip>:<port> for this machine's tailnet, then private,
// IPv4 addresses (container and VM bridges left out).
func t3URLs(port int) []string {
	var tailnet, lan []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if n := ifc.Name; strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth") || strings.HasPrefix(n, "virbr") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil {
				continue
			}
			u := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
			switch {
			case t3Tailnet.Contains(ip):
				tailnet = append(tailnet, u)
			case ip.IsPrivate():
				lan = append(lan, u)
			}
		}
	}
	return append(append([]string{}, tailnet...), lan...)
}

// cutOut keeps the last 600 bytes of a command's output.
func cutOut(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = "…" + s[len(s)-600:]
	}
	return s
}

// logTail is the end of the server's log.
func logTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "no log"
	}
	return cutOut(string(b))
}
