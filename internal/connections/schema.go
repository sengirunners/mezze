package connections

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// The show contract's bounds (SPEC 24.2). A provider over one is refused,
// never cut short: a partial summary must not look complete.
const (
	MaxBody          = 256 << 10
	MaxHeadline      = 32
	MaxMeters        = 32
	MaxRunning       = 200
	MaxLinks         = 4
	DefaultStale     = 300 * time.Second
	MinStale         = 30 * time.Second
	MaxStale         = 24 * time.Hour
	MaxAge           = 24 * time.Hour  // older than this as_of: unavailable
	Skew             = 2 * time.Minute // a stamp this far ahead is still now
	maxProblemsNamed = 5
)

// Invalid is a summary that breaks the contract: every problem, by JSON path.
type Invalid struct{ Problems []string }

func (e *Invalid) Error() string {
	p := e.Problems
	more := ""
	if len(p) > maxProblemsNamed {
		more = fmt.Sprintf(" (and %d more)", len(p)-maxProblemsNamed)
		p = p[:maxProblemsNamed]
	}
	return "the summary breaks " + orchapi.ConnectionSchema + ": " + strings.Join(p, "; ") + more
}

// Validate decodes body as a show summary and checks it against the
// contract at now. Unknown fields are ignored (the contract grows
// additively); missing lists are empty. It is the conformance check: a
// provider passes when Validate accepts what it serves.
func Validate(body []byte, now time.Time) (orchapi.ConnectionSummary, error) {
	var s orchapi.ConnectionSummary
	if len(body) > MaxBody {
		return s, &Invalid{[]string{fmt.Sprintf("body over %d KiB", MaxBody>>10)}}
	}
	if !utf8.Valid(body) {
		return s, &Invalid{[]string{"body is not UTF-8"}}
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(&s); err != nil {
		return s, &Invalid{[]string{"not JSON in the summary's shape: " + err.Error()}}
	}
	if d.More() {
		return s, &Invalid{[]string{"more than one JSON value"}}
	}
	v := &checker{now: now}
	v.summary(&s)
	if len(v.problems) > 0 {
		return orchapi.ConnectionSummary{}, &Invalid{v.problems}
	}
	return s, nil
}

// StaleAfter is how long after as_of s counts as fresh.
func StaleAfter(s orchapi.ConnectionSummary) time.Duration {
	if s.StaleAfterS == 0 {
		return DefaultStale
	}
	return time.Duration(s.StaleAfterS) * time.Second
}

type checker struct {
	now      time.Time
	problems []string
}

func (c *checker) bad(path, format string, a ...any) {
	c.problems = append(c.problems, path+": "+fmt.Sprintf(format, a...))
}

// text trims *p and checks it: required or not, at most n runes, no control characters.
func (c *checker) text(path string, p *string, n int, required bool) {
	*p = strings.TrimSpace(*p)
	switch {
	case *p == "" && required:
		c.bad(path, "required")
	case utf8.RuneCountInString(*p) > n:
		c.bad(path, "longer than %d characters", n)
	case strings.IndexFunc(*p, unicode.IsControl) >= 0:
		c.bad(path, "contains a control character")
	}
}

func (c *checker) key(path string, k *string, seen map[string]bool, n int) {
	c.text(path, k, n, true)
	if *k == "" {
		return
	}
	if seen[*k] {
		c.bad(path, "%q repeats", *k)
	}
	seen[*k] = true
}

func (c *checker) when(path string, t *time.Time) {
	if t == nil {
		return
	}
	if t.IsZero() {
		c.bad(path, "not a time")
	} else if t.After(c.now.Add(Skew)) {
		c.bad(path, "in the future")
	}
}

func (c *checker) number(path string, f float64, positive bool) {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		c.bad(path, "not a number")
	case positive && f <= 0:
		c.bad(path, "must be above 0")
	case f < 0:
		c.bad(path, "must not be negative")
	}
}

func (c *checker) summary(s *orchapi.ConnectionSummary) {
	if s.Schema != orchapi.ConnectionSchema {
		c.bad("schema", "want %q, got %q", orchapi.ConnectionSchema, s.Schema)
	}
	c.text("title", &s.Title, 80, false)
	if s.AsOf.IsZero() {
		c.bad("as_of", "required (when these numbers were true, RFC 3339)")
	} else {
		c.when("as_of", &s.AsOf)
	}
	if s.StaleAfterS != 0 {
		if d := time.Duration(s.StaleAfterS) * time.Second; d < MinStale || d > MaxStale {
			c.bad("stale_after_s", "want %d to %d", int(MinStale.Seconds()), int(MaxStale.Seconds()))
		}
	}
	if s.Headline == nil {
		s.Headline = []orchapi.ConnectionRow{}
	}
	if s.Meters == nil {
		s.Meters = []orchapi.ConnectionMeter{}
	}
	if s.Running == nil {
		s.Running = []orchapi.ConnectionRunning{}
	}
	if len(s.Headline) > MaxHeadline {
		c.bad("headline", "more than %d rows", MaxHeadline)
		return
	}
	if len(s.Meters) > MaxMeters {
		c.bad("meters", "more than %d meters", MaxMeters)
		return
	}
	if len(s.Running) > MaxRunning {
		c.bad("running", "more than %d items", MaxRunning)
		return
	}
	seen := map[string]bool{}
	for i := range s.Headline {
		r, p := &s.Headline[i], fmt.Sprintf("headline[%d]", i)
		c.key(p+".key", &r.Key, seen, 64)
		c.text(p+".label", &r.Label, 80, true)
		switch r.State {
		case orchapi.ConnectionOK, orchapi.ConnectionWarn, orchapi.ConnectionRed:
		default:
			c.bad(p+".state", "want ok, warn or red, got %q", r.State)
		}
		if r.Count != nil && *r.Count < 0 {
			c.bad(p+".count", "must not be negative")
		}
		c.when(p+".since", r.Since)
		c.text(p+".detail", &r.Detail, 200, false)
		c.text(p+".account", &r.Account, 80, true)
	}
	seen = map[string]bool{}
	for i := range s.Meters {
		m, p := &s.Meters[i], fmt.Sprintf("meters[%d]", i)
		c.key(p+".key", &m.Key, seen, 64)
		c.text(p+".label", &m.Label, 80, true)
		c.number(p+".used", m.Used, false)
		c.number(p+".reserved", m.Reserved, false)
		c.number(p+".total", m.Total, true)
		c.text(p+".unit", &m.Unit, 16, true)
		c.text(p+".account", &m.Account, 80, true)
	}
	seen = map[string]bool{}
	for i := range s.Running {
		r, p := &s.Running[i], fmt.Sprintf("running[%d]", i)
		c.key(p+".id", &r.ID, seen, 128)
		c.text(p+".title", &r.Title, 200, true)
		c.text(p+".owner", &r.Owner, 80, false)
		c.when(p+".started", r.Started)
		if len(r.Links) > MaxLinks {
			c.bad(p+".links", "more than %d links", MaxLinks)
		}
		for j := range r.Links {
			l, lp := &r.Links[j], fmt.Sprintf("%s.links[%d]", p, j)
			c.text(lp+".label", &l.Label, 40, true)
			c.link(lp+".url", &l.URL)
		}
		c.text(p+".account", &r.Account, 80, true)
	}
}

// link allows only absolute http(s) URLs: a client renders them as links.
func (c *checker) link(path string, raw *string) {
	*raw = strings.TrimSpace(*raw)
	if len(*raw) > 2048 {
		c.bad(path, "longer than 2048 bytes")
		return
	}
	u, err := url.Parse(*raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		c.bad(path, "want an absolute http(s) URL without credentials")
	}
}
