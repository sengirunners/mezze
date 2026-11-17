package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const root = "../.."

// The committed document is this generator's output: a shape change shows
// as a diff of it (and needs a "Clients" line in CHANGELOG.md).
func TestSchemaIsCurrent(t *testing.T) {
	want, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, schemaFile))
	if err != nil {
		t.Fatalf("%v: run go run ./scripts/clientschema from the repository root", err)
	}
	if !bytes.Equal(got, want) {
		gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		i := 0
		for i < len(gl) && i < len(wl) && gl[i] == wl[i] {
			i++
		}
		line := func(ls []string) string {
			if i < len(ls) {
				return ls[i]
			}
			return "(end of file)"
		}
		t.Fatalf("%s is not the Go types' schema (first difference at line %d: committed %q, generated %q).\n"+
			"A client-visible shape changed: run go run ./scripts/clientschema from the repository root, commit the file, "+
			"and add a \"Clients\" line to CHANGELOG.md unless the change only adds", schemaFile, i+1, line(gl), line(wl))
	}
}

var stableRow = regexp.MustCompile("^\\| `([A-Z]+) (/[^`]*)` \\| stable \\|")

// The route table is API.md's stable rows, no more and no fewer.
func TestRoutesAreAPIStableRows(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "API.md"))
	if err != nil {
		t.Fatal(err)
	}
	inDoc := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if m := stableRow.FindStringSubmatch(l); m != nil {
			inDoc[m[1]+" "+m[2]] = true
		}
	}
	if len(inDoc) == 0 {
		t.Fatal("API.md has no stable rows: did its table format change?")
	}
	here := map[string]bool{}
	for _, r := range routes {
		k := r.method + " " + r.path
		if here[k] {
			t.Errorf("%s is listed twice in routes", k)
		}
		here[k] = true
		if !inDoc[k] {
			t.Errorf("routes lists %s, which API.md does not label stable", k)
		}
		if len(r.ok) == 0 {
			t.Errorf("%s has no success body", k)
		}
	}
	for k := range inDoc {
		if !here[k] {
			t.Errorf("API.md labels %s stable: add it to routes (scripts/clientschema/routes.go) with its Go types", k)
		}
	}
}

// Every dev.zm.* type constant in the API type packages is in events (typed,
// or untyped with a reason) or in notEvents.
func TestEventsCoverEveryType(t *testing.T) {
	dirs := []string{filepath.Join(root, "internal", "orchapi")}
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}",
		"github.com/sengirunners/potluck/kit/api", "github.com/sengirunners/potluck/kit/events",
		"github.com/sengirunners/potluck/computeapi").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dirs = append(dirs, strings.Fields(string(out))...)
	consts := map[string]string{} // value -> where
	for _, d := range dirs {
		fset := token.NewFileSet()
		files, _ := filepath.Glob(filepath.Join(d, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			af, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range af.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					for _, v := range spec.(*ast.ValueSpec).Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						s, _ := strconv.Unquote(lit.Value)
						if strings.HasPrefix(s, "dev.zm.") && !strings.HasSuffix(s, ".") { // a prefix is no type
							consts[s] = fset.Position(lit.Pos()).String()
						}
					}
				}
			}
		}
	}
	if len(consts) < len(events) {
		t.Fatalf("found %d dev.zm.* constants, fewer than the %d events listed: did they move?", len(consts), len(events))
	}
	listed := map[string]bool{}
	for _, e := range events {
		listed[e.typ] = true
		if e.data == nil && e.why == "" {
			t.Errorf("%s has no data type and no reason", e.typ)
		}
		if consts[e.typ] == "" {
			t.Errorf("events lists %s, which no API package defines", e.typ)
		}
	}
	for typ := range notEvents {
		listed[typ] = true
		if consts[typ] == "" {
			t.Errorf("notEvents lists %s, which no API package defines", typ)
		}
	}
	for typ, at := range consts {
		if !listed[typ] {
			t.Errorf("%s (%s) is not in events: add it with its data's Go type (scripts/clientschema/routes.go)", typ, at)
		}
	}
}

// goldenSchemas is each cmd/zm/testdata/clients golden's schema: the stable
// route body that CLI command prints (array: it prints the list's elements
// as an array), or why it has none.
var goldenSchemas = map[string]struct {
	schema string
	array  bool
	none   string
}{
	"ack":               {schema: "AckResponse"},
	"ask_answer":        {schema: "Decision"},
	"ask_inbox":         {schema: "Inbox"},
	"ask_ls":            {schema: "Ask", array: true},
	"ask_new":           {schema: "Ask"},
	"ask_show":          {schema: "Ask"},
	"ask_show_answered": {schema: "Ask"},
	"ask_withdraw":      {schema: "Ask"},
	"claim":             {schema: "ClaimResponse"},
	"inbox":             {schema: "MailList"},
	"inflight":          {schema: "Inflight", array: true},
	"leases":            {schema: "Lease", array: true},
	"ls":                {schema: "SessionList"},
	"ls_all":            {schema: "SessionList"},
	"mail":              {schema: "MailList"},
	"people":            {schema: "PeopleList"},
	"project_ls":        {schema: "Project", array: true},
	"receipt_key":       {schema: "ReceiptKey"},
	"receipt_verify":    {schema: "ReceiptCheck"},
	"remotes":           {none: "GET /v0/team/remotes is beta"},
	"send":              {schema: "Mail"},
	"sent":              {schema: "MailList"},
	"session_show":      {schema: "Session"},
	"status":            {schema: "Health"},
	"thread":            {schema: "MailList"},
	"token_ls":          {schema: "Token", array: true},
	"token_mint":        {schema: "MintTokenResponse"},
	"who":               {schema: "HolderResponse"},
	"whoami":            {schema: "Whoami"},
	"work_new":          {schema: "WorkItem"},
	"work_show":         {schema: "WorkItem"},
}

// The CLI goldens (TestClientJSONShapes) and the schema agree: every key a
// golden holds is in its schema with the same JSON type, and every key the
// schema requires is in the golden.
func TestGoldensMatchSchema(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, schemaFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct{ Schemas map[string]any }
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(root, "cmd", "zm", "testdata", "clients", "*.json"))
	if len(files) == 0 {
		t.Fatal("no goldens in cmd/zm/testdata/clients")
	}
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		seen[name] = true
		g, ok := goldenSchemas[name]
		if !ok {
			t.Errorf("%s: no entry in goldenSchemas: name the schema of what that command prints", f)
			continue
		}
		if g.none != "" {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var shape any
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		s := map[string]any{"$ref": "#/components/schemas/" + g.schema}
		if g.array {
			s = map[string]any{"type": "array", "items": s}
		}
		for _, p := range (validator{doc.Components.Schemas}).check(s, shape, "$") {
			t.Errorf("%s against %s: %s", filepath.Base(f), g.schema, p)
		}
	}
	for name := range goldenSchemas {
		if !seen[name] {
			t.Errorf("goldenSchemas names %s, which has no golden file", name)
		}
	}
}

// validator checks a golden shape (cmd/zm/clients_golden_test.go shapeOf:
// each value replaced by its JSON type name) against a schema.
type validator struct{ defs map[string]any }

func (v validator) check(s map[string]any, shape any, at string) []string {
	if ref, ok := s["$ref"].(string); ok {
		def, ok := v.defs[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
		if !ok {
			return []string{at + ": " + ref + " is not defined"}
		}
		return v.check(def, shape, at)
	}
	if parts, ok := s["allOf"].([]any); ok { // one object: every part's properties and required keys
		merged := map[string]any{"type": "object"}
		props, req := map[string]any{}, []any{}
		for _, p := range append(parts, without(s, "allOf")) {
			p := v.resolve(p.(map[string]any))
			if _, ok := p["allOf"]; ok {
				return []string{at + ": nested allOf is not supported"}
			}
			pp, _ := p["properties"].(map[string]any)
			for k, x := range pp {
				props[k] = x
			}
			r, _ := p["required"].([]any)
			req = append(req, r...)
		}
		merged["properties"], merged["required"] = props, req
		return v.check(merged, shape, at)
	}
	if alts, ok := s["anyOf"].([]any); ok {
		var first []string
		for i, a := range alts {
			p := v.check(a.(map[string]any), shape, at)
			if len(p) == 0 {
				return nil
			}
			if i == 0 {
				first = p
			}
		}
		return first
	}
	types := map[string]bool{}
	switch ty := s["type"].(type) {
	case string:
		types[ty] = true
	case []any:
		for _, x := range ty {
			types[x.(string)] = true
		}
	case nil:
		return nil // any value
	}
	switch sh := shape.(type) {
	case map[string]any:
		if !types["object"] {
			return []string{fmt.Sprintf("%s is an object, the schema says %s", at, typeList(types))}
		}
		props, _ := s["properties"].(map[string]any)
		extra, _ := s["additionalProperties"].(map[string]any)
		var probs []string
		for _, k := range sortedKeys(sh) {
			ps, ok := props[k].(map[string]any)
			if !ok {
				ps = extra
			}
			if ps == nil {
				probs = append(probs, fmt.Sprintf("%s.%s is in the golden, not in the schema", at, k))
				continue
			}
			probs = append(probs, v.check(ps, sh[k], at+"."+k)...)
		}
		req, _ := s["required"].([]any)
		for _, r := range req {
			if _, ok := sh[r.(string)]; !ok {
				probs = append(probs, fmt.Sprintf("%s.%s is required by the schema, absent from the golden", at, r))
			}
		}
		return probs
	case []any:
		if !types["array"] {
			return []string{fmt.Sprintf("%s is an array, the schema says %s", at, typeList(types))}
		}
		items, _ := s["items"].(map[string]any)
		if items == nil || len(sh) == 0 {
			return nil
		}
		return v.check(items, sh[0], at+"[]")
	case string:
		want := map[string][]string{"string": {"string"}, "number": {"integer", "number"}, "bool": {"boolean"},
			"null": {"null"}, "[]": {"array"}}[sh]
		for _, w := range want {
			if types[w] {
				return nil
			}
		}
		return []string{fmt.Sprintf("%s is %s, the schema says %s", at, sh, typeList(types))}
	}
	return []string{fmt.Sprintf("%s: %v is not a shape", at, shape)}
}

// resolve follows a $ref.
func (v validator) resolve(s map[string]any) map[string]any {
	if ref, ok := s["$ref"].(string); ok {
		if def, ok := v.defs[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any); ok {
			return def
		}
	}
	return s
}

func without(s map[string]any, key string) map[string]any {
	c := make(map[string]any, len(s))
	for k, x := range s {
		if k != key {
			c[k] = x
		}
	}
	return c
}

func typeList(types map[string]bool) string {
	var out []string
	for k := range types {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The generator agrees with encoding/json on the cases it handles by hand:
// an embedded pointer, ",string" on a pointer, a struct embedded twice at
// one depth, "-" and omitempty. Each value's JSON is valid against the
// schema, and the schema has exactly the keys encoding/json can write.
func TestFieldsMatchEncodingJSON(t *testing.T) {
	type inner struct {
		A *int     `json:"a"`
		B []string `json:"b"`
	}
	type dup struct {
		X int `json:"x"`
	}
	type left struct{ dup }
	type right struct{ dup }
	type sample struct {
		*inner
		left
		right
		N    *int64 `json:"n,string"`
		M    int    `json:"m,string,omitempty"`
		Skip string `json:"-"`
		Opt  []int  `json:"opt,omitempty"`
	}
	g := newGen()
	name := g.component(of[sample](), response)
	if len(g.errs) > 0 {
		t.Fatal(g.errs)
	}
	defs := map[string]any{}
	b, _ := json.Marshal(g.defs)
	if err := json.Unmarshal(b, &defs); err != nil {
		t.Fatal(err)
	}
	five, one := int64(5), 1
	all := map[string]bool{}
	for _, v := range []sample{{}, {inner: &inner{}}, {inner: &inner{A: &one, B: []string{"b"}}, N: &five, M: 2, Opt: []int{1}}} {
		out, _ := json.Marshal(v)
		var got any
		_ = json.Unmarshal(out, &got)
		for k := range got.(map[string]any) {
			all[k] = true
		}
		ref := map[string]any{"$ref": "#/components/schemas/" + name}
		for _, p := range (validator{defs}).check(ref, shapeOf(got), "$") {
			t.Errorf("%s: %s", out, p)
		}
	}
	props := defs[name].(map[string]any)["properties"].(map[string]any)
	for k := range props {
		if !all[k] {
			t.Errorf("the schema has %q, which encoding/json never writes", k)
		}
	}
	if len(props) != len(all) {
		t.Errorf("schema keys %v, encoding/json writes %v", sortedKeys(props), all)
	}
}

// shapeOf is cmd/zm's golden shape of a decoded JSON value.
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
		return []any{shapeOf(x[0])}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	}
	return "null"
}

// Every 202 says when it answers.
func TestPendingSaysWhen(t *testing.T) {
	for _, r := range routes {
		if _, ok := r.ok[202]; ok && r.pending == "" {
			t.Errorf("%s %s answers 202 without pending", r.method, r.path)
		}
	}
}

// requiredOf is every required list of every component, by its place in the
// document ("Mail", "Mail/properties/origin"): docs/clients.md "Stability": a
// field added within potluck/1 is optional, so a list may only lose entries.
func requiredOf(t *testing.T, b []byte) map[string]map[string]bool {
	t.Helper()
	var doc struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			// An object schema's place counts with no required list too (the
			// generator drops an empty one), so its first entry is a new one.
			_, props := x["properties"]
			req, hasReq := x["required"].([]any)
			if props || hasReq {
				out[path] = map[string]bool{}
				for _, r := range req {
					out[path][fmt.Sprint(r)] = true
				}
			}
			for k, c := range x {
				if k != "required" {
					walk(path+"/"+k, c)
				}
			}
		case []any:
			for i, c := range x {
				walk(path+"/"+strconv.Itoa(i), c)
			}
		}
	}
	for name, s := range doc.Components.Schemas {
		walk(name, s)
	}
	return out
}

// newRequired lists every "Place.field" required in now but not in the
// released schema, for a place the released one already had.
func newRequired(released, now map[string]map[string]bool) []string {
	var bad []string
	for place, req := range now {
		old, had := released[place]
		if !had {
			continue
		}
		for f := range req {
			if !old[f] {
				bad = append(bad, place+"."+f)
			}
		}
	}
	sort.Strings(bad)
	return bad
}

// The schema may not require what the last release's did not: against the
// committed floor (releasedFile, refreshed when the contract's release is cut),
// and against main's copy when git has it (every main commit is a release, so
// this catches a PR that also refreshed the floor; ci's shallow checkout has
// no main, so there only the floor is checked).
func TestNoNewRequiredField(t *testing.T) {
	now, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	floor, err := os.ReadFile(filepath.Join(root, releasedFile))
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string][]byte{releasedFile: floor}
	if out, err := exec.Command("git", "-C", root, "show", "origin/main:"+schemaFile).Output(); err == nil {
		bases["origin/main:"+schemaFile] = out
	}
	for name, base := range bases {
		if bad := newRequired(requiredOf(t, base), requiredOf(t, now)); len(bad) > 0 {
			t.Fatalf("required since %s: %s.\nA field added within potluck/1 is optional: "+
				"give it omitempty (or omitzero), or take it out of the response; only a protocol bump may require it",
				name, strings.Join(bad, ", "))
		}
	}
}

func TestNewRequiredFieldIsCaught(t *testing.T) {
	released := map[string]map[string]bool{"Mail": {"id": true}, "Old": {}, "Mail/properties/o": {"a": true}}
	now := map[string]map[string]bool{"Mail": {"id": true, "kind": true}, "Old": {"x": true}, "Brand": {"y": true}, "Mail/properties/o": {"a": true, "b": true}}
	if got := strings.Join(newRequired(released, now), ","); got != "Mail.kind,Mail/properties/o.b,Old.x" {
		t.Fatalf("got %q", got)
	}
}

// DaemonStopping is said here because the Potluck module's type is unexported:
// its fields must stay the module's (kit/server.stopNote).
func TestDaemonStoppingMatchesPotluck(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/sengirunners/potluck").Output()
	if err != nil {
		t.Fatalf("potluck module not found: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(strings.TrimSpace(string(out)), "kit/server/swap.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var theirs []string
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == "stopNote" {
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("kit/server.stopNote is no longer a struct")
			}
			for _, fl := range st.Fields.List {
				if len(fl.Names) != 1 || fl.Tag == nil {
					t.Fatalf("kit/server.stopNote has a field this test cannot compare (embedded or untagged)")
				}
				theirs = append(theirs, fl.Names[0].Name+" "+types.ExprString(fl.Type)+" "+fl.Tag.Value)
			}
		}
		return true
	})
	var ours []string
	st := of[DaemonStopping]()
	for i := range st.NumField() {
		ours = append(ours, st.Field(i).Name+" "+st.Field(i).Type.String()+" `"+string(st.Field(i).Tag)+"`")
	}
	if strings.Join(theirs, "|") != strings.Join(ours, "|") {
		t.Fatalf("kit/server.stopNote is now %v; DaemonStopping says %v", theirs, ours)
	}
}
