package approvals

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"regexp"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// Repeating cards (SPEC 16): an ask keyed to a thing that can
// break again (repeat_key, e.g. an automation's id) files one card. While
// that card is open, a new ask with the same key in the same area and
// namespace files nothing: the open card counts it and shows its latest
// state. The key is syntax, matched exactly; nothing reads its meaning.

var repeatMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0013_ask_repeats", Script: `
CREATE TABLE ask_repeats (
	ask       TEXT PRIMARY KEY,
	project   TEXT NOT NULL,
	scope_key TEXT NOT NULL,
	key       TEXT NOT NULL,
	seen      INTEGER NOT NULL DEFAULT 1,
	last_seen INTEGER NOT NULL,
	latest    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ask_repeats_key ON ask_repeats (project, scope_key);`},
}

var repeatKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,119}$`)

const maxLatest = 300

// cleanRepeat checks a repeat key's syntax and the latest line as card text
// (a person reads it beside the card).
func cleanRepeat(key, latest string, prefixes []string) (string, string, error) {
	key = strings.TrimSpace(key)
	if !repeatKeyRe.MatchString(key) {
		return "", "", api.Errorf(api.CodeBadRequest, "repeat_key %q: 3 to 120 letters, digits, dots, colons, dashes or underscores (an automation's id)", key)
	}
	if unsafePresentation(latest) || len(latest) > maxLatest {
		return "", "", api.Errorf(api.CodeBadRequest, "latest: one plain line of at most %d bytes", maxLatest)
	}
	latest = strings.TrimSpace(textclean.Clean(latest))
	if hit := machineToken(latest, prefixes); hit != "" {
		return "", "", api.Errorf(api.CodeBadRequest, "a person reads latest: no ids, hashes or paths; it has %s. Say it in plain words", hit)
	}
	return key, latest, nil
}

// repeatScope is key's namespace, as an ask's dedupe slot: the area, then
// operators (who share one) or the guest's own address.
func repeatScope(sub *authz.Subject, project, area, key string) string {
	authority := "operators"
	if !sub.Can(authz.Operate, authz.Object{Project: project, Area: area}) {
		authority = "party:" + sub.Address
	}
	return area + "\x1f" + authority + "\x1f" + key
}

// lockRepeat holds scoped's lock (one of a fixed set, by hash) until the returned func runs: from the
// look for an open card to the new card's key being stored.
func (s *Service) lockRepeat(project, scoped string) func() {
	h := fnv.New32a()
	h.Write([]byte(project + "\x1f" + scoped))
	mu := &s.repeatLocks[h.Sum32()%uint32(len(s.repeatLocks))]
	mu.Lock()
	return mu.Unlock
}

// dropRepeat forgets an ask that was never sent.
func (s *Service) dropRepeat(ctx context.Context, id string) {
	if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM ask_repeats WHERE ask = ?`, id); err != nil {
		s.d.Log.Warn("approvals: drop an unsent ask's repeat key", "ask", id, "err", err)
	}
}

// countRepeat counts a repeat on project's open card keyed scoped, if there
// is one, and returns that card. Open means not answered, withdrawn or
// defaulted; a card the card judge holds counts only for its own asker.
func (s *Service) countRepeat(ctx context.Context, caller, project, scoped, latest string) (orchapi.Ask, bool, error) {
	reg := s.d.Store.Registry()
	var id string
	// A card the judge holds is its asker's alone until it passes; one whose
	// default applied is done for its asker (the next failure is a new card).
	err := reg.QueryRowContext(ctx, `SELECT r.ask FROM ask_repeats r JOIN asks a ON a.id = r.ask
		WHERE r.project = ? AND r.scope_key = ? AND a.withdrawn IS NULL
		AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.ask = a.id)
		AND (a.default_applied IS NULL OR a.default_undone IS NOT NULL)
		AND (a.asked_by = ? OR NOT EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = a.id))
		ORDER BY a.created DESC LIMIT 1`, project, scoped, caller).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return orchapi.Ask{}, false, nil
	}
	if err != nil {
		return orchapi.Ask{}, false, err
	}
	res, err := reg.ExecContext(ctx, `UPDATE ask_repeats SET seen = seen + 1, last_seen = ?, latest = CASE WHEN ? != '' THEN ? ELSE latest END
		WHERE ask = ? AND EXISTS (SELECT 1 FROM asks a WHERE a.id = ask_repeats.ask AND a.withdrawn IS NULL)
		AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.ask = ask_repeats.ask)`, ms(s.now()), latest, latest, id)
	if err != nil {
		return orchapi.Ask{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return orchapi.Ask{}, false, nil // answered or withdrawn just now: file a new card
	}
	a, err := s.ask(ctx, id)
	if err != nil {
		return orchapi.Ask{}, false, err
	}
	if !s.isHeld(ctx, a.ID) { // nobody was asked yet about a held card
		if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: orchapi.EventAskRepeated, Subject: a.ID,
			Project: a.Project, Area: a.Area, Data: a, Mentions: []string{a.AskedBy},
			Audience: &events.Audience{Need: authz.Operate, Parties: []string{a.AskedBy, a.To}}}); err != nil {
			s.d.Log.Warn("approvals: publish ask repeat", "ask", a.ID, "err", err)
		}
	}
	return a, true, nil
}

// recordRepeat keys a newly stored ask. A failure leaves the card filed and
// only uncounted.
func (s *Service) recordRepeat(ctx context.Context, a orchapi.Ask, scoped, key, latest string) {
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO ask_repeats (ask, project, scope_key, key, last_seen, latest) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.Project, scoped, key, ms(a.Created), latest); err != nil {
		s.d.Log.Warn("approvals: key a repeating ask", "ask", a.ID, "err", err)
	}
}

// repeatOf is ask id's repeat count, or nil when it is not keyed.
func (s *Service) repeatOf(ctx context.Context, id string) *orchapi.AskRepeat {
	var r orchapi.AskRepeat
	var last int64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT key, seen, last_seen, latest FROM ask_repeats WHERE ask = ?`, id).
		Scan(&r.Key, &r.Seen, &last, &r.Latest)
	if err != nil {
		return nil
	}
	r.LastSeen = store.FromMillis(last)
	return &r
}
