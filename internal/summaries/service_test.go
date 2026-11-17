package summaries

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

type fakeWork struct {
	orchcore.WorkService
	rows  map[orchapi.WorkStatus]orchapi.WorkList
	calls int
	fail  bool
}

func (f *fakeWork) ReadWork(_ context.Context, sub *authz.Subject, q orchapi.WorkQuery) (orchapi.WorkList, error) {
	f.calls++
	if f.fail {
		return orchapi.WorkList{}, fmt.Errorf("private canary credential")
	}
	if q.Project == "" || q.Limit != sourceLimit || !sub.Scope.WholeProject(q.Project) {
		return orchapi.WorkList{}, fmt.Errorf("invalid reader contract")
	}
	return f.rows[q.Status], nil
}

func fixture(t *testing.T) (*Service, *authz.Subject, *authz.Subject, api.Project, *fakeWork) {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm-summary")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := project.Open(ctx, st, &config.Config{Home: home, Human: "ivo", Machine: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	owner := reg.Local("")
	p, err := reg.Create(ctx, owner, api.NewProjectRequest{Slug: "tools"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reg.AddArea(ctx, owner, p.ID, api.Area{Name: "public"}); err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, owner, api.MintTokenRequest{As: "guest", Project: p.ID, Area: "public", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := reg.FromToken(ctx, tok.Secret, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWork{rows: map[orchapi.WorkStatus]orchapi.WorkList{}}
	d := &core.Deps{Store: st, Projects: reg, Config: &config.Config{Home: home, Machine: "mini"}}
	orchcore.Wire(d).Work = f
	s := New(d)
	s.now = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
	return s, owner, guest, p, f
}

func request(s *Service, sub *authz.Subject, method, path, body string) *httptest.ResponseRecorder {
	router := core.NewRouter()
	s.Routes(router)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(core.WithSubject(r.Context(), sub))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestSummaryCalendarAndDurableRetry(t *testing.T) {
	s, owner, _, p, f := fixture(t)
	ctx := context.Background()
	c := orchapi.SummaryConfig{Project: p.ID, Timezone: "America/New_York", Hour: 7}
	if err := validateConfig(&c); err != nil {
		t.Fatal(err)
	}
	f.rows[orchapi.WorkDone] = orchapi.WorkList{Items: []orchapi.WorkItem{
		{Project: p.ID, ID: "bd:first", Title: "First DST hour", Updated: time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{Project: p.ID, ID: "bd:last", Title: "Last local hour", Updated: time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC)},
		{Project: p.ID, ID: "bd:tomorrow", Title: "Next day excluded", Updated: time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)},
	}}
	a, err := s.generate(ctx, owner, c, "2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Counts[orchapi.WorkDone] != 2 || strings.Contains(a.Body, "Next day excluded") {
		t.Fatalf("DST day: %+v", a)
	}
	if f.calls != 4 {
		t.Fatal(f.calls)
	}
	other := New(s.d)
	other.now = s.now
	b, err := other.generate(ctx, owner, c, "2026-11-01")
	if err != nil || a.ID != b.ID || a.Body != b.Body || f.calls != 4 {
		t.Fatalf("durable replay %+v %v calls%d", b, err, f.calls)
	}
}

func TestSummaryPrivacyBeforeArchiveAndGeneration(t *testing.T) {
	s, owner, guest, p, f := fixture(t)
	r := request(s, owner, "POST", "/v0/summaries?project=tools", `{}`)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	before := f.calls
	for _, method := range []string{"GET", "POST"} {
		for _, route := range []string{"/v0/summaries", "/v0/summaries/config"} {
			for _, name := range []string{"tools", "missing"} {
				if method == "POST" && route != "/v0/summaries" {
					continue
				}
				res := request(s, guest, method, route+"?project="+name, `{}`)
				if res.Code != 404 || strings.Contains(res.Body.String(), p.ID) {
					t.Fatalf("scope %s %s: %d %s", method, name, res.Code, res.Body.String())
				}
			}
		}
	}
	if f.calls != before {
		t.Fatal("hidden generation reached work reader")
	}
	agent := *owner
	agent.Session = "mini/agent"
	agent.Address = agent.Session
	res := request(s, &agent, "PUT", "/v0/summaries/config?project=tools", `{"enabled":true,"hour":7,"minute":0,"timezone":"UTC"}`)
	if res.Code != 403 {
		t.Fatalf("agent configured schedule: %d", res.Code)
	}
}

func TestScheduleOptInAndRestartCatchUp(t *testing.T) {
	s, owner, _, p, f := fixture(t)
	s.tick(context.Background())
	if f.calls != 0 {
		t.Fatal("default schedule ran")
	}
	res := request(s, owner, "PUT", "/v0/summaries/config?project=tools", `{"enabled":true,"hour":7,"minute":0,"timezone":"America/New_York"}`)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	s.now = func() time.Time { return time.Date(2026, 11, 4, 11, 59, 0, 0, time.UTC) }
	s.tick(context.Background())
	if f.calls != 0 {
		t.Fatal("ran before configured local time")
	}
	s.now = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
	s.tick(context.Background())
	s.tick(context.Background())
	if f.calls != 4 {
		t.Fatalf("daily repeats: %d", f.calls)
	}
	c, _ := s.config(context.Background(), p.ID)
	v, err := s.Get(context.Background(), p.ID, "2026-11-03", c.Version)
	if err != nil || v.Day != "2026-11-03" {
		t.Fatal(v, err)
	}
	s.now = func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) }
	f.fail = true
	s.tick(context.Background())
	c, _ = s.config(context.Background(), p.ID)
	if c.LastError == "" || strings.Contains(c.LastError, "canary") || c.LastAttempt == nil {
		t.Fatalf("scheduled failure hidden or leaked: %+v", c)
	}
	f.fail = false
	s.tick(context.Background())
	c, _ = s.config(context.Background(), p.ID)
	if c.LastError != "" || c.LastGenerated == nil {
		t.Fatalf("successful retry not recorded: %+v", c)
	}
}

func TestPartialBoundsErrorsAndScrub(t *testing.T) {
	s, owner, _, p, f := fixture(t)
	ctx := context.Background()
	c, _ := s.config(ctx, p.ID)
	items := make([]orchapi.WorkItem, sourceLimit)
	for i := range items {
		items[i] = orchapi.WorkItem{Project: p.ID, ID: fmt.Sprintf("bd:%d", i), Title: strings.Repeat("界", 500)}
	}
	items[0].Title = "private canary title"
	f.rows[orchapi.WorkOpen] = orchapi.WorkList{Items: items, Behind: []orchapi.WorkBehind{{Project: p.ID, Source: "bd", State: "stale"}}}
	out, err := s.generate(ctx, owner, c, "2026-11-04")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Partial || len(out.Notes) == 0 || len(out.Body) > 64<<10 || !utf8.ValidString(out.Body) {
		t.Fatalf("bounds %+v", out)
	}
	if err = s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{"bd:0"}, Texts: []string{"private canary title"}}); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Get(ctx, p.ID, out.Day, c.Version)
	if strings.Contains(saved.Body, "private canary title") {
		t.Fatal("archive retained redacted title")
	}
	f.fail = true
	if _, err = s.generate(ctx, owner, c, "2026-11-03"); err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatal("source error not masked", err)
	}
	var count int
	_ = s.d.Store.Registry().QueryRow(`SELECT count(*) FROM project_summaries`).Scan(&count)
	if count != 1 {
		t.Fatalf("failed read saved a digest: %d", count)
	}
	res := request(s, owner, http.MethodGet, "/v0/summaries?project=tools&q=redacted&limit=1", "")
	var list orchapi.SummaryList
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &list) != nil || len(list.Summaries) != 1 {
		t.Fatalf("search %d %s", res.Code, res.Body.String())
	}
}

// The day's machine notices that went to no person's inbox are counted in the
// digest by label for the local day; a counter that fails marks it
// partial and the others still count.
func TestSummaryCountsMachineNotices(t *testing.T) {
	s, owner, _, p, _ := fixture(t)
	ctx := context.Background()
	c := orchapi.SummaryConfig{Project: p.ID, Timezone: "America/New_York", Hour: 7}
	if err := validateConfig(&c); err != nil {
		t.Fatal(err)
	}
	var from, to time.Time
	orchcore.AddNoticeCounter(s.d, func(_ context.Context, project string, f, t time.Time) (map[string]int, error) {
		from, to = f, t
		return map[string]int{"CI green, mailed to no one": 3, "nothing": 0}, nil
	})
	orchcore.AddNoticeCounter(s.d, func(context.Context, string, time.Time, time.Time) (map[string]int, error) {
		return map[string]int{"lease expiries, told to the holding session": 2}, nil
	})
	got, err := s.generate(ctx, owner, c, "2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	if !from.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, ny)) || !to.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, ny)) {
		t.Fatalf("counted %v to %v, want the local day", from, to)
	}
	if got.Partial || got.Notices["CI green, mailed to no one"] != 3 || got.Notices["lease expiries, told to the holding session"] != 2 || len(got.Notices) != 2 ||
		!strings.Contains(got.Body, "machine notices (in no one's inbox): 5\n• CI green, mailed to no one: 3\n• lease expiries, told to the holding session: 2\n") {
		t.Fatalf("notices: %+v", got)
	}
	orchcore.AddNoticeCounter(s.d, func(context.Context, string, time.Time, time.Time) (map[string]int, error) {
		return nil, fmt.Errorf("down")
	})
	got, err = s.generate(ctx, owner, c, "2026-11-02")
	if err != nil || !got.Partial || got.Notices["CI green, mailed to no one"] != 3 ||
		!strings.Contains(got.Body, "A machine notice count was unavailable") {
		t.Fatalf("a failing counter: %+v %v", got, err)
	}
}
