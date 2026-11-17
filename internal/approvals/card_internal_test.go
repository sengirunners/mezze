package approvals

import (
	"encoding/json"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A card stored before a choice's effect and the card's after_press existed
// marshals to the same bytes, so its card_sha256 and receipts still verify.
func TestOldCardHashUnchanged(t *testing.T) {
	stored := `{"title":"T","context":"C","question":"Q","choices":[{"key":"a","label":"A","pros":"p"},{"key":"b","label":"B"}],"recommend":"a","why":"W","searched":"s s s"}`
	var c orchapi.AskCard
	if err := json.Unmarshal([]byte(stored), &c); err != nil {
		t.Fatal(err)
	}
	if got := marshal(c); cardSHA(got) != cardSHA(stored) {
		t.Fatalf("re-marshalled old card changed:\n%s\n%s", got, stored)
	}
}
