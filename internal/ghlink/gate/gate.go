// Package gate is the GitHub linker's gate brick (SPEC 18.3): a project's
// own rule for what a pull request may do to the items it names. The linker
// asks a Gate before every write to an item; the gate answers apply, note,
// wait or skip. zm stays generic; a project's policy (deploy evidence,
// owner-only items, reverts) lives in its gate.
//
// One interface (Gate), one driver (Process: an executable speaking JSON on
// stdin/stdout, like section 11's adapters), and the checks every driver must
// pass (Conform). The second driver we expect is an HTTP gate (a POST per
// verb, same JSON) for a policy service that is not on this machine.
package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Decisions.
const (
	Apply = "apply" // do what the linker proposes
	Note  = "note"  // comment (and link) only; move nothing
	Wait  = "wait"  // ask again later; write nothing now
	Skip  = "skip"  // write nothing; do not ask again for this PR state
)

// Verbs.
const (
	VerbDescribe = "describe"
	VerbMatch    = "match"
	VerbDecide   = "decide"
)

// Bounds (SPEC 18.3).
const (
	DefaultTimeout   = 60 * time.Second
	MaxTimeout       = 300 * time.Second
	DefaultRecheck   = 10 * time.Minute
	MinRecheck       = time.Minute
	DefaultRetention = 14 * 24 * time.Hour
	MaxRetention     = 90 * 24 * time.Hour
	MaxNote          = 2000
	MaxIDs           = 10
	MaxOutput        = 1 << 20
)

// PR is the pull request a gate decides on.
type PR struct {
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Branch   string     `json:"branch"`
	URL      string     `json:"url"`
	State    string     `json:"state"` // open | merged | closed
	Draft    bool       `json:"draft"`
	Labels   []string   `json:"labels"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	MergeSHA string     `json:"merge_sha,omitempty"`
	HeadSHA  string     `json:"head_sha,omitempty"`
	Base     string     `json:"base,omitempty"` // the branch it merges into
	Updated  time.Time  `json:"updated"`
}

// Item is a work item as the linker reads it through /v0/work.
type Item struct {
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Status string         `json:"status"`
	Owner  string         `json:"owner,omitempty"`
	Source string         `json:"source"`
	Labels []string       `json:"labels,omitempty"`
	Extra  map[string]any `json:"extra,omitempty"`
	// LastWrite is when the linker last wrote to this item for this PR.
	LastWrite *time.Time `json:"last_write,omitempty"`
}

// Proposal is what the linker would do without a gate (SPEC 18.2).
type Proposal struct {
	To      string `json:"to"` // a zm status, "" = comment (and link) only
	Comment string `json:"comment"`
}

// Request is decide's argument.
type Request struct {
	PR         PR        `json:"pr"`
	Item       Item      `json:"item"`
	Proposed   Proposal  `json:"proposed"`
	FirstAsked time.Time `json:"first_asked"`
	Attempt    int       `json:"attempt"`
	Final      bool      `json:"final"`
}

// Decision is decide's answer.
type Decision struct {
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

// Description is describe's answer.
type Description struct {
	Zm               string   `json:"zm"`
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	Verbs            []string `json:"verbs"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
	RecheckSeconds   int      `json:"recheck_seconds,omitempty"`
	RetentionSeconds int      `json:"retention_seconds,omitempty"`
}

// Has reports whether the gate offers verb.
func (d Description) Has(verb string) bool { return slices.Contains(d.Verbs, verb) }

// Timeout, Recheck and Retention are the bounded values (defaults filled).
func (d Description) Timeout() time.Duration {
	return bound(d.TimeoutSeconds, DefaultTimeout, time.Second, MaxTimeout)
}
func (d Description) Recheck() time.Duration {
	return bound(d.RecheckSeconds, DefaultRecheck, MinRecheck, 7*24*time.Hour)
}
func (d Description) Retention() time.Duration {
	return bound(d.RetentionSeconds, DefaultRetention, time.Hour, MaxRetention)
}

func bound(secs int, def, lo, hi time.Duration) time.Duration {
	if secs <= 0 {
		return def
	}
	return min(max(time.Duration(secs)*time.Second, lo), hi)
}

// ErrNoMatch is Match on a gate without the match verb.
var ErrNoMatch = errors.New("the gate has no match verb")

// Gate is a project's rule for what a PR may do to an item.
type Gate interface {
	Describe(ctx context.Context) (Description, error)
	// Match returns the ids the PR names, or ErrNoMatch.
	Match(ctx context.Context, pr PR) ([]string, error)
	Decide(ctx context.Context, req Request) (Decision, error)
}

// CheckDescription validates a describe answer.
func CheckDescription(d Description) error {
	if d.Zm != "zm/v0" {
		return fmt.Errorf("describe: zm %q, want zm/v0", d.Zm)
	}
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("describe: no name")
	}
	if !d.Has(VerbDescribe) || !d.Has(VerbDecide) {
		return fmt.Errorf("describe: verbs %v must include describe and decide", d.Verbs)
	}
	for _, v := range d.Verbs {
		if v != VerbDescribe && v != VerbDecide && v != VerbMatch {
			return fmt.Errorf("describe: unknown verb %q", v)
		}
	}
	return nil
}

// CheckDecision validates and cleans a decide answer: a known decision and a
// note of plain text, cut to MaxNote bytes.
func CheckDecision(d Decision) (Decision, error) {
	switch d.Decision {
	case Apply, Note, Wait, Skip:
	default:
		return Decision{}, fmt.Errorf("decide: unknown decision %q", d.Decision)
	}
	d.Note = CleanNote(d.Note)
	return d, nil
}

// CleanNote keeps printable text and newlines, at most MaxNote bytes.
func CleanNote(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, strings.ToValidUTF8(s, ""))
	s = strings.TrimSpace(s)
	if len(s) > MaxNote {
		s = s[:MaxNote]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

var idOK = func(s string) bool {
	if s == "" || len(s) > 160 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-' || r == ':'):
		default:
			return false
		}
	}
	return true
}

// CheckIDs validates a match answer: well-formed ids, no duplicates, at most MaxIDs.
func CheckIDs(ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !idOK(id) {
			return nil, fmt.Errorf("match: bad id %q", id)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) > MaxIDs {
		out = out[:MaxIDs]
	}
	return out, nil
}
