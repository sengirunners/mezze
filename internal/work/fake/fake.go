// Package fake is a stand-in work tracker for tests and the bench: a work
// adapter (zm-work-fake) whose issues live in a JSON state file named by
// $ZM_FAKE_STATE. A test binary links itself onto PATH as zm-work-fake
// (Install) and calls Main from TestMain, so the work module runs a real
// adapter process end to end with no tracker of its own. Owned by the work
// builder.
//
// Mapping: closed -> done; in_progress with label "in-review" -> in_review;
// in_progress -> claimed (owner = assignee); everything else -> open. Links
// are kept as "zm: link <rel> <target>" lines in the issue's notes.
//
// Config (sent with every request):
//
//	down     a file: while it exists every call fails as an unreachable tracker
//	hang     non-empty: every call waits until the daemon gives up on it
//	readonly "true": refuse every write verb
//	page     a full listing's page size (default FullPage)
//	partial  "true": every page of a full listing says it may leave items out
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// EnvState names the fake's JSON state file.
const EnvState = "ZM_FAKE_STATE"

// Name is the adapter's name: its executable is zm-work-fake.
const Name = "fake"

// Prefix is the fake's own id prefix (tests bind it under the same one).
const Prefix = "bd"

// ReviewLabel marks an in_progress issue that is in review.
const ReviewLabel = "in-review"

// Main runs the fake adapter when os.Args[0]'s base name is zm-work-fake
// and reports whether it did; TestMain calls os.Exit(code) when handled.
func Main() (code int, handled bool) {
	if filepath.Base(os.Args[0]) != adapter.ExecPrefix+Name {
		return 0, false
	}
	return adapter.Main(Handlers()), true
}

// Install links the running test binary into dir as zm-work-fake and each
// other name in names, and points the fake at a fresh state file there. It
// returns the PATH with dir first and the state file (t.Setenv is left to
// the caller).
func Install(dir string, names ...string) (pathEnv, stateFile string, err error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	for _, n := range append([]string{adapter.ExecPrefix + Name}, names...) {
		if err := os.Symlink(self, filepath.Join(dir, n)); err != nil {
			return "", "", err
		}
	}
	stateFile = filepath.Join(dir, "state.json")
	if err := os.WriteFile(stateFile, []byte("{}"), 0o600); err != nil {
		return "", "", err
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH"), stateFile, nil
}

// State is everything the fake remembers.
type State struct {
	Issues map[string]*Issue `json:"issues,omitempty"`
	// Calls is every call the adapter answered: "<verb> <id> [k=v,...]"
	// (the id and the tracker config, when there are any).
	Calls []string `json:"calls,omitempty"`
	// Env is the environment of the last call, EnvKeys the names of every
	// variable any call saw (environment allowlist tests).
	Env     []string `json:"env,omitempty"`
	EnvKeys []string `json:"env_keys,omitempty"`
	// FailList makes list and changes fail with this message.
	FailList string `json:"fail_list,omitempty"`
}

// Issue is one tracker issue.
type Issue struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Notes       string   `json:"notes,omitempty"`
	Status      string   `json:"status"`
	Assignee    string   `json:"assignee,omitempty"`
	CreatedBy   string   `json:"created_by,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	CommentLog  []string `json:"comment_log,omitempty"`
	IssueType   string   `json:"issue_type,omitempty"`
	Priority    *int     `json:"priority,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	DueAt       string   `json:"due_at,omitempty"`
	DeferUntil  string   `json:"defer_until,omitempty"`
}

// Load reads the state file.
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Issues == nil {
		s.Issues = map[string]*Issue{}
	}
	return &s, nil
}

// Update serializes a test edit with the adapter's calls. Load and Save
// alone are snapshots: an in-flight call also saves call metadata and could
// otherwise overwrite a concurrent test edit.
func Update(path string, fn func(*State)) error {
	defer lock(path)()
	s, err := Load(path)
	if err != nil {
		return err
	}
	fn(s)
	return s.Save(path)
}

// Save writes the state file.
func (s *State) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: a call running at the same time must never read a half-written file.
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Now is an updated_at for a change: RFC3339, one-second resolution.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// statePath is $ZM_FAKE_STATE, else state.json next to the adapter on PATH
// (where Install puts it): adapters the daemon runs get an allowlisted
// environment without ZM_FAKE_STATE, but with PATH.
func statePath() string {
	if p := os.Getenv(EnvState); p != "" {
		return p
	}
	if exe, err := exec.LookPath(filepath.Base(os.Args[0])); err == nil {
		return filepath.Join(filepath.Dir(exe), "state.json")
	}
	return ""
}

// tx runs fn on the state under the file lock, records the call, and
// saves (fn's changes too, even when it fails).
func tx(verb string, req orchapi.AdapterRequest, fn func(s *State) (any, error)) (any, error) {
	path := statePath()
	defer lock(path)()
	s, err := Load(path)
	if err != nil {
		return nil, api.Errorf(api.CodeInternal, "fake: no state: %v", err)
	}
	call := verb
	if req.ID != "" {
		call += " " + req.ID
	}
	if len(req.Config) > 0 {
		kv := make([]string, 0, len(req.Config))
		for k, v := range req.Config {
			kv = append(kv, k+"="+v)
		}
		slices.Sort(kv)
		call += " [" + strings.Join(kv, ",") + "]"
	}
	s.Calls = append(s.Calls, call)
	s.Env = os.Environ()
	for _, kv := range s.Env {
		if k, _, _ := strings.Cut(kv, "="); !slices.Contains(s.EnvKeys, k) {
			s.EnvKeys = append(s.EnvKeys, k)
		}
	}
	out, ferr := fn(s)
	if err := s.Save(path); err != nil {
		return nil, api.Errorf(api.CodeInternal, "fake: save: %v", err)
	}
	return out, ferr
}

// zmStatus maps an issue onto zm's four statuses.
func (is *Issue) zmStatus() orchapi.WorkStatus {
	switch is.Status {
	case "closed":
		return orchapi.WorkDone
	case "in_progress":
		if slices.Contains(is.Labels, ReviewLabel) {
			return orchapi.WorkInReview
		}
		return orchapi.WorkClaimed
	}
	return orchapi.WorkOpen
}

// Item converts an issue to a WorkItem.
func (is *Issue) Item() orchapi.WorkItem {
	it := orchapi.WorkItem{ID: Prefix + ":" + is.ID, Source: Prefix, Title: is.Title, Body: is.Description,
		Status: is.zmStatus(), CreatedBy: is.CreatedBy, Labels: is.Labels, Links: linksIn(is.Notes),
		Extra: map[string]any{"status": is.Status}}
	if is.Status == "in_progress" {
		it.Owner = is.Assignee
	}
	it.Created, _ = time.Parse(time.RFC3339, is.CreatedAt)
	it.Updated, _ = time.Parse(time.RFC3339, is.UpdatedAt)
	for k, v := range map[string]string{"assignee": is.Assignee, "issue_type": is.IssueType,
		"parent": is.Parent, "due_at": is.DueAt, "defer_until": is.DeferUntil} {
		if v != "" {
			it.Extra[k] = v
		}
	}
	if is.Priority != nil {
		it.Extra["priority"] = *is.Priority
	}
	return it
}

const linkMarker = "zm: link "

func linksIn(notes string) []orchapi.Link {
	var out []orchapi.Link
	for _, line := range strings.Split(notes, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), linkMarker); ok {
			if rel, target, ok := strings.Cut(rest, " "); ok && orchapi.ValidLinkRel(rel) && target != "" {
				out = append(out, orchapi.Link{Rel: rel, Target: strings.TrimSpace(target)})
			}
		}
	}
	return out
}

func ref(id string) (string, error) {
	r, ok := strings.CutPrefix(id, Prefix+":")
	if !ok || r == "" || strings.ContainsAny(r, " \t\n") {
		return "", api.Errorf(api.CodeBadRequest, "not a fake id: %q (want %s:<id>)", id, Prefix)
	}
	return r, nil
}

func (s *State) get(id string) (*Issue, error) {
	r, err := ref(id)
	if err != nil {
		return nil, err
	}
	is := s.Issues[r]
	if is == nil {
		return nil, api.Errorf(api.CodeNotFound, "no fake issue %s", r)
	}
	return is, nil
}

func (s *State) sorted() []*Issue {
	ids := make([]string, 0, len(s.Issues))
	for id := range s.Issues {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*Issue, len(ids))
	for i, id := range ids {
		out[i] = s.Issues[id]
	}
	return out
}

func reopen(is *Issue) {
	is.Status, is.Assignee, is.UpdatedAt = "open", "", Now()
	is.Labels = slices.DeleteFunc(is.Labels, func(l string) bool { return l == ReviewLabel })
}

// Handlers are the fake's verbs.
func Handlers() map[string]adapter.Handler {
	h := map[string]adapter.Handler{
		orchapi.VerbList:      guard("list", list, false),
		orchapi.VerbGet:       guard("get", get, false),
		orchapi.VerbClaim:     guard("claim", claim, true),
		orchapi.VerbRelease:   guard("release", release, true),
		orchapi.VerbComment:   guard("comment", comment, true),
		orchapi.VerbSetStatus: guard("set_status", setStatus, true),
		orchapi.VerbLink:      guard("link", link, true),
		orchapi.VerbChanges:   guard("changes", changes, false),
	}
	verbs := append(adapter.Verbs(h), orchapi.VerbDescribe)
	slices.Sort(verbs)
	h[orchapi.VerbDescribe] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		return orchapi.AdapterDescribe{Spec: api.AdapterProtocol, Name: Name, Prefix: Prefix, Version: "0.1.0", Verbs: verbs, Serve: true}, nil
	}
	return h
}

type verbFunc func(s *State, req orchapi.AdapterRequest) (any, error)

// guard runs a verb in a transaction after the config's switches: down,
// hang and readonly.
func guard(verb string, fn verbFunc, write bool) adapter.Handler {
	return func(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
		if req.Config["hang"] != "" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if f := req.Config["down"]; f != "" {
			if _, err := os.Stat(f); err == nil {
				return nil, api.Errorf(api.CodeUnavailable, "fake %s: server unreachable at 10.0.0.9:3307", verb)
			}
		}
		if write && req.Config["readonly"] == "true" {
			return nil, api.Errorf(api.CodeForbidden, "this fake tracker is configured readonly")
		}
		return tx(verb, req, func(s *State) (any, error) { return fn(s, req) })
	}
}

func get(s *State, req orchapi.AdapterRequest) (any, error) {
	is, err := s.get(req.ID)
	if err != nil {
		return nil, err
	}
	return is.Item(), nil
}

func list(s *State, req orchapi.AdapterRequest) (any, error) {
	if s.FailList != "" {
		return nil, api.Errorf(api.CodeInternal, "fake list: %s", s.FailList)
	}
	la, err := adapter.Args[orchapi.AdapterListArgs](req)
	if err != nil {
		return nil, err
	}
	limit := la.Limit
	if limit <= 0 {
		limit = 50
	}
	q := strings.ToLower(la.Q)
	items := []orchapi.WorkItem{}
	for _, is := range s.sorted() {
		it := is.Item()
		switch {
		case la.Status != "" && it.Status != la.Status, la.Status == "" && it.Status == orchapi.WorkDone:
			continue
		case la.Owner != "" && it.Owner != la.Owner:
			continue
		case len(la.LabelsAny) > 0 && !slices.ContainsFunc(la.LabelsAny, func(l string) bool { return slices.Contains(is.Labels, l) }):
			continue
		case q != "" && !strings.Contains(strings.ToLower(is.Title+" "+is.Description), q):
			continue
		}
		if items = append(items, it); len(items) >= limit {
			break
		}
	}
	return orchapi.WorkList{Items: items}, nil
}

func claim(s *State, req orchapi.AdapterRequest) (any, error) {
	ca, err := adapter.Args[orchapi.AdapterClaimArgs](req)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ca.Holder) == "" {
		return nil, api.Errorf(api.CodeBadRequest, "claim needs a holder")
	}
	is, err := s.get(req.ID)
	if err != nil {
		return nil, err
	}
	switch {
	case is.Status == "closed":
		return nil, api.Errorf(api.CodeConflict, "%s is closed", req.ID)
	case is.Assignee != "" && is.Assignee != ca.Holder:
		return nil, api.Errorf(api.CodeConflict, "%s is assigned to %s", req.ID, is.Assignee)
	}
	is.Status, is.Assignee, is.UpdatedAt = "in_progress", ca.Holder, Now()
	return is.Item(), nil
}

// release ends the holder's claim: an in_progress issue assigned to the
// holder (or to nobody) goes back to open with no assignee. Someone else's
// claim is a conflict; an issue not in progress is returned unchanged.
func release(s *State, req orchapi.AdapterRequest) (any, error) {
	ra, err := adapter.Args[orchapi.AdapterReleaseArgs](req)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ra.Holder) == "" {
		return nil, api.Errorf(api.CodeBadRequest, "release needs a holder")
	}
	is, err := s.get(req.ID)
	if err != nil {
		return nil, err
	}
	if is.Status != "in_progress" {
		return is.Item(), nil
	}
	if is.Assignee != "" && is.Assignee != ra.Holder {
		return nil, api.Errorf(api.CodeConflict, "%s is assigned to %s, not %s", req.ID, is.Assignee, ra.Holder)
	}
	reopen(is)
	return is.Item(), nil
}

func comment(s *State, req orchapi.AdapterRequest) (any, error) {
	ca, err := adapter.Args[orchapi.AdapterCommentArgs](req)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ca.Body) == "" {
		return nil, api.Errorf(api.CodeBadRequest, "empty comment")
	}
	is, err := s.get(req.ID)
	if err != nil {
		return nil, err
	}
	is.CommentLog = append(is.CommentLog, ca.Author+": "+ca.Body)
	is.UpdatedAt = Now()
	return struct{}{}, nil
}

func setStatus(s *State, req orchapi.AdapterRequest) (any, error) {
	sa, err := adapter.Args[orchapi.AdapterSetStatusArgs](req)
	if err != nil {
		return nil, err
	}
	is, err := s.get(req.ID)
	if err != nil {
		return nil, err
	}
	noReview := func() { is.Labels = slices.DeleteFunc(is.Labels, func(l string) bool { return l == ReviewLabel }) }
	switch sa.Status {
	case orchapi.WorkDone:
		is.Status = "closed"
	case orchapi.WorkOpen:
		reopen(is)
	case orchapi.WorkClaimed:
		is.Status = "in_progress"
		noReview()
	case orchapi.WorkInReview:
		is.Status = "in_progress"
		if !slices.Contains(is.Labels, ReviewLabel) {
			is.Labels = append(is.Labels, ReviewLabel)
		}
	default:
		return nil, api.Errorf(api.CodeBadRequest, "bad status %q", sa.Status)
	}
	is.UpdatedAt = Now()
	return is.Item(), nil
}

func link(s *State, req orchapi.AdapterRequest) (any, error) {
	l, err := adapter.Args[orchapi.AdapterLinkArgs](req)
	if err != nil {
		return nil, err
	}
	if !orchapi.ValidLinkRel(l.Rel) || l.Target == "" || strings.ContainsAny(l.Target, " \t\n") {
		return nil, api.Errorf(api.CodeBadRequest, "bad link %s %q", l.Rel, l.Target)
	}
	is, err := s.get(req.ID)
	if err != nil {
		return nil, err
	}
	if slices.Contains(linksIn(is.Notes), l) {
		return is.Item(), nil
	}
	if is.Notes != "" {
		is.Notes += "\n"
	}
	is.Notes += linkMarker + l.Rel + " " + l.Target
	is.UpdatedAt = Now()
	return is.Item(), nil
}

// FullPage is how many items one page of a full listing holds.
var FullPage = 2000

// Overlap is how far before the cursor an incremental listing looks again
// (updated_at has one-second resolution).
const Overlap = 5 * time.Minute

// page is the opaque page token of a full listing.
type page struct {
	Start time.Time `json:"t"`
	Off   int       `json:"o"`
}

// changes is a full listing in pages of FullPage (ids in order) or, since
// a cursor, every item updated after it (less Overlap); both report the count.
func changes(s *State, req orchapi.AdapterRequest) (any, error) {
	if s.FailList != "" {
		return nil, api.Errorf(api.CodeInternal, "fake changes: %s", s.FailList)
	}
	ca, err := adapter.Args[orchapi.AdapterChangesArgs](req)
	if err != nil {
		return nil, err
	}
	all := s.sorted()
	count := len(all)
	cursor := func(t time.Time) string { return "u=" + t.UTC().Format(time.RFC3339) }
	if ca.Since != "" && ca.Page == "" {
		v, ok := strings.CutPrefix(ca.Since, "u=")
		t, err := time.Parse(time.RFC3339, v)
		if !ok || err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "bad cursor %q", ca.Since)
		}
		items := []orchapi.WorkItem{}
		cur := t
		for _, is := range all {
			it := is.Item()
			if it.Updated.After(t.Add(-Overlap)) {
				items = append(items, it)
				if it.Updated.After(cur) {
					cur = it.Updated
				}
			}
		}
		if n := time.Now().UTC().Truncate(time.Second); cur.After(n) {
			cur = n
		}
		return orchapi.AdapterChanges{Items: items, Cursor: cursor(cur), Count: &count}, nil
	}
	pg := page{Start: time.Now().UTC().Truncate(time.Second)}
	if ca.Page != "" {
		if json.Unmarshal([]byte(ca.Page), &pg) != nil || pg.Start.IsZero() {
			return nil, api.Errorf(api.CodeBadRequest, "bad page token")
		}
	}
	size := FullPage
	if n, err := strconv.Atoi(req.Config["page"]); err == nil && n > 0 {
		size = n
	}
	end := min(pg.Off+size, len(all))
	items := make([]orchapi.WorkItem, 0, max(end-pg.Off, 0))
	for _, is := range all[min(pg.Off, end):end] {
		items = append(items, is.Item())
	}
	out := orchapi.AdapterChanges{Items: items, Count: &count, Partial: req.Config["partial"] == "true"}
	if end < len(all) {
		b, _ := json.Marshal(page{Start: pg.Start, Off: end})
		out.Next = string(b)
	} else {
		out.Cursor = cursor(pg.Start)
	}
	return out, nil
}
