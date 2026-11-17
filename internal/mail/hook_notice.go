package mail

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

var hookNoticeMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "mail/0003_hook_notices", Script: `CREATE TABLE mail_hook_notices (
 address TEXT NOT NULL, event TEXT NOT NULL, PRIMARY KEY(address,event));`},
}

// HookNotice implements orchcore.HookNoticer. Delivery records belong to the
// session address and survive daemon restarts. They never take/ack mail or mark
// announcements seen. Explicit inbox reads remain the only way to read bodies.
func (s *Service) HookNotice(ctx context.Context, sub *authz.Subject) (orchapi.Notice, error) {
	return s.hookNotice(ctx, sub, sub.Scope)
}

var _ orchcore.HookNoticer = (*Service)(nil)

type hookMailEvent struct {
	key, from, ref string
	n, group, team int
	created        time.Time
}

func mailHookEvent(m orchcore.MailNotice) hookMailEvent {
	e := hookMailEvent{key: m.Project + "/" + m.ID, n: 1, created: m.Created}
	if m.Group {
		e.group = 1
	} else {
		e.from, e.ref = m.From, m.Ref
		if e.ref == "" {
			e.ref = "mail:" + short(m.ID)
		}
	}
	return e
}

func (s *Service) hookNotice(ctx context.Context, sub *authz.Subject, sc authz.Scope) (orchapi.Notice, error) {
	addr, err := api.ParseAddress(sub.Address)
	if err != nil || addr.Kind != api.KindSession {
		return s.notice(ctx, sub, sc) // humans have no agent context to deduplicate
	}
	rs, err := s.inbox(ctx, s.callerOf(ctx, sub), sc, false, 500)
	if err != nil {
		return orchapi.Notice{}, err
	}
	var es []hookMailEvent
	unread := 0
	for _, r := range rs {
		if r.HeldFor != "" {
			continue
		}
		unread++
		m := orchcore.MailNotice{Project: r.Project, ID: r.ID, Created: r.Created,
			Group: r.Recipient == "all" || strings.HasPrefix(r.Recipient, "machine:")}
		if !m.Group {
			safe := s.redact(ctx, sub, r)
			m.From, m.Ref = safe.From, safe.Ref
		}
		es = append(es, mailHookEvent(m))
	}
	// Ready copies supply scoped metadata; before the first copy, peers only
	// expose aggregate parts. Never contact the authority on the hook path.
	if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok && sc.IsAll() {
		var ps []orchapi.NoticePart
		if tm, ok := tr.(orchcore.TeamMailNotices); ok {
			ms, fallback := tm.TeamMailNotices(ctx, sub.Address)
			ps = fallback
			for _, m := range ms {
				unread++
				es = append(es, mailHookEvent(m))
			}
		} else {
			ps = tr.TeamNotice(sub.Address)
		}
		for _, p := range ps {
			if p.Unread <= 0 {
				continue
			}
			unread += p.Unread
			b, _ := json.Marshal(p)
			e := hookMailEvent{key: fmt.Sprintf("team/%s/%x", p.Project, sha256.Sum256(b)), n: p.Unread, group: p.Group + p.Announce, team: p.Unread}
			if p.Unread > p.Group+p.Announce+p.Owners {
				// Aggregate From mixes group and direct senders. Preserve a
				// redacted direct-sender placeholder, never guess attribution.
				e.from = "someone"
			}
			es = append(es, e)
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].created.After(es[j].created) })
	fresh, err := s.freshHookEvents(ctx, sub.Address, es)
	if err != nil {
		return orchapi.Notice{}, err
	}
	line := hookMailLine(fresh)
	// Tracker health is an event per failure streak, not a reminder on every
	// prompt as the duration/failure count grows. Keep it separate from mail.
	if line == "" {
		if th, ok := orchcore.Work(s.d).(orchcore.TrackerHealth); ok {
			fs := th.FailingTrackers(sc, NoticeTrackerAfter)
			if len(fs) > 0 {
				f := fs[0]
				fresh, err = s.freshHookEvents(ctx, sub.Address, []hookMailEvent{{key: fmt.Sprintf("tracker/%s/%s/%d", f.Project, f.Prefix, f.Since.UnixNano())}})
				if err != nil {
					return orchapi.Notice{}, err
				}
				if len(fresh) > 0 {
					line = trackerNotice(fs, false)
				}
			}
		}
	}
	return orchapi.Notice{Line: line, Unread: unread}, nil
}

// freshHookEvents first reads the indexed delivery set, so an unchanged prompt
// takes no write lock. INSERT RETURNING arbitrates simultaneous hook/MCP polls:
// only the poll that inserts an event may announce it (at most once delivery).
func (s *Service) freshHookEvents(ctx context.Context, address string, es []hookMailEvent) ([]hookMailEvent, error) {
	if len(es) == 0 {
		return nil, nil
	}
	args := []any{address}
	for _, e := range es {
		args = append(args, e.key)
	}
	db := s.d.Store.Registry()
	rs, err := db.QueryContext(ctx, `SELECT event FROM mail_hook_notices WHERE address=? AND event IN (`+strings.TrimSuffix(strings.Repeat("?,", len(es)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for rs.Next() {
		var key string
		if err := rs.Scan(&key); err != nil {
			rs.Close()
			return nil, err
		}
		seen[key] = true
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return nil, err
	}
	var values []string
	args = nil
	for _, e := range es {
		if !seen[e.key] {
			values = append(values, "(?,?)")
			args = append(args, address, e.key)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	rs, err = db.QueryContext(ctx, `INSERT OR IGNORE INTO mail_hook_notices(address,event) VALUES `+strings.Join(values, ",")+` RETURNING event`, args...)
	if err != nil {
		return nil, err
	}
	inserted := map[string]bool{}
	for rs.Next() {
		var key string
		if err := rs.Scan(&key); err != nil {
			rs.Close()
			return nil, err
		}
		inserted[key] = true
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return nil, err
	}
	var fresh []hookMailEvent
	for _, e := range es {
		if inserted[e.key] {
			fresh = append(fresh, e)
			delete(inserted, e.key)
		}
	}
	return fresh, nil
}

func hookMailLine(es []hookMailEvent) string {
	var n, group, team int
	var from, ref string
	for _, e := range es {
		n, group, team = n+e.n, group+e.group, team+e.team
		if from == "" && e.from != "" {
			from, ref = e.from, e.ref // inbox order: newest direct mail first
		}
	}
	if n == 0 {
		return ""
	}
	base := fmt.Sprintf("zm: %d unread", n)
	var counts []string
	if group > 0 {
		counts = append(counts, fmt.Sprintf("%d group", group))
	}
	if team > 0 {
		counts = append(counts, fmt.Sprintf("%d team", team))
	}
	if len(counts) > 0 {
		base += " (" + strings.Join(counts, ", ") + ")"
	}
	for _, limit := range []int{40, 24, 12, 0} {
		line := base
		if from != "" && limit > 0 {
			line += " from " + noticeToken(from, limit)
			if ref != "" {
				line += "; newest " + noticeToken(ref, limit)
			}
		}
		line += ". Run: zm inbox"
		if len(line) <= orchapi.MaxNoticeLen {
			return line
		}
	}
	return "zm: unread mail. Run: zm inbox"
}

func noticeToken(s string, limit int) string {
	s = strings.Join(strings.Fields(textclean.Clean(s)), " ")
	if len(s) <= limit {
		return s
	}
	end := limit - 3
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}
