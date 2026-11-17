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
)

// The irreversible judge's eval cases (testdata/irreversible):
// fail/ are asks whose choice or work takes a step that cannot be undone
// (two the old word list missed among them); pass/ are reversible, internal
// choices, some naming risky words as things. Every run checks each case's
// shape; with ZM_JUDGE_EVAL=1 the configured model (ZM_JUDGE, default
// claude:haiku) judges each case ZM_JUDGE_EVAL_RUNS times (default 3), and
// every run must agree. Run it after changing irreversible.md.
func TestIrreversibleJudgeEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	for _, want := range []judge.Status{judge.Fail, judge.Pass} {
		files, err := filepath.Glob(filepath.Join("testdata", "irreversible", string(want), "*.json"))
		if err != nil || len(files) < 3 {
			t.Fatalf("%s cases: want at least 3, have %v %v", want, files, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Fails  string            `json:"why_it_fails"`
				Passes string            `json:"why_it_passes"`
				Input  irreversibleInput `json:"input"`
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil || c.Fails+c.Passes == "" || c.Input.Card == nil || len(c.Input.Card.Choices) < 2 {
				t.Errorf("%s: not an irreversible case: %v", f, err)
				continue
			}
			if !live {
				continue
			}
			name := string(want) + "/" + strings.TrimSuffix(filepath.Base(f), ".json")
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for i := range runs {
					v := irreversibleVerdict(context.Background(), judge.FromEnv(nil), c.Input, 2*time.Minute)
					if v.Status != want {
						t.Errorf("run %d: want %s, got %s %s fixes %q raw %s", i+1, want, v.Status, v.Reason, v.Fixes, v.Raw)
					} else {
						t.Logf("run %d: %s %s", i+1, v.Status, v.Reason)
					}
				}
			})
		}
	}
	if !live {
		t.Log("cases checked for shape; ZM_JUDGE_EVAL=1 runs them through the model")
	}
}
