package orchapi

import "testing"

func TestCheckSafeOutputs(t *testing.T) {
	for _, c := range []struct {
		name string
		caps map[string]int
		ok   bool
	}{
		{"none", nil, true},
		{"every kind", map[string]int{"bead-write": 3, "zm-ask": 0, "mail-send": MaxSafeOutput}, true},
		{"unknown kind", map[string]int{"pr-merge": 1}, false},
		{"negative", map[string]int{"bead-write": -1}, false},
		{"too many", map[string]int{"zm-ask": MaxSafeOutput + 1}, false},
	} {
		if err := CheckSafeOutputs(c.caps); (err == nil) != c.ok {
			t.Errorf("%s: CheckSafeOutputs(%v) = %v", c.name, c.caps, err)
		}
	}
}
