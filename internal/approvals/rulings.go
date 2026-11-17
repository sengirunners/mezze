package approvals

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// Owner rulings (SPEC 16 "Card judge"): standing answers the project's
// owners gave, kept as data in the project's own project.toml, never in
// code:
//
//	[[rulings]]
//	name = "merge-after-review"
//	text = "An agent merges its own pull request once ..."
//
// The card judge reads them and sends back a card whose question one of
// them already answers, naming it. Changing them is an owner decision: an
// agent proposes a change as a card, and only after an owner's yes is the
// file changed: zm project ruling add|rm (rulings_routes.go) at the
// authority, which takes effect at once, or by hand, read when the project
// loads.

const rulingsSectionName = "rulings"

func init() { project.RegisterSection(rulingsSectionName, rulingsSection{}) }

// Ruling is one standing owner answer (Ask: the approved ask that added
// it, when zm project ruling add did).
type Ruling = orchapi.Ruling

const (
	maxRulings    = 64
	maxRulingText = 1000
)

var (
	rulingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	askIDRe      = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`) // a ULID
)

type rulingsSection struct{}

func (rulingsSection) Parse(where string, raw any) (any, error) {
	tables, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: rulings must be [[rulings]] tables", where)
	}
	var out []Ruling
	for i, t := range tables {
		var r Ruling
		for k, v := range t {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s [[rulings]] %d: %s must be a string", where, i+1, k)
			}
			switch k {
			case "name":
				r.Name = s
			case "text":
				r.Text = s
			case "ask":
				r.Ask = s
			default:
				return nil, fmt.Errorf("%s [[rulings]] %d: unknown key %s (name, text, ask)", where, i+1, k)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (rulingsSection) Validate(v any, _ project.File) error {
	rs, _ := v.([]Ruling)
	if len(rs) > maxRulings {
		return api.Errorf(api.CodeBadRequest, "[[rulings]]: at most %d", maxRulings)
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if !rulingNameRe.MatchString(r.Name) {
			return api.Errorf(api.CodeBadRequest, "[[rulings]] name %q: 1 to 64 lower-case letters, digits and dashes", r.Name)
		}
		if seen[r.Name] {
			return api.Errorf(api.CodeBadRequest, "[[rulings]] name %q: named twice", r.Name)
		}
		seen[r.Name] = true
		t := strings.TrimSpace(r.Text)
		if t == "" || len(t) > maxRulingText || !utf8.ValidString(t) || strings.ContainsRune(t, 0) {
			return api.Errorf(api.CodeBadRequest, "[[rulings]] %s: text is 1 to %d bytes", r.Name, maxRulingText)
		}
		if r.Ask != "" && !askIDRe.MatchString(r.Ask) {
			return api.Errorf(api.CodeBadRequest, "[[rulings]] %s: ask %q is not an ask id", r.Name, r.Ask)
		}
	}
	return nil
}

func (rulingsSection) Encode(v any) string {
	rs, _ := v.([]Ruling)
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "\n[[rulings]]\nname = %s\ntext = %s\n", project.TOMLString(r.Name), project.TOMLString(r.Text))
		if r.Ask != "" {
			fmt.Fprintf(&b, "ask = %s\n", project.TOMLString(r.Ask))
		}
	}
	return b.String()
}

// rulings is project id's owner rulings (none when it has no section).
func (s *Service) rulings(id string) []Ruling {
	if s.d.Projects == nil {
		return nil
	}
	rs, _ := s.d.Projects.Section(id, rulingsSectionName).([]Ruling)
	return rs
}
