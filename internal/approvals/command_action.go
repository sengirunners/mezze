package approvals

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Command actions (SPEC 16 "Asks that carry an action"): one shell command
// line the approver reads word for word on the card, run on a named machine
// as them. It is a script run whose program is the line: the same trusted
// machine, the same clean checkout of a pinned commit, the same queue,
// output and result mail. Here the line is checked and frozen; the card
// says what it touches.

// secretLiterals are values that must never sit on a card: a command that
// needs a secret reads it on the machine (its keychain or a file there),
// never from the line. Plain patterns, failing closed: a false hit costs a
// rewrite that reads the value from the machine.
var secretLiterals = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY`),
	regexp.MustCompile(`(?i)\bauthorization:\s*(?:bearer|basic|token)\s+[^\s'"$]{8,}`),
	regexp.MustCompile(`://[^/\s:@]+:[^/\s@$]+@`),
	// curl -u user:password, --user user:password.
	regexp.MustCompile(`(?:^|\s)(?:-u|--user)[\s=]*['"]?[^\s:'"$]+:[^\s'"$]{3,}`),
	// mysql -pPASSWORD, --password=...: covered above for the long form.
	regexp.MustCompile(`\bmysql(?:dump|admin)?\b.*\s-p[^\s'"$]{3,}`),
}

// secretSetting finds NAME=value, NAME: value and --name[= ]value where the
// name is secret-shaped, also inside an identifier (AWS_SECRET_ACCESS_KEY=,
// NPM_TOKEN=, --api-token x), never a variable read ($PASSWORD, ${TOKEN}).
var secretSetting = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_${-])(-{1,2})?([A-Za-z0-9_-]*(?:password|passwd|token|secret|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_-]*)['"]?(\s*[=:]\s*|\s+)['"]?([^\s'"]+)`)

// notSecretName is a secret-shaped name that names where a secret is, not
// the secret: a file, a path, a variable, a name or id.
var notSecretName = regexp.MustCompile(`(?i)(?:file|path|dir|env|stdin|cmd|command|name|id|url|ref|count|len|ttl|type)s?$`)

// carriesSecret reports whether line sets a secret-shaped name to a literal.
func carriesSecret(line string) bool {
	for _, m := range secretSetting.FindAllStringSubmatch(line, -1) {
		flag, name, sep, value := m[1], m[2], m[3], m[4]
		if strings.TrimSpace(sep) == "" && flag == "" {
			continue // "gh secret list": a word, not a setting
		}
		if notSecretName.MatchString(name) || len(value) < 4 || strings.ContainsAny(value[:1], "$-/~.<") {
			continue
		}
		return true
	}
	return false
}

// elevated finds a privilege change: zm never uses sudo, and a run has no
// terminal to type a password into.
var elevated = regexp.MustCompile("(?:^|[\\s;&|(`!{/=]|\\$\\()(sudo|su|doas|pkexec|run0)(?:[\\s;&|)`]|$)")

// shellRisks are what commandWords miss in shell: removals and
// rewrites that name no English word.
var shellRisks = regexp.MustCompile(`(?i)(?:^|[\s;&|(])(rm|rmdir|shred|unlink|mkfs\S*|dd|dropdb|git\s+clean|git\s+push\s+(?:\S+\s+)*(?:-f|--force\S*)|kubectl\s+delete|terraform\s+(?:destroy|apply)|chmod\s+-R|chown\s+-R|truncate)(?:\s|$)|\|\s*((?:ba|z|da)?sh)(?:\s|$)`)

// checkCommand checks one command line for a card: it must read on the
// card exactly as it runs, so it is refused, never cleaned.
func checkCommand(line string) error {
	switch {
	case strings.TrimSpace(line) == "" || len(line) > orchapi.MaxActionCommand:
		return fmt.Errorf("a command is one line of 1 to %d bytes", orchapi.MaxActionCommand)
	case !utf8.ValidString(line) || strings.ContainsAny(line, "\x00\r\n  "):
		return fmt.Errorf("a command is one line: chain steps with && or ;, or put them in a script and use --script")
	case line != strings.TrimSpace(line) || strings.IndexFunc(line, notPlainASCII) >= 0:
		return fmt.Errorf("a command is plain printable ASCII with no leading or trailing space (no tab, control or look-alike letters): the card shows it exactly as it runs. Put anything else in a script and use --script")
	case unsafePresentation(line):
		return fmt.Errorf("a command never carries text that reads as zm's own (an owner decision, a recommendation, a fingerprint)")
	}
	for _, re := range secretLiterals {
		if re.MatchString(line) || carriesSecret(line) {
			return fmt.Errorf("the command carries what looks like a secret: a card never shows one. Read it on the machine instead (its keychain or a file there), e.g. \"$(security find-generic-password -w -s NAME)\"")
		}
	}
	if m := elevated.FindStringSubmatch(line); m != nil {
		return fmt.Errorf("the command uses %s: zm runs it as the approver and never changes user", m[1])
	}
	return nil
}

// notPlainASCII is a rune outside printable ASCII: a look-alike letter, a
// control, an invisible or direction character reads differently from what
// runs.
func notPlainASCII(r rune) bool { return r < 0x20 || r > 0x7e }

// commandWords are the risky names a command line can carry without false
// alarms on ordinary tools: production and deletion, publishing, secrets,
// forced rewrites. Money, access and messaging words are left out: in a
// shell line they are mostly paths and tool names (npm, billing/, admin).
// A command line is code, not prose, so this stays a scan (it keeps
// syntax deterministic); it only adds the card's "Read it closely" note, and
// what an ask's prose means is a judge's question.
var commandWords = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(prod|production|deploy\w*|live (site|data|database|users?)|drop (the )?(table|database)|delete\w*|destroy\w*|wipe\w*|purge\w*)\b`),
	regexp.MustCompile(`(?i)\b(publish\w*|go live)\b`),
	regexp.MustCompile(`(?i)\b(secrets?|tokens?|passwords?|credentials?|api keys?|private keys?|keys? rotation|rotat\w* (the )?keys?)\b`),
	regexp.MustCompile(`(?i)\b(irreversible|permanent\w*|cannot be undone|can't be undone|force[- ]push\w*|rm -rf|reset --hard|truncat\w*|overwrit\w*|terminat\w*)\b`),
}

// commandRisk is the first word of line a reader must weigh before choosing:
// production, deletion, publishing, secrets and forced rewrites, then shell
// removals and a download piped into a shell. "" when it names none.
func commandRisk(line string) string {
	for _, re := range commandWords {
		if m := re.FindString(line); m != "" {
			return m
		}
	}
	if m := shellRisks.FindStringSubmatch(line); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				return strings.Join(strings.Fields(g), " ")
			}
		}
	}
	return ""
}

// riskNote is the card's warning for a command line that names a risk, or "".
// Script runs and command actions read the same rule.
func riskNote(line string) string {
	w := commandRisk(line)
	if w == "" {
		return ""
	}
	return fmt.Sprintf(" Read it closely: it names %s, which can delete, touch production, money, access or credentials, and may not be undone.", quoted(w))
}
