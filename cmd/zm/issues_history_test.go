package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// The advertised zm door must actually reach history, with its project scope,
// and print the previous field values in both human and machine output.
func TestIssuesHistoryCommand(t *testing.T) {
	history := []orchapi.IssueChange{{ID: "change", IssueID: "oc-one", Kind: "updated", Actor: "ana",
		At: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), Fields: map[string]json.RawMessage{"notes": json.RawMessage(`["old evidence","new evidence"]`)}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.Method != http.MethodGet || r.URL.Path != "/v0/issues/oc-one/history" || api.GetHeader(r.Header, api.HeaderProject) != "oc" {
			t.Errorf("unexpected request %s %s project=%q", r.Method, r.URL.Path, api.GetHeader(r.Header, api.HeaderProject))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		api.WriteJSON(w, http.StatusOK, map[string]any{"history": history})
	}))
	defer server.Close()
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_HOST", server.URL)
	t.Setenv("ZM_PROJECT", "oc")
	t.Setenv("ZM_TOKEN", "")
	oldJSON, oldAs, oldProject := flagJSON, flagAs, flagProject
	oldStdout := os.Stdout
	t.Cleanup(func() { flagJSON, flagAs, flagProject = oldJSON, oldAs, oldProject; os.Stdout = oldStdout })
	flagAs, flagProject = "", ""
	for _, machine := range []bool{false, true} {
		flagJSON = machine
		path := filepath.Join(t.TempDir(), "output")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = f
		cmd := issuesCmd()
		cmd.SetArgs([]string{"history", "oc-one"})
		err = cmd.ExecuteContext(context.Background())
		os.Stdout = oldStdout
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if machine {
			var got []orchapi.IssueChange
			if err := json.Unmarshal(out, &got); err != nil || len(got) != 1 {
				t.Fatalf("JSON history: %s, %v", out, err)
			}
			var values []string
			if err := json.Unmarshal(got[0].Fields["notes"], &values); err != nil || len(values) != 2 || values[0] != "old evidence" || values[1] != "new evidence" {
				t.Fatalf("JSON history lost evidence: %s, %v", out, err)
			}
		} else if !strings.Contains(string(out), "old evidence") || !strings.Contains(string(out), "new evidence") || !strings.Contains(string(out), "ana") {
			t.Fatalf("human history lost evidence: %s", out)
		}
	}
}
