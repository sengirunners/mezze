package publictree

import "testing"

// Exclude patterns read as bash's [[ f == $glob ]] in scripts/public-files.sh: * and ? cross /.
func TestShellMatch(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"OWNERSHIP.md", "OWNERSHIP.md", true},
		{"OWNERSHIP.md", "docs/OWNERSHIP.md", false},
		{"docs/*.md", "docs/a/b.md", true},
		{"*.env", "deploy/prod.env", true},
		{"scripts/?.sh", "scripts/a.sh", true},
		{"scripts/[ab].sh", "scripts/b.sh", true},
		{"scripts/[!ab].sh", "scripts/b.sh", false},
		{"scripts/[a-c]x", "scripts/bx", true},
		{"a\\*", "a*", true},
		{"a\\*", "ab", false},
		{"[x", "[x", true},
	} {
		if got := shellMatch(c.pat, c.s); got != c.want {
			t.Errorf("shellMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

// Each documented credential format is caught, a lookalike is not, and a report shows only its start. Samples are
// built from pieces so this file passes the gate.
func TestSecretRule(t *testing.T) {
	for _, s := range []string{
		"ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8", "AKIA" + "ABCDEFGHIJKLMNOP", "xoxb-" + "1234567890-abc",
		"-----BEGIN RSA " + "PRIVATE KEY-----", "sk_live_" + "51HxQ2bCdEfGhK8mP3vR7tY1", "tskey-auth-" + "kXyZ123456CNTRL",
		"https://bot:" + "hunter22@github.com/x", "sk-ant-" + "api03-abcdefghijklmnopqrstuvwxyz012345",
	} {
		hits := Scan("x.go", []byte("v := \""+s+"\"\n"), nil)
		if len(hits) != 1 || hits[0].Rule != "secret" {
			t.Errorf("%s: hits %v, want one secret", s[:6], hits)
		} else if len(hits[0].String()) > len("x.go:1: secret ")+14 {
			t.Errorf("a report shows the whole secret: %s", hits[0])
		}
	}
	for _, s := range []string{"ghp_short", "AKIA lowercase", "https://github.com/x", "git@github.com:x/y", "task-1234567890123456789012345678901234"} {
		if hits := Scan("x.go", []byte(s+"\n"), nil); len(hits) != 0 {
			t.Errorf("%q: false hit %v", s, hits)
		}
	}
	if Scan(AllowFile, []byte("AKIA"+"ABCDEFGHIJKLMNOP"), nil) != nil {
		t.Error("the gate's own allow list was scanned")
	}
}

// The publish-only rules (gitleaks', the judge) are allowed only by a line naming them: "*" covers the gate's own.
func TestAllowPublishOnlyRules(t *testing.T) {
	allow, err := ParseAllow([]byte("a/** *  # all gate rules\nb.go gitleaks:github-pat  # one fixture\nc.yml judge  # read by a person\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file, rule string
		want       bool
	}{
		{"a/x.go", "secret", true}, {"a/x.go", "gitleaks:github-pat", false}, {"a/x.yml", JudgeRule, false},
		{"b.go", "gitleaks:github-pat", true}, {"b.go", "gitleaks:jwt", false}, {"c.yml", JudgeRule, true},
	} {
		got := false
		for _, a := range allow {
			got = got || a.Matches(c.file, c.rule)
		}
		if got != c.want {
			t.Errorf("%s %s allowed = %v, want %v", c.file, c.rule, got, c.want)
		}
	}
	for _, bad := range []string{"x gitleaks:  # empty", "x gitleaks:Bad_ID  # not an id", "x judges  # typo"} {
		if _, err := ParseAllow([]byte(bad + "\n")); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// The names rule: case-insensitive unless a pattern says otherwise, per line, covered only by an allow line that
// names it (or "*"), and never run over the list itself. The names here are invented.
func TestNameRule(t *testing.T) {
	names, err := ParseNames([]byte("# a comment\nzorbo  # a person\n(?-i)\\bAcmeCo\\b  # a company, case-sensitive\n"))
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParseAllow([]byte("fixtures/**  private-name  # invented\nlegacy.md *  # every gate rule\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file, body string
		hits       int
	}{
		{"a.go", "x := \"Zorbo's laptop\"\n", 1},
		{"a.go", "// zorbo and ZORBO\n", 2},
		{"a.go", "AcmeCo and acmeco\n", 1}, // case-sensitive pattern: the lowercase spelling is not a hit
		{"a.go", "AcmeCorp\n", 0},          // a word boundary, not a prefix
		{"a.go", "nothing\nof\ninterest\n", 0},
		{"fixtures/a.json", "zorbo\n", 0},
		{"legacy.md", "zorbo\n", 0},
		{"b.bin", "zorbo\x00", 0},
		{NamesFile, "zorbo\n", 0},          // the list itself is the one file never read
		{AllowFile, "a.go * # zorbo\n", 1}, // the gate's other files ship, so they are read
		{"docs/zorbo.md", "nothing\n", 1},  // a path is read too (line 0)
		{"fixtures/zorbo.json", "x\n", 0},  // unless an allow line covers it
	} {
		if got := names.Scan(c.file, []byte(c.body), allow); len(got) != c.hits {
			t.Errorf("%s %q: %d hits %v, want %d", c.file, c.body, len(got), got, c.hits)
		}
	}
	if got := names.Scan("a.go", []byte("x\nzorbo\n"), nil); len(got) != 1 || got[0].Line != 2 || got[0].Rule != NameRule {
		t.Errorf("hit = %v, want line 2 of rule %s", got, NameRule)
	}
	if got := (Names(nil)).Scan("a.go", []byte("zorbo\n"), nil); got != nil {
		t.Errorf("no list scanned: %v", got)
	}
	// An allow line for the rule never covers a secret, and one for another rule never covers a name.
	other, _ := ParseAllow([]byte("a.go secret  # a planted key\n"))
	if got := names.Scan("a.go", []byte("zorbo\n"), other); len(got) != 1 {
		t.Errorf("an allow line for another rule covered a name: %v", got)
	}
	for _, bad := range []string{"zorbo\n", "zorbo  #\n", "(unclosed  # why\n", "a*  # matches the empty string\n", "# only a comment\n", ""} {
		if _, err := ParseNames([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// Public is what a copy keeps: the excluded files go, an override stands in for the file it replaces and is never a
// file itself, and an override of a file the copy leaves out is dropped.
func TestPublicStandsOverridesIn(t *testing.T) {
	ex, err := ParseExclude([]byte("secret.md  # ours\nops/**  # ours\n" + OverrideDir + "**  # the copy's own versions\n"))
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"a.go", "secret.md", "ops/run.sh", "ci.yml", OverrideDir + "ci.yml", OverrideDir + "secret.md", OverrideDir + "ops/x"}
	got := Public(ex, files)
	want := map[string]string{"a.go": "a.go", "ci.yml": OverrideDir + "ci.yml"}
	if len(got) != len(want) {
		t.Fatalf("Public = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Public[%s] = %q, want %q", k, got[k], v)
		}
	}
}

// A retired public name is refused only by ScanPublic, and only as the product word: identifiers, ids, approver
// names, the lowercase store value and the upstream T3 Code pass. Samples are built from pieces so this file passes.
func TestRetiredRule(t *testing.T) {
	old := "Z" + "3"
	for _, s := range []string{"the " + old + " door", old + "'s Decisions", "in " + old + ": Hand to my agent", "(" + old + ")"} {
		hits := ScanPublic("SPEC.md", []byte(s+"\n"), nil)
		if len(hits) != 1 || hits[0].Rule != RetiredRule {
			t.Errorf("%q: hits %v, want one %s", s, hits, RetiredRule)
		}
		if hits := Scan("SPEC.md", []byte(s+"\n"), nil); len(hits) != 0 {
			t.Errorf("%q: Scan ran a public-copy rule: %v", s, hits)
		}
	}
	for _, s := range []string{"ErrWanted" + old, old + "_AUTOMATION_STORE", "01J9" + old + "K0QX7Y8AZ5V2N4B6C8D0", "ivo-" + "z3",
		`"store": "z3"`, "T3 Code", "the Mezze app", "TestStore" + old + "Refuses"} {
		if hits := ScanPublic("x.go", []byte(s+"\n"), nil); len(hits) != 0 {
			t.Errorf("%q: false hit %v", s, hits)
		}
	}
	allow, err := ParseAllow([]byte("a.go " + RetiredRule + "  # a shipped migration\n"))
	if err != nil {
		t.Fatal(err)
	}
	if hits := ScanPublic("a.go", []byte("-- "+old+"'s id\n"), allow); len(hits) != 0 || !allow[0].Used {
		t.Errorf("an allow line naming the rule did not cover it: %v", hits)
	}
	// The gate's own files ship, so the public-copy rules read them; the private names list never ships.
	if hits := ScanPublic(AllowFile, []byte("x.go "+RetiredRule+"  # the "+old+" door\n"), nil); len(hits) != 1 {
		t.Errorf("an allow line's reason was not read: %v", hits)
	}
	if hits := ScanPublic(NamesFile, []byte(old+"\n"), nil); len(hits) != 0 {
		t.Errorf("the names list was scanned: %v", hits)
	}
}
