// Package publictree is the public-tree gate as a library: the rules that keep personal
// addresses, our own names and secrets out of the public tree, the allow list that lets an intentional mention
// through, and the exclude list that says which files a public copy leaves out. CI runs it over every tracked file
// (internal/deps/publictree_test.go); scripts/publish runs it over the exact tree it pushes.
package publictree

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The repository paths the gate reads.
const (
	AllowFile   = "internal/deps/publictree_allow.txt"
	ExcludeFile = "internal/deps/public_exclude.txt"
	// NamesFile lists the names of our own people, handles, accounts, organisations and machines the public copy
	// must not carry. The list itself is private: the exclude list leaves it out and scripts/publish refuses a
	// commit whose copy would keep it, so a public copy has the rule (NameRule) and no names.
	NamesFile = "internal/deps/private_names.txt"
	// OverrideDir holds the public copy's own versions of a few files (the workflows that would otherwise use
	// the internal runners): OverrideDir+p replaces p in a public copy. The exclude list leaves the directory
	// itself out, and scripts/publish refuses an override whose path the copy does not keep.
	OverrideDir = "internal/deps/publiccopy/"
)

// Override says whether file is a public copy's version of another file, and which.
func Override(file string) (target string, ok bool) {
	target, ok = strings.CutPrefix(file, OverrideDir)
	return target, ok && target != ""
}

// Exempt says whether file is the gate itself, whose rules and allow list name what they look for.
func Exempt(file string) bool {
	return file == AllowFile || file == NamesFile || file == "internal/deps/publictree_test.go" ||
		strings.HasPrefix(file, "internal/deps/publictree/")
}

// Rule is one thing the public tree must not carry.
type Rule struct {
	ID    string
	re    *regexp.Regexp
	scope func(file string) bool // nil: every file
	skip  func(match []string) bool
	// public: only ScanPublic runs it, over the files a public copy keeps (the private repo's own plans and
	// workflows may still say what the public copy must not).
	public bool
}

// Names the fixtures use for a home directory: /Users/ana, /home/zm and so on.
var fixtureHomes = map[string]bool{
	"ana": true, "you": true, "a": true, "x": true, "me": true, "u": true, "user": true, "runner": true,
	"zm": true, "hometest": true, "testdata": true, "ubuntu": true, "shared": true, "alice": true, "bob": true,
}

var personalEmail = regexp.MustCompile(`(?i)\b([a-z0-9._%+-]+)@(gmail|googlemail|icloud|me|mac|yahoo|ymail|outlook|hotmail|live|aol|proton|protonmail|pm|fastmail|hey)\.(com|me|net)\b`)

func notPersonal(m []string) bool {
	l := strings.ToLower(m[1])
	return l == "mailer-daemon" || l == "postmaster" || l == "noreply" || l == "no-reply"
}

// PersonalEmail says whether s holds a personal mail address (a consumer mail provider's).
func PersonalEmail(s string) bool {
	for _, m := range personalEmail.FindAllStringSubmatch(s, -1) {
		if !notPersonal(m) {
			return true
		}
	}
	return false
}

// secret is a credential in a format its issuer documents, in the spirit of gitleaks' default rules: high
// precision, no entropy guessing. A fixture that needs one is allowed by file in publictree_allow.txt.
var secret = regexp.MustCompile(strings.Join([]string{
	`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`,      // GitHub tokens
	`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,                                      // AWS access key id
	`\bxox[abeprs]-[A-Za-z0-9-]{10,}`,                                    // Slack tokens
	`hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`,      // Slack webhook
	`\bglpat-[A-Za-z0-9_-]{20,}`,                                         // GitLab token
	`-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----`,                        // PEM, OpenSSH and PGP private keys
	`\bAIza[0-9A-Za-z_-]{35}`,                                            // Google API key
	`\b[rs]k_live_[0-9A-Za-z]{20,}`,                                      // Stripe live key
	`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{32,}`,                             // Anthropic and OpenAI keys
	`\bnpm_[A-Za-z0-9]{36}\b`,                                            // npm token
	`\btskey-[a-z]+-[A-Za-z0-9-]{10,}`,                                   // Tailscale keys
	`\bAGE-SECRET-KEY-1[0-9A-Z]{50,}`,                                    // age identity
	`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`, // JWT
	`\b[a-z][a-z0-9+.-]*://[^/\s:@'"$]+:[^/\s@'"$]{3,}@[A-Za-z0-9]`,      // password in a URL
}, "|"))

// Rules is the gate, in the order hits are reported.
var Rules = []Rule{
	{ID: "personal-email", re: personalEmail, skip: notPersonal},
	{ID: "home-path",
		re:   regexp.MustCompile(`(?:^|[\s"'=(:\x60])/(?:Users|home)/([A-Za-z0-9._-]+)`),
		skip: func(m []string) bool { return fixtureHomes[strings.ToLower(m[1])] }},
	// A real tailnet's MagicDNS suffix (tail + hex); fixtures use tail1, tail9, tail1234.
	{ID: "tailnet-name", re: regexp.MustCompile(`\btail[0-9a-f]{5,}\.ts\.net\b`)},
	// <worker>.<account>.workers.dev names a Cloudflare account; fixtures use example or test.
	{ID: "workers-dev-account",
		re:   regexp.MustCompile(`\b[a-z0-9-]+\.([a-z0-9-]+)\.workers\.dev\b`),
		skip: func(m []string) bool { return m[1] == "example" || m[1] == "test" }},
	{ID: "secret", re: secret},
	// A retired public name: the app is the Mezze app. Its old name only as a whole word in capitals, so
	// identifiers (ErrWantedZ3, Z3_AUTOMATION_STORE), ids, the lowercase store value and approver names, and the
	// upstream T3 Code do not match. The daemon's old public name is in the private names list (NamesFile).
	{ID: RetiredRule, re: regexp.MustCompile(`\bZ3\b`), public: true},
}

// RetiredRule is the rule for a retired public name (Rules); it holds only in the files a public copy keeps.
const RetiredRule = "retired-name"

// Rules the publish script runs over the exact tree it pushes, beside the gate's own (scripts/publish):
// gitleaks' default rules, named GitleaksRule + its rule id, and the model judge of config-like files. CI does not
// run them, so their allow lines are checked there only for their form; "*" never covers them.
const (
	GitleaksRule = "gitleaks:"
	JudgeRule    = "judge"
)

var gitleaksID = regexp.MustCompile(`^` + GitleaksRule + `[a-z0-9][a-z0-9-]*$`)

// PublishOnly says whether rule is one only scripts/publish runs.
func PublishOnly(rule string) bool { return rule == JudgeRule || gitleaksID.MatchString(rule) }

// Allow is one line of publictree_allow.txt: `<path, glob or dir/**> <rule or *>  # why`.
type Allow struct {
	Glob, Rule, Why string
	Used            bool // a scan matched it
}

// ParseAllow reads publictree_allow.txt.
func ParseAllow(b []byte) ([]*Allow, error) {
	var out []*Allow
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		body, why, _ := strings.Cut(line, "#")
		f := strings.Fields(body)
		if len(f) != 2 || strings.TrimSpace(why) == "" {
			return nil, fmt.Errorf("%s:%d: want `<path or glob> <rule>  # reason`, got %q", AllowFile, n, line)
		}
		known := f[1] == "*" || f[1] == NameRule || PublishOnly(f[1])
		for _, r := range Rules {
			known = known || r.ID == f[1]
		}
		if !known {
			return nil, fmt.Errorf("%s:%d: unknown rule %q for %s", AllowFile, n, f[1], f[0])
		}
		out = append(out, &Allow{Glob: f[0], Rule: f[1], Why: strings.TrimSpace(why)})
	}
	return out, sc.Err()
}

// Matches: a glob ending in /** covers that directory's whole tree; otherwise path.Match. A publish-only rule
// (gitleaks, the judge) is covered only by a line naming it.
func (a *Allow) Matches(file, rule string) bool {
	if a.Rule != rule && (a.Rule != "*" || PublishOnly(rule)) {
		return false
	}
	if dir, ok := strings.CutSuffix(a.Glob, "/**"); ok {
		return strings.HasPrefix(file, dir+"/")
	}
	ok, _ := path.Match(a.Glob, file)
	return ok
}

// Hit is a rule matching a line no allow line covers.
type Hit struct {
	File  string
	Line  int
	Rule  string
	Match string
}

// String names the hit; a secret shows only its first characters, so a report or CI log does not spread it.
func (h Hit) String() string {
	m := h.Match
	if h.Rule == "secret" && len(m) > 8 {
		m = m[:8] + "..."
	}
	return fmt.Sprintf("%s:%d: %s %q", h.File, h.Line, h.Rule, m)
}

// Scan returns body's hits, marking the allow lines it used. The gate's own files and binary files (a NUL byte)
// are not scanned.
func Scan(file string, body []byte, allow []*Allow) []Hit { return scan(file, body, allow, false) }

// ScanPublic runs the rules that hold only in a public copy (RetiredRule) over a file it keeps, by its public path.
func ScanPublic(file string, body []byte, allow []*Allow) []Hit { return scan(file, body, allow, true) }

// The public-copy rules read the gate's own files too (they ship, and an allow line's reason is free text), as
// Names.Scan does; only the names list, which a public copy never carries, is skipped.
func scan(file string, body []byte, allow []*Allow, public bool) []Hit {
	if (public && file == NamesFile) || (!public && Exempt(file)) || bytes.IndexByte(body, 0) >= 0 {
		return nil
	}
	var hits []Hit
	lines := strings.Split(string(body), "\n")
	for _, r := range Rules {
		if r.public != public || (r.scope != nil && !r.scope(file)) {
			continue
		}
		for i, line := range lines {
			for _, m := range r.re.FindAllStringSubmatch(line, -1) {
				if r.skip != nil && r.skip(m) {
					continue
				}
				if !markAllowed(allow, file, r.ID) {
					hits = append(hits, Hit{File: file, Line: i + 1, Rule: r.ID, Match: m[0]})
				}
			}
		}
	}
	return hits
}

// NameRule is the rule for the names in NamesFile. It runs where that file exists (the private repo's test, and
// scripts/publish over the exact tree it pushes) and over the files a public copy keeps, never over the file itself.
const NameRule = "private-name"

// Names is NamesFile: case-insensitive patterns, one per line, each with a reason.
type Names []*regexp.Regexp

// ParseNames reads NamesFile: `<regular expression>  # why`. A line that does not compile is an error, and so is a
// list with no pattern: a list that cannot be read must not pass a tree.
func ParseNames(b []byte) (Names, error) {
	var out Names
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		body, why, ok := strings.Cut(line, "  #")
		body = strings.TrimSpace(body)
		if !ok || body == "" || strings.TrimSpace(why) == "" {
			return nil, fmt.Errorf("%s:%d: want `<regular expression>  # reason`", NamesFile, i+1)
		}
		re, err := regexp.Compile("(?i)" + body)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", NamesFile, i+1, err)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("%s:%d: %q matches the empty string", NamesFile, i+1, body)
		}
		out = append(out, re)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists nothing", NamesFile)
	}
	return out, nil
}

// Scan returns the hits of a name in file's path (line 0) and in the lines of body that no allow line covers,
// marking the lines it used. Unlike the other rules it reads the gate's own files too (the allow list and the
// tests ship, and an allow line's reason is free text): only the list itself is skipped. Binary bodies are not read.
func (n Names) Scan(file string, body []byte, allow []*Allow) []Hit {
	if len(n) == 0 || file == NamesFile {
		return nil
	}
	var hits []Hit
	add := func(line int, text string) {
		for _, re := range n {
			for _, m := range re.FindAllString(text, -1) {
				if !markAllowed(allow, file, NameRule) {
					hits = append(hits, Hit{File: file, Line: line, Rule: NameRule, Match: m})
				}
			}
		}
	}
	add(0, file)
	if bytes.IndexByte(body, 0) >= 0 {
		return hits
	}
	for i, line := range strings.Split(string(body), "\n") {
		add(i+1, line)
	}
	return hits
}

// markAllowed says whether an allow line covers rule for file, marking every line that does.
func markAllowed(allow []*Allow, file, rule string) bool {
	ok := false
	for _, a := range allow {
		if a.Matches(file, rule) {
			a.Used, ok = true, true
		}
	}
	return ok
}

// Public maps each path a public copy of files carries to the file that supplies its content: the file itself, or
// its override under OverrideDir. A file the exclude list names is left out, and so is an override (it stands in
// for its target, whether or not the target is kept).
func Public(ex Exclude, files []string) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		if _, over := Override(f); !over && !ex.Excludes(f) {
			out[f] = f
		}
	}
	for _, f := range files {
		if target, over := Override(f); over {
			if _, kept := out[target]; kept {
				out[target] = f
			}
		}
	}
	return out
}

// Exclude is public_exclude.txt: the paths a public copy leaves out.
type Exclude []ExcludeLine

// ExcludeLine is `<path, glob or dir/**>  # why`; a reason starting "guard:" marks a dir/** kept for files that
// may come back.
type ExcludeLine struct {
	Glob, Why string
	Line      int
}

// ParseExclude reads public_exclude.txt. A line that is not one path and a reason is an error, never skipped:
// a list that cannot be read must not publish more than it says.
func ParseExclude(b []byte) (Exclude, error) {
	var out Exclude
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		body, why, _ := strings.Cut(line, "#")
		f := strings.Fields(body)
		if len(f) != 1 || strings.TrimSpace(why) == "" {
			return nil, fmt.Errorf("%s:%d: want `<path or glob>  # reason`, got %q", ExcludeFile, i+1, line)
		}
		out = append(out, ExcludeLine{Glob: f[0], Why: strings.TrimSpace(why), Line: i + 1})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists nothing", ExcludeFile)
	}
	return out, nil
}

// Matches: dir/** covers that directory's tree; otherwise a shell pattern as scripts/public-files.sh reads it
// (`[[ f == $glob ]]`: * and ? also match /), so the Go copy and the script leave out the same files.
func (l ExcludeLine) Matches(file string) bool {
	if dir, ok := strings.CutSuffix(l.Glob, "/**"); ok {
		return strings.HasPrefix(file, dir+"/")
	}
	return shellMatch(l.Glob, file)
}

// Excludes says whether a public copy leaves file out.
func (e Exclude) Excludes(file string) bool {
	for _, l := range e {
		if l.Matches(file) {
			return true
		}
	}
	return false
}

// shellMatch is bash's pattern match for *, ? and [...] (with ! or ^ negation), where both wildcards cross /.
// A malformed bracket matches itself literally, as in bash.
func shellMatch(pat, s string) bool {
	if pat == "" {
		return s == ""
	}
	switch pat[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if shellMatch(pat[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && shellMatch(pat[1:], s[1:])
	case '[':
		if end := strings.IndexByte(pat[1:], ']'); end > 0 && s != "" {
			set := pat[1 : end+1]
			neg := set[0] == '!' || set[0] == '^'
			if neg {
				set = set[1:]
			}
			in := false
			for i := 0; i < len(set); i++ {
				if i+2 < len(set) && set[i+1] == '-' {
					in = in || (s[0] >= set[i] && s[0] <= set[i+2])
					i += 2
				} else {
					in = in || s[0] == set[i]
				}
			}
			return in != neg && shellMatch(pat[end+2:], s[1:])
		}
	case '\\':
		if len(pat) > 1 {
			return s != "" && s[0] == pat[1] && shellMatch(pat[2:], s[1:])
		}
	}
	return s != "" && s[0] == pat[0] && shellMatch(pat[1:], s[1:])
}
