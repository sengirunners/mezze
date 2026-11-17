package orchclient

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// A script run never carries the caller's directory: the daemon refuses a cwd
// on one (zm exec from a real shell); an agent task still does.
func TestTaskStartCwdOnlyForAgents(t *testing.T) {
	var got []orchapi.TaskStartRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req orchapi.TaskStartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		got = append(got, req)
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		api.WriteJSON(w, 200, orchapi.Task{ID: "t1"})
	})
	c := client.WithHandler(h)
	c.Cwd = "/home/me/repo"
	ctx := context.Background()
	if _, err := TaskStart(ctx, c, orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "auto", Run: &orchapi.TaskRun{Ref: "main", Script: "x.sh"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := TaskStart(ctx, c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Cwd != "" || got[1].Cwd != "/home/me/repo" {
		t.Fatalf("cwds sent: %+v", got)
	}
}

// A daemon that predates thread_title and pr_url refuses the ask as an unknown
// field: the ask is sent once more without them, and any other 400 is not retried.
func TestNewAskRetriesWithoutOriginOnOldDaemon(t *testing.T) {
	var got []orchapi.NewAskRequest
	refuse := ""
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req orchapi.NewAskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		got = append(got, req)
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch {
		case refuse != "" && (req.ThreadTitle != "" || req.PRURL != ""):
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "%s", refuse))
		default:
			api.WriteJSON(w, 200, orchapi.Ask{ID: "a1", ThreadTitle: req.ThreadTitle})
		}
	})
	c := client.WithHandler(h)
	ctx := context.Background()
	req := orchapi.NewAskRequest{Topic: "x", ThreadTitle: "Fix it", PRURL: "https://github.com/a/b/pull/1"}
	for _, tc := range []struct {
		refuse string
		sent   int
		err    bool
	}{
		{"", 1, false},
		{`json: unknown field "thread_title"`, 2, false},
		{`json: unknown field "pr_url"`, 2, false},
		{`json: unknown field "topic"`, 1, true},
		{"card needs context", 1, true},
	} {
		got, refuse = nil, tc.refuse
		a, err := NewAsk(ctx, c, req)
		if (err != nil) != tc.err || len(got) != tc.sent {
			t.Errorf("%q: sent %d, err %v", tc.refuse, len(got), err)
		}
		if tc.sent == 2 && (got[1].ThreadTitle != "" || got[1].PRURL != "" || got[1].Topic != "x" || a.ID != "a1" || a.ThreadTitle != "") {
			t.Errorf("%q: retry %+v gave %+v", tc.refuse, got[1], a)
		}
	}
}

// VerifyReceipt with Signed never takes a yes from a daemon that did not
// check it (one from before signed=1 ignores the parameter).
func TestVerifyReceiptSignedNeedsTheCheck(t *testing.T) {
	ctx := context.Background()
	checked := []string{"ask"}
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		api.WriteJSON(w, 200, orchapi.ReceiptCheck{Valid: true, Checked: checked, KeyID: "zm:k"})
	}))
	rc := []byte(`{"project":"01J"}`)
	if got, err := VerifyReceipt(ctx, c, rc, orchapi.ReceiptExpect{Ask: "A", Signed: true}); err != nil || got.Valid {
		t.Fatalf("an old daemon's yes: %+v %v", got, err)
	}
	if got, err := VerifyReceipt(ctx, c, rc, orchapi.ReceiptExpect{Ask: "A"}); err != nil || !got.Valid {
		t.Fatalf("without signed: %+v %v", got, err)
	}
	checked = []string{"ask", "signed"}
	if got, err := VerifyReceipt(ctx, c, rc, orchapi.ReceiptExpect{Ask: "A", Signed: true}); err != nil || !got.Valid {
		t.Fatalf("a daemon that checked it: %+v %v", got, err)
	}
}
