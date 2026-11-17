package orchapi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/judge"
)

// The mail-chore judge's eval cases (testdata/mailchore): fail/
// are reports that put a step to their reader, the old regex's misses
// among them; pass/ are reports in the agent's own voice, status lines and
// the regex's false alarms. Every run checks each case's shape; with
// ZM_JUDGE_EVAL=1 the configured model (ZM_JUDGE, default claude:haiku)
// judges each case ZM_JUDGE_EVAL_RUNS times (default 3) on the machine's
// plan login, and every run must agree. Run it after changing
// mailchore.md.
func TestMailChoreJudgeEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	for _, want := range []judge.Status{judge.Fail, judge.Pass} {
		files, err := filepath.Glob(filepath.Join("testdata", "mailchore", string(want), "*.json"))
		if err != nil || len(files) < 3 {
			t.Fatalf("%s cases: want at least 3, have %v %v", want, files, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Fails  string         `json:"why_it_fails"`
				Passes string         `json:"why_it_passes"`
				Input  MailChoreInput `json:"input"`
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil || c.Fails+c.Passes == "" || c.Input.Body == "" || c.Input.Reader == "" {
				t.Errorf("%s: not a mail-chore case: %v", f, err)
				continue
			}
			if !live {
				continue
			}
			name := string(want) + "/" + strings.TrimSuffix(filepath.Base(f), ".json")
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for i := range runs {
					v := MailChoreVerdict(context.Background(), judge.FromEnv(nil), c.Input)
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
