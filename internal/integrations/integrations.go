// Package integrations wires agent runtimes to zm: hooks that register the
// session, report its state and print the unread-mail notice, the MCP server
// entry, the Claude Code plugin folder (repo: integrations/claude-code) and
// the AGENTS.md snippet. Owned by the integrations builder. Each runtime has
// one file (claude.go, codex.go) that plans edits to its config files;
// apply.go writes them.
//
// Install never writes unless Options.Apply is set, backs up every file it
// changes, and is idempotent: a second Install reports every file unchanged.
// This package never reads CLAUDE_CONFIG_DIR / CODEX_HOME itself (the CLI
// does), so tests that pass a temp Home can never reach the real config.
//
// Hooks run on the agent's prompt path: they never block, never fail the
// prompt, and print nothing when there is no project or no mail.
package integrations

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Options says where and how to install.
type Options struct {
	Agent string // claude, codex
	// Home is the user's home dir; default config locations are under it.
	// Tests always pass t.TempDir().
	Home string
	// ConfigDir overrides the agent's config dir (the CLI fills it from
	// CLAUDE_CONFIG_DIR / CODEX_HOME). Default <Home>/.claude or <Home>/.codex.
	ConfigDir string
	// PeoplesCommune is the command hooks and MCP entries run: an absolute path, or "zm" (PATH).
	PeoplesCommune string
	// Apply writes the changes. False (the default) is a dry run.
	Apply bool
	// Now stamps backup names; nil means time.Now.
	Now func() time.Time
}

// File actions.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
)

// FileChange is one config file Install/Uninstall touches (or would).
type FileChange struct {
	Path    string   `json:"path"`
	Action  string   `json:"action"`            // create, update, unchanged
	Changes []string `json:"changes,omitempty"` // "+ hooks.Stop: zm hook claude stop", "- ..."
	// Diff is a unified diff of the file before and after ("" when unchanged).
	Diff    string `json:"diff,omitempty"`
	Backup  string `json:"backup,omitempty"` // where the old file is (or would be) copied
	Written bool   `json:"written"`

	before []byte // nil: the file does not exist
	after  []byte
}

// Result reports what Install/Uninstall changed (or would change, on a dry run).
type Result struct {
	Agent  string       `json:"agent"`
	DryRun bool         `json:"dry_run"`
	Files  []FileChange `json:"files"`
	Notes  []string     `json:"notes,omitempty"` // things zm left alone, and why
	Next   []string     `json:"next,omitempty"`  // what to do once the change is made
	// Trust is, for codex, whether Codex has trusted each zm hook yet
	// (Codex skips untrusted hooks; only the user trusts them, with /hooks).
	Trust []HookTrust `json:"trust,omitempty"`
}

// Untrusted lists the events of hooks Codex will not run until the user acts.
func (r Result) Untrusted() []string {
	var out []string
	for _, t := range r.Trust {
		if t.State != TrustTrusted {
			out = append(out, t.Event)
		}
	}
	return out
}

// Changed reports whether any file differs.
func (r Result) Changed() bool {
	for _, f := range r.Files {
		if f.Action == ActionCreate || f.Action == ActionUpdate {
			return true
		}
	}
	return false
}

// Integration is one agent runtime.
type Integration interface {
	Name() string
	// Plan computes the edits for install (or uninstall) without writing
	// anything, plus notes (what was left alone) and next steps.
	Plan(o Options, uninstall bool) (files []FileChange, notes, next []string, err error)
}

var registry = map[string]Integration{
	"claude": claude{},
	"codex":  codex{},
}

// Agents lists supported agent names.
func Agents() []string { return []string{"claude", "codex"} }

// ConfigDirEnv is the environment variable each agent reads to move its
// config dir. Only the CLI consults it, and only when --home is not given.
var ConfigDirEnv = map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME"}

// Install installs zm's hooks and MCP entry for o.Agent (a dry run unless o.Apply).
func Install(o Options) (Result, error) { return run(o, false) }

// Uninstall removes what Install added, leaving the user's other settings alone.
func Uninstall(o Options) (Result, error) { return run(o, true) }

func run(o Options, uninstall bool) (Result, error) {
	in, ok := registry[o.Agent]
	if !ok {
		return Result{}, api.Errorf(api.CodeBadRequest, "unknown agent %q", o.Agent).
			WithHint("supported: " + strings.Join(Agents(), ", "))
	}
	if o.Home == "" && o.ConfigDir == "" {
		return Result{}, api.Errorf(api.CodeBadRequest, "no home directory to install into").WithHint("pass --home DIR")
	}
	if o.PeoplesCommune == "" {
		o.PeoplesCommune = "zm"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	files, notes, next, err := in.Plan(o, uninstall)
	if err != nil {
		return Result{}, err
	}
	r := Result{Agent: o.Agent, DryRun: !o.Apply, Notes: notes, Next: next}
	for i := range files {
		if err := apply(&files[i], o); err != nil {
			return r, fmt.Errorf("%s: %w", files[i].Path, err)
		}
	}
	r.Files = files
	if o.Agent == "codex" && !uninstall {
		dir := codexDir(o)
		if tr, err := CodexHookTrust(dir); err == nil {
			for _, f := range files {
				if filepath.Base(f.Path) == "hooks.json" && (f.Action == ActionCreate || f.Action == ActionUpdate) {
					for i := range tr {
						tr[i].State = TrustChanged // new or rewritten: Codex asks for trust again
					}
				}
			}
			r.Trust = tr
		}
	}
	return r, nil
}
