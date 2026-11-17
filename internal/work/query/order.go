package query

import (
	"github.com/sengirunners/mezze/internal/orchapi"
)

func MergeRefs(a, b map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, m := range []map[string][]string{a, b} {
		for k, v := range m {
			out[k] = append(out[k], v...)
		}
	}
	return out
}

// IntersectRefs keeps b's refs that a also names, by source; a nil a keeps
// all of b.
func IntersectRefs(a, b map[string][]string) map[string][]string {
	if a == nil {
		return b
	}
	out := map[string][]string{}
	for k, refs := range b {
		in := map[string]bool{}
		for _, r := range a[k] {
			in[r] = true
		}
		for _, r := range refs {
			if in[r] {
				out[k] = append(out[k], r)
			}
		}
	}
	return out
}

// Less is the total order: key, then project, then id; newest first
// unless asc.
func Less(aKey, bKey int64, a, b orchapi.WorkItem, asc bool) bool {
	if aKey != bKey {
		return (aKey < bKey) == asc
	}
	if a.Project != b.Project {
		return (a.Project < b.Project) == asc
	}
	return (a.ID < b.ID) == asc
}
