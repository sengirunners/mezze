package main

// zm ci doctor --probe is Potluck's own ci doctor --probe
// (potluck cli/ci_probe.go: check 18, a script that proves the path with a
// generated test job), run in this process on zm's home as zm ci pr is. zm
// keeps no copy of the probe; it only says a yes the probe waits on (a public
// or internal repository's owner's) as an ask, never as a command for an
// agent to run.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	potluckcli "github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runners"
)

// probeCheck is Potluck's check 18, the probe.
const probeCheck = 18

// ciProbeOpts are --probe's flags, Potluck's own.
type ciProbeOpts struct {
	labels, size, osName string
	public               bool // the owner's own yes, on their command line
}

// ciDoctorOut is zm ci doctor --probe's answer: the doctor's checks with
// check 18, and Ask when the probe waits on a person's yes (an agent puts
// it to them, never gives it).
type ciDoctorOut struct {
	runners.DoctorResult
	Ask *ciStep `json:"ask,omitempty"`
	// script is the probe script Potluck wrote (check 18's sh fix), shown
	// in text mode as Potluck's own ci doctor --probe shows it; --json
	// carries only its path, in the fix.
	script string
}

// ciDoctorProbe runs Potluck's ci doctor repo --probe --json against this
// machine's zm daemon and says its answer in zm's words.
func ciDoctorProbe(ctx context.Context, repo string, o ciProbeOpts) (ciDoctorOut, error) {
	var out ciDoctorOut
	// Potluck's own repository rule; a leading - would also be read as a flag.
	if !jobrule.ValidRepo(repo) || strings.HasPrefix(repo, "-") {
		return out, api.Errorf(api.CodeBadRequest, "the probe needs one repository as owner/name, not %q", repo)
	}
	args := []string{"ci", "doctor", repo, "--probe", "--json"}
	for _, f := range []struct{ name, v string }{{"--labels", o.labels}, {"--size", o.size}, {"--os", o.osName}} {
		if f.v != "" {
			args = append(args, f.name, f.v)
		}
	}
	if o.public {
		args = append(args, "--public")
	}
	if err := os.Setenv(potluckcli.EnvNoAutostart, "1"); err != nil { // never Potluck's start, past zm's lock
		return out, err
	}
	var stdout, stderr bytes.Buffer
	code := potluckcli.Run(ctx, args, strings.NewReader(""), &stdout, &stderr)
	if stdout.Len() == 0 || json.Unmarshal(stdout.Bytes(), &out.DoctorResult) != nil {
		return out, potluckError(stderr.Bytes(), code)
	}
	out.script = probeScript(out.DoctorResult) // before zm's words touch its path
	for i := range out.Warnings {
		out.Warnings[i] = potluckToZm.Replace(out.Warnings[i])
	}
	for ri := range out.Reports {
		r := &out.Reports[ri]
		for ci := range r.Checks {
			c := &r.Checks[ci]
			c.Message, c.Fix = potluckToZm.Replace(c.Message), potluckToZm.Replace(c.Fix)
			// Without the owner's --public, any fix that adds it is their yes:
			// Potluck says which repositories need it, zm says it as an ask.
			if c.Number == probeCheck && c.Status == "fail" && !o.public && strings.Contains(c.Fix, "--public") {
				own := c.Fix
				if _, after, ok := strings.Cut(own, "with their yes: "); ok {
					own = after
				}
				out.Ask = &ciStep{Kind: "ask", Say: c.Message + " Probe it on this Mac? Only the owner says yes, by running " + own + " themselves.",
					Choices: []string{"yes", "no"}}
				c.Fix = "the repository owner's yes (ask them; never give it for them)"
			}
			// The script pushes a branch with the person's own gh sign-in: their
			// step, as zm ci add's pull-request script is.
			if c.Number == probeCheck && strings.HasPrefix(c.Fix, "sh ") && out.Ask == nil {
				out.Ask = &ciStep{Kind: "ask", Say: "Run the probe with your own gh sign-in (it pushes one throwaway branch to " + repo +
					", runs its one job on this Mac and deletes the branch; pushing a workflow needs gh auth refresh -s workflow).", Command: c.Fix}
			}
		}
	}
	for _, r := range out.Reports {
		for _, c := range r.Checks {
			if c.Number == probeCheck {
				return out, nil
			}
		}
	}
	return out, fmt.Errorf("the CI doctor answered without its probe (check 18): %s", potluckError(stderr.Bytes(), code))
}

// potluckError is what Potluck said on stderr (--json: an api error, kept
// with its code), in zm's words.
func potluckError(stderr []byte, code int) error {
	var er api.ErrorResponse
	if json.Unmarshal(bytes.TrimSpace(stderr), &er) == nil && er.Error != nil {
		e := *er.Error
		e.Message, e.Hint = potluckToZm.Replace(e.Message), potluckToZm.Replace(e.Hint)
		return &e
	}
	msg := potluckToZm.Replace(strings.TrimSpace(string(stderr)))
	if msg == "" {
		return fmt.Errorf("the CI doctor's probe ended with exit %d", code)
	}
	return errors.New(strings.TrimPrefix(msg, "zm: "))
}

// probeScript is the script Potluck wrote, from its own check 18 fix: only
// the probed repository's (the first report), and only Potluck's probe.sh.
func probeScript(res runners.DoctorResult) string {
	if len(res.Reports) == 0 {
		return ""
	}
	for _, c := range res.Reports[0].Checks {
		if p, ok := strings.CutPrefix(c.Fix, "sh "); ok && c.Number == probeCheck && filepath.Base(shellUnquote(p)) == "probe.sh" {
			return shellUnquote(p)
		}
	}
	return ""
}

// shellUnquote undoes Potluck's shellQuote: a bare word, or one
// single-quoted word whose own quotes are written as quote, backslash,
// quote, quote.
func shellUnquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}
