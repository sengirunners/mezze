package orchapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// An agent's report that asks a person to do a step is
// refused with the mail-chore judge's fixes; one the judge could not read is
// refused as unavailable, never sent; only an agent's report to a person is
// judged. The judge's own cases: testdata/mailchore.
func TestRefuseChoreMail(t *testing.T) {
	ctx := context.Background()
	const agent = "mac/a1b2c3d4"
	body := "You need to log in to the analytics site."
	var seen []string
	fail := judge.Fake{Fn: func(prompt string) (json.RawMessage, error) {
		seen = append(seen, prompt)
		return json.RawMessage(`{"pass":false,"fixes":["Log in yourself and report the numbers."],"reason":"It asks the reader to log in."}`), nil
	}}
	err := orchapi.RefuseChoreMail(ctx, fail, agent, orchapi.MailKindReport, "human:bea", orchapi.NeedsHuman, body)
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "asks bea to do a step (It asks the reader to log in)") {
		t.Fatalf("report to bea: %v", err)
	}
	var ae *api.Error
	if !errors.As(err, &ae) || !strings.Contains(ae.Hint, "Log in yourself and report the numbers. To carry the step") || !strings.Contains(ae.Hint, "zm ask new --to human:bea") {
		t.Fatalf("hint: %+v", ae)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], `"body": "You need to log in to the analytics site."`) || !strings.Contains(seen[0], `"reader": "bea"`) {
		t.Fatalf("judge read %q", seen)
	}
	held := judge.Fake{Err: errors.New("claude: not on PATH")}
	if err := orchapi.RefuseChoreMail(ctx, held, agent, orchapi.MailKindReport, "human:bea", orchapi.NeedsHuman, body); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("held: %v", err)
	}
	if err := orchapi.RefuseChoreMail(ctx, nil, agent, orchapi.MailKindReport, "owners", orchapi.NeedsHuman, body); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("no provider: %v", err)
	}
	pass := judge.Fake{Answer: json.RawMessage(`{"pass":true,"fixes":[],"reason":"It only reports."}`)}
	if err := orchapi.RefuseChoreMail(ctx, pass, agent, orchapi.MailKindReport, "human:bea", orchapi.NeedsHuman, "Shipped; nothing to do."); err != nil {
		t.Errorf("nothing asked: %v", err)
	}
	seen = nil
	for name, c := range map[string]struct{ from, kind, to string }{
		"a person's own mail": {"human:ivo", orchapi.MailKindReport, "human:bea"},
		"a nudge":             {agent, orchapi.MailKindNudge, "human:bea"},
		"to a session":        {agent, orchapi.MailKindReport, "mac/e5f6a7b8"}, // needs=human to a session would be its machine's person
	} {
		if err := orchapi.RefuseChoreMail(ctx, fail, c.from, c.kind, c.to, orchapi.NeedsAny, body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(seen) != 0 {
		t.Errorf("the judge read mail it does not judge: %d", len(seen))
	}
}
