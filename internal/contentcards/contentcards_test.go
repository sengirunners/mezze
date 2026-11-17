package contentcards

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// The judge's eval cases (testdata/content): content/ are
// cards that ask a person to approve wording that goes out to people outside
// the team; other/ are cards that are not, the near misses among them. Every
// run checks each case's shape; with ZM_JUDGE_EVAL=1 the configured model
// (ZM_JUDGE, default claude:haiku) judges each case ZM_JUDGE_EVAL_RUNS times
// (default 3) and every run must agree. Run it after changing
// contentcards.md.
func TestContentJudgeEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	for dir, want := range map[string]judge.Status{"content": judge.Pass, "other": judge.Fail} {
		files, err := filepath.Glob(filepath.Join("testdata", "content", dir, "*.json"))
		if err != nil || len(files) < 4 {
			t.Fatalf("%s cases: want at least 4, have %v %v", dir, files, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Why   string `json:"why"`
				Input Input  `json:"input"`
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil || c.Why == "" || c.Input.Title == "" || c.Input.Question == "" {
				t.Errorf("%s: not a content-card case: %v", f, err)
				continue
			}
			if !live {
				continue
			}
			t.Run(dir+"/"+strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
				t.Parallel()
				for i := range runs {
					v := Verdict(context.Background(), judge.FromEnv(nil), c.Input)
					if v.Status != want {
						t.Errorf("run %d: want %s, got %s %s raw %s", i+1, want, v.Status, v.Reason, v.Raw)
					}
				}
			})
		}
	}
	if !live {
		t.Log("cases checked for shape; ZM_JUDGE_EVAL=1 runs them through the model")
	}
}

// byTitle is a fake judge for the counting tests: a card whose title starts
// "content" passes, "held" gets no answer, any other fails.
func byTitle() judge.Fake {
	return judge.Fake{Fn: func(p string) (json.RawMessage, error) {
		switch {
		case strings.Contains(p, `"title": "content`):
			return json.RawMessage(`{"pass":true,"fixes":[],"reason":"x"}`), nil
		case strings.Contains(p, `"title": "held`):
			return nil, errors.New("model down")
		}
		return json.RawMessage(`{"pass":false,"fixes":[],"reason":"x"}`), nil
	}}
}

func ask(id, title, at, state string) orchapi.Ask {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	a := orchapi.Ask{ID: id, Created: t, Card: &orchapi.AskCard{Title: title, Question: "q?"}}
	switch state {
	case "answered":
		a.Decision = &orchapi.Decision{}
	case "withdrawn":
		a.Status = orchapi.AskWithdrawn
	}
	return a
}

func window() Options {
	return Options{Since: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC), Caller: byTitle()}
}

func TestCountMergesContentCardsPerDay(t *testing.T) {
	asks := []orchapi.Ask{
		ask("1", "content one", "2030-01-02T09:00:00Z", "answered"),
		ask("2", "content two", "2030-01-02T15:00:00Z", "withdrawn"),
		ask("3", "content three", "2030-01-02T23:00:00Z", "answered"),
		ask("4", "content four", "2030-01-04T10:00:00Z", "open"),
		ask("5", "price question", "2030-01-04T11:00:00Z", "answered"),
		ask("6", "content out of window", "2030-01-09T11:00:00Z", "answered"),
		ask("1", "content one", "2030-01-02T09:00:00Z", "answered"), // listed twice
	}
	r := Count(context.Background(), asks, window())
	if r.Cards != 5 || r.Content != 4 || r.DailyCards != 2 || r.Merged != 2 {
		t.Fatalf("cards %d content %d daily %d merged %d, want 5 4 2 2", r.Cards, r.Content, r.DailyCards, r.Merged)
	}
	if r.Answered != 2 || r.Withdrawn != 1 || r.Open != 1 || r.NotSent != 2 || !r.Complete {
		t.Fatalf("%+v", r)
	}
	if len(r.Days) != 2 || r.Days[0].Date != "2030-01-02" || r.Days[0].Content != 3 {
		t.Fatalf("days %+v", r.Days)
	}
	if !strings.Contains(r.Row().Value, "2 of 4") {
		t.Fatalf("row %+v", r.Row())
	}
}

func TestCountUsesTheGivenTimezone(t *testing.T) {
	loc := time.FixedZone("test", -8*3600)
	asks := []orchapi.Ask{
		ask("1", "content a", "2030-01-02T03:00:00Z", "answered"), // 01-01 19:00 in test
		ask("2", "content b", "2030-01-02T09:00:00Z", "answered"), // 01-02 01:00 in test
	}
	o := window()
	if r := Count(context.Background(), asks, o); r.DailyCards != 1 {
		t.Fatalf("utc: daily %d, want 1", r.DailyCards)
	}
	o.Location = loc
	if r := Count(context.Background(), asks, o); r.DailyCards != 2 || r.Merged != 0 || r.Timezone != "test" {
		t.Fatalf("test zone: %+v", r)
	}
}

func TestHeldCardIsNeverContentAndMakesTheReportIncomplete(t *testing.T) {
	asks := []orchapi.Ask{ask("1", "held card", "2030-01-02T09:00:00Z", "answered"), ask("2", "content a", "2030-01-02T10:00:00Z", "answered")}
	r := Count(context.Background(), asks, window())
	if r.Held != 1 || r.Content != 1 || r.Complete || !strings.Contains(r.Row().Note, "floor") {
		t.Fatalf("%+v %+v", r, r.Row())
	}
	// No provider at all: everything is held, nothing is content.
	o := window()
	o.Caller = nil
	if r := Count(context.Background(), asks, o); r.Held != 2 || r.Content != 0 || r.Complete {
		t.Fatalf("nil caller: %+v", r)
	}
}

func TestInputOfFallsBackToTheNote(t *testing.T) {
	in := InputOf(orchapi.Ask{Body: "Approve the post?\nmore", Options: []string{"yes", "no"}})
	if in.Title != "Approve the post?" || len(in.Choices) != 2 {
		t.Fatalf("%+v", in)
	}
}

func TestCardsWithoutAnIDAreNotCollapsed(t *testing.T) {
	asks := []orchapi.Ask{ask("", "content a", "2030-01-02T09:00:00Z", "answered"), ask("", "content b", "2030-01-02T10:00:00Z", "answered")}
	if r := Count(context.Background(), asks, window()); r.Content != 2 {
		t.Fatalf("content %d, want 2", r.Content)
	}
}

func TestCappedListMakesTheReportIncomplete(t *testing.T) {
	o := window()
	o.Capped = true
	r := Count(context.Background(), []orchapi.Ask{ask("1", "content a", "2030-01-02T09:00:00Z", "answered")}, o)
	if r.Complete || !strings.Contains(r.Row().Note, "cap") || !strings.Contains(r.Text(), "INCOMPLETE") {
		t.Fatalf("%+v %s", r, r.Text())
	}
	if !strings.Contains(r.Text(), "2030-01-01 to 2030-01-07") {
		t.Fatalf("the window prints its last day, not the exclusive end: %s", r.Text())
	}
}
