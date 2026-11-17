package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// The step judge's eval cases (testdata/stepjudge): fail/ are steps that
// are a decision, that an agent could do without the person, that lean on
// what the agent cannot see, whose check proves nothing, that are a goal
// rather than steps, that need the person's own hands, and one that talks to
// the judge; pass/ are updates, an invite, an app install and a setting on
// the person's own machine and accounts. Every run checks each case's shape
// and limits; with ZM_JUDGE_EVAL=1 the configured model (ZM_JUDGE, default
// claude:haiku) judges each case ZM_JUDGE_EVAL_RUNS times (default 3), as
// the judge's three votes, and every run must agree. Run it after changing
// stepjudge.md.
func TestStepJudgeEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	for _, want := range []judge.Status{judge.Fail, judge.Pass} {
		files, err := filepath.Glob(filepath.Join("testdata", "stepjudge", string(want), "*.json"))
		if err != nil || len(files) < 3 {
			t.Fatalf("%s cases: want at least 3, have %v %v", want, files, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Fails    string         `json:"why_it_fails"`
				Passes   string         `json:"why_it_passes"`
				Decision bool           `json:"want_decision"`
				Input    stepJudgeInput `json:"input"`
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil || c.Fails+c.Passes == "" || c.Input.Person == "" {
				t.Errorf("%s: not a step judge case: %v", f, err)
				continue
			}
			// Each case is a step the plain limits let through: only meaning sends it back.
			if _, _, _, err := cleanStepText(c.Input.Title, c.Input.Brief, c.Input.Check); err != nil {
				t.Errorf("%s: past the plain limits: %v", f, err)
				continue
			}
			if !live {
				continue
			}
			name := string(want) + "/" + strings.TrimSuffix(filepath.Base(f), ".json")
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for i := range runs {
					v := stepVote(context.Background(), judge.FromEnv(nil), c.Input, 2*time.Minute)
					switch {
					case v.Status != want:
						t.Errorf("run %d: want %s, got %s %s fixes %q raw %s", i+1, want, v.Status, v.Reason, v.Fixes, v.Raw)
					case c.Decision && !isDecision(v.Verdict):
						t.Errorf("run %d: a decision, but its first fix is %q (raw %s)", i+1, v.Fixes, v.Raw)
					default:
						t.Logf("run %d: %s risk %s fixes %q", i+1, v.Status, v.Risk, v.Fixes)
					}
				}
			})
		}
	}
	if !live {
		t.Log("cases checked for shape; ZM_JUDGE_EVAL=1 runs them through the model")
	}
}

// The producer's rule over one vote: a pass needs the model's pass and every
// answer fit; a real choice leads the fixes; an answer outside the schema
// (one missing, a risk outside the three) is held, never a pass.
func TestStepRule(t *testing.T) {
	fit := func(over map[string]any) judge.Caller {
		ans := map[string]any{"reasoning": "r", "needs_the_person": true, "agent_doable": true, "numbered_to_done": true, "self_contained": true,
			"no_real_choice": true, "check_proves_done": true, "title_plain": true, "risk": "none", "pass": true, "fixes": []string{}}
		for k, v := range over {
			if v == nil {
				delete(ans, k)
			} else {
				ans[k] = v
			}
		}
		b, _ := json.Marshal(ans)
		return judge.Fake{Answer: b}
	}
	in := stepJudgeInput{Person: "bea", Title: "t", Brief: "1. b", Check: "true"}
	for name, c := range map[string]struct {
		over     map[string]any
		want     judge.Status
		decision bool
		risk     string
	}{
		"all fit":            {nil, judge.Pass, false, "none"},
		"spending":           {map[string]any{"risk": orchapi.RiskSpending}, judge.Pass, false, orchapi.RiskSpending},
		"model fails it":     {map[string]any{"pass": false, "fixes": []string{"x"}}, judge.Fail, false, "none"},
		"pass with a gap":    {map[string]any{"agent_doable": false}, judge.Fail, false, "none"},
		"a decision":         {map[string]any{"no_real_choice": false, "self_contained": false, "pass": false}, judge.Fail, true, "none"},
		"an answer missing":  {map[string]any{"title_plain": nil}, judge.Held, false, ""},
		"a risk not in list": {map[string]any{"risk": "big"}, judge.Held, false, ""},
	} {
		v := stepVote(context.Background(), fit(c.over), in, time.Second)
		if v.Status != c.want || isDecision(v.Verdict) != c.decision || (c.want != judge.Held && v.Risk != c.risk) {
			t.Errorf("%s: %s decision %v risk %q fixes %q", name, v.Status, isDecision(v.Verdict), v.Risk, v.Fixes)
		}
		if v.Status == judge.Fail && len(v.Fixes) == 0 {
			t.Errorf("%s: a fail with no fix", name)
		}
	}
}
