package mcp

// Compute tools: the agent's view of `zm ci status`,
// `zm ci doctor`, `zm ci add` and `zm lend pause`, answering the same JSON as those
// commands' --json. Opt in (Options.Compute, `zm mcp --compute`) for the
// budget reason sessionTools gives. None ever prompts: a person's own step
// (a GitHub press, a public repository's yes, a pull request to open)
// comes back as a step of kind ask, never taken for them.

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

var computeTools = []Tool{
	{"compute_status", "zm ci status --json: each repo's runs-on line and jobs, with why one waits; untrusted."},
	{"compute_doctor", "zm ci doctor --json: read-only checks of repo (default: all connected), each fail with its fix. probe (one repo): check 18 writes (never runs) a temp test-job script; an ask is the owner's, never yours. Untrusted."},
	{"compute_setup", "zm ci add --json: connect repos to this Mac; re-run until state done. Steps of kind ask are a person's: zm ask them, never do them. Untrusted."},
	{"compute_pause", "zm lend pause --json: stop lending this Mac for now, keeping its choices. state refused (refused.reason job_running, lend_running or admission) changes nothing; resuming is the owner's zm lend resume."},
}

// ComputeTools lists the opt-in compute tools.
func ComputeTools() []Tool { return append([]Tool(nil), computeTools...) }

type computeDoctorIn struct {
	Repo string `json:"repo,omitempty" jsonschema:"owner/name; default: every connected repo"`
	ProbeRequest
}

// ProbeRequest is compute_doctor's probe: zm ci doctor --probe's flags. It
// has no public: a public or internal repository's yes is the owner's own
// (zm ci doctor <repo> --probe --public, run by them), so it comes back as
// an ask.
type ProbeRequest struct {
	Probe  bool   `json:"probe,omitempty" jsonschema:"with repo: also check 18, Potluck's probe (writes, never runs, a test-job script)"`
	Labels string `json:"labels,omitempty" jsonschema:"with probe: the probe job's labels, comma-separated"`
	Size   string `json:"size,omitempty" jsonschema:"with probe: a pool size to ask for"`
	OS     string `json:"os,omitempty" jsonschema:"with probe: the OS label (default macos)"`
}

// SetupRequest is compute_setup's input: zm ci add's arguments and flags.
type SetupRequest struct {
	Repos    []string `json:"repos,omitempty" jsonschema:"owner/name each"`
	Mine     bool     `json:"mine,omitempty" jsonschema:"also this machine's zm projects' repos"`
	Job      string   `json:"job,omitempty" jsonschema:"the job to move onto this Mac, as a pull request; default: the one on a GitHub-hosted macOS label"`
	Workflow string   `json:"workflow,omitempty" jsonschema:"the workflow file that has the job"`
	Org      string   `json:"org,omitempty" jsonschema:"make the GitHub app under this org"`
	Own      bool     `json:"own_account,omitempty" jsonschema:"make the GitHub app under your own account (zm ci add --org=)"`
}

// A public repository's yes is the owner's alone (zm ci add <repo> --public,
// run by them): SetupRequest has no field for it, so an agent never gives it.

// The compute tools reach GitHub (through the broker, or gh): open world.
var (
	readOnlyOpen = &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	additiveOpen = &sdk.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
	// Pause takes this Mac's lent sizes off (zm lend resume puts them back):
	// destructive but idempotent, and closed world (the daemon only).
	pauseClosed = &sdk.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)}
)

func addComputeTools(srv *sdk.Server, s *server) {
	add(srv, s, "compute_status", readOnlyOpen, true, func(ctx context.Context, c *client.Client, _ none) (any, error) {
		return orchclient.CIStatus(ctx, c)
	})
	// Doctor only reads, but its probe writes a new temporary script folder
	// each call (never run): additive, not read-only.
	add(srv, s, "compute_doctor", additiveOpen, true, func(ctx context.Context, c *client.Client, in computeDoctorIn) (any, error) {
		if in.Probe || in.Labels != "" || in.Size != "" || in.OS != "" {
			if !in.Probe || in.Repo == "" {
				return nil, api.Errorf(api.CodeBadRequest, "labels, size and os go with probe, and probe with one repo")
			}
			if s.o.Probe == nil {
				return nil, api.Errorf(api.CodeUnavailable, "compute_doctor's probe is served by zm mcp --compute")
			}
			return s.o.Probe(ctx, c, in.Repo, in.ProbeRequest)
		}
		return orchclient.CIDoctor(ctx, c, in.Repo)
	})
	add(srv, s, "compute_setup", additiveOpen, true, func(ctx context.Context, c *client.Client, in SetupRequest) (any, error) {
		if s.o.Setup == nil {
			return nil, api.Errorf(api.CodeUnavailable, "compute_setup is served by zm mcp --compute")
		}
		return s.o.Setup(ctx, c, in)
	})
	add(srv, s, "compute_pause", pauseClosed, false, func(ctx context.Context, c *client.Client, _ none) (any, error) {
		if s.o.Pause == nil {
			return nil, api.Errorf(api.CodeUnavailable, "compute_pause is served by zm mcp --compute")
		}
		return s.o.Pause(ctx, c)
	})
}
