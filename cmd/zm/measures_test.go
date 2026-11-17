package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/measures"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestMeasuresWindow(t *testing.T) {
	loc := time.FixedZone("test", -5*3600)
	now := time.Date(2030, 3, 10, 12, 0, 0, 0, loc)
	for _, tc := range []struct {
		name     string
		f        measuresFlags
		from, to string
		fail     bool
	}{
		{"default seven days ending today", measuresFlags{days: 7, tz: "UTC"}, "2030-03-04T00:00:00Z", "2030-03-11T00:00:00Z", false},
		{"explicit days", measuresFlags{days: 1, from: "2030-03-01", to: "2030-03-02", tz: "UTC"}, "2030-03-01T00:00:00Z", "2030-03-03T00:00:00Z", false},
		{"from beats days", measuresFlags{days: 1, from: "2030-03-08", tz: "UTC"}, "2030-03-08T00:00:00Z", "2030-03-11T00:00:00Z", false},
		{"bad zone", measuresFlags{days: 7, tz: "Not/AZone"}, "", "", true},
		{"bad day", measuresFlags{days: 7, from: "yesterday"}, "", "", true},
		{"no days", measuresFlags{days: 0}, "", "", true},
		{"backwards", measuresFlags{days: 7, from: "2030-03-12", to: "2030-03-10"}, "", "", true},
	} {
		from, to, _, err := tc.f.window(now)
		if (err != nil) != tc.fail {
			t.Errorf("%s: err %v", tc.name, err)
			continue
		}
		if err == nil && (from.UTC().Format(time.RFC3339) != tc.from || to.UTC().Format(time.RFC3339) != tc.to) {
			t.Errorf("%s: %s to %s", tc.name, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		}
	}
	// With no zone named, the days are cut in the machine's own (the clock's) zone.
	var f measuresFlags
	f.days = 1
	from, _, loc2, _ := f.window(now)
	if loc2 != loc || from.Hour() != 0 || from.Day() != 10 {
		t.Errorf("default zone: %v %v", loc2, from)
	}
}

// fakeAsks serves GET /v0/asks the way the daemon pages it: newest first, a
// cursor (before_id), Next set when the page was full.
func fakeAsks(total int) http.Handler {
	base := time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 200 // the daemon's default
		}
		limit = min(limit, 1000)
		start := total
		if v := r.URL.Query().Get("before_id"); v != "" {
			start, _ = strconv.Atoi(strings.TrimPrefix(v, "A"))
		}
		var out orchapi.AskList
		out.Asks = []orchapi.Ask{}
		for i := start - 1; i >= 0 && len(out.Asks) < limit; i-- {
			out.Asks = append(out.Asks, orchapi.Ask{ID: fmt.Sprintf("A%05d", i), Created: base.Add(time.Duration(i) * time.Hour)})
		}
		if len(out.Asks) == limit {
			out.Next = out.Asks[len(out.Asks)-1].ID
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

func TestCollectAsksReadsPastTheDefaultPage(t *testing.T) {
	c := client.WithHandler(speaksZm(fakeAsks(2500)))
	all, _, err := collectAsks(context.Background(), c, "", time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2500 {
		t.Fatalf("read %d of 2500 cards", len(all))
	}
	// Stops once a page reaches back past the lookback.
	cut := time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC).Add(2400 * time.Hour)
	some, _, err := collectAsks(context.Background(), c, "", cut, false)
	if err != nil || len(some) >= 2500 || len(some) < 100 {
		t.Fatalf("with a lookback: %d cards, %v", len(some), err)
	}
}

// An old daemon ignores limit and before_id: it answers its newest 200 every time.
func TestCollectAsksRefusesADaemonThatCannotPage(t *testing.T) {
	old := speaksZm(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := orchapi.AskList{Asks: []orchapi.Ask{}}
		for i := 199; i >= 0; i-- {
			out.Asks = append(out.Asks, orchapi.Ask{ID: fmt.Sprintf("A%05d", i), Created: time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	if _, _, err := collectAsks(context.Background(), client.WithHandler(old), "", time.Time{}, false); err == nil || !strings.Contains(err.Error(), "cannot page") {
		t.Fatalf("an old daemon read silently: %v", err)
	}
	// Accepted on request, and the report says it is short.
	got, note, err := collectAsks(context.Background(), client.WithHandler(old), "", time.Time{}, true)
	if err != nil || len(got) != 200 || !strings.Contains(note, "newest 200 cards") || !strings.Contains(note, "back to 2030-03-01 00:00:00 UTC") {
		t.Fatalf("--allow-short: %d cards, note %q, %v", len(got), note, err)
	}
	// Exactly 200 cards on a daemon that pages is just 200 cards.
	all, _, err := collectAsks(context.Background(), client.WithHandler(speaksZm(fakeAsks(200))), "", time.Time{}, false)
	if err != nil || len(all) != 200 {
		t.Fatalf("200 cards: %d, %v", len(all), err)
	}
}

func TestCollectMailPagesBackwardByTime(t *testing.T) {
	base := time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)
	const total = 1300
	var calls int
	c := client.WithHandler(speaksZm(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		until := base.Add(total * time.Minute)
		if v := r.URL.Query().Get("until"); v != "" {
			until, _ = time.Parse(time.RFC3339Nano, v)
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out := orchapi.MailList{Mail: []orchapi.Mail{}}
		for i := total - 1; i >= 0 && len(out.Mail) < limit; i-- {
			if ts := base.Add(time.Duration(i) * time.Minute); !ts.After(until) {
				out.Mail = append(out.Mail, orchapi.Mail{ID: fmt.Sprintf("M%05d", i), Created: ts})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})))
	all, _, err := collectMail(context.Background(), c, "", base, base.Add(total*time.Minute), false)
	if err != nil || len(all) != total {
		t.Fatalf("read %d of %d mails, %v (%d calls)", len(all), total, err, calls)
	}
}

func TestSendMeasuresReportIsOncePerWindow(t *testing.T) {
	var got orchapi.SendMailRequest
	sent := false
	c := client.WithHandler(speaksZm(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if sent {
			api.WriteError(w, api.Errorf(api.CodeConflict, "client_request_id already used with a different payload or operation"))
			return
		}
		sent = true
		_ = json.NewEncoder(w).Encode(orchapi.Mail{ID: "M1", Recipient: "owners"})
	})))
	rep := measures.Compute(measures.Input{From: time.Date(2030, 3, 4, 0, 0, 0, 0, time.UTC), To: time.Date(2030, 3, 11, 0, 0, 0, 0, time.UTC), Now: time.Now()}, measures.Rows)
	if _, err := sendMeasuresReport(context.Background(), c, rep, "owners"); err != nil {
		t.Fatal(err)
	}
	if got.To != "owners" || got.Kind != orchapi.MailKindReport || got.ClientRequestID != "measures-2030-03-04-2030-03-10" || !strings.Contains(got.Body, "| measure |") {
		t.Fatalf("sent %+v", got)
	}
	if _, err := sendMeasuresReport(context.Background(), c, rep, "owners"); err == nil || !strings.Contains(err.Error(), "already sent") {
		t.Fatalf("second send: %v", err)
	}
}

// speaksZm stamps the protocol header a daemon's every answer carries.
func speaksZm(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
		h.ServeHTTP(w, r)
	})
}

func TestMeasuresNeedOneProject(t *testing.T) {
	c := client.WithHandler(speaksZm(fakeAsks(3)))
	f := measuresFlags{days: 1}
	if _, err := computeMeasures(context.Background(), c, "", &f); err == nil || !strings.Contains(err.Error(), "name the project") {
		t.Fatalf("no project: %v", err)
	}
}

// The project the table is for reaches every read: asks, mail and people.
func TestMeasuresReadOnlyTheNamedProject(t *testing.T) {
	seen := map[string]string{}
	c := client.WithHandler(speaksZm(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.URL.Query().Get("project")
		switch r.URL.Path {
		case "/v0/mail":
			_ = json.NewEncoder(w).Encode(orchapi.MailList{Mail: []orchapi.Mail{}})
		case "/v0/people":
			_ = json.NewEncoder(w).Encode(api.PeopleList{})
		default:
			_ = json.NewEncoder(w).Encode(orchapi.AskList{Asks: []orchapi.Ask{}})
		}
	})))
	f := measuresFlags{days: 1}
	if _, err := computeMeasures(context.Background(), c, "demo/area", &f); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v0/asks", "/v0/mail", "/v0/people"} {
		if seen[path] != "demo" {
			t.Errorf("%s was read for project %q, want demo", path, seen[path])
		}
	}
}

// A daemon that ignores since and until answers its newest page every time.
func TestCollectMailShortReadOnRequest(t *testing.T) {
	stuck := client.WithHandler(speaksZm(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := orchapi.MailList{Mail: []orchapi.Mail{}}
		for i := 0; i < mailPageSize; i++ {
			out.Mail = append(out.Mail, orchapi.Mail{ID: fmt.Sprintf("M%05d", i), Created: time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Minute)})
		}
		_ = json.NewEncoder(w).Encode(out)
	})))
	if _, _, err := collectMail(context.Background(), stuck, "", time.Time{}, time.Now(), false); err == nil {
		t.Fatal("a daemon that cannot window was read silently")
	}
	got, note, err := collectMail(context.Background(), stuck, "", time.Time{}, time.Now(), true)
	if err != nil || len(got) != mailPageSize || !strings.Contains(note, "does not take since and until") || !strings.Contains(note, "back to 2030-02-28 15:41:00 UTC") {
		t.Fatalf("--allow-short: %d mails, note %q, %v", len(got), note, err)
	}
}

// A short read is said at the top of the table and of the mailed report, and does not
// use up the window's one complete report.
func TestShortReadIsSaidAndKeepsItsOwnReportKey(t *testing.T) {
	var key, body string
	old := speaksZm(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/mail":
			if r.Method == http.MethodPost {
				var req orchapi.SendMailRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				key, body = req.ClientRequestID, req.Body
				_ = json.NewEncoder(w).Encode(orchapi.Mail{ID: "M1", Recipient: "owners"})
				return
			}
			_ = json.NewEncoder(w).Encode(orchapi.MailList{Mail: []orchapi.Mail{}})
		case "/v0/people":
			_ = json.NewEncoder(w).Encode(api.PeopleList{})
		default:
			out := orchapi.AskList{Asks: []orchapi.Ask{}}
			for i := 0; i < 200; i++ {
				out.Asks = append(out.Asks, orchapi.Ask{ID: fmt.Sprintf("A%05d", i), Created: time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)})
			}
			_ = json.NewEncoder(w).Encode(out)
		}
	}))
	c := client.WithHandler(old)
	f := measuresFlags{days: 1, allowShort: true}
	rep, err := computeMeasures(context.Background(), c, "demo", &f)
	if err != nil || len(rep.Notes) != 1 {
		t.Fatalf("notes %v, %v", rep.Notes, err)
	}
	if out := measures.Render(rep, measures.Text); !strings.HasPrefix(out, "NOTE: this daemon cannot page") {
		t.Fatalf("the note is not first:\n%s", out)
	}
	if _, err := sendMeasuresReport(context.Background(), c, rep, "owners"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(key, "-short") || !strings.Contains(body, "NOTE: this daemon cannot page") {
		t.Fatalf("report key %q, body %q", key, body)
	}
}
