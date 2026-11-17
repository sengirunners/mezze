package main

// bd's JSON shapes (beads v1.0.5, types.Issue and friends), field for field
// and in bd's order, so `bd ... --json` through zm prints what bd printed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api/textclean"
)

type bdDep struct {
	IssueID     string    `json:"issue_id"`
	DependsOnID string    `json:"depends_on_id"`
	Type        string    `json:"type"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	Metadata    string    `json:"metadata,omitempty"`
	ThreadID    string    `json:"thread_id,omitempty"`
}

type bdComment struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// bdIssue is types.Issue.
type bdIssue struct {
	ID                 string          `json:"id"`
	MovedFrom          string          `json:"moved_from,omitempty"`
	Title              string          `json:"title"`
	Description        string          `json:"description,omitempty"`
	Design             string          `json:"design,omitempty"`
	AcceptanceCriteria string          `json:"acceptance_criteria,omitempty"`
	Notes              string          `json:"notes,omitempty"`
	SpecID             string          `json:"spec_id,omitempty"`
	Status             string          `json:"status,omitempty"`
	Priority           int             `json:"priority"`
	IssueType          string          `json:"issue_type,omitempty"`
	Assignee           string          `json:"assignee,omitempty"`
	Owner              string          `json:"owner,omitempty"`
	EstimatedMinutes   *int            `json:"estimated_minutes,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	CreatedBy          string          `json:"created_by,omitempty"`
	UpdatedAt          time.Time       `json:"updated_at"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	ClosedAt           *time.Time      `json:"closed_at,omitempty"`
	CloseReason        string          `json:"close_reason,omitempty"`
	ClosedBySession    string          `json:"closed_by_session,omitempty"`
	DueAt              *time.Time      `json:"due_at,omitempty"`
	DeferUntil         *time.Time      `json:"defer_until,omitempty"`
	ExternalRef        *string         `json:"external_ref,omitempty"`
	SourceSystem       string          `json:"source_system,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	CompactionLevel    int             `json:"compaction_level,omitempty"`
	CompactedAt        *time.Time      `json:"compacted_at,omitempty"`
	CompactedAtCommit  *string         `json:"compacted_at_commit,omitempty"`
	OriginalSize       int             `json:"original_size,omitempty"`
	Labels             []string        `json:"labels,omitempty"`
	Dependencies       []*bdDep        `json:"dependencies,omitempty"`
	Comments           []*bdComment    `json:"comments,omitempty"`
	Sender             string          `json:"sender,omitempty"`
	Ephemeral          bool            `json:"ephemeral,omitempty"`
	NoHistory          bool            `json:"no_history,omitempty"`
	WispType           string          `json:"wisp_type,omitempty"`
	Pinned             bool            `json:"pinned,omitempty"`
	IsTemplate         bool            `json:"is_template,omitempty"`
	AwaitType          string          `json:"await_type,omitempty"`
	AwaitID            string          `json:"await_id,omitempty"`
	Timeout            int64           `json:"timeout,omitempty"`
	Waiters            []string        `json:"waiters,omitempty"`
	MolType            string          `json:"mol_type,omitempty"`
	WorkType           string          `json:"work_type,omitempty"`
	EventKind          string          `json:"event_kind,omitempty"`
	Actor              string          `json:"actor,omitempty"`
	Target             string          `json:"target,omitempty"`
	Payload            string          `json:"payload,omitempty"`
	// Number is zm's per-project issue number: an extra key after every bd
	// key, so bd's own keys keep their order.
	Number int `json:"number,omitempty"`
	// ClaimedBy is the zm address that claimed it (a session: who holds it,
	// whatever the assignee label says), the second extra key.
	ClaimedBy string `json:"claimed_by,omitempty"`
	// Attachments are the item's images, metadata only (show; zm issues
	// attachments get serves the bytes), the third extra key.
	Attachments []orchapi.IssueAttachment `json:"attachments,omitempty"`
	// LeaseFence is the lease fence update --claim --json took (zm: pass it
	// back as --fence on writes under the claim), the fourth extra key.
	LeaseFence int64 `json:"lease_fence,omitempty"`
}

// bdWithCounts is types.IssueWithCounts (list, ready, search, children).
type bdWithCounts struct {
	*bdIssue
	DependencyCount int     `json:"dependency_count"`
	DependentCount  int     `json:"dependent_count"`
	CommentCount    int     `json:"comment_count"`
	Parent          *string `json:"parent,omitempty"`
	// Score is zm's relevance in a ranked search (bd search; higher is
	// better): an extra key after bd's.
	Score float64 `json:"score,omitempty"`
}

// bdWithType is types.IssueWithDependencyMetadata.
type bdWithType struct {
	bdIssue
	DependencyType string `json:"dependency_type"`
}

// bdDetails is types.IssueDetails (show): its own labels, dependencies and
// comments shadow the issue's, so they come after every issue field.
type bdDetails struct {
	bdIssue
	Labels          []string      `json:"labels,omitempty"`
	Dependencies    []*bdWithType `json:"dependencies,omitempty"`
	Dependents      []*bdWithType `json:"dependents,omitempty"`
	Comments        []*bdComment  `json:"comments,omitempty"`
	Parent          *string       `json:"parent,omitempty"`
	DependentCount  *int64        `json:"dependent_count,omitempty"`
	DependencyCount *int64        `json:"dependency_count,omitempty"`
	CommentCount    *int64        `json:"comment_count,omitempty"`
}

// sec is a stored time as bd reads it back from Dolt: UTC seconds.
func sec(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

func secp(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := sec(*t)
	return &v
}

func toBD(it orchapi.Issue, precise bool) *bdIssue {
	b := &bdIssue{ID: it.ID, Title: it.Title, Description: it.Description, Design: it.Design, AcceptanceCriteria: it.AcceptanceCriteria,
		Notes: it.Notes, SpecID: it.SpecID, Status: it.Status, Priority: it.Priority, IssueType: it.IssueType, Assignee: it.Assignee,
		Owner: it.Owner, EstimatedMinutes: it.EstimatedMinutes, CreatedAt: sec(it.CreatedAt), CreatedBy: it.CreatedBy,
		UpdatedAt: sec(it.UpdatedAt), StartedAt: secp(it.StartedAt), ClosedAt: secp(it.ClosedAt), CloseReason: it.CloseReason,
		ClosedBySession: it.ClosedBySession, DueAt: secp(it.DueAt), DeferUntil: secp(it.DeferUntil), SourceSystem: it.SourceSystem,
		Metadata: textclean.JSON(it.Metadata), Labels: it.Labels, Pinned: it.Pinned, IsTemplate: it.IsTemplate, Ephemeral: it.Ephemeral,
		Number: it.Number, ClaimedBy: it.ClaimedBy, Attachments: it.Attachments}
	if precise {
		b.CreatedAt, b.UpdatedAt = it.CreatedAt.UTC(), it.UpdatedAt.UTC()
	}
	if it.ExternalRef != "" {
		ref := it.ExternalRef
		b.ExternalRef = &ref
	}
	if string(b.Metadata) == "{}" || string(b.Metadata) == "null" {
		b.Metadata = nil
	}
	// Columns the importer kept verbatim.
	x := it.Extra
	es := func(k string) string {
		switch v := x[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case nil:
			return ""
		}
		return fmt.Sprint(x[k])
	}
	en := func(k string) int64 { n, _ := strconv.ParseInt(es(k), 10, 64); return n }
	if len(x) > 0 {
		b.Sender, b.WispType, b.AwaitType, b.AwaitID = es("sender"), es("wisp_type"), es("await_type"), es("await_id")
		b.MolType, b.EventKind, b.Actor, b.Target, b.Payload = es("mol_type"), es("event_kind"), es("actor"), es("target"), es("payload")
		b.WorkType = es("work_type")
		b.CompactionLevel, b.OriginalSize, b.Timeout = int(en("compaction_level")), int(en("original_size")), en("timeout_ns")
		b.NoHistory = en("no_history") != 0
		if w := es("waiters"); w != "" {
			b.Waiters = strings.Split(w, ",")
		}
		if c := es("compacted_at"); c != "" {
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", c, time.UTC); err == nil {
				b.CompactedAt = &t
			}
		}
		if c := es("compacted_at_commit"); c != "" {
			b.CompactedAtCommit = &c
		}
	}
	return b
}

func toBDDeps(ds []orchapi.IssueDep) []*bdDep {
	var out []*bdDep
	for _, d := range ds {
		m := string(textclean.JSON([]byte(d.Metadata)))
		if m == "" {
			m = "{}"
		}
		out = append(out, &bdDep{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type, CreatedAt: sec(d.CreatedAt),
			CreatedBy: d.CreatedBy, Metadata: m, ThreadID: d.ThreadID})
	}
	return out
}

// withCounts is list/ready/children's element; search drops deps and parent.
func withCounts(it orchapi.Issue, deps bool) *bdWithCounts {
	b := toBD(it, false)
	if deps {
		b.Dependencies = toBDDeps(it.Dependencies)
	}
	w := &bdWithCounts{bdIssue: b, DependencyCount: it.DependencyCount, DependentCount: it.DependentCount, CommentCount: it.CommentCount, Score: it.Score}
	if deps && it.Parent != "" {
		p := it.Parent
		w.Parent = &p
	}
	return w
}

func toBDComment(c orchapi.IssueComment, precise bool) *bdComment {
	t := sec(c.CreatedAt)
	if precise {
		t = c.CreatedAt.UTC()
	}
	return &bdComment{ID: c.ID, IssueID: c.IssueID, Author: c.Author, Text: c.Text, CreatedAt: t}
}

func details(d orchapi.IssueDetail, withDependents, withComments bool) *bdDetails {
	b := toBD(d.Issue, false)
	b.Labels = nil
	out := &bdDetails{bdIssue: *b, Labels: d.Labels}
	out.MovedFrom = d.MovedFrom
	for _, l := range d.DependsOn {
		out.Dependencies = append(out.Dependencies, &bdWithType{bdIssue: *toBD(l.Issue, false), DependencyType: l.DependencyType})
		if l.DependencyType == orchapi.DepParentChild && out.Parent == nil {
			p := l.ID
			out.Parent = &p
		}
	}
	if withDependents {
		for _, l := range d.Dependents {
			// bd's dependents are shallow: id, title, status, priority, type, zero times.
			out.Dependents = append(out.Dependents, &bdWithType{bdIssue: bdIssue{ID: l.ID, Title: l.Title, Status: l.Status,
				Priority: l.Priority, IssueType: l.IssueType}, DependencyType: l.DependencyType})
		}
	}
	if withComments {
		for _, c := range d.Comments {
			out.Comments = append(out.Comments, toBDComment(c, false))
		}
	}
	nDeps, nDependents, nComments := int64(len(d.DependsOn)), int64(len(d.Dependents)), int64(len(d.Comments))
	out.DependencyCount, out.DependentCount, out.CommentCount = &nDeps, &nDependents, &nComments
	return out
}

// writeJSON is bd's outputJSON: arrays as they are; objects with their keys
// sorted and schema_version 1 added. Two-space indent, trailing newline.
func writeJSON(w io.Writer, v any, object bool) error {
	if object {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		m["schema_version"] = 1
		v = m
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(textclean.JSONStrings(buf.Bytes()))
	return err
}

// ---- human output ----

func statusIcon(s string) string {
	switch s {
	case orchapi.IssueOpen:
		return "○"
	case orchapi.IssueInProgress:
		return "◐"
	case orchapi.IssueBlocked:
		return "●"
	case orchapi.IssueClosed:
		return "✓"
	case orchapi.IssueDeferred:
		return "❄"
	case orchapi.IssuePinned:
		return "📌"
	}
	return "◇"
}

func typeTag(t string) string {
	switch t {
	case "epic":
		return "[epic] "
	case "bug":
		return "[bug] "
	}
	return ""
}

func title255(t string) string {
	if r := []rune(t); len(r) > 255 {
		return string(r[:254]) + "…"
	}
	return t
}

const statusLegend = "Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred"

func listLine(it orchapi.Issue) string {
	it.Title = textclean.Clean(it.Title)
	it.IssueType = textclean.Clean(it.IssueType)
	if it.Status == orchapi.IssueClosed {
		return fmt.Sprintf("✓ %s ● P%d %s %s", it.ID, it.Priority, it.IssueType, it.Title)
	}
	return fmt.Sprintf("%s %s ● P%d %s%s", statusIcon(it.Status), it.ID, it.Priority, typeTag(it.IssueType), it.Title)
}

// printTree is bd list's default view: roots by priority then id, children
// (by parent) under them.
func printTree(w io.Writer, its []orchapi.Issue) {
	byID := map[string]bool{}
	kids := map[string][]orchapi.Issue{}
	for _, it := range its {
		byID[it.ID] = true
	}
	var roots []orchapi.Issue
	for _, it := range its {
		if it.Parent != "" && byID[it.Parent] {
			kids[it.Parent] = append(kids[it.Parent], it)
		} else {
			roots = append(roots, it)
		}
	}
	sortTree(roots)
	var walk func(it orchapi.Issue, prefix string, last, root bool)
	walk = func(it orchapi.Issue, prefix string, last, root bool) {
		branch, next := "", ""
		if !root {
			branch, next = "├── ", "│   "
			if last {
				branch, next = "└── ", "    "
			}
		}
		fmt.Fprintln(w, prefix+branch+listLine(it))
		cs := kids[it.ID]
		sortTree(cs)
		for i, c := range cs {
			walk(c, prefix+next, i == len(cs)-1, false)
		}
	}
	for _, r := range roots {
		walk(r, "", true, true)
	}
}

func sortTree(its []orchapi.Issue) {
	for i := 1; i < len(its); i++ {
		for j := i; j > 0 && treeLess(its[j], its[j-1]); j-- {
			its[j], its[j-1] = its[j-1], its[j]
		}
	}
}

func treeLess(a, b orchapi.Issue) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return naturalLess(a.ID, b.ID)
}

// naturalLess compares ids with their numbers as numbers (x.2 < x.10).
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		ad, bd := digits(a), digits(b)
		if ad > 0 && bd > 0 {
			na, _ := strconv.Atoi(a[:ad])
			nb, _ := strconv.Atoi(b[:bd])
			if na != nb {
				return na < nb
			}
			a, b = a[ad:], b[bd:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

func flatLine(it orchapi.Issue) string {
	icon := statusIcon(it.Status)
	if it.Status == orchapi.IssueOpen && it.Blocked {
		icon = "●"
	}
	var b strings.Builder
	if it.Status == orchapi.IssueClosed {
		fmt.Fprintf(&b, "✓ %s [P%d] [%s]", it.ID, it.Priority, it.IssueType)
	} else {
		fmt.Fprintf(&b, "%s %s [● P%d] [%s]", icon, it.ID, it.Priority, it.IssueType)
	}
	if it.Assignee != "" {
		fmt.Fprintf(&b, " @%s", it.Assignee)
	}
	if len(it.Labels) > 0 {
		fmt.Fprintf(&b, " [%s]", strings.Join(it.Labels, " "))
	}
	fmt.Fprintf(&b, " - %s", it.Title)
	var rel []string
	if it.Parent != "" {
		rel = append(rel, "parent: "+it.Parent)
	}
	if len(rel) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(rel, ", "))
	}
	return textclean.Clean(b.String())
}

func dateOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// printShow is bd show's human layout.
func printShow(w io.Writer, d orchapi.IssueDetail) {
	tag := ""
	switch d.IssueType {
	case "epic":
		tag = " [EPIC]"
	case "bug":
		tag = " [BUG]"
	}
	fmt.Fprintf(w, "%s %s%s · %s   [● P%d · %s]\n", statusIcon(d.Status), d.ID, tag, d.Title, d.Priority, strings.ToUpper(d.Status))
	var meta []string
	if d.CreatedBy != "" {
		meta = append(meta, "Owner: "+d.CreatedBy)
	}
	if d.Assignee != "" {
		meta = append(meta, "Assignee: "+d.Assignee)
	}
	meta = append(meta, "Type: "+d.IssueType)
	fmt.Fprintln(w, strings.Join(meta, " · "))
	dates := []string{"Created: " + dateOf(d.CreatedAt)}
	if d.StartedAt != nil {
		dates = append(dates, "Started: "+dateOf(*d.StartedAt))
	}
	dates = append(dates, "Updated: "+dateOf(d.UpdatedAt))
	if d.DueAt != nil {
		dates = append(dates, "Due: "+dateOf(*d.DueAt))
	}
	if d.DeferUntil != nil {
		dates = append(dates, "Deferred: "+dateOf(*d.DeferUntil))
	}
	fmt.Fprintln(w, strings.Join(dates, " · "))
	if d.Status == orchapi.IssueClosed && d.CloseReason != "" {
		fmt.Fprintln(w, "Close reason: "+d.CloseReason)
	}
	if d.ExternalRef != "" {
		fmt.Fprintln(w, "External: "+d.ExternalRef)
	}
	if d.SpecID != "" {
		fmt.Fprintln(w, "Spec: "+d.SpecID)
	}
	fmt.Fprintln(w, "\nDESCRIPTION")
	if d.Description == "" {
		fmt.Fprintln(w, "  (none)")
	} else {
		fmt.Fprintln(w, d.Description)
	}
	for _, s := range []struct{ h, v string }{{"DESIGN", d.Design}, {"NOTES", d.Notes}, {"ACCEPTANCE CRITERIA", d.AcceptanceCriteria}} {
		if s.v != "" {
			fmt.Fprintf(w, "\n%s\n%s\n", s.h, s.v)
		}
	}
	if len(d.Labels) > 0 {
		fmt.Fprintf(w, "\nLABELS: %s\n", strings.Join(d.Labels, ", "))
	}
	if len(d.Attachments) > 0 {
		fmt.Fprintln(w, "\nATTACHMENTS (zm issues attachments get "+d.ID+" <id> --out <file>)")
		for _, a := range d.Attachments {
			fmt.Fprintf(w, "  %s %s %d bytes, expires %s\n", a.ID, a.Type, a.Size, dateOf(a.Expires))
		}
	}
	depLine := func(prefix string, it orchapi.Issue) string {
		tag := ""
		switch it.IssueType {
		case "epic":
			tag = "(EPIC) "
		case "bug":
			tag = "(BUG) "
		}
		return fmt.Sprintf("  %s %s %s: %s%s ● P%d", prefix, statusIcon(it.Status), it.ID, tag, it.Title, it.Priority)
	}
	section := func(h, prefix string, ls []orchapi.IssueLink, match func(string) bool) {
		var lines []string
		seen := map[string]bool{}
		for _, l := range ls {
			if match(l.DependencyType) && !seen[l.ID] {
				seen[l.ID] = true
				lines = append(lines, depLine(prefix, l.Issue))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(w, "\n%s\n%s\n", h, strings.Join(lines, "\n"))
		}
	}
	is := func(ts ...string) func(string) bool {
		return func(t string) bool {
			for _, x := range ts {
				if x == t {
					return true
				}
			}
			return false
		}
	}
	known := is(orchapi.DepParentChild, orchapi.DepDiscoveredFrom, orchapi.DepRelated, orchapi.DepRelatesTo)
	section("PARENT", "↑", d.DependsOn, is(orchapi.DepParentChild))
	section("DEPENDS ON", "→", d.DependsOn, func(t string) bool { return !known(t) })
	section("DISCOVERED FROM", "◊", d.DependsOn, is(orchapi.DepDiscoveredFrom))
	section("CHILDREN", "↳", d.Dependents, is(orchapi.DepParentChild))
	section("BLOCKS", "←", d.Dependents, is(orchapi.DepBlocks, orchapi.DepConditionalBlocks))
	section("DISCOVERED", "◊", d.Dependents, is(orchapi.DepDiscoveredFrom))
	section("RELATED", "↔", append(append([]orchapi.IssueLink{}, d.DependsOn...), d.Dependents...), is(orchapi.DepRelated, orchapi.DepRelatesTo))
	if len(d.Comments) > 0 {
		fmt.Fprintln(w, "\nCOMMENTS")
		for _, c := range d.Comments {
			fmt.Fprintf(w, "  %s %s\n", c.CreatedAt.UTC().Format("2006-01-02 15:04"), c.Author)
			for _, line := range strings.Split(c.Text, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
	fmt.Fprintln(w)
}
