package summaries

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/core"
)

func TestIndependentSummaryScrubsRenderedTitle(t *testing.T) {
	for _, title := range []string{"sensitive-canary-" + strings.Repeat("private", 40), "sensitive-canary\nprivate-detail"} {
		t.Run(title[:16], func(t *testing.T) {
			s, owner, _, p, f := fixture(t)
			ctx := context.Background()
			c, _ := s.config(ctx, p.ID)
			f.rows[orchapi.WorkOpen] = orchapi.WorkList{Items: []orchapi.WorkItem{{Project: p.ID, ID: "bd:private", Title: title}}}
			out, err := s.generate(ctx, owner, c, "2026-11-04")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.scrub(ctx, core.Scrub{Project: p.ID, Subjects: []string{"bd:private"}, Texts: []string{title}}); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Get(ctx, p.ID, out.Day, c.Version)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(saved.Body, "sensitive-canary") {
				t.Errorf("redacted title survived render transform: %s", saved.Body)
			}
		})
	}
}
