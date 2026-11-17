package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func askCard() map[string]any {
	return map[string]any{
		"context":  "Residents can swap call shifts, and last year's swaps still sit in the new schedule.",
		"question": "Should we keep last year's swaps?",
		"choices": []map[string]any{
			{"label": "Keep them", "pros": "nobody is surprised", "cons": "two people work two holidays"},
			{"label": "Drop them", "pros": "fair again", "cons": "residents must redo plans"},
		},
		"recommend": "a", "why": "Keeping them breaks no promise.", "searched": "past rulings: nothing on this",
	}
}

func askNew(t *testing.T, cs *sdk.ClientSession, args map[string]any) (bool, string) {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "ask_new", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return r.IsError, b.String()
}

// ask_new from a session of one project lands on a work item of another
// project it can see, in that item's project (like `zm ask new -P`), and a
// project or item the caller cannot see answers exactly like a missing one.
func TestAskNewUsesTheItemsProject(t *testing.T) {
	ctx := context.Background()
	dm := daemon(t)
	owner := client.WithHandler(dm.SocketHandler())
	for _, slug := range []string{"zm", "harbor"} {
		if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
			t.Fatal(err)
		}
	}
	in := func(project string) *client.Client {
		c := client.WithHandler(dm.SocketHandler())
		c.Project, c.ProjectFrom = project, "flag"
		return c
	}
	item, err := orchclient.CreateWork(ctx, in("harbor"), orchapi.CreateWorkRequest{Title: "swaps"})
	if err != nil {
		t.Fatal(err)
	}
	harbor, err := owner.GetProject(ctx, "harbor")
	if err != nil {
		t.Fatal(err)
	}

	zs := session(t, in("zm"))
	for name, args := range map[string]map[string]any{
		"item's own project": {"work": item.ID, "card": askCard()},
		"explicit project":   {"work": item.ID, "project": "harbor", "card": askCard()},
	} {
		isErr, text := askNew(t, zs, args)
		var a orchapi.Ask
		if isErr || json.Unmarshal([]byte(text), &a) != nil {
			t.Fatalf("%s: %s", name, text)
		}
		if a.Project != harbor.ID || a.Subject.Work != item.ID {
			t.Errorf("%s: ask in %q on %q, want %q on %q", name, a.Project, a.Subject.Work, harbor.ID, item.ID)
		}
		if _, err := orchclient.WithdrawAsk(ctx, in("harbor"), a.ID); err != nil { // frees the subject's one-ask slot
			t.Fatal(err)
		}
	}

	// A guest of zm sees neither harbor nor its items.
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "zm", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	gc := client.WithHandler(dm.Handler)
	gc.Token, gc.Project, gc.ProjectFrom = tok.Secret, "zm", "flag"
	gs := session(t, gc)
	missing := "local:zzzzzz"
	pairs := [][2]map[string]any{
		{{"work": item.ID, "card": askCard()}, {"work": missing, "card": askCard()}},
		{{"work": item.ID, "project": "harbor", "card": askCard()}, {"work": item.ID, "project": "nope", "card": askCard()}},
	}
	for _, p := range pairs {
		hErr, hidden := askNew(t, gs, p[0])
		mErr, miss := askNew(t, gs, p[1])
		norm := func(s string) string {
			return strings.NewReplacer(item.ID, "ID", missing, "ID", `"harbor"`, `"P"`, `"nope"`, `"P"`, "harbor", "P", "nope", "P").Replace(s)
		}
		if !hErr || !mErr || !strings.Contains(hidden, "not_found") || norm(hidden) != norm(miss) {
			t.Errorf("hidden answered unlike missing:\n hidden  %v %s\n missing %v %s", hErr, hidden, mErr, miss)
		}
	}
	if asks, err := orchclient.Asks(ctx, owner, "harbor", true); err != nil || len(asks) != 0 {
		t.Fatalf("open harbor asks = %+v, %v", asks, err)
	}
}
