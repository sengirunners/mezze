package summaries

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/core"
)

// Bindings are archive-private, fixed-size provenance for rendered title lines.
// They keep neither a second plaintext title nor an unbounded source identifier.
// API reads decode only ProjectSummary and never expose these implementation fields.
type titleBinding struct {
	Line      int      `json:"line"`
	Subjects  []string `json:"subjects"`
	Rendering string   `json:"rendering"`
}

type archivedSummary struct {
	orchapi.ProjectSummary
	Bindings []titleBinding `json:"title_bindings,omitempty"`
}

func archiveHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func bindTitle(line int, id, rendering string) titleBinding {
	hashes := []string{archiveHash(id)}
	if _, ref, ok := strings.Cut(id, ":"); ok && ref != "" {
		hashes = append(hashes, archiveHash(ref))
	}
	return titleBinding{Line: line, Subjects: hashes, Rendering: archiveHash(rendering)}
}

func scrubArchived(a *archivedSummary, sc core.Scrub) {
	subjects := map[string]bool{}
	for _, subject := range sc.Subjects {
		if subject != "" {
			subjects[archiveHash(subject)] = true
		}
	}
	lines := strings.Split(a.Body, "\n")
	valid := make([]bool, len(a.Bindings))
	for b, binding := range a.Bindings {
		if binding.Line < 0 || binding.Line >= len(lines) || archiveHash(lines[binding.Line]) != binding.Rendering {
			continue
		}
		valid[b] = true
		matched := false
		for _, subject := range binding.Subjects {
			if subjects[subject] {
				matched = true
				break
			}
		}
		if matched {
			// The full title is the captured unit. Removing it also covers a
			// redacted fragment clipped by the display's 200-rune boundary.
			if id, _, ok := strings.Cut(lines[binding.Line], " — "); ok && strings.HasPrefix(id, "• ") {
				lines[binding.Line] = id + " — " + core.Redacted
			}
		}
	}
	// Old retained rows have no private bindings. Match their subject line,
	// never lower the short-text guard for the entire summary. Exact display
	// variants also cover old sanitation/truncation without copying source text.
	if len(a.Bindings) == 0 {
		for i, line := range lines {
			id, title, ok := strings.Cut(line, " — ")
			if !ok || !strings.HasPrefix(id, "• ") {
				continue
			}
			id = strings.TrimPrefix(id, "• ")
			for _, subject := range sc.Subjects {
				if subject == "" {
					continue
				}
				if id != plain(subject, 100) && !(utf8.RuneCountInString(subject) <= 100 && strings.HasSuffix(id, ":"+subject)) {
					continue
				}
				for _, text := range sc.Texts {
					if text != "" && (strings.Contains(title, text) || strings.Contains(title, plain(text, 200))) {
						lines[i] = "• " + id + " — " + core.Redacted
						break
					}
				}
			}
		}
	}
	var variants []string
	var prefixes []string
	for _, text := range sc.Texts {
		if text == "" {
			continue
		}
		rendered := plain(text, 200)
		variants = append(variants, text, rendered)
		if utf8.RuneCountInString(text) > 200 {
			rendered = strings.TrimSuffix(rendered, "…")
		}
		prefixes = append(prefixes, rendered)
	}
	// Unbound long matches may occur in another subject's captured title.
	// Short text remains untouched unless the exact subject line was bound.
	for i, line := range lines {
		lines[i] = core.ScrubText(line, variants, core.MinUnscoped)
		// A copied redacted fragment can itself meet the title's display
		// boundary. Remove a matching clipped suffix, retaining the same
		// minimum for unbound text as ordinary redaction.
		if id, title, ok := strings.Cut(lines[i], " — "); ok && strings.HasPrefix(id, "• ") && strings.HasSuffix(title, "…") {
			tail := strings.TrimSuffix(title, "…")
			for _, prefix := range prefixes {
				for offset := range tail {
					fragment := tail[offset:]
					if len(fragment) >= core.MinUnscoped && strings.HasPrefix(prefix, fragment) {
						lines[i] = id + " — " + core.Redacted
						break
					}
				}
			}
		}
	}
	// Every replacement stays within its line, so provenance remains valid
	// for later independent redactions after a prior partial text replacement.
	for b := range a.Bindings {
		if valid[b] {
			a.Bindings[b].Rendering = archiveHash(lines[a.Bindings[b].Line])
		}
	}
	a.Body = strings.Join(lines, "\n")
}
