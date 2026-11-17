package orchclient

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestSessionPagePreservesCompleteness(t *testing.T) {
	payload := `{"sessions":[{"id":"aaaa1111","address":"mini/aaaa1111","machine":"mini","project":"P","agent":"codex","state":"idle","seen":"2026-11-04T12:00:00Z"}],"next":"cursor-next","skipped":[{"project":"Q","source":"authority","error":"unavailable"}],"behind":[{"project":"R","source":"authority","state":"loading"}]}`
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.Method != http.MethodGet || r.URL.Path != "/v0/sessions" {
			t.Errorf("unexpected route %s %s", r.Method, r.URL)
		}
		if q := r.URL.Query(); q.Get("project") != "P" || q.Get("before") != "cursor-before" || q.Get("limit") != "17" || q.Get("all") != "1" || q.Get("state") != "idle" || q.Get("agent") != "codex" {
			t.Errorf("query changed: %s", r.URL)
		}
		w.Write([]byte(payload))
	}))
	q := orchapi.SessionQuery{Project: "P", Before: "cursor-before", Limit: 17, All: true, State: orchapi.StateIdle, Agent: "codex"}
	page, err := SessionPage(context.Background(), c, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Next != "cursor-next" || len(page.Skipped) != 1 || page.Skipped[0].Project != "Q" || len(page.Behind) != 1 || page.Behind[0].State != orchapi.SyncLoading {
		t.Fatalf("lost page completeness: %+v", page)
	}
	rows, err := Sessions(context.Background(), c, q)
	if err != nil || !reflect.DeepEqual(rows, page.Sessions) {
		t.Fatalf("rows-only convenience changed: %+v %v", rows, err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"skipped"`) || !strings.Contains(string(encoded), `"behind"`) {
		t.Fatalf("typed page lost status when forwarded: %s", encoded)
	}
}

func TestSessionPageLegacyEnvelopeOmitsEmptyStatus(t *testing.T) {
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		w.Write([]byte(`{"sessions":[]}`))
	}))
	page, err := SessionPage(context.Background(), c, orchapi.SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(page)
	if err != nil || string(raw) != `{"sessions":[]}` {
		t.Fatalf("legacy page shape changed: %s, %v", raw, err)
	}
}
