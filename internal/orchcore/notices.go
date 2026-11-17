package orchcore

import (
	"context"
	"errors"
	"time"

	"github.com/sengirunners/potluck/kit/core"
)

// NoticeCounter counts the machine notices a module folded into project's
// daily summary instead of anyone's inbox between from and to (a
// green CI result, a lease expiry, an old notice the sweep acked), by a
// plain label ("CI green: mailed to no one"). Counts only: never a body, an
// address or an item id.
type NoticeCounter func(ctx context.Context, project string, from, to time.Time) (map[string]int, error)

// AddNoticeCounter registers a module's counter on d (at wiring time).
// Moved from potluck's core.Deps method of the same name.
func AddNoticeCounter(d *core.Deps, f NoticeCounter) {
	s := Wire(d)
	s.noticeMu.Lock()
	s.noticeCounters = append(s.noticeCounters, f)
	s.noticeMu.Unlock()
}

// NoticeCounts sums every counter registered on d for project in [from,
// to). All run; a failing one's error is joined and the others still count.
func NoticeCounts(d *core.Deps, ctx context.Context, project string, from, to time.Time) (map[string]int, error) {
	var fs []NoticeCounter
	if s := of(d); s != nil {
		s.noticeMu.Lock()
		fs = append(fs, s.noticeCounters...)
		s.noticeMu.Unlock()
	}
	out := map[string]int{}
	var errs []error
	for _, f := range fs {
		m, err := f(ctx, project, from, to)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for k, n := range m {
			if n > 0 {
				out[k] += n
			}
		}
	}
	return out, errors.Join(errs...)
}
