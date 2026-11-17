package work

import (
	"context"
	"errors"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/exportscrub"
	"github.com/sengirunners/potluck/kit/core"
)

// scrubBackups overwrites a redaction's texts in the export snapshots of
// the project's native trackers (identifiers design, stage 2b part 2). A
// snapshot holds each item's work comment copies, so one written before the
// redaction still has the text; up to Keep of them are kept. Only lines of
// items filed under sc's subjects change, and only comment bodies equal to
// a text (the rule the work_comments scrub uses); every other line is
// copied byte for byte. A backup running now is waited out by the caller's
// retry: the tracker's home lock is taken, so no snapshot is written with
// the text after this ran.
func (s *Service) scrubBackups(ctx context.Context, sc core.Scrub) error {
	f, ok := s.d.Projects.File(sc.Project)
	if !ok {
		return nil
	}
	var errs []error
	for _, tr := range orchcore.ProjectTrackers(f) {
		if tr.Adapter != orchapi.AdapterNative {
			continue
		}
		stem := f.Slug + "-" + tr.Prefix + "-"
		errs = append(errs, orchcore.Homes(s.d).ScrubExports(sc.Project, f.Slug, tr.Prefix, func(dir string) error {
			return exportscrub.Dir(dir, stem, sc)
		}))
	}
	return errors.Join(errs...)
}
