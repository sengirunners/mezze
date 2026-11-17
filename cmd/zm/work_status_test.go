package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// On a replica, `zm work status` waits briefly for its queued write
// and says what happened: done when the authority applied it, the refusal
// (non-zero) when it refused, queued (never "is done") when it has not answered.
func TestWorkStatusReportsTheReplicaOutcome(t *testing.T) {
	oldJSON, oldAs, oldProject, oldStdout := flagJSON, flagAs, flagProject, os.Stdout
	t.Cleanup(func() { flagJSON, flagAs, flagProject, os.Stdout = oldJSON, oldAs, oldProject, oldStdout })
	flagJSON, flagAs, flagProject = false, "", ""
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_PROJECT", "")
	t.Setenv("ZM_TOKEN", "")
	for _, tc := range []struct {
		name, state, msg, out, err string
		json                       bool
	}{
		{name: "acked", state: api.OutboxAcked, out: "bd:x is done"},
		{name: "settled", state: "", out: "bd:x is done"}, // gone from the outbox: the copy shows it
		{name: "refused", state: api.OutboxConflict, msg: "status changed at the authority after this write was made; it was not applied",
			err: "status changed at the authority"},
		{name: "pending", state: api.OutboxPending, out: "bd:x done is queued: it applies when the authority is reachable (zm outbox)"},
		{name: "acked json", state: api.OutboxAcked, json: true, out: `"status": "done"`},
		{name: "pending json", state: api.OutboxPending, json: true, out: `"queued": "queued: it applies when the authority is reachable (zm outbox)"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.URL.Path {
				case "/v0/work/bd:x/status":
					api.WriteJSON(w, 200, orchapi.WorkItem{ID: "bd:x", Project: "PJOINED", Status: orchapi.WorkDone, Write: "W1"})
				case "/v0/team/outbox":
					polls.Add(1)
					if r.URL.Query().Get("project") != "PJOINED" || r.URL.Query().Get("all") != "1" {
						t.Errorf("outbox query %s", r.URL.RawQuery)
					}
					l := api.OutboxList{Entries: []api.OutboxEntry{{ID: "W0", Kind: "comment", State: api.OutboxConflict, Error: "not this one"}}}
					if tc.state != "" {
						l.Entries = append(l.Entries, api.OutboxEntry{ID: "W1", Kind: "status", State: tc.state, Error: tc.msg})
					}
					api.WriteJSON(w, 200, l)
				default:
					t.Errorf("unexpected %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			t.Setenv("ZM_HOST", srv.URL)
			file := filepath.Join(home, tc.name)
			out, err := os.Create(file)
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = out
			flagJSON = tc.json
			cmd := workStatusCmd()
			cmd.SetArgs([]string{"bd:x", "done"})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			err = cmd.ExecuteContext(ctx)
			took := time.Since(start)
			os.Stdout = oldStdout
			out.Close()
			raw, _ := os.ReadFile(file)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "zm outbox") || len(raw) != 0 {
					t.Fatalf("refusal: err %v, out %q", err, raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(string(raw))
			switch {
			case tc.json && (!strings.Contains(got, tc.out) || strings.Contains(got, `"queued"`) != (tc.state == api.OutboxPending)):
				t.Fatalf("json out %q, want %s and queued only when pending", got, tc.out)
			case !tc.json && got != tc.out:
				t.Fatalf("out %q, want %q", got, tc.out)
			}
			if tc.state == api.OutboxPending && (polls.Load() < 2 || took < 2*time.Second || took > 6*time.Second) {
				t.Fatalf("pending: %d polls in %s, want a ~3 s wait", polls.Load(), took)
			}
		})
	}
}
