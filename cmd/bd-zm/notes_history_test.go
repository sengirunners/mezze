package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestShimNotesRequireExplicitReplacement(t *testing.T) {
	s := newShim(t)
	id := strings.TrimSpace(s.ok("create", "Keep my notes", "--silent"))
	s.ok("update", id, "--notes", "original notes")
	for _, args := range [][]string{
		{"update", id, "--notes", "replacement"},
		{"update", id, "--notes", ""},
		{"update", id, "--notes", "replacement", "--replace=false"},
		{"update", id, "--claim", "--notes", "replacement", "--title", "changed"},
	} {
		if _, errOut, code := s.bd(args...); code == 0 || !strings.Contains(errOut, "--append-notes") || !strings.Contains(errOut, "--replace") {
			t.Fatalf("%v: exit %d, %s", args, code, errOut)
		}
	}
	d, err := orchclient.Issue(context.Background(), s.c, id)
	if err != nil || d.Notes != "original notes" || d.Title != "Keep my notes" || d.Assignee != "" || d.Status != "open" {
		t.Fatalf("refused update changed the issue: %+v, %v", d, err)
	}
	s.ok("update", id, "--append-notes", "another line")
	d, err = orchclient.Issue(context.Background(), s.c, id)
	if err != nil || d.Notes != "original notes\nanother line" {
		t.Fatalf("append lost notes: %+v, %v", d, err)
	}
	s.ok("update", id, "--notes", "replacement", "--replace")
	s.ok("update", id, "--notes", "", "--replace")
	s.ok("update", id, "--claim", "--notes", "new notes", "--status", "in_progress")
	for _, args := range [][]string{
		{"update", id, "--replace"},
		{"update", id, "--replace", "--append-notes", "x"},
		{"update", id, "--notes", "x", "--append-notes", "y"},
	} {
		if _, _, code := s.bd(args...); code == 0 {
			t.Fatalf("invalid flags succeeded: %v", args)
		}
	}
}

// An empty field is not permission to overwrite a concurrent agent's append.
// Exercise the daemon's real version check across the CLI's read/write gap.
func TestShimNotesRefuseConcurrentAppend(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "explicit replacement"}[replace], func(t *testing.T) {
			s := newShim(t)
			id := strings.TrimSpace(s.ok("create", "race", "--silent"))
			if replace {
				s.ok("update", id, "--notes", "original")
			}
			base := s.c
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.Method {
				case http.MethodGet:
					d, err := orchclient.Issue(r.Context(), base, id)
					if err != nil {
						api.WriteError(w, err)
						return
					}
					note := "concurrent evidence"
					if _, err := orchclient.UpdateIssue(r.Context(), base, id, orchapi.UpdateIssueRequest{AppendNotes: &note}); err != nil {
						t.Error(err)
					}
					api.WriteJSON(w, http.StatusOK, d)
				case http.MethodPatch:
					var req orchapi.UpdateIssueRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					got, err := orchclient.UpdateIssue(r.Context(), base, id, req)
					if err != nil {
						api.WriteError(w, err)
						return
					}
					api.WriteJSON(w, http.StatusOK, got)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			c.Project, c.ProjectFrom = base.Project, "flag"
			newClient = func(context.Context) (*client.Client, error) { return c, nil }
			args := []string{"update", id, "--notes", "would lose evidence"}
			if replace {
				args = append(args, "--replace")
			}
			if _, errOut, code := s.bd(args...); code == 0 || !strings.Contains(errOut, "changed after your copy") {
				t.Fatalf("concurrent append overwritten: exit %d, %s", code, errOut)
			}
			d, err := orchclient.Issue(context.Background(), base, id)
			if err != nil || !strings.HasSuffix(d.Notes, "concurrent evidence") || strings.Contains(d.Notes, "would lose") {
				t.Fatalf("lost concurrent notes: %+v, %v", d, err)
			}
		})
	}
}

func TestShimHistoryIncludesPreviousNotes(t *testing.T) {
	s := newShim(t)
	id := strings.TrimSpace(s.ok("create", "history", "--notes", "old evidence", "--silent"))
	s.ok("update", id, "--notes", "replacement", "--replace")
	s.ok("comments", "add", id, "why it changed")
	var hs []orchapi.IssueChange
	out := s.ok("history", id, "--json", "--readonly")
	if err := json.Unmarshal([]byte(out), &hs); err != nil || len(hs) < 3 {
		t.Fatalf("history JSON: %s, %v", out, err)
	}
	found := false
	for _, h := range hs {
		var values []string
		if json.Unmarshal(h.Fields["notes"], &values) == nil && len(values) == 2 && values[0] == "old evidence" && values[1] == "replacement" {
			found = true
		}
	}
	if !found {
		t.Fatalf("old notes missing from history: %s", out)
	}
	if out := s.ok("history", id); !strings.Contains(out, "old evidence") || !strings.Contains(out, "replacement") || !strings.Contains(out, "commented") {
		t.Fatalf("human history lost evidence: %s", out)
	}
	if _, _, code := s.bd("history", "missing"); code == 0 {
		t.Fatal("missing history succeeded")
	}
}

// Claim and update are separate requests. Races before or after the claim must
// preserve evidence and disclose the successful claim, never silently release
// a lease that may now have another agent's changes.
func TestShimNotesClaimRaceReportsPartialOutcome(t *testing.T) {
	for _, stage := range []string{"before claim", "after claim"} {
		t.Run(stage, func(t *testing.T) {
			s := newShim(t)
			ctx := context.Background()
			if res, err := orchclient.AddTracker(ctx, s.c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
				t.Fatalf("bind native tracker: %+v %v", res, err)
			}
			id := strings.TrimSpace(s.ok("create", "race", "--silent"))
			base := s.c
			appendEvidence := func(ctx context.Context) {
				note := "concurrent evidence"
				if _, err := orchclient.UpdateIssue(ctx, base, id, orchapi.UpdateIssueRequest{AppendNotes: &note}); err != nil {
					t.Error(err)
				}
			}
			patched := false
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.Method {
				case http.MethodGet:
					got, err := orchclient.Issue(r.Context(), base, id)
					if err != nil {
						api.WriteError(w, err)
						return
					}
					if stage == "before claim" {
						appendEvidence(r.Context())
					}
					api.WriteJSON(w, http.StatusOK, got)
				case http.MethodPost:
					if r.URL.Path != "/v0/issues/"+id+"/claim" {
						t.Errorf("unexpected POST %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					var req struct {
						Assignee string `json:"assignee"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					got, err := orchclient.ClaimIssueAs(r.Context(), base, id, req.Assignee)
					if err != nil {
						api.WriteError(w, err)
						return
					}
					if stage == "after claim" {
						appendEvidence(r.Context())
					}
					api.WriteJSON(w, http.StatusOK, got)
				case http.MethodPatch:
					patched = true
					var req orchapi.UpdateIssueRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					got, err := orchclient.UpdateIssue(r.Context(), base, id, req)
					if err != nil {
						api.WriteError(w, err)
						return
					}
					api.WriteJSON(w, http.StatusOK, got)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			c.Project, c.ProjectFrom = base.Project, "flag"
			newClient = func(context.Context) (*client.Client, error) { return c, nil }
			out, errOut, code := s.bd("update", id, "--claim", "--notes", "would lose evidence")
			if code == 0 || out != "" || !strings.Contains(strings.ToLower(errOut), "claim") || !strings.Contains(errOut, "succeeded") || !strings.Contains(errOut, "not rolled back") {
				t.Fatalf("partial claim not disclosed: exit %d stdout=%q stderr=%q", code, out, errOut)
			}
			if stage == "before claim" && patched {
				t.Fatal("notes replacement attempted after claim exposed changed notes")
			}
			if stage == "after claim" && (!patched || !strings.Contains(errOut, "changed after your copy")) {
				t.Fatalf("notes CAS not exercised: patched=%v stderr=%q", patched, errOut)
			}
			got, err := orchclient.Issue(ctx, base, id)
			if err != nil || got.Notes != "concurrent evidence" || got.Status != orchapi.IssueInProgress || got.ClaimedBy == "" {
				t.Fatalf("claim/evidence not preserved: %+v %v", got, err)
			}
			leases, err := orchclient.Leases(ctx, base, orchapi.LeaseQuery{})
			if err != nil || len(leases) != 1 || leases[0].Key != "issue:"+id || leases[0].Holder != got.ClaimedBy {
				t.Fatalf("claim lease lost: %+v %v", leases, err)
			}
		})
	}
}
