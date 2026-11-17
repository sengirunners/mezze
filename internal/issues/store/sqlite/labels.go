package sqlite

import (
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api/textclean"
)

func (w *writer) newLabels(cur, set, add, remove []string) ([]string, error) {
	out := slices.Clone(cur)
	for _, r := range remove {
		raw := strings.TrimSpace(r)
		if raw == "" {
			continue // an empty selector must not match every invisible legacy name
		}
		cleaned := textclean.Clean(raw)
		blank := textclean.Blank(cleaned)
		out = slices.DeleteFunc(out, func(l string) bool {
			if blank {
				return strings.TrimSpace(l) == raw
			}
			return textclean.Clean(strings.TrimSpace(l)) == cleaned
		})
	}
	if len(set) > 0 {
		out = slices.Clone(set)
	}
	out = append(out, add...)
	out, err := cleanLabels(out)
	if err != nil {
		return nil, err
	}
	for _, r := range remove {
		out = slices.DeleteFunc(out, func(l string) bool { return l == strings.TrimSpace(r) })
	}
	return out, nil
}
