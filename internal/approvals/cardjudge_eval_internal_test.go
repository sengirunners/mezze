package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/project"
)

// The card judge's eval cases (testdata/cardjudge): fail/ are cards that
// must be sent back (the three thin asks of 2026-11-13 among them), pass/
// cards that must reach the owner. Every run checks each case is the
// judge's input shape and that there are enough of each. With
// ZM_JUDGE_EVAL=1 the configured model (ZM_JUDGE, default claude:haiku)
// judges each case ZM_JUDGE_EVAL_RUNS times (default 3), on the machine's
// plan login, and every run must agree. Run it after changing cardjudge.md
// and record the runs in the PR.
func TestCardJudgeEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	type evalCase struct {
		Fails  string `json:"why_it_fails"`
		Passes string `json:"why_it_passes"`
		// RepeatOf, when the case has open cards, is the one it repeats
		// ("" for none); live runs check it too.
		RepeatOf *string `json:"want_repeat_of"`
		// Risk is the action's risk a pass must find (risk.go): every pass
		// case with an action names it ("" for none); live runs check it.
		Risk  *string        `json:"want_risk"`
		Input cardJudgeInput `json:"input"`
	}
	risks := map[string]int{}
	for _, want := range []judge.Status{judge.Fail, judge.Pass} {
		files, err := filepath.Glob(filepath.Join("testdata", "cardjudge", string(want), "*.json"))
		if err != nil || len(files) < 8 {
			t.Fatalf("%s cases: want at least 8, have %d (%v)", want, len(files), err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var c evalCase
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			err = dec.Decode(&c)
			if err != nil || (want == judge.Fail) != (c.Fails != "") || (want == judge.Pass) != (c.Passes != "") || c.Input.Card.Question == "" || len(c.Input.Card.Choices) < 2 {
				t.Errorf("%s: not a card judge case: %v", f, err)
				continue
			}
			// As judgeInput counts it, never as the case file says.
			jc := c.Input.Card
			c.Input.Card.FaceWords = faceWords(orchapi.AskCard{Question: jc.Question, Context: jc.Context,
				Choices: jc.Choices, Why: jc.Why, AfterPress: jc.AfterPress})
			if (c.RepeatOf != nil) != (len(c.Input.OpenCards) > 0) || (c.RepeatOf != nil && *c.RepeatOf != "" &&
				!slices.ContainsFunc(c.Input.OpenCards, func(o judgeOpenCard) bool { return o.ID == *c.RepeatOf })) {
				t.Errorf("%s: a case with open cards names the one it repeats (want_repeat_of, \"\" for none), and only then", f)
				continue
			}
			if want == judge.Pass && (c.Risk != nil) != (c.Input.Action != nil) {
				t.Errorf("%s: a pass case with an action names its risk (want_risk, \"\" for none), and only then", f)
				continue
			}
			if c.Risk != nil {
				risks[*c.Risk]++
			}
			if !live {
				continue
			}
			name := string(want) + "/" + strings.TrimSuffix(filepath.Base(f), ".json")
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for i := range runs {
					v := cardVerdict(context.Background(), judge.FromEnv(nil), c.Input, 2*time.Minute)
					if v.Status != want || (c.RepeatOf != nil && v.RepeatOf != *c.RepeatOf) || (c.Risk != nil && v.Risk != *c.Risk) {
						t.Errorf("run %d: want %s (repeat %v, risk %v), got %s (repeat %q, risk %q) %s fixes %q raw %s", i+1, want, c.RepeatOf, c.Risk, v.Status, v.RepeatOf, v.Risk, v.Reason, v.Fixes, v.Raw)
					} else {
						t.Logf("run %d: %s risk %q fixes %q", i+1, v.Status, v.Risk, v.Fixes)
					}
				}
			})
		}
	}
	for _, r := range []string{"", "irreversible", "spending"} {
		if risks[r] < 2 {
			t.Errorf("pass cases with want_risk %q: want at least 2, have %d", r, risks[r])
		}
	}
	if !live {
		t.Log("cases checked for shape; ZM_JUDGE_EVAL=1 runs them through the model")
	}
}

// A card's verdict is the majority of its votes, each the producer's rule
// over the model's three answers: two passes pass, two fails fail, a split
// around a vote with no answer is held, never a pass.
func TestCardVotes(t *testing.T) {
	const (
		ok        = `{"reasoning":"","decidable":true,"person_step":false,"step_carried":true,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`
		okSaysNo  = `{"reasoning":"","decidable":true,"person_step":false,"step_carried":true,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":false,"fixes":["nitpick"]}`
		bad       = `{"reasoning":"","decidable":false,"person_step":false,"step_carried":true,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":false,"fixes":["say what it is"]}`
		badSaysOK = `{"reasoning":"","decidable":true,"person_step":true,"step_carried":false,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`
	)
	in := cardJudgeInput{Card: judgeCard{Question: "q?"}}
	for _, c := range []struct {
		name    string
		answers []string // "" = the call errs
		want    judge.Status
		fix     string
	}{
		{"all pass", []string{ok, ok, ok}, judge.Pass, ""},
		{"answers outrank the model's pass", []string{okSaysNo, okSaysNo, bad}, judge.Pass, ""},
		{"two fails fail", []string{ok, bad, bad}, judge.Fail, "say what it is"},
		{"a pass that leaves a step fails", []string{badSaysOK, badSaysOK, ok}, judge.Fail, "carry the whole step"},
		{"split around no answer is held", []string{ok, bad, ""}, judge.Held, "split"},
		{"no answers are held", []string{"", "", ok}, judge.Held, "down"},
		{"two answers agree past one missing", []string{ok, "", ok}, judge.Pass, ""},
	} {
		var mu sync.Mutex
		i := 0
		fake := judge.Fake{Fn: func(string) (json.RawMessage, error) {
			mu.Lock()
			defer mu.Unlock()
			a := c.answers[i]
			i++
			if a == "" {
				return nil, errors.New("the provider is down")
			}
			return json.RawMessage(a), nil
		}}
		v := cardVerdict(context.Background(), fake, in, time.Second)
		if v.Status != c.want || (c.fix != "" && !strings.Contains(v.Reason+strings.Join(v.Fixes, "|"), c.fix)) || (c.want == judge.Pass && len(v.Fixes) > 0) {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
}

// An action's risk (risk.go) is the one a majority of votes names, as pass
// and fail are, else (a split) the highest any answering vote names; a
// missing or unknown risk is held; a card with no action has none.
func TestCardVotesRisk(t *testing.T) {
	ans := func(risk string) string {
		m := map[string]any{"reasoning": "", "decidable": true, "person_step": false, "step_carried": true, "brief_needed": false,
			"brief_complete": true, "asks_decision": true, "ruled_by": "", "repeat_of": "", "pass": true, "fixes": []string{}}
		if risk != "-" {
			m["risk"] = risk
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	withAction := cardJudgeInput{Card: judgeCard{Question: "q?"}, Action: &judgeAction{Kind: "command", On: "a"}}
	for _, c := range []struct {
		name    string
		in      cardJudgeInput
		answers []string
		status  judge.Status
		risk    string
	}{
		{"none", withAction, []string{ans("none"), ans("none"), ans("none")}, judge.Pass, ""},
		{"a majority holds", withAction, []string{ans("none"), ans("spending"), ans("none")}, judge.Pass, ""},
		{"a majority raises", withAction, []string{ans("spending"), ans("none"), ans("spending")}, judge.Pass, "spending"},
		{"a split takes the highest", withAction, []string{ans("spending"), ans("irreversible"), ans("none")}, judge.Pass, "spending"},
		{"irreversible over none", withAction, []string{ans("irreversible"), ans("none"), ans("spending")}, judge.Pass, "spending"},
		{"a split around no answer takes the highest", withAction, []string{ans("none"), ans("spending"), "x"}, judge.Pass, "spending"},
		{"a missing risk is held", withAction, []string{ans("-"), ans("-"), ans("none")}, judge.Held, ""},
		{"an unknown risk is held", withAction, []string{ans("maybe"), ans("maybe"), ans("none")}, judge.Held, ""},
		{"no action, no risk", cardJudgeInput{Card: judgeCard{Question: "q?"}}, []string{ans("spending"), ans("spending"), ans("spending")}, judge.Pass, ""},
	} {
		var mu sync.Mutex
		i := 0
		fake := judge.Fake{Fn: func(string) (json.RawMessage, error) {
			mu.Lock()
			defer mu.Unlock()
			a := c.answers[i]
			i++
			return json.RawMessage(a), nil
		}}
		v := cardVerdict(context.Background(), fake, c.in, time.Second)
		if v.Status != c.status || v.Risk != c.risk {
			t.Errorf("%s: %s risk %q %s", c.name, v.Status, v.Risk, v.Reason)
		}
	}
}

// The answers zm acts on beyond pass and fail:
// a ruling or a status report sends the card back with zm's own fix (the
// ruling named and quoted, never "go ahead"); a ruling or open card the
// input does not hold, or a missing answer, is held; the open card a card
// repeats is the one a majority names, and a split with no majority holds.
func TestCardVotesRulingsStatusRepeats(t *testing.T) {
	ans := func(status bool, ruled, repeat string) string {
		b, _ := json.Marshal(map[string]any{"reasoning": "", "decidable": true, "person_step": false, "step_carried": true,
			"brief_needed": false, "brief_complete": true, "asks_decision": !status, "ruled_by": ruled, "repeat_of": repeat, "risk": "none", "pass": true, "fixes": []string{}})
		return string(b)
	}
	const noStatus = `{"reasoning":"","decidable":true,"person_step":false,"step_carried":true,"brief_needed":false,"brief_complete":true,"ruled_by":"","repeat_of":"","risk":"none","pass":true,"fixes":[]}`
	in := cardJudgeInput{Card: judgeCard{Question: "q?"},
		Rulings:   []Ruling{{Name: "ship-when-green", Text: "Agents release once checks are green; the owners are not asked."}},
		OpenCards: []judgeOpenCard{{ID: "OPEN1"}, {ID: "OPEN2"}}}
	for _, c := range []struct {
		name    string
		answers []string
		want    judge.Status
		repeat  string
		fix     string
	}{
		{"a ruling sends it back, named and quoted", []string{ans(false, "ship-when-green", ""), ans(false, "ship-when-green", ""), ans(false, "", "")}, judge.Fail, "", `"ship-when-green" already answers this question, so the card was not sent: "Agents release once checks are green`},
		{"an unknown ruling is held", []string{ans(false, "made-up", ""), ans(false, "made-up", ""), ans(false, "", "")}, judge.Held, "", ""},
		{"a status report is sent back", []string{ans(true, "", ""), ans(true, "", ""), ans(false, "", "")}, judge.Fail, "", "a status"},
		{"a missing answer is held", []string{noStatus, noStatus, noStatus}, judge.Held, "", "shape"},
		{"a repeat the majority names", []string{ans(false, "", "OPEN1"), ans(false, "", ""), ans(false, "", "OPEN1")}, judge.Pass, "OPEN1", ""},
		{"a repeat of a card it was not shown is held", []string{ans(false, "", "OPEN9"), ans(false, "", "OPEN9"), ans(false, "", "OPEN9")}, judge.Held, "", ""},
		{"repeats that split hold", []string{ans(false, "", "OPEN1"), ans(false, "", ""), ans(false, "", "OPEN2")}, judge.Held, "", "split"},
		{"an empty answer written as quote marks is empty", []string{ans(false, `""`, `""`), ans(false, `""`, `""`), ans(false, "", "")}, judge.Pass, "", ""},
		{"a status bounce stands when votes split on the repeat", []string{ans(true, "", "OPEN1"), ans(true, "", ""), ans(true, "", "OPEN2")}, judge.Fail, "", "a status"},
		{"a majority for none is none", []string{ans(false, "", "OPEN1"), ans(false, "", ""), ans(false, "", "")}, judge.Pass, "", ""},
	} {
		var mu sync.Mutex
		i := 0
		fake := judge.Fake{Fn: func(string) (json.RawMessage, error) {
			mu.Lock()
			defer mu.Unlock()
			a := c.answers[i]
			i++
			return json.RawMessage(a), nil
		}}
		v := cardVerdict(context.Background(), fake, in, time.Second)
		all := v.Reason + strings.Join(v.Fixes, "|")
		if v.Status != c.want || v.RepeatOf != c.repeat || (c.fix != "" && !strings.Contains(all, c.fix)) {
			t.Errorf("%s: %+v (repeat %q)", c.name, v.Verdict, v.RepeatOf)
		}
		for _, word := range []string{"proceed", "go ahead", "carry on"} {
			if strings.Contains(strings.ToLower(all), word) {
				t.Errorf("%s: the judge told the agent to %s: %q", c.name, word, all)
			}
		}
	}
}

// [[rulings]] in project.toml: parsed, checked and written back as data.
func TestRulingsSection(t *testing.T) {
	var sec rulingsSection
	v, err := sec.Parse("project.toml", []map[string]any{{"name": "ship-when-green", "text": "Agents release once checks are green."}})
	if err != nil || sec.Validate(v, project.File{}) != nil {
		t.Fatalf("parse: %v %v", v, err)
	}
	if got := sec.Encode(v); !strings.Contains(got, "[[rulings]]\nname = \"ship-when-green\"\ntext = \"Agents release once checks are green.\"") {
		t.Fatalf("encode: %q", got)
	}
	if _, err := sec.Parse("project.toml", []map[string]any{{"name": "x", "why": "y"}}); err == nil {
		t.Error("an unknown key parsed")
	}
	for name, rs := range map[string][]Ruling{
		"bad name":  {{Name: "Ship When Green", Text: "t"}},
		"no text":   {{Name: "a", Text: " "}},
		"twice":     {{Name: "a", Text: "t"}, {Name: "a", Text: "u"}},
		"long text": {{Name: "a", Text: strings.Repeat("x", maxRulingText+1)}},
	} {
		if sec.Validate(rs, project.File{}) == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
