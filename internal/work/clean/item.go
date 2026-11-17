// Package clean prepares tracker text before it enters the work mirror.
package clean

import (
	"encoding/json"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api/textclean"
)

func Item(it *orchapi.WorkItem) {
	rawTitle := it.Title
	it.Title = strings.ReplaceAll(textclean.Clean(it.Title), "\n", " ")
	if rawTitle != "" && textclean.Blank(it.Title) {
		it.Title = "(untitled)"
	}
	it.Body = textclean.Clean(it.Body)
	it.Owner = textclean.Clean(it.Owner)
	it.CreatedBy = textclean.Clean(it.CreatedBy)
	it.URL = textclean.Clean(it.URL)
	it.Stale = textclean.Clean(it.Stale)
	it.Write = "" // only a replica's status answer sets it (internal/team), never a tracker
	it.Labels = ImportedLabels(it.Labels)
	if b, err := json.Marshal(it.Extra); err == nil {
		var out map[string]any
		if json.Unmarshal(textclean.JSON(b), &out) == nil {
			it.Extra = out
		}
	}
}
func Labels(labels []string) {
	for i := range labels {
		labels[i] = textclean.Clean(labels[i])
	}
}

// ImportedLabels drops blank labels and collapses labels with the same cleaned name.
func ImportedLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]bool{}
	for _, label := range labels {
		label = strings.TrimSpace(strings.ReplaceAll(textclean.Clean(label), "\n", " "))
		if !textclean.Blank(label) && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}
