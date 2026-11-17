package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Check is one conformance result.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// rawCaller is a driver that can be sent a request zm would never send
// (Process): Conform checks it refuses one instead of deciding.
type rawCaller interface {
	callRaw(ctx context.Context, verb string, args any) (json.RawMessage, error)
}

func (p *Process) callRaw(ctx context.Context, verb string, args any) (json.RawMessage, error) {
	var out json.RawMessage
	err := p.call(ctx, describeTimeout, verb, args, &out)
	return out, err
}

// Conform runs the checks every gate driver must pass (SPEC 18.3): describe's
// shape and bounds; a valid decision for each PR state, with and without
// final; match (when offered) answering valid ids; an unknown verb refused.
// It only reads: a gate must not write anything when asked.
func Conform(ctx context.Context, g Gate) []Check {
	var out []Check
	add := func(name string, err error) {
		c := Check{Name: name, OK: err == nil}
		if err != nil {
			c.Detail = err.Error()
		}
		out = append(out, c)
	}
	d, err := g.Describe(ctx)
	add("describe answers zm/v0 with describe and decide", err)
	if err != nil {
		return out
	}
	add("describe's bounds are within SPEC 18.3", func() error {
		if d.TimeoutSeconds < 0 || d.RecheckSeconds < 0 || d.RetentionSeconds < 0 {
			return fmt.Errorf("negative bound in %+v", d)
		}
		if d.TimeoutSeconds > int(MaxTimeout/time.Second) || d.RetentionSeconds > int(MaxRetention/time.Second) {
			return fmt.Errorf("bound over the maximum in %+v (zm caps it)", d)
		}
		return nil
	}())
	now := time.Now().UTC().Truncate(time.Second)
	merged := now.Add(-time.Hour)
	base := PR{Repo: "conformance/repo", Number: 1, Title: "Conformance [conf-abc12]", Body: "body", Branch: "conf/branch",
		URL: "https://github.com/conformance/repo/pull/1", Labels: []string{}, Updated: now, HeadSHA: "0123456789abcdef0123456789abcdef01234567"}
	item := Item{ID: "conf:conf-abc12", Title: "a conformance item", Status: "open", Source: "conf"}
	for _, st := range []struct {
		state, to string
		final     bool
	}{{"open", "claimed", false}, {"merged", "done", false}, {"merged", "done", true}, {"closed", "", false}} {
		pr := base
		pr.State = st.state
		if st.state == "merged" {
			pr.MergedAt, pr.MergeSHA = &merged, "89abcdef0123456789abcdef0123456789abcdef"
		}
		req := Request{PR: pr, Item: item, Proposed: Proposal{To: st.to, Comment: "conformance"}, FirstAsked: now, Attempt: 1, Final: st.final}
		dec, err := g.Decide(ctx, req)
		name := fmt.Sprintf("decide answers a decision for a %s PR", st.state)
		if st.final {
			name += " (final)"
		}
		if err == nil {
			_, err = CheckDecision(dec)
		}
		add(name, err)
	}
	if d.Has(VerbMatch) {
		ids, err := g.Match(ctx, base)
		if err == nil {
			_, err = CheckIDs(ids)
		}
		add("match answers valid ids", err)
	} else {
		_, err := g.Match(ctx, base)
		add("no match verb: Match is ErrNoMatch", func() error {
			if !errors.Is(err, ErrNoMatch) {
				return fmt.Errorf("Match = %v, want ErrNoMatch", err)
			}
			return nil
		}())
	}
	if rc, ok := g.(rawCaller); ok {
		_, err := rc.callRaw(ctx, "conformance-unknown-verb", map[string]any{"pr": "not a pr"})
		add("an unknown verb is refused (ok false or an error), never decided", func() error {
			if err == nil {
				return errors.New("the gate answered ok to an unknown verb")
			}
			return nil
		}())
	}
	return out
}
