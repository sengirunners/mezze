package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// zm-c5y6c.14: zm inbox --peek reads only the peek route (never the marking
// inbox), and a daemon too old to know it says so instead of a bare 404.
func TestPeekInboxOldDaemon(t *testing.T) {
	oldAs, oldProject := flagAs, flagProject
	t.Cleanup(func() { flagAs, flagProject = oldAs, oldProject })
	flagAs, flagProject = "", ""
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_PROJECT", "")
	t.Setenv("ZM_TOKEN", "")
	for _, tc := range []struct {
		name, answer, err string
		rows              int
	}{
		{name: "new daemon", rows: 1},
		{name: "old daemon", answer: "no route GET /v0/mail/inbox/peek", err: "older than zm inbox --peek (nothing was read or marked)"},
		{name: "other not_found", answer: "project x not found", err: "project x not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				if r.URL.Path != "/v0/mail/inbox/peek" {
					t.Errorf("read %s: only the peek route", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				if tc.answer != "" {
					api.WriteError(w, api.Errorf(api.CodeNotFound, "%s", tc.answer))
					return
				}
				api.WriteJSON(w, 200, orchapi.MailList{Mail: []orchapi.Mail{{ID: "m1"}}})
			}))
			defer srv.Close()
			t.Setenv("ZM_HOST", srv.URL)
			c, _, err := newClient()
			if err != nil {
				t.Fatal(err)
			}
			ms, err := peekInbox(context.Background(), c, orchapi.InboxQuery{})
			if tc.err == "" {
				if err != nil || len(ms) != tc.rows {
					t.Fatalf("got %v, %v", ms, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err %v, want %q", err, tc.err)
			}
			if tc.name == "other not_found" && strings.Contains(err.Error(), "older") {
				t.Fatalf("blamed the daemon's age: %v", err)
			}
		})
	}
}
