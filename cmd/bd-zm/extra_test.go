package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// inDir runs the rest of the test in a fresh directory with HOME there too
// (bd prime reads ./.beads, $BEADS_DIR and the user config dir).
func inDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	return dir
}

func TestShimMemories(t *testing.T) {
	s := newShim(t)
	s.ok("create", "make the store exist", "--silent")
	if out := s.ok("remember", "Always run tests with -race flag"); out != "Remembered [always-run-tests-with-race-flag]: Always run tests with -race flag\n" {
		t.Fatalf("remember %q", out)
	}
	out := s.ok("remember", "dolt phantoms hide\nin three places", "--key", "dolt-phantoms", "--json")
	if got := keys(t, out); strings.Join(got, ",") != "action,key,schema_version,value" || !strings.Contains(out, `"action": "remembered"`) {
		t.Fatalf("remember json %s", out)
	}
	if out := s.ok("remember", "new text", "--key", "dolt-phantoms"); !strings.HasPrefix(out, "Updated [dolt-phantoms]") {
		t.Fatalf("update %q", out)
	}
	// .beads/prime.mjs reads `bd memories --json`: an object of key -> text plus schema_version.
	out = s.ok("memories", "--json")
	var ms map[string]any
	if err := json.Unmarshal([]byte(out), &ms); err != nil || ms["dolt-phantoms"] != "new text" || ms["schema_version"] != float64(1) || len(ms) != 3 {
		t.Fatalf("memories json %s", out)
	}
	if out := s.ok("memories", "race"); !strings.Contains(out, "Memories matching \"race\":") || strings.Contains(out, "dolt-phantoms") {
		t.Fatalf("memories search %q", out)
	}
	if out := s.ok("recall", "dolt-phantoms"); out != "new text\n" {
		t.Fatalf("recall %q", out)
	}
	if _, errOut, code := s.bd("recall", "nope"); code != 1 || !strings.Contains(errOut, `No memory with key "nope"`) {
		t.Fatalf("recall missing: %d %q", code, errOut)
	}
	if out := s.ok("forget", "dolt-phantoms"); out != "Forgot [dolt-phantoms]: new text\n" {
		t.Fatalf("forget %q", out)
	}
	if _, _, code := s.bd("forget", "dolt-phantoms"); code != 1 {
		t.Fatal("forget twice succeeded")
	}
	if _, _, code := s.bd("remember", "x", "--readonly"); code == 0 {
		t.Fatal("remember under --readonly")
	}
}

func TestShimPrime(t *testing.T) {
	s := newShim(t)
	s.ok("create", "x", "--silent")
	s.ok("remember", "keep the race flag", "--key", "race")
	dir := inDir(t)
	// No PRIME.md: the zm-native context with every memory in full.
	out := s.ok("prime")
	if !strings.Contains(out, "zm-native") || !strings.Contains(out, "### race\nkeep the race flag") || strings.Contains(out, "git push") {
		t.Fatalf("default prime:\n%s", out)
	}
	if out := s.ok("prime", "--memories-only"); !strings.Contains(out, "## Persistent Memories (1)") || strings.Contains(out, "Session close") {
		t.Fatalf("memories-only:\n%s", out)
	}
	// $BEADS_DIR/PRIME.md, then ./.beads/PRIME.md wins.
	bdir := filepath.Join(dir, "elsewhere", ".beads")
	_ = os.MkdirAll(bdir, 0o755)
	_ = os.WriteFile(filepath.Join(bdir, "PRIME.md"), []byte("from BEADS_DIR\n"), 0o644)
	t.Setenv("BEADS_DIR", bdir)
	if out := s.ok("prime"); out != "from BEADS_DIR\n" {
		t.Fatalf("BEADS_DIR prime %q", out)
	}
	_ = os.MkdirAll(".beads", 0o755)
	_ = os.WriteFile(filepath.Join(".beads", "PRIME.md"), []byte("# Beads (harbor)\nlocal rules\n"), 0o644)
	if out := s.ok("prime"); out != "# Beads (harbor)\nlocal rules\n" {
		t.Fatalf("local prime %q", out)
	}
	out = s.ok("prime", "--hook-json")
	var env struct {
		H struct {
			Event string `json:"hookEventName"`
			Ctx   string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.H.Event != "SessionStart" || env.H.Ctx != "# Beads (harbor)\nlocal rules\n" {
		t.Fatalf("hook json %q", out)
	}
	if out := s.ok("prime", "--export"); !strings.Contains(out, "zm-native") {
		t.Fatalf("export ignores PRIME.md: %q", out)
	}
	// zm down: prime still exits 0 with nothing on stderr (it runs in every SessionStart hook).
	newClient = func(context.Context) (*client.Client, error) { return nil, os.ErrNotExist }
	_ = os.Remove(filepath.Join(".beads", "PRIME.md"))
	t.Setenv("BEADS_DIR", "")
	if out, errOut, code := s.bd("prime"); code != 0 || errOut != "" || !strings.Contains(out, "zm-native") {
		t.Fatalf("prime with zm down: %d %q %q", code, out, errOut)
	}
}

func TestShimDoltAndNoops(t *testing.T) {
	s := newShim(t)
	// bd-guard greps `bd dolt show` for this line.
	if out := s.ok("dolt", "show"); !strings.Contains(out, "✓ Server connection OK") {
		t.Fatalf("dolt show %q", out)
	}
	for _, v := range [][]string{{"dolt", "push"}, {"dolt", "pull"}, {"dolt", "commit", "-m", "x"}, {"sync"}} {
		if out := s.ok(v...); !strings.HasPrefix(out, "zm-native: nothing to") {
			t.Fatalf("%v: %q", v, out)
		}
	}
	for _, v := range [][]string{{"dolt", "remote", "add", "x", "y"}, {"config", "set", "a", "b"}, {"init"}, {"import", "x.jsonl"},
		{"backup", "sync"}, {"prune", "--older-than", "1d"}, {"edit", "x"}, {"delete", "x"}, {"frobnicate"}} {
		_, errOut, code := s.bd(v...)
		if code != 1 || !strings.Contains(errOut, "zm") {
			t.Fatalf("%v: %d %q", v, code, errOut)
		}
	}
	if out := s.ok("version"); out != "bd version 1.0.5 (bd-zm: zm-native)\n" {
		t.Fatalf("version %q", out)
	}
	if out := s.ok("where"); !strings.HasPrefix(out, "zm-native: oc") {
		t.Fatalf("where %q", out)
	}
}

// embeadify's doctor runs `bd --version`, then `bd context --json`, and
// writes only when both exit 0 and context names a target (keys matching
// host|port|database|^db|path|dir|backend|server|mode|workspace|remote|url).
func TestShimVersionAndContext(t *testing.T) {
	s := newShim(t)
	id := s.ok("create", "make the store exist", "--silent")
	out := s.ok("context", "--json")
	var ctx map[string]any
	if err := json.Unmarshal([]byte(out), &ctx); err != nil {
		t.Fatalf("context json %q: %v", out, err)
	}
	prefix, _, _ := strings.Cut(strings.TrimSpace(id), "-")
	if ctx["backend"] != "zm-native" || ctx["database"] != "oc" || ctx["server"] != "zm" || ctx["bd_version"] != "1.0.5" ||
		ctx["issue_prefix"] != prefix || ctx["project_id"] == "" || ctx["schema_version"] != float64(1) {
		t.Fatalf("context json %s", out)
	}
	if _, ok := ctx["sync_remote"]; ok {
		t.Fatalf("a project this daemon holds has no authority elsewhere: %s", out)
	}
	if out := s.ok("context"); !strings.HasPrefix(out, "bd version:     1.0.5 (bd-zm: zm-native)\n") ||
		!strings.Contains(out, "  database:     oc (zm project)\n") || !strings.Contains(out, "  issue prefix: "+prefix+"\n") {
		t.Fatalf("context text:\n%s", out)
	}
	if _, _, code := s.bd("context", "extra"); code != 1 {
		t.Fatalf("context with an arg: exit %d", code)
	}
	// No project resolves: no write target, so context fails (embeadify then refuses).
	inDir(t)
	fresh := newShim(t)
	fresh.c.Project = ""
	if _, errOut, code := fresh.bd("context", "--json"); code != 1 || !strings.Contains(errOut, "no project") {
		t.Fatalf("context with no project: %d %q", code, errOut)
	}
	// A project with no issues yet is still the target (its first create makes them).
	fresh.c.Project, fresh.c.ProjectFrom = "oc", "env"
	if out := fresh.ok("context", "--json"); !strings.Contains(out, `"database": "oc"`) || !strings.Contains(out, `"project_id": "`) ||
		strings.Contains(out, "issue_prefix") {
		t.Fatalf("context, project without issues: %s", out)
	}
	// A named project zm does not have is no target.
	if _, errOut, code := fresh.bd("--project", "nosuch", "context"); code != 1 || !strings.Contains(errOut, `no project "nosuch"`) {
		t.Fatalf("context, unknown project: %d %q", code, errOut)
	}
	// $ZM_PROJECT set but zm does not answer: exit 3 (zm down), never a target.
	sockDir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	down := client.NewUnix(filepath.Join(sockDir, "zm.sock"))
	down.Project, down.ProjectFrom = "oc", "env"
	newClient = func(context.Context) (*client.Client, error) { return down, nil }
	for _, args := range [][]string{{"context", "--json"}, {"context"}} {
		if out, errOut, code := s.bd(args...); code != 3 || out != "" {
			t.Fatalf("%v with zm down: %d %q %q", args, code, out, errOut)
		}
	}
	down.Project = ""
	if _, errOut, code := s.bd("context"); code != 3 || strings.Contains(errOut, "no project") {
		t.Fatalf("context, no project and zm down: %d %q", code, errOut)
	}
	t.Setenv("BEADS_DB", "x")
	if out := s.ok("--version"); out != "bd version 1.0.5 (bd-zm: zm-native)\n" {
		t.Fatalf("--version under BEADS_DB %q", out)
	}
	t.Setenv("BEADS_DB", "")
	// --version and -V need no zm (bd answers them without a database).
	newClient = func(context.Context) (*client.Client, error) { return nil, os.ErrNotExist }
	for _, f := range []string{"--version", "-V"} {
		if out := s.ok(f); out != "bd version 1.0.5 (bd-zm: zm-native)\n" {
			t.Fatalf("%s %q", f, out)
		}
	}
	if _, _, code := s.bd("context"); code == 0 {
		t.Fatal("context with zm down exited 0")
	}
	if out := s.ok(); !strings.Contains(out, "Usage:") {
		t.Fatalf("bare bd %q", out)
	}
}

func TestShimNote(t *testing.T) {
	s := newShim(t)
	id := strings.TrimSpace(s.ok("create", "n", "--silent"))
	if out := s.ok("note", id, "first", "line"); out != "✓ Note added to "+id+" — n\n" {
		t.Fatalf("note %q", out)
	}
	s.ok("note", id, "second")
	if out := s.ok("show", id, "--json"); !strings.Contains(out, `"notes": "first line\nsecond"`) {
		t.Fatalf("notes %s", out)
	}
}

func TestShimExport(t *testing.T) {
	s := newShim(t)
	a := strings.TrimSpace(s.ok("create", "alpha", "-p", "1", "-a", "ana", "-l", "x", "--silent"))
	b := strings.TrimSpace(s.ok("create", "beta", "-p", "0", "-a", "ana", "--silent"))
	s.ok("create", "an agent", "-t", "agent", "--silent")
	s.ok("dep", "add", a, b)
	s.ok("comments", "add", a, "hi")
	s.ok("close", b, "-r", "done")
	s.ok("remember", "m1", "--key", "m1")
	exportPage = 1 // page by page
	t.Cleanup(func() { exportPage = 2000 })
	out := s.ok("export")
	sc := bufio.NewScanner(strings.NewReader(out))
	var recs []map[string]any
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), `{"_type":"issue","id":`) {
			t.Fatalf("line %q", sc.Text())
		}
		var m map[string]any
		_ = json.Unmarshal(sc.Bytes(), &m)
		recs = append(recs, m)
	}
	// Closed issues are exported, agents are not; priority order.
	if len(recs) != 2 || recs[0]["id"] != b || recs[1]["id"] != a {
		t.Fatalf("export order/filter: %s", out)
	}
	ra := recs[1]
	if ra["comment_count"] != float64(1) || ra["dependency_count"] != float64(1) || len(ra["comments"].([]any)) != 1 ||
		len(ra["dependencies"].([]any)) != 1 || ra["labels"].([]any)[0] != "x" {
		t.Fatalf("export record %v", ra)
	}
	if _, ok := ra["parent"]; ok {
		t.Fatal("export carries parent")
	}
	out = s.ok("export", "--all", "--include-memories")
	if !strings.Contains(out, `"issue_type":"agent"`) || !strings.HasSuffix(out, `{"_type":"memory","key":"m1","value":"m1"}`+"\n") {
		t.Fatalf("export --all:\n%s", out)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "issues.jsonl")
	_, errOut, code := s.bd("export", "-o", f)
	if code != 0 || errOut != "Exported 2 issues to "+f+"\n" {
		t.Fatalf("export -o: %d %q", code, errOut)
	}
	if b, _ := os.ReadFile(f); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("file %q", b)
	}
}

func TestShimSQL(t *testing.T) {
	s := newShim(t)
	a := strings.TrimSpace(s.ok("create", "alpha", "-l", "ruled", "--silent"))
	s.ok("create", "beta", "--silent")
	out := s.ok("sql", "--json", "select id, title, priority, created_at, assignee from issues where id = '"+a+"'")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("sql json %s", out)
	}
	r := rows[0]
	if r["title"] != "alpha" || r["priority"] != float64(2) || r["assignee"] != nil || !strings.HasSuffix(r["created_at"].(string), "Z") {
		t.Fatalf("row %v", r)
	}
	if got := keys(t, firstElem(t, out)); strings.Join(got, ",") != "assignee,created_at,id,priority,title" {
		t.Fatalf("sql keys %v", got)
	}
	if out := s.ok("sql", "--csv", "select l.label, count(*) n from labels l group by l.label"); out != "label,n\nruled,1\n" {
		t.Fatalf("csv %q", out)
	}
	if out := s.ok("sql", "select count(*) n from issues"); out != "n\n-\n2\n(1 rows)\n" {
		t.Fatalf("table %q", out)
	}
	for _, q := range []string{"delete from issues", "select 1; delete from issues", "select nosuch from issues"} {
		if _, errOut, code := s.bd("sql", q); code != 1 || errOut == "" {
			t.Fatalf("%q: %d %q", q, code, errOut)
		}
	}
}

// From a directory that resolves no project, bd works in the only project
// with native issues (bd here always reached one database).
func TestShimProjectFallback(t *testing.T) {
	s := newShim(t)
	id := strings.TrimSpace(s.ok("create", "in oc", "--silent"))
	if _, err := s.c.CreateProject(context.Background(), api.NewProjectRequest{Slug: "other"}); err != nil {
		t.Fatal(err)
	}
	s.c.Project, s.c.ProjectFrom = "", ""
	inDir(t)
	if out := s.ok("list", "--json"); !strings.Contains(out, id) {
		t.Fatalf("list from nowhere %s", out)
	}
	scopeDone = false
	s.c.Project, s.c.ProjectFrom = "", ""
	id2 := strings.TrimSpace(s.ok("create", "also oc", "--silent"))
	if !strings.HasPrefix(id2, "oc-") {
		t.Fatalf("create from nowhere went to %q", id2)
	}
	scopeDone = false
	s.c.Project, s.c.ProjectFrom = "", ""
	if kid := strings.TrimSpace(s.ok("create", "child", "--parent", id, "--json")); !strings.Contains(kid, `"id": "`+id+`.1"`) {
		t.Fatalf("child from nowhere: %s", kid)
	}
	s.c.Project, s.c.ProjectFrom = "", ""
	if out := s.ok("where"); !strings.HasPrefix(out, "zm-native: oc,") {
		t.Fatalf("where %q", out)
	}
}

// A claim's assignee is bd's actor (a routing label such as a hub address):
// --actor, then BEADS_ACTOR, BD_ACTOR, FARM_BD_ACTOR, git's user.name, $USER.
// Authors stay the zm identity.
func TestShimClaimActor(t *testing.T) {
	s := newShim(t)
	assignee := func(id string) string {
		var d []map[string]any
		_ = json.Unmarshal([]byte(s.ok("show", id, "--json")), &d)
		a, _ := d[0]["assignee"].(string)
		return a
	}
	t.Setenv("BD_ACTOR", "ivo-mac/58565660")
	a := strings.TrimSpace(s.ok("create", "a", "--silent"))
	out := s.ok("update", a, "--claim", "--json")
	if !strings.Contains(out, `"assignee": "ivo-mac/58565660"`) {
		t.Fatalf("claim under BD_ACTOR: %s", out)
	}
	if out := s.ok("show", a, "--json"); !strings.Contains(out, `"created_by": "ana"`) {
		t.Fatalf("author changed: %s", out)
	}
	s.ok("comments", "add", a, "hi")
	if out := s.ok("comments", a, "--json"); !strings.Contains(out, `"author": "ana"`) {
		t.Fatalf("comment author: %s", out)
	}
	// --actor (bd-guard passes FARM_BD_ACTOR this way) beats the environment.
	b := strings.TrimSpace(s.ok("create", "b", "--silent"))
	s.ok("--actor", "dev-02/cafe0001", "update", b, "--claim")
	if got := assignee(b); got != "dev-02/cafe0001" {
		t.Fatalf("--actor: %q", got)
	}
	// ready --claim too.
	t.Setenv("BD_ACTOR", "")
	t.Setenv("BEADS_ACTOR", "ivo-mac/beadsact")
	s.ok("create", "ready one", "--silent")
	out = s.ok("ready", "--claim", "--json")
	if !strings.Contains(out, `"assignee": "ivo-mac/beadsact"`) {
		t.Fatalf("ready --claim: %s", out)
	}
	t.Setenv("BEADS_ACTOR", "")
	t.Setenv("FARM_BD_ACTOR", "dev-04/farm0001")
	c := strings.TrimSpace(s.ok("create", "c", "--silent"))
	s.ok("update", c, "--claim")
	if got := assignee(c); got != "dev-04/farm0001" {
		t.Fatalf("FARM_BD_ACTOR: %q", got)
	}
	// Another label conflicts, naming the holder (bd refused a second actor).
	if _, errOut, code := s.bd("--actor", "ivo-mac/other", "update", c, "--claim"); code == 0 || !strings.Contains(errOut, "dev-04/farm0001") {
		t.Fatalf("other actor: %d %q", code, errOut)
	}
	// Inside an agent session (the call answers as a session: $ZM_AS or the
	// runtime's id), the session, plus the lane from $ZM_LANE; never git's
	// user.name, which named a person and so routed mail to the whole Mac.
	t.Setenv("FARM_BD_ACTOR", "")
	gitUserName = func() string { return "Ivo Doe\n" }
	s.c.As = "mini/aaaa0001"
	g := strings.TrimSpace(s.ok("create", "g", "--silent"))
	out = s.ok("update", g, "--claim", "--json")
	if got := assignee(g); got != "mini/aaaa0001" || !strings.Contains(out, `"claimed_by": "mini/aaaa0001"`) {
		t.Fatalf("session claim: assignee %q, %s", got, out)
	}
	t.Setenv("ZM_LANE", "zm-claims")
	h := strings.TrimSpace(s.ok("create", "h", "--silent"))
	s.ok("ready", "--claim")
	if got := assignee(h); got != "zm-claims@mini/aaaa0001" {
		t.Fatalf("lane claim (ready --claim): %q", got)
	}
	t.Setenv("ZM_LANE", "Bad Lane")
	k := strings.TrimSpace(s.ok("create", "k", "--silent"))
	if _, errOut, code := s.bd("update", k, "--claim"); code != 0 || !strings.Contains(errOut, "ZM_LANE") || !strings.Contains(errOut, "rename it") {
		t.Fatalf("bad lane: %d %q", code, errOut)
	}
	if got := assignee(k); got != "mini/aaaa0001" {
		t.Fatalf("bad lane claim: %q", got)
	}
	t.Setenv("ZM_LANE", "")
	s.c.As = ""
	// Outside a session: git's user.name, then $USER.
	d := strings.TrimSpace(s.ok("create", "d", "--silent"))
	s.ok("update", d, "--claim")
	if got := assignee(d); got != "Ivo Doe" {
		t.Fatalf("git user.name: %q", got)
	}
	gitUserName = func() string { return "" }
	t.Setenv("USER", "ivogh")
	e := strings.TrimSpace(s.ok("create", "e", "--silent"))
	s.ok("update", e, "--claim")
	if got := assignee(e); got != "ivogh" {
		t.Fatalf("$USER: %q", got)
	}
	// Nothing at all: the zm identity's name, as before.
	t.Setenv("USER", "")
	f := strings.TrimSpace(s.ok("create", "f", "--silent"))
	s.ok("update", f, "--claim")
	if got := assignee(f); got != "ana" {
		t.Fatalf("no actor: %q", got)
	}
}

// bd dep list reads what bd show --include-dependents reads, in bd's
// IssueWithDependencyMetadata shape; the dep error and help point at both.
func TestShimDepList(t *testing.T) {
	s := newShim(t)
	a := strings.TrimSpace(s.ok("create", "A", "--silent"))
	b := strings.TrimSpace(s.ok("create", "B", "--silent"))
	c := strings.TrimSpace(s.ok("create", "C", "--silent"))
	s.ok("dep", "add", a, b)
	s.ok("dep", "add", c, a, "-t", "related")

	var down []map[string]any
	if err := json.Unmarshal([]byte(s.ok("dep", "list", a, "--json")), &down); err != nil || len(down) != 1 ||
		down[0]["id"] != b || down[0]["dependency_type"] != "blocks" || down[0]["created_at"] == nil {
		t.Fatalf("down %v %v", down, err)
	}
	var up []map[string]any
	if err := json.Unmarshal([]byte(s.ok("dep", "list", a, "--direction", "up", "--json")), &up); err != nil || len(up) != 1 ||
		up[0]["id"] != c || up[0]["dependency_type"] != "related" {
		t.Fatalf("up %v %v", up, err)
	}
	var both []map[string]any
	if err := json.Unmarshal([]byte(s.ok("dep", "list", a, "--direction=both", "--json")), &both); err != nil || len(both) != 2 {
		t.Fatalf("both %v %v", both, err)
	}
	if out := s.ok("dep", "list", c, "--direction", "up", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty %q", out)
	}
	if out := s.ok("dep", "list", a, "--direction", "both", "-t", "related", "--json"); !strings.Contains(out, c) || strings.Contains(out, `"id": "`+b+`"`) {
		t.Fatalf("type filter %s", out)
	}
	// bd v1.0.5's per-ID text: no heading, one line per issue, a blank line after.
	if out := s.ok("dep", "list", a); out != "  "+b+": B [P2] (open) via blocks\n\n" {
		t.Fatalf("text %q", out)
	}
	if out := s.ok("dep", "list", b); out != "\n"+b+" has no dependencies\n" {
		t.Fatalf("empty down text %q", out)
	}
	if out := s.ok("dep", "list", c, "--direction", "up"); out != "\nNo issues depend on "+c+"\n" {
		t.Fatalf("empty up text %q", out)
	}
	// Batch: one flat array; an id that does not resolve is skipped with a warning.
	var batch []map[string]any
	out, errOut, code := s.bd("dep", "list", a, "oc-zzz", c, "--json")
	if err := json.Unmarshal([]byte(out), &batch); code != 0 || err != nil || len(batch) != 2 || !strings.Contains(errOut, "oc-zzz") {
		t.Fatalf("batch %d %q %q %v", code, out, errOut, err)
	}
	if _, errOut, code := s.bd("dep", "list", a, "--direction", "sideways"); code == 0 || !strings.Contains(errOut, "--direction") {
		t.Fatalf("bad direction %d %s", code, errOut)
	}
	if _, errOut, code := s.bd("dep", "list", "oc-zzz"); code == 0 || errOut == "" {
		t.Fatalf("missing %d %s", code, errOut)
	}
	for _, args := range [][]string{{"dep", "tree", a}, {"dep", "--help"}} {
		out, errOut, _ := s.bd(args...)
		if all := out + errOut; !strings.Contains(all, "dep list") || !strings.Contains(all, "show <id> --include-dependents") {
			t.Fatalf("bd %v does not point at dep list and show: %s", args, all)
		}
	}
}
