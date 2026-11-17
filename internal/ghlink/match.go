package ghlink

import (
	"regexp"
	"slices"
	"strings"
)

// MaxIDs bounds how many work ids one PR may name (each is looked up).
const MaxIDs = 10

var explicitRe = regexp.MustCompile(`\b([a-z][a-z0-9-]{0,31}):([A-Za-z0-9][A-Za-z0-9._-]{0,127})`)

// Matcher finds work ids in branch names, PR titles and bodies.
type Matcher struct {
	sources []string
	bare    []*regexp.Regexp
}

// NewMatcher recognises "<source>:<ref>" for every source in sources (and
// local), and bare ids "<prefix>-<ref>" for every prefix, which it looks up
// as "<source>:<prefix>-<ref>" in each source.
func NewMatcher(prefixes, sources []string) *Matcher {
	m := &Matcher{sources: sources}
	for _, p := range prefixes {
		if p = strings.TrimSpace(p); p != "" {
			m.bare = append(m.bare, regexp.MustCompile(`(?i)\b(`+regexp.QuoteMeta(p)+`-[a-z0-9]+(?:\.[0-9]+)*)\b`))
		}
	}
	return m
}

// IDs returns the candidate work ids named in texts, explicit ones first,
// without duplicates, at most MaxIDs.
func (m *Matcher) IDs(texts ...string) []string {
	var out []string
	add := func(id string) {
		if len(out) < MaxIDs && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	allowed := func(src string) bool { return src == "local" || slices.Contains(m.sources, src) }
	for _, t := range texts {
		for _, sm := range explicitRe.FindAllStringSubmatch(t, -1) {
			ref := strings.TrimRight(sm[2], ".-_")
			if allowed(sm[1]) && ref != "" {
				add(sm[1] + ":" + ref)
			}
		}
	}
	for _, t := range texts {
		for _, re := range m.bare {
			for _, sm := range re.FindAllStringSubmatch(t, -1) {
				for _, src := range m.sources {
					add(src + ":" + strings.ToLower(sm[1]))
				}
			}
		}
	}
	return out
}
