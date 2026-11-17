package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
)

// The client contract (docs/clients.md): the --json output of each listed
// command keeps its shape. A golden file holds the shape (key names and
// JSON types, not values) of one command's output against a seeded fresh
// machine. Adding keys passes; a missing key or a changed type fails, and
// is a break that needs a "Clients" line in CHANGELOG.md.
//
// Refresh after an intended change: go test -p 2 ./cmd/zm -run TestClientJSONShapes -update-clients
var updateClients = flag.Bool("update-clients", false, "rewrite cmd/zm/testdata/clients/*.json from this run")

// clientCase is one listed command: its golden name, the argv it runs
// (placeholders <session>, <item>, <mail>, <ask>, <answered...> and
// <receipt> filled from the seed) and the row docs/clients.md lists it under.
type clientCase struct {
	name string
	doc  string
	argv []string
}

// clientCases is the listed table, in run order: writes that seed first,
// then reads, then the writes that change what reads see.
var clientCases = []clientCase{
	{"work_new", "zm work new", []string{"work", "new", "--", "golden item"}},
	{"send", "zm send", []string{"send", "<session>", "--ref", "<item>", "please look at the golden item"}},
	{"claim", "zm claim", []string{"--as", "<session>", "claim", "<item>"}},
	{"ask_new", "zm ask new", []string{"--as", "<session>", "ask", "new", "--work", "<item>",
		"--title", "Pick a colour", "--context", "The golden page needs one colour for its header.",
		"--choice", "Blue | calm | common", "--choice", "Green | fresh | less common",
		"--recommend", "a", "--why", "calm reads well",
		"--searched", "I read the page's earlier comments and found no colour chosen before",
		"--", "Which colour should the header use?"}},
	{"token_mint", "zm token mint", []string{"token", "mint", "--as", "golden-bot", "--role", "member", "-P", "demo", "--note", "the golden dashboard", "--ttl", "720h"}},
	{"whoami", "zm whoami", []string{"whoami"}},
	{"status", "zm status", []string{"status"}},
	{"project_ls", "zm project ls", []string{"project", "ls"}},
	{"ls", "zm ls", []string{"ls"}},
	{"ls_all", "zm ls --all", []string{"ls", "--all"}},
	{"session_show", "zm session show", []string{"session", "show", "<session>"}},
	{"inbox", "zm inbox", []string{"--as", "<session>", "inbox"}},
	{"mail", "zm mail", []string{"mail"}},
	{"sent", "zm sent", []string{"sent"}},
	{"thread", "zm thread", []string{"thread", "<mail>"}},
	{"people", "zm people", []string{"people"}},
	{"inflight", "zm inflight", []string{"inflight"}},
	{"leases", "zm leases", []string{"leases"}},
	{"work_show", "zm work show", []string{"work", "show", "<item>"}},
	{"who", "zm who", []string{"who", "<item>"}},
	{"ask_ls", "zm ask ls", []string{"ask", "ls"}},
	{"ask_show", "zm ask show", []string{"ask", "show", "<ask>"}},
	{"ask_inbox", "zm ask inbox", []string{"ask", "inbox"}},
	{"token_ls", "zm token ls", []string{"token", "ls"}},
	{"remotes", "zm remotes", []string{"remotes"}},
	{"ack", "zm ack", []string{"--as", "<session>", "ack", "<mail>"}},
	{"ask_withdraw", "zm ask withdraw", []string{"--as", "<session>", "ask", "withdraw", "<ask>"}},
	// The owner answers the second ask (POST /v0/asks/{id}/choose), then a
	// client reads the answered ask and checks its receipt with the
	// expectations it already holds: the ask, its work item, the card it
	// showed and the choice it sent.
	{"ask_answer", "zm ask answer", []string{"ask", "answer", "<answered>", "a"}},
	{"ask_show_answered", "zm ask show", []string{"ask", "show", "<answered>"}},
	{"receipt_key", "zm receipt key", []string{"receipt", "key"}},
	{"receipt_verify", "zm receipt verify", []string{"receipt", "verify", "<receipt>", "--for-project", "demo",
		"--ask", "<answered>", "--work", "<answered-work>", "--card", "<answered-card>", "--decision", "a"}},
}

func TestClientJSONShapes(t *testing.T) {
	z := newZeroEnv(t)
	// No model is ever called: a test binary's card judge is a fake that
	// passes every card (judge.Default), so both asks are open and the owner
	// can answer one; no real provider CLI runs from a test.
	z.env = append(z.env, "ZM_JUDGE=none")
	repo := gitRepo(t, filepath.Join(z.home, "demo"), "https://example.com/acme/demo.git")
	zm := func(args ...string) string {
		t.Helper()
		out, errOut, code := z.run(repo, nil, args...)
		if code != 0 {
			t.Fatalf("zm %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}
	zm("init", "--slug", "demo")
	var reg struct{ Address string }
	decodeInto(t, zm("--json", "session", "register", "--agent", "claude", "--runtime-id", "rt-golden-1", "--title", "golden", "--cwd", repo), &reg)
	if reg.Address == "" {
		t.Fatal("session register answered no address")
	}
	seed := map[string]string{"<session>": reg.Address}
	// States clients render, so their optional fields are in the goldens: a
	// session that needs its human, and a revoked token beside the minted one
	// (revoked; the minted one has note and expires).
	zm("--json", "session", "state", reg.Address, "needs_human", "--note", "waiting on a colour")
	var gone struct{ Token struct{ ID string } }
	decodeInto(t, zm("--json", "token", "mint", "--as", "gone-bot", "--role", "guest", "-P", "demo"), &gone)
	zm("--json", "token", "revoke", gone.Token.ID)
	// A second card ask, on its own item, for the owner to answer.
	var item2 struct{ ID string }
	decodeInto(t, zm("--json", "work", "new", "--", "golden answered item"), &item2)
	var asked struct {
		ID      string
		CardSHA string `json:"card_sha256"`
		Subject struct{ Work string }
	}
	decodeInto(t, zm("--json", "--as", reg.Address, "ask", "new", "--work", item2.ID,
		"--title", "Pick a footer colour", "--context", "The golden page needs one colour for its footer.",
		"--choice", "Grey | quiet | common", "--choice", "Black | strong | heavy",
		"--recommend", "a", "--why", "grey stays out of the way",
		"--searched", "I read the page's earlier comments and found no footer colour chosen before",
		"--", "Which colour should the footer use?"), &asked)
	if asked.ID == "" || asked.CardSHA == "" || asked.Subject.Work == "" {
		t.Fatalf("second ask: id %q, card_sha256 %q, work %q", asked.ID, asked.CardSHA, asked.Subject.Work)
	}
	seed["<answered>"], seed["<answered-card>"], seed["<answered-work>"] = asked.ID, asked.CardSHA, asked.Subject.Work
	waitSelfCheck(t, zm)

	for _, c := range clientCases {
		argv := []string{"--json"}
		for _, a := range c.argv {
			if v, ok := seed[a]; ok {
				a = v
			} else if strings.HasPrefix(a, "<") && strings.HasSuffix(a, ">") {
				t.Fatalf("%s: %s is not seeded yet", c.name, a)
			}
			argv = append(argv, a)
		}
		out := zm(argv...)
		var v any
		decodeInto(t, out, &v)
		switch c.name { // ids later cases need
		case "work_new":
			seed["<item>"] = stringField(t, c.name, v, "id")
		case "send":
			seed["<mail>"] = stringField(t, c.name, v, "id")
		case "ask_new":
			seed["<ask>"] = stringField(t, c.name, v, "id")
		case "ask_answer": // the decision as a client got it: verify reads its receipt
			f := filepath.Join(t.TempDir(), "decision.json")
			if err := os.WriteFile(f, []byte(out), 0o600); err != nil {
				t.Fatal(err)
			}
			seed["<receipt>"] = f
		case "receipt_verify": // valid, and every expectation sent was compared
			m, _ := v.(map[string]any)
			checked, _ := m["checked"].([]any)
			got := map[any]bool{}
			for _, c := range checked {
				got[c] = true
			}
			if m["valid"] != true || len(got) != 5 || !got["card"] || !got["project"] || !got["ask"] || !got["work"] || !got["decision"] {
				t.Fatalf("the answered ask's receipt does not verify against all it was given: %s", out)
			}
		}
		checkShape(t, c, shapeOf(v))
	}
	checkClientsCommandTable(t)
}

// waitSelfCheck waits for the daemon's first self-check (zm status carries
// selfcheck only once it ends), so the status golden never races it.
func waitSelfCheck(t *testing.T, zm func(...string) string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var st struct{ Selfcheck *json.RawMessage }
		decodeInto(t, zm("--json", "status"), &st)
		if st.Selfcheck != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("zm status carried no selfcheck after 15 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func decodeInto(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
}

func stringField(t *testing.T, name string, v any, key string) string {
	t.Helper()
	m, _ := v.(map[string]any)
	s, _ := m[key].(string)
	if s == "" {
		t.Fatalf("%s: no %q in its answer", name, key)
	}
	return s
}

// shapeOf replaces every value with its JSON type ("string", "number",
// "bool", "null"); an array becomes [the merged shape of its elements], or
// "[]" when empty.
func shapeOf(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = shapeOf(e)
		}
		return m
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		s := shapeOf(x[0])
		for _, e := range x[1:] {
			s = mergeShape(s, shapeOf(e))
		}
		return []any{s}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	default:
		return "null"
	}
}

// mergeShape joins two element shapes: objects take every key, arrays merge
// elements, and different types become "a|b".
func mergeShape(a, b any) any {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		m := make(map[string]any, len(am))
		for k, v := range am {
			m[k] = v
		}
		for k, v := range bm {
			if old, ok := m[k]; ok {
				m[k] = mergeShape(old, v)
			} else {
				m[k] = v
			}
		}
		return m
	}
	aa, aok := a.([]any)
	ba, bok := b.([]any)
	if aok && bok {
		return []any{mergeShape(aa[0], ba[0])}
	}
	as, bs := shapeName(a), shapeName(b)
	if as == bs {
		return a
	}
	set := map[string]bool{}
	for _, s := range append(strings.Split(as, "|"), strings.Split(bs, "|")...) {
		set[s] = true
	}
	names := make([]string, 0, len(set))
	for s := range set {
		names = append(names, s)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

func shapeName(s any) string {
	switch x := s.(type) {
	case string:
		return x
	case map[string]any:
		return "object"
	default:
		return "array"
	}
}

func goldenPath(name string) string { return filepath.Join("testdata", "clients", name+".json") }

func checkShape(t *testing.T, c clientCase, got any) {
	t.Helper()
	path := goldenPath(c.name)
	if *updateClients {
		if u := findUnion(got, "$"); u != "" {
			t.Errorf("%s --json: %s mixes types, which a shape cannot check inside: seed it so each value has one type, or check it by hand", c.doc, u)
			return
		}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: no golden shape (%v): run go test -p 2 ./cmd/zm -run TestClientJSONShapes -update-clients", c.doc, err)
	}
	var want any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var breaks, added []string
	diffShape(want, got, "$", &breaks, &added)
	for _, b := range breaks {
		t.Errorf("%s --json: %s. A client reading it breaks: if this is intended, add a \"Clients\" line to CHANGELOG.md and refresh %s (-update-clients)", c.doc, b, path)
	}
	if len(added) > 0 {
		t.Logf("%s --json adds %s (allowed; refresh %s with -update-clients to record it)", c.doc, strings.Join(added, ", "), path)
	}
}

// diffShape reports keys the golden shape has that got lacks, or whose type
// changed (breaks), and keys only got has (added).
func diffShape(want, got any, at string, breaks, added *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*breaks = append(*breaks, fmt.Sprintf("%s is %s, was an object", at, shapeName(got)))
			return
		}
		for _, k := range sortedKeys(w) {
			gv, ok := g[k]
			if !ok {
				*breaks = append(*breaks, fmt.Sprintf("%s.%s is gone", at, k))
				continue
			}
			diffShape(w[k], gv, at+"."+k, breaks, added)
		}
		for _, k := range sortedKeys(g) {
			if _, ok := w[k]; !ok {
				*added = append(*added, at+"."+k)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			*breaks = append(*breaks, fmt.Sprintf("%s is %s, was an array", at, shapeName(got)))
			return
		}
		if len(w) == 0 {
			return
		}
		diffShape(w[0], g[0], at+"[]", breaks, added)
	default:
		if want == "[]" { // recorded empty: an array, its elements unchecked
			if _, ok := got.([]any); ok || got == "[]" {
				return
			}
		}
		if shapeName(got) != shapeName(want) {
			*breaks = append(*breaks, fmt.Sprintf("%s is %s, was %s", at, shapeName(got), shapeName(want)))
		}
	}
}

// findUnion names the first place a shape holds two types ("a|b"), else "".
func findUnion(s any, at string) string {
	switch x := s.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if u := findUnion(x[k], at+"."+k); u != "" {
				return u
			}
		}
	case []any:
		return findUnion(x[0], at+"[]")
	case string:
		if strings.Contains(x, "|") {
			return at
		}
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// clientsCommandRow matches a row of docs/clients.md's "CLI --json commands" table.
var clientsCommandRow = regexp.MustCompile("^\\| `(zm [^`]+)` \\|")

// checkClientsCommandTable keeps docs/clients.md's command table equal to
// clientCases, so the listed table and the tested table are one list.
func checkClientsCommandTable(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("../../docs/clients.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(b), "\n## CLI --json commands\n")
	if !ok {
		t.Fatal(`docs/clients.md has no "## CLI --json commands" section`)
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	inDoc := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if m := clientsCommandRow.FindStringSubmatch(line); m != nil {
			inDoc[m[1]] = true
		}
	}
	tested := map[string]bool{}
	for _, c := range clientCases {
		tested[c.doc] = true
		if !inDoc[c.doc] {
			t.Errorf("docs/clients.md \"CLI --json commands\" does not list `%s`, which TestClientJSONShapes checks", c.doc)
		}
	}
	for d := range inDoc {
		if !tested[d] {
			t.Errorf("docs/clients.md \"CLI --json commands\" lists `%s`, which TestClientJSONShapes does not check", d)
		}
	}
	// Each golden file belongs to a listed command.
	files, _ := filepath.Glob(filepath.Join("testdata", "clients", "*.json"))
	names := map[string]bool{}
	for _, c := range clientCases {
		names[c.name] = true
	}
	for _, f := range files {
		if n := strings.TrimSuffix(filepath.Base(f), ".json"); !names[n] {
			t.Errorf("%s belongs to no listed command: remove it", f)
		}
	}
}

// answerNet is a tailnet with one phone, as the daemon's node sees it.
type answerNet struct{}

func (answerNet) Dial(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
func (answerNet) Peer(context.Context, string) (core.Remote, error) {
	return core.Remote{}, net.ErrClosed
}
func (answerNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Running: true, NodeID: "nSELF", DNSName: "zm-mini.example.ts.net", PeerList: []core.NetPeer{
		{Host: "ivo-phone", DNSName: "ivo-phone.example.ts.net", NodeID: "nPHONE", NodeKey: "nodekey:phone", Login: "ivo@example.com", OS: "iOS"}}}, nil
}
func (answerNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

// An answer-only token's decision keeps the shape clients read from zm ask
// answer (golden ask_answer): the same keys as the socket's answer, only
// surface, machine and the receipt's node say it came from the token. The
// golden walk above cannot pin a device (no tailnet in a test binary), so
// this one runs the daemon in process.
func TestAnswerTokenDecisionShape(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Full(d)
		d.Net = answerNet{}
		return mods, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	ctx := context.Background()
	sock := client.WithHandler(dm.SocketHandler())
	if _, err := sock.CreateProject(ctx, api.NewProjectRequest{Slug: "demo"}); err != nil {
		t.Fatal(err)
	}
	sock.Project, sock.ProjectFrom = "demo", "flag"
	ask := func(title string) orchapi.Ask {
		t.Helper()
		it, err := orchclient.CreateWork(ctx, sock, orchapi.CreateWorkRequest{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		a, err := orchclient.NewAsk(ctx, sock, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: &orchapi.AskCard{
			Context: "The golden page needs one colour.", Question: "Which colour?",
			Choices:   []orchapi.AskChoice{{Label: "Grey", Pros: "quiet", Cons: "common"}, {Label: "Black", Pros: "strong", Cons: "heavy"}},
			Recommend: "a", Why: "grey stays out of the way", Searched: "the page's earlier comments: no colour chosen before"}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	tok, err := sock.MintToken(ctx, api.MintTokenRequest{Limit: orchapi.AnswerLimit, Project: "demo", Node: "ivo-phone", TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	phone := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dm.Handler.ServeHTTP(w, r.WithContext(core.WithRemote(r.Context(), core.Remote{Login: "ivo@example.com", Node: "ivo-phone.example.ts.net", NodeID: "nPHONE", NodeKey: "nodekey:phone"})))
	}))
	phone.Token = tok.Secret
	viaToken, err := orchclient.Choose(ctx, phone, "demo", ask("by token").ID, orchapi.DecideRequest{Decision: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if viaToken.Surface != orchapi.SurfaceAnswerToken || viaToken.Method != orchapi.MethodOwner || viaToken.Receipt.Node != "owner:answer-token:"+tok.Token.ID {
		t.Fatalf("the token's decision: %+v", viaToken)
	}
	bySocket, err := orchclient.Choose(ctx, sock, "demo", ask("by socket").ID, orchapi.DecideRequest{Decision: "a"})
	if err != nil {
		t.Fatal(err)
	}
	shape := func(d orchapi.Decision) any {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		decodeInto(t, string(b), &v)
		return shapeOf(v)
	}
	got, sockShape := shape(viaToken), shape(bySocket)
	var breaks, added []string
	diffShape(sockShape, got, "$", &breaks, &added)
	diffShape(got, sockShape, "$", &added, &breaks)
	if len(breaks)+len(added) > 0 {
		t.Errorf("an answer-token decision's shape differs from the socket's: %v %v", breaks, added)
	}
	b, err := os.ReadFile(goldenPath("ask_answer"))
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	breaks, added = nil, nil
	diffShape(want, got, "$", &breaks, &added)
	for _, b := range breaks {
		t.Errorf("an answer-token decision breaks the ask_answer client shape: %s", b)
	}
}

// zm token mint's flags: --answer-only takes --project, --node and --ttl
// and refuses --as, --role, --no-grant and an area; without it --as and
// --role are required and --node is refused.
func TestTokenMintFlags(t *testing.T) {
	run := func(args ...string) (api.MintTokenRequest, error) {
		cmd := tokenCmd()
		mint, _, err := cmd.Find([]string{"mint"})
		if err != nil {
			t.Fatal(err)
		}
		if err := mint.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		var req api.MintTokenRequest
		req.As, _ = mint.Flags().GetString("as")
		req.Role, _ = mint.Flags().GetString("role")
		req.Node, _ = mint.Flags().GetString("node")
		req.Area, _ = mint.Flags().GetString("area")
		answerOnly, _ := mint.Flags().GetBool("answer-only")
		ttl, _ := mint.Flags().GetDuration("ttl")
		req.Project = "demo" // the global -P
		return req, tokenMintFlags(mint, &req, answerOnly, ttl)
	}
	ok := []string{"--answer-only", "--node", "ivo-phone", "--ttl", "168h"}
	if req, err := run(ok...); err != nil || req.Limit != orchapi.AnswerLimit || req.Node != "ivo-phone" || req.As != "" || req.Role != "" {
		t.Fatalf("answer-only: %+v %v", req, err)
	}
	if req, err := run("--as", "bot", "--role", "member"); err != nil || req.Limit != "" {
		t.Fatalf("an owner-style mint: %+v %v", req, err)
	}
	for name, args := range map[string][]string{
		"answer-only with --as":       append([]string{"--as", "ivo"}, ok...),
		"answer-only with --role":     append([]string{"--role", "owner"}, ok...),
		"answer-only with --no-grant": append([]string{"--no-grant"}, ok...),
		"answer-only with an area":    append([]string{"--area", "solver"}, ok...),
		"answer-only without --node":  {"--answer-only", "--ttl", "168h"},
		"answer-only without --ttl":   {"--answer-only", "--node", "ivo-phone"},
		"--node without answer-only":  {"--as", "bot", "--role", "member", "--node", "ivo-phone"},
		"no --as":                     {"--role", "member"},
		"no --role":                   {"--as", "bot"},
	} {
		if req, err := run(args...); err == nil {
			t.Errorf("%s: accepted %+v", name, req)
		}
	}
}
