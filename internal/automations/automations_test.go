package automations

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// fix is an authority ("auth") whose team project zm has a home on another
// machine, bea-mac, reached through bea-mac's node-bound token.
type fix struct {
	s                          *Service
	d                          *core.Deps
	p                          api.Project
	owner, bea, minted, guest  *authz.Subject
	beaUnmarked, beaOtherwhere *authz.Subject
}

func newFix(t *testing.T) *fix {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "ivo", Machine: "auth"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := reg.Local("")
	p, err := reg.Create(ctx, owner, api.NewProjectRequest{Slug: "zm"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "billing"} {
		if _, err := reg.AddArea(ctx, owner, "zm", api.Area{Name: a}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetTeam(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	sub := func(req api.MintTokenRequest) *authz.Subject {
		tok, err := reg.Mint(ctx, owner, req)
		if err != nil {
			t.Fatal(err)
		}
		s, err := reg.FromToken(ctx, tok.Secret, false)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	member := sub(api.MintTokenRequest{As: "bea", Project: p.ID, Role: "member"})
	bea := *member
	bea.Node, bea.Machine = "nBEA", "bea-mac"
	other := bea
	other.Node, other.Machine = "nCAL", "cal-mac"
	d := &core.Deps{Store: st, Projects: reg, Config: cfg}
	f := &fix{d: d, p: p, owner: owner, bea: &bea, beaUnmarked: &bea, beaOtherwhere: &other, minted: member,
		guest: sub(api.MintTokenRequest{As: "vida", Project: p.ID, Area: "solver", Role: "guest"})}
	f.s = New(d)
	clock := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	f.s.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return f
}

type resp struct {
	code int
	body string
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("%d %s: %v", r.code, r.body, err)
	}
}

// call is one request as sub; marked adds the origin mark a checking
// daemon's gate sets.
func (f *fix) call(sub *authz.Subject, marked bool, method, path string, body any) resp {
	r := core.NewRouter()
	f.s.Routes(r)
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	if marked {
		path += sep(path) + orchapi.OriginChecked + "=1"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req = req.WithContext(core.WithSubject(req.Context(), sub))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return resp{w.Code, w.Body.String()}
}

func sep(p string) string {
	if strings.Contains(p, "?") {
		return "&"
	}
	return "?"
}

func want(t *testing.T, what string, r resp, code int, has string) {
	t.Helper()
	if r.code != code || !strings.Contains(r.body, has) {
		t.Fatalf("%s: %d %s (want %d with %q)", what, r.code, r.body, code, has)
	}
}

func create(name, area string) orchapi.AutomationCreate {
	return orchapi.AutomationCreate{Project: "zm", Area: area, Home: "bea-mac", Install: "i1", Name: name, Body: "Summarize the night's failures.",
		Fields: json.RawMessage(`{"on":{"schedule":"0 3 * * *","webhook":{"signature":{"header":"X-Hub-Signature-256","encoding":"hex","prefix":"sha256="}}},"action":"report"}`)}
}

func (f *fix) claimed(t *testing.T, store string) {
	t.Helper()
	want(t, "bea claims its home", f.call(f.bea, true, "PUT", "/v0/automation-homes/bea-mac?project=zm", orchapi.AutomationHomeClaim{Install: "i1", Store: store, Path: "/srv/z3/bea-z3"}), 200, `"install":"i1"`)
}

func (f *fix) record(t *testing.T, c orchapi.AutomationCreate) orchapi.Automation {
	t.Helper()
	r := f.call(f.bea, true, "POST", "/v0/automations", c)
	want(t, "create", r, 200, `"rev":1`)
	var a orchapi.Automation
	r.decode(t, &a)
	return a
}

// Only the home's own daemon, naming the install holding the claim, from
// outside any agent session, writes a home's records.
func TestHomeRule(t *testing.T) {
	f := newFix(t)
	f.claimed(t, "zm")
	a := f.record(t, create("nightly", "billing"))
	if a.Home != "bea-mac" || a.TaskID != "automation:"+a.ID || a.UpdatedBy != "machine:bea-mac" || a.Area != "billing" || !a.Enabled || a.Project != "zm" {
		t.Fatalf("created %+v", a)
	}
	patch := orchapi.AutomationPatch{Rev: 1, Install: "i1", Body: ptr("Summarize, briefly.")}
	path := "/v0/automations/" + a.ID
	// Not the home: the authority's own socket owner, a minted token, another member machine.
	want(t, "the authority's socket owner", f.call(f.owner, true, "PATCH", path, patch), 403, "written only by bea-mac's own daemon")
	want(t, "a minted token", f.call(f.minted, true, "PATCH", path, patch), 403, "home machine's own daemon")
	want(t, "another member machine", f.call(f.beaOtherwhere, true, "PATCH", path, patch), 403, "this call comes from cal-mac")
	want(t, "another machine creates for bea-mac", f.call(f.beaOtherwhere, true, "POST", "/v0/automations", create("x", "")), 403, "bea-mac's own daemon")
	want(t, "the owner creates for bea-mac", f.call(f.owner, true, "POST", "/v0/automations", create("x", "")), 403, "bea-mac's own daemon")
	// The home's machine, but the call says it is an agent session (Zm-As),
	// even from a process the gate found outside every session.
	sess := f.bea.ForSession("session:bea-mac/agent")
	want(t, "an agent session", f.call(sess, true, "PATCH", path, patch), 403, "acts as agent session")
	// The home, but its daemon did not check for an agent session.
	want(t, "unmarked", f.call(f.beaUnmarked, false, "PATCH", path, patch), 403, "too old to check who calls it")
	// The home, but not the install holding the claim.
	wrong := patch
	wrong.Install = "i2"
	want(t, "a wrong install", f.call(f.bea, true, "PATCH", path, wrong), 409, "install i2 does not hold home bea-mac")
	want(t, "no install", f.call(f.bea, true, "PATCH", path, orchapi.AutomationPatch{Rev: 1}), 400, "install")
	want(t, "health from a wrong install", f.call(f.bea, true, "PUT", path+"/health", orchapi.AutomationHealth{Install: "i2", State: "healthy", AsOf: "2026-11-16T12:00:00Z"}), 409, "does not hold")
	want(t, "delete from a wrong install", f.call(f.bea, true, "DELETE", path+"?install=i2", nil), 409, "does not hold")
	// None of them changed it.
	if r := f.call(f.owner, false, "GET", path, nil); !strings.Contains(r.body, `"rev":1`) {
		t.Fatalf("after refusals: %s", r.body)
	}
	// The home does.
	r := f.call(f.bea, true, "PATCH", path, patch)
	want(t, "the home's patch", r, 200, `"rev":2`)
	// A patch from a revision read before is refused.
	want(t, "stale rev", f.call(f.bea, true, "PATCH", path, patch), 409, "changed since")
	// A second install's claim is 409 and recorded as wanted.
	want(t, "a second install", f.call(f.bea, true, "PUT", "/v0/automation-homes/bea-mac?project=zm", orchapi.AutomationHomeClaim{Install: "i2", Store: "zm", Path: "/srv/z3/bea-copy"}), 409, "recorded as the home's wanted install")
	var hs orchapi.AutomationHomes
	f.call(f.owner, false, "GET", "/v0/automation-homes?project=zm", nil).decode(t, &hs)
	if len(hs.Homes) != 1 || hs.Homes[0].Install != "i1" || hs.Homes[0].Wanted == nil || hs.Homes[0].Wanted.Install != "i2" || hs.Homes[0].Wanted.Path != "/srv/z3/bea-copy" {
		t.Fatalf("homes %+v", hs)
	}
	// The claim route takes the home rule too.
	want(t, "the owner claims bea-mac", f.call(f.owner, true, "PUT", "/v0/automation-homes/bea-mac?project=zm", orchapi.AutomationHomeClaim{Install: "i9", Store: "zm"}), 403, "bea-mac's own daemon")
	want(t, "a guest reads homes", f.call(f.guest, false, "GET", "/v0/automation-homes?project=zm", nil), 404, "no project")
}

func ptr[T any](v T) *T { return &v }

// A home whose claim says store z3 takes no definition write; health still.
func TestStoreZ3RefusesDefinitionWrites(t *testing.T) {
	f := newFix(t)
	want(t, "no claim yet", f.call(f.bea, true, "POST", "/v0/automations", create("x", "")), 409, "has no claim")
	f.claimed(t, "z3")
	want(t, "create on z3", f.call(f.bea, true, "POST", "/v0/automations", create("x", "")), 409, "store z3")
	f.claimed(t, "zm")
	a := f.record(t, create("x", ""))
	f.claimed(t, "z3")
	want(t, "patch on z3", f.call(f.bea, true, "PATCH", "/v0/automations/"+a.ID, orchapi.AutomationPatch{Rev: 1, Install: "i1", Name: ptr("y")}), 409, "store z3")
	want(t, "health on z3", f.call(f.bea, true, "PUT", "/v0/automations/"+a.ID+"/health", orchapi.AutomationHealth{Install: "i1", State: "healthy", AppliedRev: 1, AsOf: "2026-11-16T12:00:00Z"}), 200, `"state":"healthy"`)
}

func TestPatchShapes(t *testing.T) {
	f := newFix(t)
	f.claimed(t, "zm")
	c := create("x", "")
	c.Settings = map[string]orchapi.AutomationSetting{"budget": {Type: "number", Value: json.RawMessage(`5`)}, "mode": {Type: "enum", Value: json.RawMessage(`"fast"`), Options: []string{"fast", "slow"}}}
	a := f.record(t, c)
	path := "/v0/automations/" + a.ID
	patch := func(rev int64, raw string) resp {
		var p map[string]any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber() // a big number keeps its digits on the way
		_ = dec.Decode(&p)
		p["rev"], p["install"] = rev, "i1"
		return f.call(f.bea, true, "PATCH", path, p)
	}
	// fields merge: one key changed, one removed, the rest kept.
	var got orchapi.Automation
	dec := func(r resp) { got = orchapi.Automation{}; r.decode(t, &got) }
	dec(patch(1, `{"fields":{"on":{"schedule":"0 4 * * *"},"action":null}}`))
	if got.Rev != 2 || string(got.Fields) != `{"on":{"schedule":"0 4 * * *","webhook":{"signature":{"encoding":"hex","header":"X-Hub-Signature-256","prefix":"sha256="}}}}` {
		t.Fatalf("merge: %s", got.Fields)
	}
	// replace_fields sets a null a merge patch cannot.
	dec(patch(2, `{"fields":{"on":{"webhook":{"signature":null}}},"replace_fields":true}`))
	if got.Rev != 3 || string(got.Fields) != `{"on":{"webhook":{"signature":null}}}` {
		t.Fatalf("replace: %s", got.Fields)
	}
	// settings merge; owner set then cleared; enabled off.
	dec(patch(3, `{"settings":{"budget":{"value":7},"mode":null},"owner":"zm-abc","enabled":false}`))
	if got.Rev != 4 || string(got.Settings["budget"].Value) != "7" || len(got.Settings) != 1 || got.Owner != "zm-abc" || got.Enabled {
		t.Fatalf("settings: %+v", got)
	}
	dec(patch(4, `{"owner":null}`))
	if got.Owner != "" || got.Rev != 5 {
		t.Fatalf("owner null: %+v", got)
	}
	// The same values again make no revision.
	dec(patch(5, `{"owner":null}`))
	if got.Rev != 5 {
		t.Fatalf("a no-op patch made rev %d", got.Rev)
	}
	// Refusals.
	want(t, "a secret in the signature", patch(5, `{"fields":{"on":{"webhook":{"signature":{"secret":"s3cr3t"}}}}}`), 400, "signing secret stays in the Mezze app")
	want(t, "a secret-ref in the signature", patch(5, `{"fields":{"on":{"webhook":{"signature":{"secret-ref":"op://x"}}}}}`), 400, "signing secret stays in the Mezze app")
	want(t, "name inside fields", patch(5, `{"fields":{"name":"x"}}`), 400, "top-level key")
	want(t, "fields not an object", patch(5, `{"fields":[1],"replace_fields":true}`), 400, "JSON object")
	want(t, "a setting of the wrong type", patch(5, `{"settings":{"budget":{"value":"seven"}}}`), 400, "not a valid number")
	want(t, "an enum value not listed", patch(5, `{"settings":{"m":{"type":"enum","value":"x","options":["a"]}}}`), 400, "not a valid enum")
	want(t, "a secret type", patch(5, `{"settings":{"k":{"type":"secret","value":"x"}}}`), 400, "no secret type")
	big := map[string]any{}
	for i := range 4 {
		big["k"+string(rune('a'+i))] = map[string]any{"type": "string", "value": strings.Repeat("v", 900)}
	}
	bb, _ := json.Marshal(map[string]any{"settings": big})
	want(t, "settings over 3,500 bytes", patch(5, string(bb)), 400, "at most 3500")
	ok := map[string]any{}
	for i := range 3 {
		ok["k"+string(rune('a'+i))] = map[string]any{"type": "string", "value": strings.Repeat("v", 1100)}
	}
	ob, _ := json.Marshal(map[string]any{"settings": ok})
	want(t, "settings just under", patch(5, string(ob)), 200, `"rev":6`)
	want(t, "an unknown key", patch(6, `{"area":"billing"}`), 400, "unknown field")
	want(t, "a body over 64 KiB", patch(6, `{"body":"`+strings.Repeat("b", 64<<10+1)+`"}`), 400, "body is over")
	// A merge keeps big numbers' digits and makes no revision when it changes nothing.
	dec(patch(6, `{"fields":{"chan":12345678901234567891,"x":"<a&b>"}}`))
	if got.Rev != 7 || !strings.Contains(string(got.Fields), `"chan":12345678901234567891`) {
		t.Fatalf("big number: %d %s", got.Rev, got.Fields)
	}
	dec(patch(7, `{"fields":{"chan":12345678901234567891}}`))
	if got.Rev != 7 {
		t.Fatalf("an unchanged merge made rev %d", got.Rev)
	}
	dec(patch(7, `{"fields":{"chan":null,"x":null}}`))
	if got.Rev != 8 {
		t.Fatalf("removing them: rev %d", got.Rev)
	}
	// Fields as written (keys unsorted, an HTML character) take an unchanged merge without a revision.
	asWritten := create("as-written", "")
	asWritten.Fields = json.RawMessage(`{"on":{"schedule":"0 3 * * *"},"note":"<a&b>","action":"report"}`)
	w := f.record(t, asWritten)
	want(t, "an unchanged merge on fields as written", f.call(f.bea, true, "PATCH", "/v0/automations/"+w.ID,
		map[string]any{"rev": 1, "install": "i1", "fields": map[string]any{"action": "report"}}), 200, `"rev":1`)
	want(t, "an unknown kind", f.call(f.bea, true, "POST", "/v0/automations", func() orchapi.AutomationCreate { c := create("y", ""); c.Kind = "loop"; return c }()), 400, "unknown kind")
	want(t, "the same task id again", f.call(f.bea, true, "POST", "/v0/automations", func() orchapi.AutomationCreate { c := create("y", ""); c.TaskID = a.TaskID; return c }()), 409, a.ID)
	// History: newest first, each revision's definition.
	var h orchapi.AutomationHistory
	f.call(f.guest, false, "GET", path+"/history", nil)
	f.call(f.owner, false, "GET", path+"/history", nil).decode(t, &h)
	if len(h.Revisions) != 8 || h.Revisions[0].Rev != 8 || h.Revisions[7].Op != "create" || h.Revisions[0].UpdatedBy != "machine:bea-mac" {
		t.Fatalf("history %+v", h)
	}
}

// A guest reads only its area's records; a hidden record answers exactly
// like a missing one on every record route.
func TestHiddenLikeMissing(t *testing.T) {
	f := newFix(t)
	f.claimed(t, "zm")
	hidden := f.record(t, create("HIDDEN billing job", "billing"))
	vis := f.record(t, create("solver job", "solver"))
	var l orchapi.AutomationList
	f.call(f.guest, false, "GET", "/v0/automations?project=zm&home=bea-mac", nil).decode(t, &l)
	if len(l.Automations) != 1 || l.Automations[0].ID != vis.ID || l.Holder != "i1" {
		t.Fatalf("guest list %+v", l)
	}
	missing := "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	for _, c := range []struct{ method, suffix string }{{"GET", ""}, {"GET", "/history"}, {"PATCH", ""}, {"DELETE", "?install=i1"}, {"PUT", "/health"}} {
		h := f.call(f.guest, false, c.method, "/v0/automations/"+hidden.ID+c.suffix, map[string]any{})
		m := f.call(f.guest, false, c.method, "/v0/automations/"+missing+c.suffix, map[string]any{})
		n := f.call(f.guest, false, c.method, "/v0/automations/nope"+c.suffix, map[string]any{})
		if h.code != 404 || h.code != m.code || strings.ReplaceAll(h.body, hidden.ID, "X") != strings.ReplaceAll(m.body, missing, "X") ||
			strings.ReplaceAll(n.body, "nope", "X") != strings.ReplaceAll(m.body, missing, "X") || strings.Contains(h.body, "HIDDEN") {
			t.Fatalf("%s %s: hidden %d %s, missing %d %s, malformed %s", c.method, c.suffix, h.code, h.body, m.code, m.body, n.body)
		}
	}
	// The guest may read its area's record but not write it (no home).
	want(t, "guest reads its area", f.call(f.guest, false, "GET", "/v0/automations/"+vis.ID, nil), 200, "solver job")
	want(t, "guest patches its area", f.call(f.guest, true, "PATCH", "/v0/automations/"+vis.ID, orchapi.AutomationPatch{Rev: 1, Install: "i1"}), 403, "home machine's own daemon")
	// A hidden project answers like a missing one.
	h := f.call(f.guest, false, "GET", "/v0/automations?project=nope-zz", nil)
	if h.code != 404 {
		t.Fatalf("missing project: %d %s", h.code, h.body)
	}
	want(t, "no project", f.call(f.guest, false, "GET", "/v0/automations", nil), 400, "name the project")
}

func TestTombstoneAndHealth(t *testing.T) {
	f := newFix(t)
	f.claimed(t, "zm")
	a := f.record(t, create("x", ""))
	path := "/v0/automations/" + a.ID
	want(t, "health", f.call(f.bea, true, "PUT", path+"/health", orchapi.AutomationHealth{Install: "i1", State: "unapplied", AppliedRev: 0, Secret: "missing",
		ApplyError: "a past once", AsOf: "2026-11-16T12:00:00Z"}), 200, `"received"`)
	want(t, "an apply error over 300 bytes", f.call(f.bea, true, "PUT", path+"/health", orchapi.AutomationHealth{Install: "i1", State: "degraded", ApplyError: strings.Repeat("e", 301), AsOf: "2026-11-16T12:00:00Z"}), 400, "apply_error")
	want(t, "a run's text as status", f.call(f.bea, true, "PUT", path+"/health", orchapi.AutomationHealth{Install: "i1", State: "degraded", LastRunStatus: "Error: boom at line 3", AsOf: "2026-11-16T12:00:00Z"}), 400, "last_run_status")
	if r := f.call(f.owner, false, "GET", path, nil); !strings.Contains(r.body, `"apply_error":"a past once"`) || !strings.Contains(r.body, `"rev":1`) {
		t.Fatalf("health on the record: %s", r.body)
	}
	want(t, "delete with a stale rev", f.call(f.bea, true, "DELETE", path+"?install=i1&rev=7", nil), 409, "changed since")
	want(t, "delete", f.call(f.bea, true, "DELETE", path+"?install=i1&rev=1", nil), 200, `"deleted":"`)
	want(t, "delete again", f.call(f.bea, true, "DELETE", path+"?install=i1", nil), 200, `"rev":2`)
	want(t, "patch a tombstone", f.call(f.bea, true, "PATCH", path, orchapi.AutomationPatch{Rev: 2, Install: "i1", Name: ptr("y")}), 409, "deleted")
	want(t, "the last health post", f.call(f.bea, true, "PUT", path+"/health", orchapi.AutomationHealth{Install: "i1", State: "healthy", AppliedRev: 2, AsOf: "2026-11-16T12:00:00Z"}), 200, `"applied_rev":2`)
	var l orchapi.AutomationList
	f.call(f.owner, false, "GET", "/v0/automations?project=zm", nil).decode(t, &l)
	if len(l.Automations) != 0 {
		t.Fatalf("a tombstone listed: %+v", l)
	}
	f.call(f.owner, false, "GET", "/v0/automations?project=zm&deleted=1", nil).decode(t, &l)
	if len(l.Automations) != 1 || l.Automations[0].Deleted == "" || l.Automations[0].Health == nil {
		t.Fatalf("deleted=1: %+v", l)
	}
}

// The scrubber reaches bodies, fields, history and apply errors.
func TestScrubber(t *testing.T) {
	f := newFix(t)
	f.claimed(t, "zm")
	const secret = "ghp_0123456789abcdefSECRET"
	c := create("x", "")
	c.Body = "use " + secret
	c.Fields = json.RawMessage(`{"note":"` + secret + `"}`)
	a := f.record(t, c)
	path := "/v0/automations/" + a.ID
	want(t, "patch", f.call(f.bea, true, "PATCH", path, orchapi.AutomationPatch{Rev: 1, Install: "i1", Name: ptr("again")}), 200, `"rev":2`)
	want(t, "health", f.call(f.bea, true, "PUT", path+"/health", orchapi.AutomationHealth{Install: "i1", State: "degraded", ApplyError: "bad " + secret, AsOf: "2026-11-16T12:00:00Z"}), 200, "")
	if err := f.d.ScrubAll(context.Background(), core.Scrub{Project: f.p.ID, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	all := f.call(f.owner, false, "GET", path, nil).body + f.call(f.owner, false, "GET", path+"/history", nil).body
	if strings.Contains(all, secret) || !strings.Contains(all, core.Redacted) {
		t.Fatalf("after scrub: %s", all)
	}
}
