package sqlite

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Handles: the alphabet (no 0 1 i l o), at least one letter, no blocked
// word, every symbol used.
func TestNewHandle(t *testing.T) {
	re := regexp.MustCompile(`^[2-9a-hjkmnp-z]+$`)
	seen := map[byte]int{}
	for range 20000 {
		h := newHandle(3)
		if len(h) != 3 || !re.MatchString(h) {
			t.Fatalf("handle %q", h)
		}
		if strings.Trim(h, "23456789") == "" {
			t.Fatalf("all-digit handle %q", h)
		}
		if isBlocked(h) {
			t.Fatalf("blocked handle %q", h)
		}
		for i := range len(h) {
			seen[h[i]]++
		}
	}
	if len(seen) != len(handleAlphabet) {
		t.Fatalf("%d of %d symbols used: %v", len(seen), len(handleAlphabet), seen)
	}
	for _, c := range "01ilo" {
		if strings.ContainsRune(handleAlphabet, c) {
			t.Fatalf("look-alike %q in the alphabet", c)
		}
	}
	if len(handleAlphabet) != 31 {
		t.Fatalf("alphabet has %d symbols", len(handleAlphabet))
	}
}

// New issues get <prefix>-<handle> ids; ids minted before stay valid.
func TestNewIssueIDsUseHandles(t *testing.T) {
	p := testProject(t)
	re := regexp.MustCompile(`^[a-z0-9_-]+-[2-9a-hjkmnp-z]{3}$`)
	for range 25 {
		it := create(t, p, orchapi.NewIssueRequest{Title: "x"})
		if !re.MatchString(it.ID) {
			t.Fatalf("new id %q", it.ID)
		}
	}
}
