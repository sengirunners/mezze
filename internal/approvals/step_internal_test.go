package approvals

import (
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A join never drops or swaps a person's step.
func TestSameStepGuardsJoins(t *testing.T) {
	st := &orchapi.AskStep{Label: "Accept", Steps: []string{"Sign in.", "Press Agree."}, Link: "https://example.com/a"}
	other := *st
	other.Link = "https://example.com/b"
	for _, c := range []struct {
		name        string
		mine, their *orchapi.AskStep
		want        bool
	}{
		{"neither", nil, nil, true},
		{"step joins a plain card", st, nil, false},
		{"plain card joins a step", nil, st, false},
		{"same step", st, &orchapi.AskStep{Label: "Accept", Steps: []string{"Sign in.", "Press Agree."}, Link: "https://example.com/a"}, true},
		{"other link", st, &other, false},
	} {
		if got := sameStep(c.mine, c.their); got != c.want {
			t.Errorf("%s: sameStep %v, want %v", c.name, got, c.want)
		}
	}
}
