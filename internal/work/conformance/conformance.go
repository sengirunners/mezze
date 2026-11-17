// Package conformance checks a work adapter against the zm/v0 adapter
// contract (SPEC.md "Work adapters"). The zm-work-conformance command runs
// it against an executable; tests run it in process. Owned by the work builder.
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
)

// RoundTrip sends one raw request and returns the adapter's response. An
// error means the adapter itself failed (crash, non-zero exit, bad JSON).
type RoundTrip func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error)

// Exec is the RoundTrip for an adapter executable.
func Exec(path string) RoundTrip {
	return func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
		return adapter.Exec(ctx, path, in)
	}
}

// Options select what to check.
type Options struct {
	// ID is an existing item to read (and, with Write, to change). Default:
	// the first item list returns.
	ID string
	// Write also exercises claim, release, comment, set_status and link on
	// ID. It changes that item in its tracker. Without it the run never sends
	// a write verb (orchapi.WriteVerbs), whatever the adapter answers.
	Write bool
	// Config is sent as the request's tracker config (project.toml [[trackers]] config).
	Config map[string]string
	// Setup also runs an adapter's setup verb (SPEC 11) in a throwaway
	// data_dir: a dry run writes nothing, a frozen call only probes, undo
	// paths stay inside data_dir, and no result carries Secret. It runs the
	// real setup once: only for a tracker that can be set up again. The
	// daemon's checks never set it (tracker test never calls setup).
	Setup bool
	// SetupOptions are the setup options to send (e.g. server=h:p); the
	// run fills args.zm itself (a throwaway data_dir).
	SetupOptions map[string]string
	// HasPassword sets args.zm.has_password: the caller put a password in
	// the adapter's secret slot (its environment).
	HasPassword bool
	// Secret is a value the caller put in the adapter's secret slot (its
	// environment): no setup result or refusal may carry it.
	Secret string
	// Hold, when set, runs the check that check writes nothing, which
	// compares the tracker's files before and after: anything else reading
	// the tracker meanwhile counts against it, because bd's embedded Dolt
	// rewrites its files on a read. zm passes SlotHold on its mirror's slot,
	// so no sync overlaps. An error means check did not run: reported as
	// skipped.
	Hold func(check func()) error
}

// Result is one check.
type Result struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Holders the write checks claim as.
const (
	HolderA = "conformance/aaaaaaaa"
	HolderB = "conformance/bbbbbbbb"
)

var prefixRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type run struct {
	ctx    context.Context
	rt     RoundTrip
	opt    Options
	out    []Result
	prefix string
	verbs  []string
	desc   orchapi.AdapterDescribe
}

func (r *run) add(name string, err error) bool {
	res := Result{Name: name, OK: err == nil}
	if err != nil {
		res.Detail = err.Error()
	}
	r.out = append(r.out, res)
	return err == nil
}

func (r *run) skip(name, why string) {
	r.out = append(r.out, Result{Name: name, OK: true, Skipped: true, Detail: why})
}

// call sends a well-formed request. It returns the decoded result or the
// adapter's refusal as *api.Error; a transport failure is a plain error.
func (r *run) call(verb, id string, args any, out any) (*api.Error, error) {
	if !r.opt.Write && slices.Contains(orchapi.WriteVerbs, verb) {
		return nil, fmt.Errorf("conformance: refusing to send write verb %q in a read-only run", verb)
	}
	req := orchapi.AdapterRequest{Spec: api.AdapterProtocol, Verb: verb, ID: id, Config: r.opt.Config}
	if args != nil {
		req.Args, _ = json.Marshal(args)
	}
	in, _ := json.Marshal(req)
	resp, err := r.rt(r.ctx, in)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		if resp.Error == nil || resp.Error.Code == "" {
			return nil, fmt.Errorf("ok:false without error.code")
		}
		return resp.Error, nil
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return nil, fmt.Errorf("result does not decode: %v", err)
		}
	}
	return nil, nil
}

// must turns a refusal into an error for checks that expect success.
func must(e *api.Error, err error) error {
	if err != nil {
		return err
	}
	if e != nil {
		return fmt.Errorf("refused: %s", e.Error())
	}
	return nil
}

func (r *run) checkItem(it orchapi.WorkItem, wantID string) error {
	if !strings.HasPrefix(it.ID, r.prefix+":") {
		return fmt.Errorf("id %q does not start with %q", it.ID, r.prefix+":")
	}
	if wantID != "" && it.ID != wantID {
		return fmt.Errorf("id %q, want %q", it.ID, wantID)
	}
	if it.Source != r.prefix {
		return fmt.Errorf("%s: source %q, want %q", it.ID, it.Source, r.prefix)
	}
	if !orchapi.ValidStatus(it.Status) {
		return fmt.Errorf("%s: status %q is not open|claimed|in_review|done", it.ID, it.Status)
	}
	if it.Title == "" {
		return fmt.Errorf("%s: empty title", it.ID)
	}
	return nil
}

// Run checks the adapter behind rt and returns every result in order.
func Run(ctx context.Context, rt RoundTrip, opt Options) []Result {
	r := &run{ctx: ctx, rt: rt, opt: opt}
	if !r.describe() {
		return r.out
	}
	if opt.Setup {
		r.setup()
	}
	r.add("unknown verb answers not_implemented", r.expectCode("no-such-verb", "", nil, api.CodeNotImplemented))
	r.add("malformed request answers ok:false", r.malformed())

	var listed orchapi.WorkList
	err := must(r.call(orchapi.VerbList, "", orchapi.AdapterListArgs{Limit: 3}, &listed))
	if err == nil && listed.Items == nil {
		err = fmt.Errorf(`result has no "items" array`)
	}
	if err == nil && len(listed.Items) > 3 {
		err = fmt.Errorf("limit 3 returned %d items", len(listed.Items))
	}
	for _, it := range listed.Items {
		if err == nil {
			err = r.checkItem(it, "")
		}
	}
	r.add("list returns valid items within limit", err)
	r.changes()
	if slices.Contains(r.verbs, orchapi.VerbCheck) { // optional: never sent to an adapter that does not list it
		r.checkWritesNothing(r.opt.Config, nil)
	}

	id := opt.ID
	if id == "" && len(listed.Items) > 0 {
		id = listed.Items[0].ID
	}
	r.add("get of a missing item answers not_found (or bad_request for an id it cannot parse)",
		r.expectCode(orchapi.VerbGet, r.prefix+":zm-conformance-missing-0", nil, api.CodeNotFound, api.CodeBadRequest))
	r.add("get of a foreign id is refused", r.expectRefusal(orchapi.VerbGet, "zz-other:1", nil))
	if id == "" {
		r.skip("get returns the item", "no item: pass --id or create one")
		if opt.Write {
			r.skip("write checks", "no item to write to")
		}
		return r.out
	}
	var it orchapi.WorkItem
	err = must(r.call(orchapi.VerbGet, id, nil, &it))
	if err == nil {
		err = r.checkItem(it, id)
	}
	r.add("get returns the item", err)
	if !opt.Write {
		r.skip("write checks", "read-only run (pass --write to claim, release, comment, set_status and link)")
		return r.out
	}
	r.writes(id)
	return r.out
}

func (r *run) describe() bool {
	var d orchapi.AdapterDescribe
	err := must(r.call(orchapi.VerbDescribe, "", nil, &d))
	if err == nil {
		switch {
		case !adapter.SameMajor(d.Spec):
			err = fmt.Errorf("zm %q, want %s", d.Spec, api.AdapterProtocol)
		case d.Name == "":
			err = fmt.Errorf("empty name")
		case !prefixRe.MatchString(d.Prefix) || d.Prefix == "local" || d.Prefix == "human" || d.Prefix == "machine":
			err = fmt.Errorf("bad or reserved prefix %q", d.Prefix)
		default:
			var missing []string
			for _, v := range orchapi.RequiredVerbs {
				if !slices.Contains(d.Verbs, v) {
					missing = append(missing, v)
				}
			}
			if len(missing) > 0 {
				err = fmt.Errorf("verbs %v lack required %v", d.Verbs, missing)
			}
		}
	}
	r.prefix, r.verbs, r.desc = d.Prefix, d.Verbs, d
	if err == nil && d.Setup != nil && !slices.Contains(d.Verbs, orchapi.VerbSetup) {
		err = fmt.Errorf("describe.setup is set but setup is not among the verbs")
	}
	if !r.add("describe", err) {
		return false
	}
	r.add(CheckEnvName, adapter.CheckEnv(d.Env))
	return true
}

// CheckEnvName is describe.env's check (SPEC 11, 17.2). The proof that an
// adapter gets nothing undeclared is the daemon's (internal/work
// TestAdapterGetsOnlyDeclaredEnv).
const CheckEnvName = "describe.env is valid and zm can honour it"

func (r *run) expectCode(verb, id string, args any, codes ...string) error {
	e, err := r.call(verb, id, args, nil)
	switch {
	case err != nil:
		return err
	case e == nil:
		return fmt.Errorf("answered ok, want %s", strings.Join(codes, " or "))
	case !slices.Contains(codes, e.Code):
		return fmt.Errorf("answered %s (%s), want %s", e.Code, e.Message, strings.Join(codes, " or "))
	}
	return nil
}

func (r *run) expectRefusal(verb, id string, args any) error {
	e, err := r.call(verb, id, args, nil)
	if err != nil {
		return err
	}
	if e == nil {
		return fmt.Errorf("answered ok")
	}
	return nil
}

func (r *run) malformed() error {
	resp, err := r.rt(r.ctx, []byte("this is not json"))
	if err != nil {
		return fmt.Errorf("adapter failed instead of answering: %v", err)
	}
	if resp.OK || resp.Error == nil {
		return fmt.Errorf("answered ok")
	}
	return nil
}

func (r *run) status(id string, want orchapi.WorkStatus) error {
	var it orchapi.WorkItem
	if err := must(r.call(orchapi.VerbGet, id, nil, &it)); err != nil {
		return err
	}
	if it.Status != want {
		return fmt.Errorf("status %q, want %q", it.Status, want)
	}
	return nil
}

func (r *run) writes(id string) {
	claimA := orchapi.AdapterClaimArgs{Holder: HolderA, TTLSeconds: 600, Note: "zm conformance"}
	var it orchapi.WorkItem
	err := must(r.call(orchapi.VerbClaim, id, claimA, &it))
	if err == nil {
		err = r.checkItem(it, id)
	}
	if err == nil && it.Status != orchapi.WorkClaimed {
		err = fmt.Errorf("after claim status is %q, want claimed", it.Status)
	}
	if !r.add("claim", err) {
		r.skip("remaining write checks", "claim failed")
		return
	}
	r.add("claim again by the same holder renews", must(r.call(orchapi.VerbClaim, id, claimA, nil)))
	r.add("claim by another holder answers conflict",
		r.expectCode(orchapi.VerbClaim, id, orchapi.AdapterClaimArgs{Holder: HolderB, TTLSeconds: 600}, api.CodeConflict))
	r.add("comment", must(r.call(orchapi.VerbComment, id, orchapi.AdapterCommentArgs{Body: "zm conformance check", Author: HolderA}, nil)))
	if r.desc.IdempotentComments {
		args := orchapi.AdapterCommentArgs{DeliveryID: ids.New(), Body: "zm durable delivery check", Author: HolderA}
		r.add("idempotent comment first delivery", must(r.call(orchapi.VerbComment, id, args, nil)))
		r.add("idempotent comment replay", must(r.call(orchapi.VerbComment, id, args, nil)))
		args.Body = "changed payload"
		r.add("idempotent comment payload conflict", r.expectCode(orchapi.VerbComment, id, args, api.CodeConflict))
		args.Body = "zm durable delivery check"
		args.Author = HolderB
		r.add("idempotent comment author conflict", r.expectCode(orchapi.VerbComment, id, args, api.CodeConflict))
	}
	r.add("comment with an empty body is refused", r.expectRefusal(orchapi.VerbComment, id, orchapi.AdapterCommentArgs{Author: HolderA}))
	r.add("release by another holder answers conflict",
		r.expectCode(orchapi.VerbRelease, id, orchapi.AdapterReleaseArgs{Holder: HolderB}, api.CodeConflict))
	it = orchapi.WorkItem{} // decode fresh: omitted fields must not linger
	err = must(r.call(orchapi.VerbRelease, id, orchapi.AdapterReleaseArgs{Holder: HolderA}, &it))
	if err == nil && (it.Status != orchapi.WorkOpen || it.Owner != "") {
		err = fmt.Errorf("after release: status %q owner %q, want open and no owner", it.Status, it.Owner)
	}
	r.add("release by the holder reopens the item", err)
	r.add("claim by another holder after release", must(r.call(orchapi.VerbClaim, id, orchapi.AdapterClaimArgs{Holder: HolderB, TTLSeconds: 60}, nil)))
	// In review only after the release checks: what a release does to an item
	// in review is the tracker's (native issues keep in_review, SPEC "Native issues").
	err = must(r.call(orchapi.VerbSetStatus, id, orchapi.AdapterSetStatusArgs{Status: orchapi.WorkInReview}, nil))
	if err == nil {
		err = r.status(id, orchapi.WorkInReview)
	}
	r.add("set_status in_review", err)
	r.add("set_status with a bad status is refused", r.expectRefusal(orchapi.VerbSetStatus, id, map[string]string{"status": "bogus"}))
	if slices.Contains(r.verbs, orchapi.VerbLink) {
		l := orchapi.Link{Rel: "url", Target: "https://example.com/zm-conformance"}
		it = orchapi.WorkItem{}
		err = must(r.call(orchapi.VerbLink, id, l, &it))
		if err == nil && !slices.Contains(it.Links, l) {
			err = fmt.Errorf("links %v lack %v", it.Links, l)
		}
		r.add("link", err)
	} else {
		r.add("link answers not_implemented when not listed", r.expectCode(orchapi.VerbLink, id, orchapi.Link{Rel: "url", Target: "https://example.com"}, api.CodeNotImplemented))
	}
	it = orchapi.WorkItem{}
	err = must(r.call(orchapi.VerbSetStatus, id, orchapi.AdapterSetStatusArgs{Status: orchapi.WorkOpen}, &it))
	if err == nil && (it.Status != orchapi.WorkOpen || it.Owner != "") {
		err = fmt.Errorf("after set_status open: status %q owner %q, want open and no owner", it.Status, it.Owner)
	}
	r.add("set_status open ends the claim (cleanup)", err)
}

// Failed counts failed results.
func Failed(rs []Result) int {
	n := 0
	for _, r := range rs {
		if !r.OK {
			n++
		}
	}
	return n
}

// Format renders results as PASS/FAIL/SKIP lines.
func Format(rs []Result) string {
	var b strings.Builder
	for _, r := range rs {
		tag := "PASS"
		switch {
		case r.Skipped:
			tag = "SKIP"
		case !r.OK:
			tag = "FAIL"
		}
		fmt.Fprintf(&b, "%s  %s", tag, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Detail)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
