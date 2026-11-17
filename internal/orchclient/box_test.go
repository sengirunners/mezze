package orchclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// An unsigned answer is the pinned authority's tunnel or home being down
// (retryable, not a credential problem); only a different key is a
// fingerprint mismatch; no variable leaves an error as it was.
func TestBoxCallErrorSeparatesDownFromWrongKey(t *testing.T) {
	boxed := func(string) string { return "/run/box/POTLUCK_TOKEN" }
	for _, c := range []struct {
		name   string
		getenv func(string) string
		err    error
		code   string
		in     string
	}{
		{"unsigned answer", boxed, fmt.Errorf("answer: %w", tunnel.ErrUnsigned), api.CodeUnavailable, "its tunnel or home is down"},
		{"another key", boxed, fmt.Errorf("answer: %w", tunnel.ErrHomeKey), api.CodeUnauthenticated, "fingerprint mismatch"},
		{"refused token", boxed, &api.Error{Code: api.CodeUnauthenticated, Message: "expired"}, api.CodeUnauthenticated, "refused this box's token"},
	} {
		var e *api.Error
		if err := BoxCallError(c.getenv, c.err); !errors.As(err, &e) || e.Code != c.code || !strings.Contains(e.Message, c.in) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	orig := fmt.Errorf("answer: %w", tunnel.ErrUnsigned)
	if err := BoxCallError(func(string) string { return "" }, orig); err != orig {
		t.Errorf("no box credential changed the error: %v", err)
	}
	if _, ok, err := BoxClient(func(k string) string {
		if k == EnvBoxCredential {
			return "  "
		}
		return ""
	}); !ok || err == nil {
		t.Errorf("a blank variable must fail closed, not fall back: ok=%v err=%v", ok, err)
	}
}
