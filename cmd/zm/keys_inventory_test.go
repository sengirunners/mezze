package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/secrets/inventory"
	"github.com/sengirunners/potluck/kit/server"
)

// keyHome is a temp home (a short path: socket paths max 103 bytes) with the
// given key files, each 0600 in 0700 directories.
func keyHome(t *testing.T, files ...string) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	for _, f := range files {
		p := filepath.Join(home, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not a real key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func keyKinds(rep inventory.Report) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, string(f.Severity)+":"+string(f.Kind)+":"+f.Row)
	}
	return out
}

const appKeyFile = "secrets/runners/app-1-key.pem"

// zmOwnKeys is one file of every zm key row.
var zmOwnKeys = []string{
	"receipt.key", "secrets/backup.key", "secrets/proj/bd.password", "secrets/ghlink/proj.token",
	"secrets/web-push-vapid", "approvals/github/01ARZ3NDEKTSV4RRFFQ69G5FAV.token",
}

func TestKeyInventoryTableIsValid(t *testing.T) {
	if err := keyInventory().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestKeyInventoryUnlistedFails: a file under a root that no row lists is a
// key nobody classed, and fails.
func TestKeyInventoryUnlistedFails(t *testing.T) {
	for _, stray := range []string{"secrets/stray.key", "secrets/proj/other.secret", "approvals/github/extra", "remotes/x.pem"} {
		home := keyHome(t, append([]string{stray}, zmOwnKeys...)...)
		rep, err := inventory.Check(inventory.Home{Dir: home}, keyInventory())
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Failed() || len(rep.Findings) != 1 || rep.Findings[0].Kind != inventory.FindUnlisted || rep.Findings[0].Where != stray {
			t.Errorf("%s: %v", stray, keyKinds(rep))
		}
	}
}

// TestKeyInventoryCleanAndExceptions: every zm key, the bootstrap marker and
// the runner App key (class S, held at O) are green with the exceptions list,
// and the App key is a failure without it.
func TestKeyInventoryCleanAndExceptions(t *testing.T) {
	home := keyHome(t, append([]string{appKeyFile, "approvals/bootstrapped"}, zmOwnKeys...)...)
	rep, err := inventory.Check(inventory.Home{Dir: home}, keyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() || len(rep.Findings) != 0 {
		t.Errorf("with the exceptions list: %v", keyKinds(rep))
	}
	if len(rep.Held) != len(zmOwnKeys)+1 {
		t.Errorf("held %d keys, want %d", len(rep.Held), len(zmOwnKeys)+1)
	}

	bare := keyInventory()
	bare.Exceptions = nil
	rep, err = inventory.Check(inventory.Home{Dir: home}, bare)
	if err != nil {
		t.Fatal(err)
	}
	if got := keyKinds(rep); !rep.Failed() || !slices.Equal(got, []string{"FAIL:below-class:runner-app-key"}) {
		t.Errorf("without the exceptions list: %v", got)
	}
	if rep.Findings[0].Fix == "" {
		t.Error("a failure with no fix line")
	}
}

// TestKeyInventoryWarnsWithAgents: a runner App key in a home that runs agent
// sessions warns (and does not fail); the same home without agents is clean.
func TestKeyInventoryWarnsWithAgents(t *testing.T) {
	home := keyHome(t, appKeyFile)
	rep, err := inventory.Check(inventory.Home{Dir: home, RunsAgentSessions: true}, keyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if got := keyKinds(rep); rep.Failed() || !slices.Equal(got, []string{"warn:agents-can-read:runner-app-key"}) {
		t.Errorf("with agents: %v", got)
	}
	if f := rep.Findings[0]; f.Fix == "" || f.Exception != appKeyBead(t) {
		t.Errorf("warning: %+v", f)
	}
	rep, err = inventory.Check(inventory.Home{Dir: home}, keyInventory())
	if err != nil || len(rep.Findings) != 0 {
		t.Errorf("without agents: %v %v", keyKinds(rep), err)
	}
}

// TestKeyInventoryLooseKeyFails: a zm key file that others can read fails.
func TestKeyInventoryLooseKeyFails(t *testing.T) {
	home := keyHome(t, "receipt.key")
	if err := os.Chmod(filepath.Join(home, "receipt.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := inventory.Check(inventory.Home{Dir: home}, keyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if got := keyKinds(rep); !slices.Equal(got, []string{"FAIL:loose-mode:receipt-key"}) {
		t.Errorf("%v", got)
	}
}

// keyWriter is one non-test source file that names a path of a key zm keeps
// under $ZM_HOME: the shape of each such expression (see keyShapes) and the
// files it writes there, one example each.
type keyWriter struct {
	shapes  []string
	samples []string
}

// keyWriters pins every place the tree names a key path. A file that appears
// in the tree and not here is a new writer without a row; a file whose shapes
// differ from the pinned ones changed where or how it writes a key, so its
// examples (and the rows) must be looked at again; a file listed here that no
// longer names a key path is stale. A path built from a field of another type
// (approvals/github.go joins a file name onto a directory it was given) is
// seen only where the directory is named, not where the file name is.
func keyWriters() map[string]keyWriter {
	return map[string]keyWriter{
		"internal/ghlink/ghlink.go": {
			[]string{`Join(*, "secrets", "ghlink", *+".token")`},
			[]string{"secrets/ghlink/proj.token"}},
		"internal/work/trackers_env.go": {
			[]string{`Join(*, "secrets", *, *+".password")`},
			[]string{"secrets/proj/bd.password"}},
		"internal/webui/push.go": {
			[]string{`Path("secrets/web-push-vapid")`},
			[]string{"secrets/web-push-vapid"}},
		"internal/homes/bkcrypt/bkcrypt.go": {
			[]string{`Join(*, bkcrypt.KeyRel)`, `lit "secrets/backup.key"`},
			[]string{bkcrypt.KeyRel}},
		"internal/approvals/receipt.go": {
			[]string{`lit "receipt.key"`},
			[]string{approvals.KeyFile}},
		"internal/approvals/approvals.go": {
			[]string{`Join(*, "approvals", "github")`, `Join(*, approvals.KeyFile)`},
			[]string{"approvals/github/01ARZ3NDEKTSV4RRFFQ69G5FAV.token"}},
		"internal/approvals/approvers.go": {
			[]string{`Join(*, approvals.BootstrapFile)`, `lit "approvals/bootstrapped"`},
			[]string{approvals.BootstrapFile}}, // the marker is ignored, not a key
	}
}

// isKeyPathLiteral is whether a string literal names a key path of a home by
// syntax: the receipt key, or a path under secrets/ or under approvals/
// ("approvals/0007_name" is a migration's name, numbered). A lone "secrets" or
// "approvals" is a word (a capability, a route), a directory only as an
// element of a Join or Path call (isKeyDir).
func isKeyPathLiteral(s string) bool {
	if rest, ok := strings.CutPrefix(s, "approvals/"); ok {
		return rest != "" && (rest[0] < '0' || rest[0] > '9')
	}
	return s == "receipt.key" || strings.HasPrefix(s, "secrets/")
}

// isKeyDir is whether s is a directory of a home that holds keys.
func isKeyDir(s string) bool { return s == "secrets" || s == "approvals" }

// exprShape renders a path element: a string literal as itself, a key path
// constant by its name, a concatenation by its parts, anything else as "*".
func exprShape(e ast.Expr, pkg string, consts map[string]bool) string {
	if n := constName(e, pkg); consts[n] {
		return n
	}
	switch e := unslash(e).(type) {
	case *ast.BasicLit:
		if s, err := strconv.Unquote(e.Value); e.Kind == token.STRING && err == nil {
			return strconv.Quote(s)
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return exprShape(e.X, pkg, consts) + "+" + exprShape(e.Y, pkg, consts)
		}
	}
	return "*"
}

// keyConsts are the names (package.Name) of string constants whose value is a key path
// literal, across the tree: a path built from one (filepath.Join(home, KeyFile))
// names a key path as much as the literal does.
func keyConsts(f *ast.File, into map[string]bool) {
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, v := range vs.Values {
				if b, ok := v.(*ast.BasicLit); ok && b.Kind == token.STRING && i < len(vs.Names) {
					if s, err := strconv.Unquote(b.Value); err == nil && isKeyPathLiteral(s) {
						into[f.Name.Name+"."+vs.Names[i].Name] = true
					}
				}
			}
		}
	}
}

// unslash looks through filepath.FromSlash(x) and filepath.ToSlash(x).
func unslash(e ast.Expr) ast.Expr {
	if c, ok := e.(*ast.CallExpr); ok && len(c.Args) == 1 {
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "FromSlash" || sel.Sel.Name == "ToSlash") {
			return c.Args[0]
		}
	}
	return e
}

// constName is the package.Name an expression refers to, from a file of
// package pkg (KeyFile in package approvals, or approvals.KeyFile elsewhere).
func constName(e ast.Expr, pkg string) string {
	switch e := unslash(e).(type) {
	case *ast.Ident:
		return pkg + "." + e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			return x.Name + "." + e.Sel.Name
		}
	}
	return ""
}

// keyShapes lists the expressions of one file that name a key path: a Join or
// Path call with an element that is a key path literal, a key path constant
// or the "approvals" directory (rendered with its elements, so moving a key
// file or adding another beside it changes the shape), and any other literal
// that names one.
func keyShapes(f *ast.File, consts map[string]bool) []string {
	var out []string
	consumed := map[*ast.BasicLit]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Join" && sel.Sel.Name != "Path") {
				return true
			}
			names := false
			for _, a := range n.Args {
				if b, ok := a.(*ast.BasicLit); ok && b.Kind == token.STRING {
					if s, err := strconv.Unquote(b.Value); err == nil && (isKeyPathLiteral(s) || isKeyDir(s)) {
						names = true
					}
				} else if consts[constName(a, f.Name.Name)] {
					names = true
				}
			}
			if !names {
				return true
			}
			parts := make([]string, len(n.Args))
			for i, a := range n.Args {
				parts[i] = exprShape(a, f.Name.Name, consts)
				if b, ok := a.(*ast.BasicLit); ok {
					consumed[b] = true
				}
			}
			out = append(out, sel.Sel.Name+"("+strings.Join(parts, ", ")+")")
		case *ast.BasicLit:
			if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && isKeyPathLiteral(s) && !consumed[n] {
				out = append(out, "lit "+strconv.Quote(s))
			}
		}
		return true
	})
	sort.Strings(out)
	return slices.Compact(out)
}

// TestKeyInventoryCoversWhatZmWrites: the table lists every key zm's own code
// writes under $ZM_HOME/secrets (and approvals/, receipt.key). It parses the
// tree for the places that name those paths, so a new writer, or a changed
// path in a known one, fails here until it is looked at; then each known
// writer's example files must be found by a row (or be the ignored marker).
func TestKeyInventoryCoversWhatZmWrites(t *testing.T) {
	root := filepath.Join("..", "..")
	files := map[string]*ast.File{}
	consts := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if rel == "cmd/zm/keys_inventory.go" { // the table itself
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if perr != nil {
				return perr
			}
			files[rel] = f
			keyConsts(f, consts)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	found := map[string][]string{}
	for rel, f := range files {
		if shapes := keyShapes(f, consts); len(shapes) > 0 {
			found[rel] = shapes
		}
	}
	writers := keyWriters()
	for f, shapes := range found {
		w, ok := writers[f]
		switch {
		case !ok:
			t.Errorf("%s names a key path under $ZM_HOME (%s): add it to keyWriters, with an example file, and a row to zmKeyRows (cmd/zm/keys_inventory.go) if it holds a secret", f, strings.Join(shapes, "; "))
		case !slices.Equal(shapes, w.shapes):
			t.Errorf("%s now names key paths %q, pinned %q: check that its example files in keyWriters and the rows still match what it writes", f, shapes, w.shapes)
		}
	}
	for f := range writers {
		if _, ok := found[f]; !ok {
			t.Errorf("%s is in keyWriters but names no key path any more: remove it", f)
		}
	}

	// Every file a known writer makes is a listed key (or the ignored marker).
	var samples []string
	for _, w := range writers {
		samples = append(samples, w.samples...)
	}
	sort.Strings(samples)
	samples = slices.Compact(samples)
	home := keyHome(t, samples...)
	held, err := inventory.Scan(inventory.Home{Dir: home}, keyInventory())
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, h := range held {
		if h.Row == "" {
			t.Errorf("%s: written by zm and has no row", h.Where)
		}
		listed[h.Where] = true
	}
	for _, s := range samples {
		if !listed[s] && s != approvals.BootstrapFile {
			t.Errorf("%s: written by zm and not found by any row", s)
		}
	}
}

func keysOf(t *testing.T, env *doctorEnv) doctorResult {
	t.Helper()
	return doctorKeys(context.Background(), env)
}

type fakeSessions struct {
	list    orchapi.SessionList
	project string // the Zm-Project header the last request carried
}

func (f *fakeSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.project = r.Header.Get(api.HeaderProject)
	w.Header().Set(api.HeaderProtocol, api.WireProtocol)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.list)
}

// TestDoctorKeys: the doctor line for each state of a home, and a plain fix
// under every result that is not ok.
func TestDoctorKeys(t *testing.T) {
	cfgOf := func(home string) *config.Config {
		cfg, err := config.LoadFrom(home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Machine = "here"
		return cfg
	}
	sessions := func(l ...orchapi.Session) *client.Client {
		return client.WithHandler(&fakeSessions{list: orchapi.SessionList{Sessions: l}})
	}
	claude := orchapi.Session{Agent: "claude", State: orchapi.StateRunning, Machine: "here"}
	elsewhere := orchapi.Session{Agent: "claude", State: orchapi.StateRunning, Machine: "another"}

	// ok: clean home, with the exception list and no agents running.
	clean := keyHome(t, append([]string{appKeyFile}, zmOwnKeys...)...)
	r := keysOf(t, &doctorEnv{Config: cfgOf(clean), Client: sessions(orchapi.Session{Agent: "shell", State: orchapi.StateIdle, Machine: "here"}, orchapi.Session{Agent: "codex", State: orchapi.StateEnded, Machine: "here"}, elsewhere)})
	if r.Status != doctorOK {
		t.Errorf("clean (an agent on another machine of the team cannot read this home): %+v", r)
	}

	// warn: the App key and a live agent session.
	r = keysOf(t, &doctorEnv{Config: cfgOf(clean), Client: sessions(claude)})
	if r.Status != doctorWarn || r.Fix == "" || !strings.Contains(r.Detail, "agent sessions") || !strings.Contains(r.Fix, appKeyBead(t)) {
		t.Errorf("app key with agents: %+v", r)
	}
	// warn: the daemon's own machine name, not the shell's, names this machine's sessions.
	daemonSide := orchapi.Session{Agent: "claude", State: orchapi.StateRunning, Machine: "daemon-name"}
	r = keysOf(t, &doctorEnv{Config: cfgOf(clean), Client: sessions(daemonSide), Health: &orchapi.Health{Health: api.Health{Machine: "daemon-name"}}})
	if r.Status != doctorWarn {
		t.Errorf("session on the daemon's machine name: %+v", r)
	}
	// warn: daemon down, so agents are unknown and assumed.
	r = keysOf(t, &doctorEnv{Config: cfgOf(clean), DaemonErr: api.Errorf(api.CodeUnavailable, "daemon not running")})
	if r.Status != doctorWarn || !strings.Contains(r.Detail, "assumed") || r.Fix == "" {
		t.Errorf("daemon down: %+v", r)
	}
	// ok: a home with no App key warns about nothing, daemon down or not.
	plain := keyHome(t, zmOwnKeys...)
	if r = keysOf(t, &doctorEnv{Config: cfgOf(plain), DaemonErr: api.Errorf(api.CodeUnavailable, "down")}); r.Status != doctorOK {
		t.Errorf("no app key: %+v", r)
	}

	// FAIL: an unlisted key, with its fix.
	stray := keyHome(t, "secrets/stray.key")
	r = keysOf(t, &doctorEnv{Config: cfgOf(stray), Client: sessions()})
	if r.Status != doctorFail || !strings.Contains(r.Detail, "secrets/stray.key") || r.Fix == "" {
		t.Errorf("unlisted: %+v", r)
	}

	// FAIL: a check that could not run is never a pass.
	if r = keysResult(inventory.Report{}, os.ErrPermission, ""); r.Status != doctorFail || r.Fix == "" {
		t.Errorf("could not check: %+v", r)
	}

	// skip: no home yet, no config, a daemon elsewhere.
	if r = keysOf(t, &doctorEnv{Config: &config.Config{Home: filepath.Join(clean, "absent")}}); r.Status != doctorSkip {
		t.Errorf("no home: %+v", r)
	}
	if r = keysOf(t, &doctorEnv{ConfErr: os.ErrInvalid}); r.Status != doctorSkip {
		t.Errorf("no config: %+v", r)
	}
	if r = keysOf(t, &doctorEnv{Config: cfgOf(clean), Remote: true}); r.Status != doctorSkip || r.Fix == "" {
		t.Errorf("remote: %+v", r)
	}

	// -P narrows other lists to one project; this one asks for all, and puts -P back.
	fake := &fakeSessions{list: orchapi.SessionList{Sessions: []orchapi.Session{claude}}}
	scoped := client.WithHandler(fake)
	scoped.Project, scoped.ProjectFrom = "p1", "flag"
	if r = keysOf(t, &doctorEnv{Config: cfgOf(clean), Client: scoped}); r.Status != doctorWarn || fake.project != "" || scoped.Project != "p1" || scoped.ProjectFrom != "flag" {
		t.Errorf("project flag: %+v, header %q, client %q/%q", r, fake.project, scoped.Project, scoped.ProjectFrom)
	}

	// An incomplete session list may hide an agent: assumed, not cleared.
	paged := client.WithHandler(&fakeSessions{list: orchapi.SessionList{Next: "more"}})
	if r = keysOf(t, &doctorEnv{Config: cfgOf(clean), Client: paged}); r.Status != doctorWarn {
		t.Errorf("paged list: %+v", r)
	}
}

// TestDoctorKeysFreshDaemonHome: a home a real (in-process) daemon just made,
// with the keys it writes on its own, is green; zm doctor lists the check.
func TestDoctorKeysFreshDaemonHome(t *testing.T) {
	home := keyHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := client.WithHandler(dm.SocketHandler())
	env := &doctorEnv{Config: cfg, Client: c, LookPath: func(string) (string, error) { return "/bin/claude", nil }}
	rs := doctorByName(runDoctor(ctx, env))
	r, ok := rs["secret keys"]
	if !ok {
		t.Fatalf("zm doctor has no secret keys line: %v", rs)
	}
	if r.Status != doctorOK {
		t.Errorf("fresh daemon home: %+v", r)
	}
}

// appKeyBead is the work item Potluck's table names for the runner App key
// exception; tests read it from the table so a change there is not a drift here.
func appKeyBead(t *testing.T) string {
	t.Helper()
	b := inventory.Potluck().Exceptions["runner-app-key"]
	if b == "" {
		t.Fatal("Potluck's table lists no exception for the runner App key")
	}
	return b
}
