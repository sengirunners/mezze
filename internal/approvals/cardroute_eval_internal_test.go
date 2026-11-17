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

// The card router's eval cases (testdata/cardroute): route/ are cards whose
// question is one known outside party's to answer (want_party names it),
// stay/ cards that stay with the owners. Every run checks their shape; with
// ZM_JUDGE_EVAL=1 the configured model routes each ZM_JUDGE_EVAL_RUNS times
// (default 3) and every run must agree. Run it after changing cardroute.md.
func TestCardRouteEval(t *testing.T) {
	live := os.Getenv("ZM_JUDGE_EVAL") == "1"
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("ZM_JUDGE_EVAL_RUNS")); err == nil && n > 0 {
		runs = n
	}
	type evalCase struct {
		Why   string         `json:"why"`
		Want  int            `json:"want_party"`
		Input cardRouteInput `json:"input"`
	}
	for _, dir := range []string{"route", "stay"} {
		files, err := filepath.Glob(filepath.Join("testdata", "cardroute", dir, "*.json"))
		if err != nil || len(files) < 6 {
			t.Fatalf("%s cases: want at least 6, have %d (%v)", dir, len(files), err)
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
			if err != nil || c.Why == "" || (dir == "route") != (c.Want > 0) || c.Want > len(c.Input.Parties) || len(c.Input.Parties) == 0 ||
				c.Input.Card.Question == "" || len(c.Input.Card.Choices) < 2 {
				t.Errorf("%s: not a card router case: %v", f, err)
				continue
			}
			if !live {
				continue
			}
			t.Run(dir+"/"+strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
				t.Parallel()
				for i := range runs {
					if got := routeVotes(context.Background(), judge.FromEnv(nil), c.Input, 2*time.Minute); got != c.Want {
						t.Errorf("run %d: want party %d, got %d", i+1, c.Want, got)
					} else {
						t.Logf("run %d: party %d", i+1, got)
					}
				}
			})
		}
	}
	if !live {
		t.Log("cases checked for shape; ZM_JUDGE_EVAL=1 runs them through the model")
	}
}

// routeVotes routes only when every vote that answered, a majority, names
// one party; a vote to stay, a split, too few answers or a number off the
// list stays.
func TestRouteVotes(t *testing.T) {
	in := cardRouteInput{Parties: []routeParty{{N: 1}, {N: 2}}, Card: judgeCard{Question: "q?"}}
	for _, c := range []struct {
		votes []string // "" = the call errs
		want  int
	}{
		{[]string{"1", "1", "1"}, 1},
		{[]string{"2", "2", ""}, 2},
		{[]string{"2", "2", "0"}, 0},
		{[]string{"1", "2", "2"}, 0},
		{[]string{"1", "", ""}, 0},
		{[]string{"3", "3", "3"}, 0},
		{[]string{"3", "1", "1"}, 0},
		{[]string{"", "1", "1"}, 1},
		{[]string{"-1", "-1", "1"}, 0},
		{[]string{"-1", "1", "1"}, 0},
		{[]string{"", "", ""}, 0},
		{[]string{"0", "0", "0"}, 0},
	} {
		i := 0
		var mu = make(chan struct{}, 1)
		fake := judge.Fake{Fn: func(string) (json.RawMessage, error) {
			mu <- struct{}{}
			v := c.votes[i]
			i++
			<-mu
			if v == "" {
				return nil, os.ErrDeadlineExceeded
			}
			return json.RawMessage(`{"reasoning":"","party":` + v + `}`), nil
		}}
		if got := routeVotes(context.Background(), fake, in, time.Second); got != c.want {
			t.Errorf("votes %q: got %d, want %d", c.votes, got, c.want)
		}
	}
}
