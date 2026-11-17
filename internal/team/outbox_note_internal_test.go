package team

import (
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// A session's note carries the refusal's code, rule and wait, never its text.
func TestQueuedMailRefusalNote(t *testing.T) {
	tail := "; zm team outbox has the authority's message"
	for _, c := range []struct {
		er   api.Error
		want string
	}{
		{*api.Errorf(api.CodeRateLimited, "to somebody").WithRetryAfter(90).WithData(map[string]any{"limit": "pair", "max": 30}),
			"queued mail failed: rate_limited (pair limit); retry after 1m30s" + tail},
		{*api.Errorf(api.CodeNotFound, "no session somebody"), "queued mail failed: not_found" + tail},
		{api.Error{}, "queued mail failed: refused" + tail},
		{*api.Errorf(api.CodeRateLimited, "x").WithData(map[string]any{"limit": "Bad Value\n"}), "queued mail failed: rate_limited" + tail},
	} {
		if got := queuedMailRefusal("mail", "failed", c.er); got != c.want {
			t.Errorf("note %q, want %q", got, c.want)
		}
	}
}
