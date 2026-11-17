package adapter

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// The environment an adapter runs with (SPEC 17.2): the base plus what its
// describe.env declares, and nothing else, so the user's shell never leaks
// into a tracker CLI (a BEADS_DOLT_SERVER_HOST exported in the shell that
// started the daemon must never reach a bd that zm runs for a project).

// BaseEnv are the daemon's variables every adapter gets, when set.
var BaseEnv = []string{"PATH", "HOME", "LANG", "TERM", "TZ"}

// zmVars are the names zm fills itself (api.EnvDataDir, api.EnvToolsDir).
var zmVars = []string{orchapi.EnvDataDir, orchapi.EnvToolsDir}

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// shellRefused are plain variables zm never passes from its shell: a
	// secret-shaped name (only as a secret, from the Secrets brick), an
	// injection or credential-agent variable, or a tracker CLI's own (an
	// adapter builds BEADS_*, BD_*, DOLT_* from its config).
	shellRefused = []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`(^|_)(TOKEN|PASSWORD|SECRET|KEY)$|(^|_)KEY_`), "is secret-shaped: declare it in secrets"},
		{regexp.MustCompile(`^(LD_|DYLD_|GIT_SSH|GIT_CONFIG)|^(SSH_AUTH_SOCK|GIT_ASKPASS)$`), "could inject code or reach a credential agent"},
		{regexp.MustCompile(`^(BEADS|BD|DOLT)_`), "is a tracker CLI's own: the adapter builds it from its config"},
	}
)

// CheckEnv refuses a describe.env zm cannot honour: a bad name, a ZM_ name
// zm does not fill, a plain variable zm never passes from its shell
// (shellRefused), a secret also declared as a plain variable, or a secret
// source zm does not know.
func CheckEnv(d *orchapi.AdapterEnv) error {
	if d == nil {
		return nil
	}
	for _, v := range d.Vars {
		if !envNameRe.MatchString(v) || (strings.HasPrefix(v, "ZM_") && !slices.Contains(zmVars, v)) {
			return fmt.Errorf("describe.env.vars: %q is not a variable zm can pass", v)
		}
		for _, r := range shellRefused {
			if r.re.MatchString(strings.ToUpper(v)) {
				return fmt.Errorf("describe.env.vars: %s %s; zm never passes it from its environment", v, r.why)
			}
		}
	}
	for _, s := range d.Secrets {
		switch {
		case !envNameRe.MatchString(s.Name) || strings.HasPrefix(s.Name, "ZM_"):
			return fmt.Errorf("describe.env.secrets: bad name %q", s.Name)
		case slices.Contains(d.Vars, s.Name) || slices.Contains(BaseEnv, s.Name):
			return fmt.Errorf("describe.env.secrets: %s is also a plain variable", s.Name)
		case s.From != orchapi.SecretSlot && (!strings.HasPrefix(s.From, orchapi.SecretConfig) || s.From == orchapi.SecretConfig):
			return fmt.Errorf("describe.env.secrets: %s from %q: want %q or %q<key>", s.Name, s.From, orchapi.SecretSlot, orchapi.SecretConfig)
		}
	}
	return nil
}

// Env is the environment of an adapter declaring d (nil: the base only):
// BaseEnv and d.Vars from lookup (the daemon's environment) when set, the
// zm-filled names from zm, d.Secrets from secret (empty: left unset). It
// also returns the secret values, which no error may relay.
func Env(d *orchapi.AdapterEnv, lookup func(string) (string, bool), zm map[string]string, secret func(orchapi.AdapterSecret) (string, error)) (env, secrets []string, err error) {
	if err := CheckEnv(d); err != nil {
		return nil, nil, err
	}
	pass := BaseEnv
	if d != nil {
		pass = append(slices.Clone(BaseEnv), d.Vars...)
	}
	for _, k := range pass {
		v, ok := lookup(k)
		if slices.Contains(zmVars, k) {
			v, ok = zm[k], zm[k] != ""
		}
		if ok && !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") }) {
			env = append(env, k+"="+v)
		}
	}
	if d == nil {
		return env, nil, nil
	}
	for _, s := range d.Secrets {
		v, err := secret(s)
		if err != nil {
			return nil, nil, err
		}
		if v != "" {
			env, secrets = append(env, s.Name+"="+v), append(secrets, v)
		}
	}
	return env, secrets, nil
}

// Describe says what d gives an adapter beyond the base, for tracker add
// and tracker test ("" for none).
func Describe(d *orchapi.AdapterEnv) string {
	if d == nil || len(d.Vars)+len(d.Secrets) == 0 {
		return ""
	}
	parts := slices.Clone(d.Vars)
	for _, s := range d.Secrets {
		parts = append(parts, s.Name+" (secret, from "+s.From+")")
	}
	return "the adapter's environment: " + strings.Join(BaseEnv, ", ") + " and what it declares: " + strings.Join(parts, ", ")
}
