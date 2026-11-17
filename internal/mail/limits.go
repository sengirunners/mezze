package mail

// Send limits (SPEC 9 "Send limits"): two agents answering each other must
// not loop without end and fill every inbox. Every mail a client sends is
// counted where it is stored (store, in the transaction that stores it, so
// every door and a team authority's replayed writes inherit it) in a short
// ledger, mail_sends: who sent, to whom (the resolved recipient), when, and a
// digest of what. Nothing reads meaning: the checks are counts and exact
// equality. Three rules, each a project setting with a default:
//
//   - repeat: the same mail (kind, ref, reply_to, body) from one sender to
//     one recipient again within repeat_seconds;
//   - pair: more than per_pair mails between two addresses, both ways
//     together, within window_seconds (a reply loop: both sides are refused);
//   - sender: more than per_sender mails from one sender within the window.
//
// Only agents (sessions, machine and agent-kind identities) are counted:
// never the daemon's own mail, an ask's mail or a person's. A person types;
// a loop with an agent in it stops on the agent's side.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

var limitMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "mail/0008_send_limits", Script: `
CREATE TABLE mail_sends (            -- counted sends, kept for the longest window (SPEC 9 "Send limits")
	at        INTEGER NOT NULL,
	sender    TEXT NOT NULL,
	recipient TEXT NOT NULL,
	digest    TEXT NOT NULL,           -- sha256 of to, area, kind, ref, reply_to and body; no text is kept
	mail_id   TEXT NOT NULL
);
CREATE INDEX mail_sends_pair ON mail_sends (sender, recipient, at);
CREATE INDEX mail_sends_sender ON mail_sends (sender, at);
CREATE INDEX mail_sends_at ON mail_sends (at);`},
}

// limitsSection is project.toml's [mail_limits] table:
//
//	[mail_limits]
//	per_sender = 1000     # mail from one sender per window
//	per_pair = 60         # mail between two addresses, both ways, per window
//	window_seconds = 600  # the window both counts use
//	repeat_seconds = 600  # the same mail to the same recipient again within this is refused
const limitsSection = "mail_limits"

// Limits are a project's send limits; a zero field takes DefaultLimits'.
type Limits struct {
	PerSender     int
	PerPair       int
	WindowSeconds int
	RepeatSeconds int
}

// DefaultLimits apply where project.toml sets none. A coordinator fanning
// out reaches many recipients, so only PerSender bounds it, generously; the
// loop stopper is PerPair.
var DefaultLimits = Limits{PerSender: 1000, PerPair: 60, WindowSeconds: 600, RepeatSeconds: 600}

// limitMax bounds each setting: a count up to a million, a time up to a day.
var limitMax = map[string]int{"per_sender": 1_000_000, "per_pair": 1_000_000, "window_seconds": 86400, "repeat_seconds": 86400}

func init() { project.RegisterSection(limitsSection, limitsTable{}) }

type limitsTable struct{}

func (l *Limits) field(k string) *int {
	switch k {
	case "per_sender":
		return &l.PerSender
	case "per_pair":
		return &l.PerPair
	case "window_seconds":
		return &l.WindowSeconds
	case "repeat_seconds":
		return &l.RepeatSeconds
	}
	return nil
}

func (limitsTable) Parse(where string, raw any) (any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: mail_limits must be one [mail_limits] table", where)
	}
	var l Limits
	for k, v := range m {
		p := l.field(k)
		if p == nil {
			return nil, fmt.Errorf("%s [mail_limits]: unknown key %s (per_sender, per_pair, window_seconds, repeat_seconds)", where, k)
		}
		n, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("%s [mail_limits]: %s must be a number", where, k)
		}
		if n < 0 || n > int64(limitMax[k]) {
			return nil, fmt.Errorf("%s [mail_limits]: %s must be 1..%d (0: the default)", where, k, limitMax[k])
		}
		*p = int(n)
	}
	return l, nil
}

func (limitsTable) Validate(v any, _ project.File) error {
	l, _ := v.(Limits)
	for k, max := range limitMax {
		if n := *l.field(k); n < 0 || n > max {
			return api.Errorf(api.CodeBadRequest, "[mail_limits] %s must be 1..%d (0: the default)", k, max)
		}
	}
	return nil
}

func (limitsTable) Encode(v any) string {
	l, _ := v.(Limits)
	keys := make([]string, 0, len(limitMax))
	for k := range limitMax {
		if *l.field(k) != 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("\n[mail_limits]\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %d\n", k, *l.field(k))
	}
	return b.String()
}

// limits is project's send limits, defaults filled in.
func (s *Service) limits(project string) Limits {
	l, _ := s.d.Projects.Section(project, limitsSection).(Limits)
	d := DefaultLimits
	for k := range limitMax {
		if p := l.field(k); *p == 0 {
			*p = *d.field(k)
		}
	}
	return l
}

func (l Limits) window() time.Duration { return time.Duration(l.WindowSeconds) * time.Second }
func (l Limits) repeat() time.Duration { return time.Duration(l.RepeatSeconds) * time.Second }

// counted is how the limits see one routed mail.
type counted struct {
	exempt bool   // the daemon's own mail, an ask's or a person's: never counted
	to     string // the recipient as the caller named it, for the refusal ("" for a reply)
}

// countedAs classifies mail m that sub sends (req.ID: the caller's own id
// for it, which an ask's mail carries).
func (s *Service) countedAs(ctx context.Context, sub *authz.Subject, m row, reqID string) (counted, error) {
	// An agent: a session, a machine identity, or an agent-kind identity
	// (whose mailbox is human:<name> like a person's).
	agent := orchapi.AgentSender(m.From) || sub.Identity.Kind == api.IdentityAgent
	if sub.Via == authz.ViaSystem || m.AskNotice || m.Kind == orchapi.MailKindNotice || !agent {
		return counted{exempt: true}, nil
	}
	if reqID != "" {
		ask, err := s.isAsk(ctx, m.Project, reqID)
		if err != nil || ask {
			return counted{exempt: ask}, err
		}
	}
	return counted{}, nil
}

// sendDigest is what makes two mails the same mail for the repeat rule:
// written the same (to, area, kind, ref, reply_to, body).
func sendDigest(m row) string {
	h := sha256.Sum256([]byte(strings.Join([]string{m.To, m.Area, m.Kind, m.Ref, m.ReplyTo, m.Body}, "\x00")))
	return hex.EncodeToString(h[:])
}

// limitData is a refusal's data (the type is exported for the client schema).
type limitData = orchapi.SendLimit

// checkSends refuses rs (rate_limited, with when to retry) when they break a
// send limit, counting what q holds now. In store's transaction it is the
// rule; before the report judge it saves a judge call on mail that would be
// refused anyway. (Mail to several people comes only from a person, who is
// not counted: need is 0 or 1.)
func (s *Service) checkSends(ctx context.Context, q querier, rs []routed, l Limits, now time.Time) error {
	need := 0
	for _, r := range rs {
		if err := s.checkCopy(ctx, q, r.m, r.lim, l, now); err != nil {
			return err
		}
		if !r.lim.exempt {
			need++
		}
	}
	if need == 0 {
		return nil
	}
	return s.overLimit(ctx, q, now, l, limitData{Limit: "sender", Max: l.PerSender, WindowS: l.WindowSeconds}, need,
		`sender = ? AND at > ?`, []any{rs[0].m.From, store.Millis(now.Add(-l.window()))},
		fmt.Sprintf("mail limit: you sent %d mails in %s; nothing was sent; try again in %%s", l.PerSender, l.window()),
		"every recipient counts: send less often")
}

// checkCopy applies the repeat and pair rules to one copy.
func (s *Service) checkCopy(ctx context.Context, q querier, m row, c counted, l Limits, now time.Time) error {
	if c.exempt {
		return nil
	}
	whom := "this recipient"
	if c.to != "" {
		whom = strings.ReplaceAll(c.to, "%", "%%") // the message is a format with one %s, the wait
	}
	var id string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT mail_id, at FROM mail_sends WHERE sender = ? AND recipient = ? AND digest = ? AND at > ? ORDER BY at DESC LIMIT 1`,
		m.From, m.Recipient, sendDigest(m), store.Millis(now.Add(-l.repeat()))).Scan(&id, &at)
	switch {
	case err == nil:
		first := store.FromMillis(at)
		return limitErr(limitData{Limit: "repeat", WindowS: l.RepeatSeconds}, first.Add(l.repeat()).Sub(now),
			"mail limit: you sent this same mail to "+whom+" "+now.Sub(first).Round(time.Second).String()+" ago ("+short(id)+
				"), so this copy was not sent; the same mail can go again in %s").
			WithHint("it is there already: send something new, or nothing")
	case err != sql.ErrNoRows:
		return err
	}
	return s.overLimit(ctx, q, now, l, limitData{Limit: "pair", Max: l.PerPair, WindowS: l.WindowSeconds}, 1,
		`at > ? AND ((sender = ? AND recipient = ?) OR (sender = ? AND recipient = ?))`,
		[]any{store.Millis(now.Add(-l.window())), m.From, m.Recipient, m.Recipient, m.From},
		fmt.Sprintf("mail limit: %d mails between you and %s (both ways) in %s; nothing was sent; try again in %%s", l.PerPair, whom, l.window()),
		"answering each other? stop: take mail with zm ack; an acknowledgement needs no reply")
}

// overLimit refuses when the rows matching where leave no room for need
// more under d.Max; the wait is until enough of them leave the window.
func (s *Service) overLimit(ctx context.Context, q querier, now time.Time, l Limits, d limitData, need int, where string, args []any, msg, hint string) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM mail_sends WHERE `+where, args...).Scan(&n); err != nil {
		return err
	}
	if n+need <= d.Max {
		return nil
	}
	var at int64
	// need is at most 1 (checkSends), so the offset is below n.
	if err := q.QueryRowContext(ctx, `SELECT at FROM mail_sends WHERE `+where+` ORDER BY at LIMIT 1 OFFSET ?`, append(args, min(n+need-d.Max-1, n-1))...).Scan(&at); err != nil {
		return err
	}
	return limitErr(d, store.FromMillis(at).Add(l.window()).Sub(now), msg).WithHint(hint)
}

// limitErr is a send limit's refusal: 429 rate_limited, retry_after in
// seconds, and the wait in words too, in msg's one %s (a CLI or MCP reader
// sees only the message and hint).
func limitErr(d limitData, wait time.Duration, msg string) *api.Error {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return api.Errorf(api.CodeRateLimited, msg, time.Duration(secs)*time.Second).WithRetryAfter(secs).WithData(d)
}

// recordSend adds m to the ledger (in store's transaction).
func recordSend(ctx context.Context, tx *sql.Tx, m row, c counted, now time.Time) error {
	if c.exempt {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO mail_sends (at, sender, recipient, digest, mail_id) VALUES (?,?,?,?,?)`,
		store.Millis(now), m.From, m.Recipient, sendDigest(m), m.ID)
	return err
}

// pruneSends drops ledger rows past every project's longest window (the
// orphan sweep's minute ticker). One project's failure skips only it.
func (s *Service) pruneSends(ctx context.Context) error {
	var errs []error
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		l := s.limits(p.ID)
		cutoff := store.Millis(s.now().Add(-max(l.window(), l.repeat())))
		var old int
		if err := p.DB.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM mail_sends WHERE at <= ? LIMIT 1)`, cutoff).Scan(&old); err != nil {
			errs = append(errs, err)
			continue
		}
		if old == 0 {
			continue // nothing to drop: no write lock taken
		}
		errs = append(errs, s.commit(ctx, p.DB, p.ID, func(tx *sql.Tx) ([]pub, error) {
			_, err := tx.ExecContext(ctx, `DELETE FROM mail_sends WHERE at <= ?`, cutoff)
			return nil, err
		}))
	}
	return errors.Join(errs...)
}
