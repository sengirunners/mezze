package session

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A detector recognises a typed outcome in an agent's last output.
type detector struct {
	kind orchapi.OutcomeKind
	re   *regexp.Regexp
}

func det(kind orchapi.OutcomeKind, patterns ...string) []detector {
	out := make([]detector, len(patterns))
	for i, p := range patterns {
		out[i] = detector{kind, regexp.MustCompile(`(?i)` + p)}
	}
	return out
}

func join(ds ...[]detector) []detector {
	var out []detector
	for _, d := range ds {
		out = append(out, d...)
	}
	return out
}

// detectorTables is compiled on first use (a session's end), not at start-up:
// every zm call links this package, and compiling ~40 patterns cost about
// 0.1 ms of each CLI call's start.
var detectorTables = sync.OnceValue(func() map[string][]detector {
	// oomDetectors apply to every runtime: the process runtimes' own words.
	oomDetectors := det(orchapi.OutcomeOOM,
		`javascript heap out of memory`, `reached heap limit`, `fatal error: runtime: out of memory`,
		`\bout of memory\b`, `cannot allocate memory`, `\bMemoryError\b`)

	// One table per runtime (SPEC section 8). Each pattern is a phrase the
	// runtime prints; tests run them over testdata/outcomes. Agents with no
	// table use "default".
	return map[string][]detector{
		"claude": join(oomDetectors,
			det(orchapi.OutcomeQuota, `usage limit reached`, `you'?ve hit your (usage |weekly |session )?limit`,
				`\b5-hour limit reached`, `(weekly|opus) limit reached`, `limit will reset at`, `rate_limit_error`,
				`credit balance is too low`),
			det(orchapi.OutcomeAuth, `invalid api key`, `please run /login`, `not logged in`, `authentication_error`,
				`oauth token (has )?expired`, `api error: 401`)),
		"codex": join(oomDetectors,
			det(orchapi.OutcomeQuota, `you'?ve hit your usage limit`, `usage_limit_reached`, `rate limit reached`,
				`exceeded retry limit, last status: 429`, `quota exceeded`, `insufficient_quota`),
			det(orchapi.OutcomeAuth, `run ['"]?codex login`, `not logged in`, `401 unauthorized`, `invalid_api_key`,
				`(access|refresh) token (has )?expired`)),
		"gemini": join(oomDetectors,
			det(orchapi.OutcomeQuota, `quota exceeded`, `resource_exhausted`, `rate limit exceeded`, `status 429`),
			det(orchapi.OutcomeAuth, `api key not valid`, `\bunauthenticated\b`, `please (re-?)?authenticate`, `login required`)),
		"opencode": join(oomDetectors,
			det(orchapi.OutcomeQuota, `usage limit reached`, `rate limit (exceeded|reached)`, `quota exceeded`, `insufficient_quota`),
			det(orchapi.OutcomeAuth, `invalid api key`, `unauthorized`, `not logged in`)),
		"default": join(oomDetectors,
			det(orchapi.OutcomeQuota, `usage limit reached`, `rate limit exceeded`, `quota exceeded`, `insufficient_quota`),
			det(orchapi.OutcomeAuth, `invalid api key`, `authentication failed`, `not logged in`)),
	}
})

func detectorsFor(agent string) []detector {
	if d, ok := detectorTables()[agent]; ok {
		return d
	}
	return detectorTables()["default"]
}

var ansiRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\x1b\[[0-9;?<=>]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
})

// plainLines strips terminal escapes and splits the tail into lines.
func plainLines(tail []byte) []string {
	s := ansiRe().ReplaceAllString(string(bytes.ToValidUTF8(tail, nil)), "")
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.Split(s, "\n")
}

// detect returns the detector match nearest the end of tail.
func detect(agent string, tail []byte) (orchapi.OutcomeKind, string, bool) {
	lines := plainLines(tail)
	ds := detectorsFor(agent)
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		for _, d := range ds {
			if d.re.MatchString(l) {
				return d.kind, oneLine(l), true
			}
		}
	}
	return "", "", false
}

// exitInfo is everything known when a managed session's process ends.
type exitInfo struct {
	agent      string
	code       int    // exit status; -1 when it died by signal or is unknown
	signal     string // TERM, KILL, ... when it died by signal
	stopped    bool   // zm session stop asked for it
	stopBy     string
	daemonStop bool
	noDaemon   bool // it ended while no zm daemon ran (read from its exit file)
	tail       []byte
}

// classify turns an exit into a typed outcome. Anything not positively ok
// is never ok: a detector match beats exit 0, an unmatched non-zero exit is
// failed, and a stop by zm is killed unless the agent exited 0 cleanly.
func classify(x exitInfo) orchapi.Outcome {
	o := orchapi.Outcome{Signal: x.signal}
	if x.code >= 0 {
		c := x.code
		o.ExitCode = &c
	}
	if x.daemonStop {
		o.Kind, o.Confidence, o.Note, o.Evidence = orchapi.OutcomeInfra, "high", "daemon stopped", "the daemon ended the session"
		return o
	}
	if kind, line, ok := detect(x.agent, x.tail); ok {
		o.Kind, o.Evidence, o.Confidence = kind, line, "low"
		if x.stopped {
			o.Note = "stopped by " + x.stopBy
		}
		return o
	}
	switch {
	case x.stopped && x.code == 0:
		o.Kind, o.Confidence, o.Note = orchapi.OutcomeOK, "high", "stopped by "+x.stopBy+"; exited 0"
	case x.stopped:
		o.Kind, o.Confidence, o.Note = orchapi.OutcomeKilled, "high", "stopped by "+x.stopBy
		o.Evidence = exitText(x)
	case x.signal != "":
		o.Kind, o.Confidence, o.Evidence = orchapi.OutcomeKilled, "high", exitText(x)
		// zm cannot see who sent it: never guess a cause. A KILL
		// is as likely a stray kill -9 or pkill -9 -f as the OS.
		o.Note = "signal from outside zm; the sender is unknown"
		if x.noDaemon {
			o.Note += "; it ended while no zm daemon ran (one kill may have taken both)"
		}
	case x.code == 0:
		o.Kind, o.Confidence, o.Evidence = orchapi.OutcomeOK, "high", "exit 0"
	default:
		o.Kind, o.Confidence, o.Evidence = orchapi.OutcomeFailed, "high", exitText(x)
	}
	return o
}

func exitText(x exitInfo) string {
	if x.signal != "" {
		return "killed by SIG" + x.signal
	}
	return fmt.Sprintf("exit %d", x.code)
}
