package ghlink

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/events"
)

type holders map[string]string

func (h holders) Holder(_ context.Context, id string) (orchapi.HolderResponse, error) {
	to, ok := h[id]
	if !ok {
		return orchapi.HolderResponse{}, errors.New("not found")
	}
	return orchapi.HolderResponse{ID: id, MailTo: to}, nil
}

// Items group only on an agent the linker can read: redacted holders
// ("someone") may be different sessions, so each goes alone, as does an item
// whose holder lookup fails. Items no agent has (nobody
// holds them, or a person, or the owners) are orphans: their CI result goes
// to the group mailbox, never to a person.
func TestByRecipientGroupsOnlyVisibleAgents(t *testing.T) {
	h := holders{"a": "mini/aaaaaaaa", "b": "mini/aaaaaaaa", "c": events.Someone, "d": events.Someone, "e": "", "f": "human:ivo",
		"g": "mini/aaaaaaaa", "m": "machine:mini", "o": "owners", "n": "machine:mini"}
	got, orphans := byRecipient(context.Background(), h, []string{"a", "c", "b", "d", "e", "x", "f", "g", "m", "o", "n"})
	want := [][]string{{"a", "b", "g"}, {"c"}, {"d"}, {"x"}, {"m", "n"}}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(orphans, []string{"e", "f", "o"}) {
		t.Fatalf("byRecipient = %v, %v; want %v, [e f o]", got, orphans, want)
	}
}
