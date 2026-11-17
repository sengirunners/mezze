package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/authz"
)

// A mirror whose syncs keep failing is named on the notice line (it used
// to be only a WARN in daemon.log), to whole-project readers only, and
// only after NoticeTrackerAfter.
func TestNoticeNamesFailingMirror(t *testing.T) {
	f := newFixture(t)
	f.work.failing = []orchapi.TrackerFailure{
		{Project: f.harbor.ID, Slug: "harbor", Prefix: "bd", Since: time.Now().Add(-72 * time.Minute), Failures: 37},
		{Project: f.side.ID, Slug: "side", Prefix: "gh", Since: time.Now().Add(-time.Minute), Failures: 2}, // a blip: not yet
	}
	n := must(orchclient.Notice(ctx, f.as("", "")))
	if n.Unread != 0 || n.Line != "zm: mirror harbor/bd failing for 1h12m (37 syncs). Run: zm tracker ls" {
		t.Fatalf("no mail: %+v", n)
	}
	// With mail: both, within the notice budget.
	must(orchclient.Send(ctx, f.as("", "harbor"), orchapi.SendMailRequest{To: "human:ana", Body: "hi"}))
	n = must(orchclient.Notice(ctx, f.as("", "")))
	if n.Unread != 1 || !strings.HasPrefix(n.Line, "zm: 1 unread in harbor") || !strings.Contains(n.Line, "| mirror harbor/bd failing") || len(n.Line) > orchapi.MaxNoticeLen {
		t.Fatalf("with mail: %+v", n)
	}
	// A guest of one area is not told about the project's trackers.
	if n := must(orchclient.Notice(ctx, f.guest())); strings.Contains(n.Line, "mirror") {
		t.Fatalf("guest notice: %+v", n)
	}
	// Two failing: the oldest is named, +1; short form when space is tight.
	f.work.failing[1].Since = time.Now().Add(-time.Hour)
	fs := f.work.FailingTrackers(authz.All(), 0)
	if l := trackerNotice(fs, false); !strings.Contains(l, " +1 failing") {
		t.Fatalf("two: %q", l)
	}
	if l := trackerNotice(fs, true); l != "zm: mirror harbor/bd +1 failing. Run: zm tracker ls" {
		t.Fatalf("short: %q", l)
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "<1m", 7 * time.Minute: "7m", time.Hour: "1h", 72 * time.Minute: "1h12m",
		50 * time.Hour: "2d2h", 72 * time.Hour: "3d"} {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%s) = %q, want %q", d, got, want)
		}
	}
}
