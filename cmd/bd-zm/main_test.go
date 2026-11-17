package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

type shim struct {
	t *testing.T
	c *client.Client
}

func newShim(t *testing.T) *shim {
	t.Helper()
	for _, k := range []string{"BEADS_DIR", "BD_ACTOR", "BEADS_ACTOR", "FARM_BD_ACTOR", "USER", "CLAUDE_CODE", "BD_AGENT_MODE", "CLAUDE_SESSION_ID", "ZM_LANE"} {
		t.Setenv(k, "")
	}
	git := gitUserName
	gitUserName = func() string { return "" }
	t.Cleanup(func() { gitUserName = git })
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	c := client.WithHandler(dm.SocketHandler())
	if _, err := c.CreateProject(context.Background(), api.NewProjectRequest{Slug: "oc"}); err != nil {
		t.Fatal(err)
	}
	c.Project, c.ProjectFrom = "oc", "flag"
	newClient = func(context.Context) (*client.Client, error) { return c, nil }
	t.Cleanup(func() { newClient = defaultClient })
	return &shim{t: t, c: c}
}

// bd runs the shim and returns stdout, stderr and the exit code.
func (s *shim) bd(args ...string) (string, string, int) {
	s.t.Helper()
	var o, e bytes.Buffer
	stdout, stderr = &o, &e
	defer func() { stdout, stderr = os.Stdout, os.Stderr }()
	code := run(context.Background(), args)
	return o.String(), e.String(), code
}

func (s *shim) ok(args ...string) string {
	s.t.Helper()
	out, errOut, code := s.bd(args...)
	if code != 0 {
		s.t.Fatalf("bd %v: exit %d: %s %s", args, code, out, errOut)
	}
	return out
}

// keys lists an object's keys in output order.
func keys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	var out []string
	depth := 0
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				depth++
				expectKey = depth == 1
			case '}':
				depth--
				expectKey = depth == 1
			case '[', ']':
			}
		case string:
			if depth == 1 && expectKey {
				out = append(out, v)
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					t.Fatal(err)
				}
				continue
			}
		}
	}
	return out
}

func firstElem(t *testing.T, raw string) string {
	t.Helper()
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil || len(arr) == 0 {
		t.Fatalf("want a non-empty JSON array, got %q (%v)", raw, err)
	}
	return string(arr[0])
}

func TestShimJSONShapes(t *testing.T) {
	s := newShim(t)
	// create: an object, keys sorted, schema_version, fractional times.
	out := s.ok("create", "Parent epic", "-t", "epic", "-p", "P1", "-d", "the **epic**", "-l", "area:solver,track-b", "--json")
	want := []string{"created_at", "created_by", "description", "id", "issue_type", "labels", "number", "priority", "schema_version", "status", "title", "updated_at"}
	if got := keys(t, out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("create keys %v\n%s", got, out)
	}
	var created map[string]any
	_ = json.Unmarshal([]byte(out), &created)
	epic := created["id"].(string)
	if !regexp.MustCompile(`^oc-[0-9a-z]{3}$`).MatchString(epic) || created["created_by"] != "ana" || created["priority"].(float64) != 1 {
		t.Fatalf("create %v", created)
	}
	child := strings.TrimSpace(s.ok("create", "Child", "--parent", epic, "--silent"))
	if child != epic+".1" {
		t.Fatalf("child id %q", child)
	}
	blocker := strings.TrimSpace(s.ok("create", "--title", "Blocker", "--silent"))
	s.ok("dep", "add", child, blocker)

	// update --claim: an array of issues.
	out = s.ok("update", child, "--claim", "--json")
	el := firstElem(t, out)
	want = []string{"id", "title", "status", "priority", "issue_type", "assignee", "created_at", "created_by", "updated_at", "started_at", "labels", "number", "claimed_by"}
	if got := keys(t, el); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("update keys %v\n%s", got, el)
	}
	if !strings.Contains(el, `"status": "in_progress"`) || !strings.Contains(el, `"assignee": "ana"`) {
		t.Fatalf("claim %s", el)
	}
	// show: an array of details; edges both ways, counts after parent.
	out = s.ok("show", child, "--json")
	el = firstElem(t, out)
	want = []string{"id", "title", "status", "priority", "issue_type", "assignee", "created_at", "created_by", "updated_at", "started_at",
		"number", "claimed_by", "labels", "dependencies", "parent", "dependent_count", "dependency_count", "comment_count"}
	if got := keys(t, el); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("show keys %v\n%s", got, el)
	}
	if !strings.Contains(el, `"dependency_type": "parent-child"`) || !strings.Contains(el, `"dependency_count": 2`) {
		t.Fatalf("show deps %s", el)
	}
	// list: IssueWithCounts with raw dependency records and parent.
	out = s.ok("list", "--json", "--parent", epic)
	el = firstElem(t, out)
	want = []string{"id", "title", "status", "priority", "issue_type", "assignee", "created_at", "created_by", "updated_at", "started_at",
		"labels", "dependencies", "number", "claimed_by", "dependency_count", "dependent_count", "comment_count", "parent"}
	if got := keys(t, el); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list keys %v\n%s", got, el)
	}
	if !strings.Contains(el, `"metadata": "{}"`) || !strings.Contains(el, `"depends_on_id": "`+blocker+`"`) {
		t.Fatalf("list deps %s", el)
	}
	// ready: the blocked child is not ready; the blocker is.
	out = s.ok("ready", "--json")
	if strings.Contains(out, `"id": "`+child+`"`) || !strings.Contains(out, `"id": "`+blocker+`"`) {
		t.Fatalf("ready %s", out)
	}
	// comments add: an object with schema_version; comments: an array.
	out = s.ok("comments", "add", child, "first note", "--json")
	want = []string{"author", "created_at", "id", "issue_id", "schema_version", "text"}
	if got := keys(t, out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("comment keys %v\n%s", got, out)
	}
	out = s.ok("comments", child, "--json")
	if got := keys(t, firstElem(t, out)); strings.Join(got, ",") != "id,issue_id,author,text,created_at" {
		t.Fatalf("comments keys %v", got)
	}
	// label add: an array of {issue_id, label, status}.
	out = s.ok("label", "add", child, "urgent", "--json")
	if got := keys(t, firstElem(t, out)); strings.Join(got, ",") != "issue_id,label,status" {
		t.Fatalf("label keys %v", got)
	}
	// close refuses while blocked, then closes with --force; reopen.
	if _, errOut, code := s.bd("close", child, "-r", "done"); code != 1 || !strings.Contains(errOut, "blocked by open issues") {
		t.Fatalf("blocked close: %d %s", code, errOut)
	}
	out = s.ok("close", child, "-r", "done", "--force", "--json")
	if el := firstElem(t, out); !strings.Contains(el, `"close_reason": "done"`) || !strings.Contains(el, `"closed_at"`) {
		t.Fatalf("close %s", el)
	}
	out = s.ok("reopen", child, "-r", "regressed")
	if strings.TrimSpace(out) != "↻ Reopened "+child+": regressed" {
		t.Fatalf("reopen %q", out)
	}
	if _, errOut, _ := s.bd("reopen", child); !strings.Contains(errOut, "is already open") {
		t.Fatalf("reopen open: %s", errOut)
	}
	// stats: {schema_version, summary{...}}.
	out = s.ok("stats", "--json")
	if got := keys(t, out); strings.Join(got, ",") != "schema_version,summary" || !strings.Contains(out, `"total_issues": 3`) {
		t.Fatalf("stats %s", out)
	}
	// dep add JSON and search JSON (no parent, no deps).
	out = s.ok("dep", "add", blocker, epic, "-t", "related", "--json")
	if got := keys(t, out); strings.Join(got, ",") != "depends_on_id,issue_id,schema_version,status,type" {
		t.Fatalf("dep keys %v", got)
	}
	out = s.ok("search", "chil", "--json")
	el = firstElem(t, out)
	if strings.Contains(el, `"parent"`) || strings.Contains(el, `"dependencies"`) || !strings.Contains(el, `"id": "`+child+`"`) {
		t.Fatalf("search %s", el)
	}
	// Bare hashes resolve with the project's prefix.
	if out := s.ok("show", strings.TrimPrefix(blocker, "oc-"), "--json"); !strings.Contains(out, blocker) {
		t.Fatalf("partial id %s", out)
	}
}

func TestShimHumanOutput(t *testing.T) {
	s := newShim(t)
	out := s.ok("create", "Fix the thing", "-p", "1", "-a", "ana")
	if !regexp.MustCompile(`^✓ Created issue: oc-[0-9a-z]{3} — Fix the thing\n  Priority: P1\n  Status: open\n$`).MatchString(out) {
		t.Fatalf("create %q", out)
	}
	id := strings.Fields(out)[3]
	if out := s.ok("update", id, "--status", "in_progress"); out != "✓ Updated issue: "+id+" — Fix the thing\n" {
		t.Fatalf("update %q", out)
	}
	if out, _, _ := s.bd("update", id); out != "No updates specified\n" {
		t.Fatalf("no updates %q", out)
	}
	out = s.ok("list")
	if !strings.Contains(out, "◐ "+id+" ● P1 Fix the thing") || !strings.Contains(out, "Total: 1 issues (0 open, 1 in progress)") {
		t.Fatalf("list %q", out)
	}
	if out := s.ok("comments", "add", id, "hello"); out != "Comment added to "+id+"\n" {
		t.Fatalf("comment %q", out)
	}
	out = s.ok("show", id)
	if !strings.HasPrefix(out, "◐ "+id+" · Fix the thing   [● P1 · IN_PROGRESS]\n") || !strings.Contains(out, "COMMENTS") {
		t.Fatalf("show %q", out)
	}
	if _, errOut, code := s.bd("show", "oc-zzz"); code != 1 || !strings.Contains(errOut, "Issue oc-zzz not found") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	if out := s.ok("close", id, "-r", "shipped"); out != "✓ Closed "+id+" — Fix the thing: shipped\n" {
		t.Fatalf("close %q", out)
	}
	// Many issues: the list footer on stderr.
	for i := 0; i < 3; i++ {
		s.ok("create", "more", "--silent")
	}
	_, errOut, _ := s.bd("list", "-n", "2")
	if !strings.Contains(errOut, "Showing 2 issues; more results matched") {
		t.Fatalf("footer %q", errOut)
	}
}

func TestShimScopeAndRefusals(t *testing.T) {
	s := newShim(t)
	ctx := context.Background()
	if _, err := s.c.CreateProject(ctx, api.NewProjectRequest{Slug: "other"}); err != nil {
		t.Fatal(err)
	}
	other := *s.c
	other.Project = "other"
	s.c.ProjectFrom = "env" // like $ZM_PROJECT or the repo: resolves writes, does not narrow reads
	theirs, err := orchclient.CreateIssue(ctx, &other, orchapi.NewIssueRequest{Title: "other project's"})
	if err != nil {
		t.Fatal(err)
	}
	mine := strings.TrimSpace(s.ok("create", "mine", "--silent"))
	// Lists, ready and stats cover the project this directory resolves to.
	for _, args := range [][]string{{"list", "--json"}, {"ready", "--json"}, {"search", "s", "--json"}} {
		out := s.ok(args...)
		if strings.Contains(out, theirs.ID) {
			t.Fatalf("bd %v leaked the other project: %s", args, out)
		}
	}
	if out := s.ok("list", "--json"); !strings.Contains(out, mine) {
		t.Fatalf("list lost mine: %s", out)
	}
	if out := s.ok("stats", "--json"); !strings.Contains(out, `"total_issues": 1`) {
		t.Fatalf("stats: %s", out)
	}
	// ... but an id is found wherever the caller may read it.
	if out := s.ok("show", theirs.ID, "--json"); !strings.Contains(out, theirs.ID) {
		t.Fatalf("show other: %s", out)
	}
	// Another database, --global, --readonly writes and unknown subcommands are refused, never ignored.
	for _, args := range [][]string{{"--db", "/tmp/x.db", "create", "x"}, {"--global", "list"}, {"--readonly", "close", mine},
		{"dep", "tree", mine}, {"label", "list", mine}} {
		if _, _, code := s.bd(args...); code == 0 {
			t.Fatalf("bd %v succeeded", args)
		}
	}
	if out := s.ok("--readonly", "list"); !strings.Contains(out, mine) {
		t.Fatalf("readonly list: %s", out)
	}
}

// bd where says who holds a joined project's issues, from what zm join
// recorded; the address itself is the daemon's business.
func TestShimWhereNamesAuthority(t *testing.T) {
	const project = "01M67TDT7RA24PC1JXTSW01VEC"
	remotes := []api.Remote{{Project: api.ProjectRef{ID: project, Slug: "harbor"}, Authority: "box-1", URL: "http://zm-box-1:7433"}}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/issues/projects":
			api.WriteJSON(w, http.StatusOK, orchapi.IssueProjects{Projects: []orchapi.IssueProjectInfo{{Project: project, Slug: "harbor"}}})
		case "/v0/team/remotes":
			api.WriteJSON(w, http.StatusOK, api.RemoteList{Remotes: remotes})
		default:
			api.WriteJSON(w, http.StatusOK, api.Resolution{})
		}
	})
	oldClient, oldOut, oldErr := newClient, stdout, stderr
	t.Cleanup(func() { newClient, stdout, stderr = oldClient, oldOut, oldErr })
	newClient = func(context.Context) (*client.Client, error) { return client.WithHandler(h), nil }
	where := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		stdout = &out
		if code := run(context.Background(), append([]string{"where"}, args...)); code != 0 {
			t.Fatalf("bd where: exit %d", code)
		}
		return out.String()
	}
	if got := where(); !strings.Contains(got, "zm-native: harbor,") || !strings.HasSuffix(strings.TrimSpace(got), "authority box-1") || strings.Contains(got, "7433") {
		t.Fatalf("where %q", got)
	}
	if got := where("--json"); !strings.Contains(got, `"authority": "box-1"`) {
		t.Fatalf("where --json %q", got)
	}
	remotes = nil // this machine's own project
	if got := where(); strings.Contains(got, "authority") {
		t.Fatalf("where on a solo project %q", got)
	}
}

// With the only project's authority away (a replica's answer), bd fails and
// says so: "0 issues" or an empty list would read as "no work".
func TestShimAuthorityAway(t *testing.T) {
	const project = "01M67TDT7RA24PC1JXTSW01VEC"
	// This machine's own project has native issues too: it must not answer
	// for the one that is away, in the away project's checkout or outside one.
	local := []orchapi.IssueProjectInfo{}
	resolved := &api.ProjectRef{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/issues/projects":
			api.WriteJSON(w, http.StatusOK, orchapi.IssueProjects{Projects: local,
				Skipped: []api.WorkSkipped{{Project: project, Source: "authority", Error: "harbor: unreachable"}}})
			return
		case "/v0/projects/resolve":
			res := api.Resolution{}
			if resolved.ID != "" {
				res.Project = resolved
			}
			api.WriteJSON(w, http.StatusOK, res)
			return
		}
		if got := r.URL.Query().Get("project") + api.GetHeader(r.Header, api.HeaderProject); !strings.Contains(got, project) {
			t.Errorf("%s %s does not name the skipped project: %q", r.Method, r.URL, got)
		}
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "authority unreachable"))
	})
	oldClient, oldOut, oldErr := newClient, stdout, stderr
	t.Cleanup(func() { newClient, stdout, stderr = oldClient, oldOut, oldErr })
	newClient = func(context.Context) (*client.Client, error) { return client.WithHandler(h), nil }
	check := func(name string) {
		t.Helper()
		for _, args := range [][]string{{"count"}, {"list"}, {"ready"}, {"stats"}} {
			var out, errb bytes.Buffer
			stdout, stderr = &out, &errb
			if code := run(context.Background(), args); code != 3 {
				t.Errorf("%s: bd %v: exit %d, stdout %q, stderr %q", name, args, code, out.String(), errb.String())
			}
			if out.Len() != 0 || !strings.Contains(errb.String(), "unreachable") {
				t.Errorf("%s: bd %v: stdout %q, stderr %q", name, args, out.String(), errb.String())
			}
		}
	}
	check("no other project")
	local = []orchapi.IssueProjectInfo{{Project: "01M67TDT7RA24PC1JXTSW01LOC", Slug: "personal"}}
	check("a local project, outside any checkout")
	resolved = &api.ProjectRef{ID: project, Slug: "harbor"}
	check("a local project, in the away project's checkout")
}

// A new P0/P1 needs a due date, an owner or a blocks link; else it is filed at
// P2 with a hint. Epics keep theirs.
func TestCreateUrgencyNeedsAnAnchor(t *testing.T) {
	s := newShim(t)
	prio := func(args ...string) (string, string) {
		t.Helper()
		out, errOut, code := s.bd(append([]string{"create"}, append(args, "--json")...)...)
		if code != 0 {
			t.Fatalf("create %v: %d %s", args, code, errOut)
		}
		var it struct{ Priority int }
		if err := json.Unmarshal([]byte(out), &it); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("P%d", it.Priority), errOut
	}
	other := strings.TrimSpace(s.ok("create", "other", "--silent"))
	for _, tc := range []struct {
		args []string
		want string
		hint bool
	}{
		{[]string{"bare", "-p", "1"}, "P2", true},
		{[]string{"bare zero", "-p", "P0"}, "P2", true},
		{[]string{"due", "-p", "1", "--due", "2030-01-02"}, "P1", false},
		{[]string{"owned", "-p", "0", "-a", "ana"}, "P0", false},
		{[]string{"blocks", "-p", "1", "--deps", "blocks:" + other}, "P1", false},
		{[]string{"depends", "-p", "1", "--deps", other}, "P2", true},
		{[]string{"theme", "-t", "epic", "-p", "1"}, "P1", false},
		{[]string{"normal", "-p", "2"}, "P2", false},
	} {
		got, errOut := prio(tc.args...)
		if got != tc.want || strings.Contains(errOut, "filed at P2") != tc.hint {
			t.Errorf("%v: %s, stderr %q", tc.args, got, errOut)
		}
	}
}
