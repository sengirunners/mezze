package issues_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

func TestMemoryValuesCleanResponseAndStorage(t *testing.T) {
	f := newAuthzFixture(t)
	o := f.ownerAt("harbor")
	ctx := context.Background()
	const key = " resources/Ü " // keys are identifiers, kept byte for byte
	for _, tc := range []struct {
		name, in, want string
	}{
		{"mixed controls", " \ttext\u200b\u202e \u2066line\u2069\n", " \ttext line\n"},
		{"emoji and script shaping", "\t👩‍💻 ❤️\n می\u200cخواهم क्\u200dष \t", "\t👩‍💻 ❤️\n می\u200cخواهم क्\u200dष \t"},
		{"line endings", " a\r\nb\rc\u2028d\u2029 ", " a\nb\nc\nd\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := orchclient.Remember(ctx, o, key, tc.in)
			if err != nil || m.Key != key || m.Value != tc.want {
				t.Fatalf("remember: %+v %v; want key %q value %q", m, err, key, tc.want)
			}
			ms, err := orchclient.Memories(ctx, o, "harbor")
			if err != nil || len(ms) != 1 || ms[key] != tc.want {
				t.Fatalf("stored memories: %v %v", ms, err)
			}
		})
	}
}

func TestMemoryInvisibleOnlyValuesDoNotMutate(t *testing.T) {
	f := newAuthzFixture(t)
	o := f.ownerAt("harbor")
	ctx := context.Background()
	if _, err := orchclient.Remember(ctx, o, "existing", "original"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"\u200b", " \u2066\t\u202e\n\u2069 ", "\u2800", "\u3164"} {
		for _, key := range []string{"existing", "new"} {
			if _, err := orchclient.Remember(ctx, o, key, value); !api.IsCode(err, api.CodeBadRequest) {
				t.Errorf("invisible memory %q: %v", value, err)
			}
		}
		ms, err := orchclient.Memories(ctx, o, "harbor")
		if err != nil || len(ms) != 1 || ms["existing"] != "original" {
			t.Fatalf("refused write changed memories: %v %v", ms, err)
		}
	}
}

func TestMemoryCleaningPreservesAuthorizationAndSizeLimit(t *testing.T) {
	f := newAuthzFixture(t)
	o := f.ownerAt("harbor")
	ctx := context.Background()
	if _, err := orchclient.Remember(ctx, o, "existing", "original"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"member", "guest"} {
		b, err := json.Marshal(orchapi.Memory{Value: "changed\u200b"})
		if err != nil {
			t.Fatal(err)
		}
		if code, body := f.as(who, http.MethodPut, "/v0/issues/memories/existing", string(b)); code != http.StatusNotFound {
			t.Fatalf("%s memory write: %d %s", who, code, body)
		}
	}
	if _, err := orchclient.Remember(ctx, o, "existing", strings.Repeat("x", (1<<20)+1)); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("oversized memory: %v", err)
	}
	ms, err := orchclient.Memories(ctx, o, "harbor")
	if err != nil || len(ms) != 1 || ms["existing"] != "original" {
		t.Fatalf("refused writes changed memories: %v %v", ms, err)
	}
}
