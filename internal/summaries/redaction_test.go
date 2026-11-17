package summaries

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

func TestSummaryPrivateBindingsDisambiguateLongIDsAndShortTitles(t *testing.T) {
	s, owner, _, p, f := fixture(t)
	ctx := context.Background()
	c, _ := s.config(ctx, p.ID)
	idA := "bd:" + strings.Repeat("x", 110) + "a"
	idB := "bd:" + strings.Repeat("x", 110) + "b"
	unicodeTitle := "sensitive-canary\n\u202e\a" + strings.Repeat("私有", 110)
	f.rows[orchapi.WorkOpen] = orchapi.WorkList{Items: []orchapi.WorkItem{
		{Project: p.ID, ID: idA, Title: "OK"}, {Project: p.ID, ID: idB, Title: "OK"},
		{Project: p.ID, ID: "bd:unicode", Title: unicodeTitle}, {Project: p.ID, ID: "bd:other", Title: "unrelated OK status"},
	}}
	out, err := s.generate(ctx, owner, c, "2026-11-04")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{idA}, Texts: []string{"OK"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, p.ID, out.Day, c.Version)
	if strings.Count(got.Body, " — OK\n") != 1 || !strings.Contains(got.Body, "unrelated OK status") {
		t.Fatalf("unrelated short title changed: %s", got.Body)
	}
	if err = s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{"unicode"}, Texts: []string{unicodeTitle}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, p.ID, out.Day, c.Version)
	if strings.Contains(got.Body, "sensitive-canary") || strings.Contains(got.Body, "私有") {
		t.Fatalf("transformed Unicode title retained: %s", got.Body)
	}
	replay, err := s.generate(ctx, owner, c, out.Day)
	if err != nil || replay.ID != got.ID || replay.Body != got.Body {
		t.Fatalf("immutable replay resurrected title: %+v %v", replay, err)
	}
	res := request(s, owner, "GET", "/v0/summaries?project=tools&q=sensitive-canary&limit=30", "")
	var list orchapi.SummaryList
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &list) != nil || len(list.Summaries) != 0 {
		t.Fatalf("redacted search retained title: %d %s", res.Code, res.Body.String())
	}
	res = request(s, owner, "GET", "/v0/summaries?project=tools", "")
	if strings.Contains(res.Body.String(), "title_bindings") || strings.Contains(res.Body.String(), "rendering") {
		t.Fatal("private archive bindings exposed by API")
	}
}

func TestSummaryBindingsSurviveIndependentPartialRedactions(t *testing.T) {
	s, owner, _, p, f := fixture(t)
	ctx := context.Background()
	c, _ := s.config(ctx, p.ID)
	f.rows[orchapi.WorkOpen] = orchapi.WorkList{Items: []orchapi.WorkItem{{Project: p.ID, ID: "bd:subject", Title: "prefix long-sensitive-canary OK"}}}
	out, err := s.generate(ctx, owner, c, "2026-11-04")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{"bd:other"}, Texts: []string{"long-sensitive-canary"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{"bd:subject"}, Texts: []string{"OK"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, p.ID, out.Day, c.Version)
	if strings.Contains(got.Body, "prefix") || strings.Contains(got.Body, " OK") {
		t.Fatalf("second redaction lost subject binding: %s", got.Body)
	}
}

func TestLegacySummaryRedactsRenderedVariantsWithoutGlobalShortReplacement(t *testing.T) {
	for i, title := range []string{"sensitive-canary-" + strings.Repeat("private", 40), "sensitive-canary\nprivate-detail", "OK"} {
		s, _, _, p, _ := fixture(t)
		ctx := context.Background()
		c, _ := s.config(ctx, p.ID)
		out := orchapi.ProjectSummary{ID: fmt.Sprint("legacy-", i), Project: p.ID, Day: "2026-11-04", ConfigVersion: c.Version, Generated: s.now(), Body: "• bd:secret — " + plain(title, 200) + "\n• bd:other — OK\n"}
		if _, err := s.Put(ctx, out); err != nil {
			t.Fatal(err)
		}
		if err := s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{"bd:secret"}, Texts: []string{title}}); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Get(ctx, p.ID, out.Day, c.Version)
		if strings.Contains(got.Body, "sensitive-canary") || strings.Contains(got.Body, "bd:secret — OK") || !strings.Contains(got.Body, "bd:other — OK") {
			t.Fatalf("legacy transform/short guard: %s", got.Body)
		}
	}
}

func TestTitleTruncationSetsPartialWithoutAnySourceFailure(t *testing.T) {
	s, owner, _, p, f := fixture(t)
	c, _ := s.config(context.Background(), p.ID)
	f.rows[orchapi.WorkOpen] = orchapi.WorkList{Items: []orchapi.WorkItem{{Project: p.ID, ID: "bd:long", Title: strings.Repeat("界", 201)}}}
	out, err := s.generate(context.Background(), owner, c, "2026-11-04")
	if err != nil || !out.Partial || !strings.Contains(strings.Join(out.Notes, " "), "200 characters") || out.Counts[orchapi.WorkOpen] != 1 {
		t.Fatalf("unreported title truncation %+v %v", out, err)
	}
}

func TestScheduleEditsPublishContentFreeScopedHints(t *testing.T) {
	s, owner, guest, p, _ := fixture(t)
	ctx := context.Background()
	bus, err := events.New(ctx, s.d.Store, "mini")
	if err != nil {
		t.Fatal(err)
	}
	s.d.Events = bus
	for _, enabled := range []bool{true, false} {
		res := request(s, owner, "PUT", "/v0/summaries/config?project=tools", fmt.Sprintf(`{"enabled":%t,"hour":7,"minute":0,"timezone":"UTC"}`, enabled))
		if res.Code != 200 {
			t.Fatalf("edit: %d %s", res.Code, res.Body.String())
		}
	}
	filter := events.Filter{Project: p.ID, Types: []string{orchapi.EventSummaryScheduleChanged}}
	hints, err := bus.Since(ctx, owner, "", filter, 10)
	if err != nil || len(hints) != 2 {
		t.Fatalf("config edits did not invalidate readers: %+v %v", hints, err)
	}
	for _, hint := range hints {
		var data map[string]string
		if json.Unmarshal(hint.Data, &data) != nil || len(data) != 1 || data["project"] != p.ID {
			t.Fatalf("hint is not content-free: %s", hint.Data)
		}
	}
	hidden, err := bus.Since(ctx, guest, "", filter, 10)
	if err != nil || len(hidden) != 0 {
		t.Fatalf("projectwide hint escaped area scope: %+v %v", hidden, err)
	}
}
