package mail

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

// projectCount is one project's share of the unread mail.
type projectCount struct {
	slug                  string
	n, group, announce, h int
}

// Notice implements orchcore.Noticer (the session module's hook route).
func (s *Service) Notice(ctx context.Context, sub *authz.Subject) (orchapi.Notice, error) {
	return s.notice(ctx, sub, sub.Scope)
}

var _ orchcore.TeamMail = (*Service)(nil)

// NoticeParts implements orchcore.TeamMail: what Notice counts, per project.
func (s *Service) NoticeParts(ctx context.Context, sub *authz.Subject) ([]orchapi.NoticePart, error) {
	return s.parts(ctx, sub, sub.Scope)
}

// OpenAsks implements orchcore.TeamMail: needs=human mail sender sent in
// project that is still open (its session stays needs_human until none is),
// leaving out held asks' mail: that waits on an outside party.
func (s *Service) OpenAsks(ctx context.Context, project, sender string) (int, error) {
	db, err := s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return 0, err
	}
	return s.openHumanCount(ctx, db, project, sender)
}

// parts counts sub's unread mail across sc, one part per project in the
// order the newest mail of each comes (senders redacted for sub).
func (s *Service) parts(ctx context.Context, sub *authz.Subject, sc authz.Scope) ([]orchapi.NoticePart, error) {
	c := s.callerOf(ctx, sub)
	rs, err := s.inbox(ctx, c, sc, false, 500)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	byProject := map[string]int{}
	var out []orchapi.NoticePart
	seen := map[string]bool{}
	for _, r := range rs {
		if r.HeldFor != "" {
			continue // waits on an outside party, not on the reader
		}
		i, ok := byProject[r.Project]
		if !ok {
			i = len(out)
			byProject[r.Project] = i
			direct := 0
			out = append(out, orchapi.NoticePart{Project: r.Project, Slug: s.d.Projects.Slug(r.Project), DirectWakeable: &direct})
		}
		pc := &out[i]
		pc.Unread++
		if r.Kind == orchapi.MailKindNotice {
			pc.Quiet++
		}
		switch {
		case r.Recipient == "all":
			pc.Announce++
		case strings.HasPrefix(r.Recipient, "machine:"):
			pc.Group++
		case r.Recipient == "owners":
			pc.Owners++
		default:
			if r.Kind != orchapi.MailKindNotice {
				*pc.DirectWakeable++
			}
		}
		f := s.d.Redact(ctx, sub, r.From)
		if !seen[r.Project+"\x00"+f] {
			seen[r.Project+"\x00"+f] = true
			pc.From = append(pc.From, f)
		}
	}
	return out, nil
}

// notice is the one line a hook prints on each prompt (SPEC section 9), or
// "" when nothing is unread. Never a body; at most api.MaxNoticeLen bytes.
// On a team replica the line also counts the caller's team mail as of its
// last presence answer (orchcore.TeamReplica): no tailnet call on this path.
func (s *Service) notice(ctx context.Context, sub *authz.Subject, sc authz.Scope) (orchapi.Notice, error) {
	ps, err := s.parts(ctx, sub, sc)
	if err != nil {
		return orchapi.Notice{}, err
	}
	if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok && sc.IsAll() {
		ps = append(ps, tr.TeamNotice(sub.Address)...)
	}
	total := 0
	var order []*projectCount
	var from []string
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Unread <= 0 {
			continue
		}
		total += p.Unread
		order = append(order, &projectCount{slug: p.Slug, n: p.Unread, group: p.Group, announce: p.Announce, h: p.Owners})
		for _, f := range p.From {
			if !seen[f] {
				seen[f] = true
				from = append(from, f)
			}
		}
	}
	var failing []orchapi.TrackerFailure
	if th, ok := orchcore.Work(s.d).(orchcore.TrackerHealth); ok {
		failing = th.FailingTrackers(sc, NoticeTrackerAfter)
	}
	if total == 0 {
		if len(failing) == 0 {
			return orchapi.Notice{}, nil
		}
		return orchapi.Notice{Line: trackerNotice(failing, false)}, nil
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].n != order[j].n {
			return order[i].n > order[j].n
		}
		return order[i].slug < order[j].slug
	})
	line := noticeLine(total, order, from)
	if len(failing) > 0 {
		for _, short := range []bool{false, true} {
			if l := line + " | " + strings.TrimPrefix(trackerNotice(failing, short), "zm: "); len(l) <= orchapi.MaxNoticeLen {
				line = l
				break
			}
		}
	}
	return orchapi.Notice{Line: line, Unread: total}, nil
}

// NoticeTrackerAfter is how long a tracker's mirror syncs must have kept
// failing before the notice line names it (a blip is not news).
var NoticeTrackerAfter = 5 * time.Minute

// trackerNotice names the trackers whose mirror keeps failing:
//
//	zm: mirror acme/bd failing for 1h2m (37 syncs). Run: zm tracker ls
//	zm: mirrors acme/bd +1 failing. Run: zm tracker ls          (short)
func trackerNotice(fs []orchapi.TrackerFailure, short bool) string {
	name := fs[0].Slug + "/" + fs[0].Prefix
	more := ""
	if len(fs) > 1 {
		more = fmt.Sprintf(" +%d", len(fs)-1)
	}
	if short {
		return "zm: mirror " + name + more + " failing. Run: zm tracker ls"
	}
	ds := shortDur(time.Since(fs[0].Since))
	l := fmt.Sprintf("zm: mirror %s%s failing for %s (%d syncs). Run: zm tracker ls", name, more, ds, fs[0].Failures)
	if len(l) > orchapi.MaxNoticeLen {
		return "zm: mirror failing. Run: zm tracker ls"
	}
	return l
}

func (pc *projectCount) extra() string {
	var x []string
	if pc.group > 0 {
		x = append(x, fmt.Sprintf("%d group", pc.group))
	}
	if pc.announce > 0 {
		x = append(x, fmt.Sprintf("%d announce", pc.announce))
	}
	if pc.h > 0 {
		x = append(x, fmt.Sprintf("%d owners", pc.h))
	}
	if len(x) == 0 {
		return ""
	}
	return " (" + strings.Join(x, ", ") + ")"
}

// noticeLine formats the notice, grouped by project, dropping senders and
// then projects until it fits api.MaxNoticeLen:
//
//	zm: 2 unread in acme (1 group) from mini/9c0e11aa, human:ana. Run: zm inbox
//	zm: 3 unread: acme 2 (1 group), side 1; from mini/9c0e11aa. Run: zm inbox
func noticeLine(total int, ps []*projectCount, from []string) string {
	const tail = ". Run: zm inbox"
	build := func(np, nf int) string {
		var b strings.Builder
		if len(ps) == 1 {
			fmt.Fprintf(&b, "zm: %d unread in %s%s", total, ps[0].slug, ps[0].extra())
		} else {
			fmt.Fprintf(&b, "zm: %d unread: ", total)
			for i := 0; i < np; i++ {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s %d%s", ps[i].slug, ps[i].n, ps[i].extra())
			}
			if np < len(ps) {
				fmt.Fprintf(&b, ", +%d more", len(ps)-np)
			}
		}
		if nf > 0 {
			if len(ps) > 1 {
				b.WriteString(";")
			}
			b.WriteString(" from " + strings.Join(from[:nf], ", "))
			if nf < len(from) {
				fmt.Fprintf(&b, " +%d", len(from)-nf)
			}
		}
		b.WriteString(tail)
		return b.String()
	}
	for np := len(ps); np >= 1; np-- {
		for nf := min(3, len(from)); nf >= 0; nf-- {
			if line := build(np, nf); len(line) <= orchapi.MaxNoticeLen {
				return line
			}
		}
	}
	return fmt.Sprintf("zm: %d unread in %d projects%s", total, len(ps), tail)
}

var _ orchcore.Noticer = (*Service)(nil)

// shortDur is 45m, 1h, 1h12m, 3d4h.
func shortDur(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	days, h := int(d.Hours())/24, int(d.Hours())%24
	if h != 0 {
		return fmt.Sprintf("%dd%dh", days, h)
	}
	return fmt.Sprintf("%dd", days)
}
