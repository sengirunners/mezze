package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

const (
	name           = "farm"
	version        = "0.1.0"
	costUnit       = "run_min"
	defaultTTL     = 1800
	defaultTTLMax  = 4 * 3600
	startWait      = 100 * time.Second // acquire answers in 120 s (SPEC 20)
	keepEnded      = 7 * 24 * time.Hour
	statusWaitSecs = 0
)

var (
	verbs         = []string{"describe", "acquire", "status", "release", "list"}
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`) // never . or ..: it names a directory
)

type refusal struct{ code, msg string }

func (r refusal) Error() string { return r.code + ": " + r.msg }

func refuse(code, format string, a ...any) error {
	return refusal{code, fmt.Sprintf(format, a...)}
}

// config is the binding's driver keys plus what zm adds (target, kind).
type config struct {
	Target     string
	Project    string
	Machine    string
	TTLMax     int
	DefaultTTL int
	StateDir   string
}

func parseConfig(m map[string]string) (config, error) {
	c := config{Target: m["target"], Project: m["project"], Machine: m["machine"], TTLMax: defaultTTLMax, DefaultTTL: defaultTTL}
	if k := m["kind"]; k != "" && k != "run" {
		return c, refuse("bad_request", "zm-run-farm serves kind run only, not %q", k)
	}
	if !namePattern.MatchString(c.Target) {
		return c, refuse("bad_request", "config target (the binding name) is required")
	}
	if c.Project == "" {
		return c, refuse("bad_request", "config project (the zm project whose team machines run it) is required")
	}
	if c.Machine == "" {
		return c, refuse("bad_request", "config machine is required: a machine of the team project, or auto")
	}
	for key, dst := range map[string]*int{"ttl_max_s": &c.TTLMax, "default_ttl_s": &c.DefaultTTL} {
		if v := m[key]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 7*24*3600 {
				return c, refuse("bad_request", "config %s must be seconds between 1 and 604800", key)
			}
			*dst = n
		}
	}
	c.DefaultTTL = min(c.DefaultTTL, c.TTLMax)
	c.StateDir = m["state_dir"]
	if c.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, refuse("internal", "no HOME for state_dir: %v", err)
		}
		c.StateDir = filepath.Join(home, ".zm-run-farm")
	}
	return c, nil
}

// record is what the driver keeps per handle.
type record struct {
	Handle    string    `json:"handle"`
	RequestID string    `json:"request_id"`
	Task      string    `json:"task"`
	Actor     string    `json:"actor"`
	Commit    string    `json:"commit"`
	Machine   string    `json:"machine,omitempty"`
	TTL       int       `json:"ttl_s"`
	Acquired  time.Time `json:"acquired"`
	Expires   time.Time `json:"expires_at"`
	Released  bool      `json:"released,omitempty"`
	State     string    `json:"state,omitempty"` // last terminal state seen
	Exit      *int      `json:"exit,omitempty"`
	Ended     time.Time `json:"ended,omitzero"`
}

type drv struct {
	cfg config
	t   tasks
	now func() time.Time
}

func dispatch(ctx context.Context, verb, id string, args json.RawMessage, m map[string]string, open func(string) (tasks, error)) (any, error) {
	if !slices.Contains(verbs, verb) {
		return nil, refuse("not_implemented", "verb %q not supported", verb)
	}
	cfg, err := parseConfig(m)
	if err != nil {
		return nil, err
	}
	t, err := open(cfg.Project)
	if err != nil {
		return nil, refuse("unavailable", "zm client: %v", err)
	}
	d := &drv{cfg: cfg, t: t, now: time.Now}
	switch verb {
	case "describe":
		return d.describe(ctx), nil
	case "acquire":
		return d.acquire(ctx, args)
	case "status":
		return d.status(ctx, id)
	case "release":
		return d.release(ctx, id)
	}
	return d.list()
}

func (d *drv) describe(ctx context.Context) map[string]any {
	out := map[string]any{"potluck": "potluck/1", "zm": "zm/v0", "name": name, "kind": "run", "version": version, "verbs": verbs,
		"available": true, "isolation": "none", "credentials": true,
		"cost": map[string]any{"unit": costUnit, "amount": 0}, "reports_spend": false,
		"ttl_max_s": d.cfg.TTLMax, "capabilities": []string{"remote"},
		"project": d.cfg.Project, "machine": d.cfg.Machine}
	if err := d.t.Health(ctx); err != nil {
		out["available"], out["why"] = false, "the local zm daemon does not answer: "+err.Error()
	}
	return out
}

type acquireArgs struct {
	RequestID string   `json:"request_id"`
	Actor     string   `json:"actor"`
	TTL       int      `json:"ttl_s"`
	Ref       string   `json:"ref"`
	Commit    string   `json:"commit"`
	Script    string   `json:"script"`
	Args      []string `json:"args"`
}

func (d *drv) acquire(ctx context.Context, raw json.RawMessage) (any, error) {
	var a acquireArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, refuse("bad_request", "invalid args: %v", err)
	}
	switch {
	case a.RequestID == "" || len(a.RequestID) > 150:
		return nil, refuse("bad_request", "request_id is required (at most 150 bytes)")
	case a.Actor == "":
		return nil, refuse("bad_request", "actor is required: the session the run is for")
	case a.Ref == "":
		return nil, refuse("bad_request", "ref is required: a run needs a pushed commit")
	case !commitPattern.MatchString(a.Commit):
		return nil, refuse("bad_request", "commit must be the full 40-character commit zm resolved")
	case a.Script == "" || strings.ContainsAny(a.Script, " \t\n;|&$`'\"<>()"):
		return nil, refuse("bad_request", "script must be a path in the repository, never a shell string")
	}
	ttl := a.TTL
	if ttl == 0 {
		ttl = d.cfg.DefaultTTL
	}
	if ttl < 0 || ttl > d.cfg.TTLMax {
		return nil, refuse("bad_request", "ttl_s must be between 1 and the lifetime cap %d", d.cfg.TTLMax)
	}
	// The binding and the request key make the task's key, so one key
	// never answers another binding's run.
	key := "zmrf-" + d.cfg.Target + "-" + shortHash(a.RequestID)
	if r, err := d.byRequest(a.RequestID); err != nil {
		return nil, err
	} else if r != nil {
		return d.acquired(r), nil
	}
	req := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: d.cfg.Machine, TimeoutSeconds: ttl, ClientRequestID: key,
		Run: &orchapi.TaskRun{Ref: a.Ref, Commit: a.Commit, Script: a.Script, Args: a.Args}}
	t, err := d.t.Start(ctx, a.Actor, req)
	if err != nil {
		return nil, apiRefusal(err)
	}
	deadline := d.now().Add(startWait)
	for t.State == orchapi.TaskPending && d.now().Before(deadline) {
		next, err := d.t.Status(ctx, t.ID, 10)
		if err != nil {
			return nil, apiRefusal(err)
		}
		t = next
	}
	switch t.State {
	case orchapi.TaskPending:
		// Nobody took it in time: withdraw it, so it never starts after zm
		// was told it did not.
		_, _ = d.t.Cancel(context.WithoutCancel(ctx), t.ID)
		return nil, refuse("unavailable", "no machine took the run within %s; try again later", startWait)
	case orchapi.TaskRefused:
		return nil, refuse(refusedCode(t.Error), "%s refused the run: %s", or(t.Machine, d.cfg.Machine), t.Error)
	case "ended":
		// It ran and ended before we looked (a fast script), or it never
		// started: withdrawn by an earlier acquire with this key, or failed.
		if state, _ := taskState(t, false); state != "ended" {
			why := "it ended before it started"
			if t.Result != nil && t.Result.Outcome.Evidence != "" {
				why = t.Result.Outcome.Evidence
			}
			return nil, refuse("unavailable", "the run with this request_id did not start (%s); acquire with a new request_id", why)
		}
	}
	now := d.now()
	r := &record{Handle: "run:" + d.cfg.Target + "/" + t.ID, RequestID: a.RequestID, Task: t.ID, Actor: a.Actor, Commit: a.Commit,
		Machine: t.Machine, TTL: ttl, Acquired: now, Expires: now.Add(time.Duration(ttl) * time.Second)}
	if err := d.save(r); err != nil {
		return nil, err
	}
	return d.acquired(r), nil
}

func (d *drv) acquired(r *record) map[string]any {
	return map[string]any{"handle": r.Handle, "expires_at": r.Expires.UTC().Format(time.RFC3339), "ttl_s": r.TTL,
		"commit": r.Commit, "log": "zm task read " + r.Task + " output"}
}

// load answers the record of handle: bad_request for a malformed handle or
// another binding's, not_found for one this binding never issued.
func (d *drv) load(handle string) (*record, error) {
	rest, ok := strings.CutPrefix(handle, "run:")
	target, id, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || target == "" || id == "" {
		return nil, refuse("bad_request", "malformed handle %q", handle)
	}
	if target != d.cfg.Target {
		return nil, refuse("bad_request", "handle %q belongs to another binding", handle)
	}
	b, err := os.ReadFile(d.path(id))
	if errors.Is(err, os.ErrNotExist) || !strings.HasPrefix(id, orchapi.RemoteTaskPrefix) {
		return nil, refuse("not_found", "no handle %s", handle)
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil || r.Handle != handle {
		return nil, refuse("internal", "unreadable record for %s", handle)
	}
	return &r, nil
}

func (d *drv) status(ctx context.Context, handle string) (any, error) {
	r, err := d.load(handle)
	if err != nil {
		return nil, err
	}
	if err := d.refresh(ctx, r); err != nil {
		return nil, err
	}
	out := map[string]any{"handle": r.Handle, "state": or(r.State, "running"), "commit": r.Commit,
		"expires_at": r.Expires.UTC().Format(time.RFC3339)}
	if r.State == "ended" && r.Exit != nil {
		out["exit"] = *r.Exit
	}
	if r.State == "expired" {
		out["exit"] = 124
	}
	return out, nil
}

// refresh asks the task's state and records a terminal one.
func (d *drv) refresh(ctx context.Context, r *record) error {
	if r.State != "" {
		return nil
	}
	t, err := d.t.Status(ctx, r.Task, statusWaitSecs)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.CodeNotFound {
			r.State, r.Ended = "lost", d.now()
			return d.save(r)
		}
		return apiRefusal(err)
	}
	state, exit := taskState(t, r.Released)
	if state == "running" {
		return nil
	}
	r.State, r.Exit, r.Ended = state, exit, d.now()
	return d.save(r)
}

// taskState maps a run task to SPEC 20's states. Only a script's own exit
// is "ended"; a cut-off run never reports success.
func taskState(t orchapi.Task, released bool) (string, *int) {
	switch t.State {
	case orchapi.TaskPending, "running":
		return "running", nil
	case orchapi.TaskRefused:
		return "lost", nil
	}
	if t.Result == nil {
		return "lost", nil
	}
	o := t.Result.Outcome
	switch {
	case o.Kind == orchapi.OutcomeKilled && o.ExitCode != nil && *o.ExitCode == 124:
		return "expired", nil
	case o.Kind == orchapi.OutcomeKilled && (released || o.Evidence == "cancelled"):
		return "released", nil
	case o.Kind == orchapi.OutcomeOK && o.ExitCode != nil:
		return "ended", o.ExitCode
	case o.Kind == orchapi.OutcomeFailed && o.ExitCode != nil && o.Signal == "" && strings.HasPrefix(o.Evidence, "script exited "):
		return "ended", o.ExitCode
	}
	return "lost", nil
}

func (d *drv) release(ctx context.Context, handle string) (any, error) {
	r, err := d.load(handle)
	if err != nil {
		return nil, err
	}
	if err := d.refresh(ctx, r); err != nil {
		return nil, err
	}
	if r.State != "" {
		out := map[string]any{"released": false}
		if r.State == "ended" && r.Exit != nil {
			out["exit"] = *r.Exit
		}
		return out, nil
	}
	t, err := d.t.Cancel(ctx, r.Task)
	if err != nil {
		return nil, apiRefusal(err) // nothing recorded: a later refresh reads the task as it is
	}
	r.Released = true
	switch state, exit := taskState(t, false); state {
	case "running", "released": // our cancel ended it (or is ending it)
		r.State, r.Ended = "released", d.now()
		return map[string]any{"released": true}, d.save(r)
	default: // it ended some other way before the cancel reached it
		r.State, r.Exit, r.Ended = state, exit, d.now()
		out := map[string]any{"released": false}
		if state == "ended" && exit != nil {
			out["exit"] = *exit
		}
		return out, d.save(r)
	}
}

func (d *drv) list() (any, error) {
	rs, err := d.all()
	if err != nil {
		return nil, err
	}
	hs := []map[string]any{}
	for _, r := range rs {
		hs = append(hs, map[string]any{"handle": r.Handle, "request_id": r.RequestID, "state": or(r.State, "running"),
			"expires_at": r.Expires.UTC().Format(time.RFC3339)})
	}
	return map[string]any{"handles": hs}, nil
}

// Records live one file per task under state_dir/<target>.

func (d *drv) dir() string { return filepath.Join(d.cfg.StateDir, d.cfg.Target) }

func (d *drv) path(task string) string {
	return filepath.Join(d.dir(), strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			return c
		}
		return '_'
	}, task)+".json")
}

func (d *drv) save(r *record) error {
	if err := os.MkdirAll(d.dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	f, err := os.CreateTemp(d.dir(), ".rec-*") // one per writer: concurrent calls never share it
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return werr
	}
	return os.Rename(f.Name(), d.path(r.Task))
}

// all is every record, dropping ones ended longer than keepEnded ago.
func (d *drv) all() ([]*record, error) {
	es, err := os.ReadDir(d.dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*record
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(d.dir(), e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r record
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		if r.State != "" && !r.Ended.IsZero() && d.now().Sub(r.Ended) > keepEnded {
			_ = os.Remove(p)
			continue
		}
		out = append(out, &r)
	}
	slices.SortFunc(out, func(a, b *record) int { return a.Acquired.Compare(b.Acquired) })
	return out, nil
}

func (d *drv) byRequest(id string) (*record, error) {
	rs, err := d.all()
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.RequestID == id {
			return r, nil
		}
	}
	return nil, nil
}

// apiRefusal carries a daemon error's code through as the driver's.
func apiRefusal(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case api.CodeBadRequest, api.CodeForbidden, api.CodeNotFound, api.CodeConflict, api.CodeUnavailable:
			return refusal{ae.Code, ae.Message}
		}
		return refusal{"internal", ae.Code + ": " + ae.Message}
	}
	return refusal{"unavailable", err.Error()}
}

// refusedCode is the code for a machine's refusal: a commit that is not the
// ref's head is forbidden; anything else, unavailable.
func refusedCode(msg string) string {
	switch {
	case strings.Contains(msg, "is not what"):
		return "forbidden"
	case strings.Contains(msg, "has no branch or tag"), strings.Contains(msg, "must be an executable file"),
		strings.Contains(msg, "is not in the repository"):
		return "bad_request"
	}
	return "unavailable"
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
