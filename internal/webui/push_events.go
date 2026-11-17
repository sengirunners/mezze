package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
)

var pushTypes = []string{orchapi.EventSessionStarted, orchapi.EventSessionRegistered, orchapi.EventSessionStateChanged, orchapi.EventSessionEnded, orchapi.EventMailSent}

func (s *Service) startPush(ctx context.Context) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `INSERT OR IGNORE INTO web_push_state(id,cursor) VALUES(1,?)`, ids.New())
	if err != nil {
		return err
	}
	s.pushCtx, s.pushCancel = context.WithCancel(ctx)
	s.pushDone = make(chan struct{})
	s.pushDrainDone = make(chan struct{})
	go s.followPush(s.pushCtx)
	go s.deliverPushLoop(s.pushCtx)
	return nil
}

// Replays the event log after the cursor, so daemon restarts and bus overflow
// do not lose notifications. The cursor advances only after processing a row.
func (s *Service) followPush(ctx context.Context) {
	defer close(s.pushDone)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		var cursor string
		err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT cursor FROM web_push_state WHERE id=1`).Scan(&cursor)
		if err == nil {
			var page []api.Event
			page, err = s.d.Events.Since(ctx, s.d.System(), cursor, events.Filter{Types: pushTypes}, 256)
			if err == nil {
				for _, e := range page {
					if err = s.processPush(ctx, e); err != nil {
						break
					}
					_, err = s.d.Store.Registry().ExecContext(ctx, `UPDATE web_push_state SET cursor=? WHERE id=1`, e.ID)
					if err != nil {
						break
					}
				}
				if err == nil && len(page) == 256 {
					continue
				}
			}
		}
		if err != nil && ctx.Err() == nil {
			s.d.Log.Warn("web push: event processing", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Service) deliverPushLoop(ctx context.Context) {
	defer close(s.pushDrainDone)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	prune := time.NewTicker(time.Minute)
	defer prune.Stop()
	for ctx.Err() == nil {
		if err := s.drainPush(ctx); err != nil && ctx.Err() == nil {
			s.d.Log.Warn("web push: queue", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			cut := time.Now().Add(-2 * time.Hour).Unix()
			for _, q := range []string{`DELETE FROM web_push_sent WHERE sent_at<?`, `DELETE FROM web_push_rate_event WHERE sent_at<?`, `DELETE FROM web_push_mail_link WHERE created<?`} {
				if _, err := s.d.Store.Registry().ExecContext(ctx, q, cut); err != nil && ctx.Err() == nil {
					s.d.Log.Warn("web push: prune", "err", err)
				}
			}
		case <-tick.C:
		}
	}
}
func (s *Service) processPush(ctx context.Context, e api.Event) error {
	var prev string
	var sess orchapi.Session
	if strings.HasPrefix(e.Type, "dev.zm.session.") {
		if json.Unmarshal(e.Data, &sess) != nil {
			return nil
		}
		err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT state FROM web_push_session_state WHERE address=?`, sess.Address).Scan(&prev)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == sql.ErrNoRows && e.Type == orchapi.EventSessionStateChanged && sess.State == orchapi.StateNeedsHuman {
			prev, err = s.previousSessionState(ctx, e)
			if err != nil {
				return err
			}
		}
	}
	interested := e.Type == orchapi.EventMailSent || e.Type == orchapi.EventSessionEnded && sess.Outcome != nil && sess.Outcome.Kind != orchapi.OutcomeOK || (e.Type == orchapi.EventSessionStateChanged || e.Type == orchapi.EventSessionStarted || e.Type == orchapi.EventSessionRegistered) && sess.State == orchapi.StateNeedsHuman && prev != string(orchapi.StateNeedsHuman)
	if interested {
		records, err := s.pushRecords(ctx, "")
		if err != nil {
			return err
		}
		for _, p := range records {
			if e.ID <= p.cursor {
				continue
			}
			sub := s.pushSubject(ctx, p)
			if sub == nil {
				continue
			}
			visible, err := s.pushVisible(ctx, sub, e)
			if err != nil {
				return err
			}
			if visible == nil {
				continue
			}
			_, ok := s.pushEventMessage(sub, *visible)
			if !ok {
				continue
			}
			if e.Type == orchapi.EventMailSent {
				var m orchapi.Mail
				if json.Unmarshal(visible.Data, &m) == nil && m.From != "" {
					if _, err = s.d.Store.Registry().ExecContext(ctx, `INSERT OR IGNORE INTO web_push_mail_link(endpoint_hash,session,mail_id,created) VALUES(?,?,?,?)`, p.hash, m.From, m.ID, time.Now().Unix()); err != nil {
						return err
					}
				}
			} else if suffix, ok := strings.CutPrefix(sess.Note, "waiting on mail "); ok {
				var n int
				if err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_mail_link WHERE endpoint_hash=? AND session=? AND mail_id LIKE ?`, p.hash, sess.Address, "%"+suffix).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					continue
				}
			}
			_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT OR IGNORE INTO web_push_queue(event_id,endpoint_hash,project,type,next_at) VALUES(?,?,?,?,?)`, e.ID, p.hash, e.Project, e.Type, time.Now().Add(2*time.Second).Unix())
			if err != nil {
				return err
			}
		}
	}
	if sess.Address != "" {
		if sess.State != orchapi.StateNeedsHuman {
			if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM web_push_sent WHERE item=?`, "session:"+sess.Address); err != nil {
				return err
			}
		}
		_, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO web_push_session_state(address,state) VALUES(?,?) ON CONFLICT(address) DO UPDATE SET state=excluded.state`, sess.Address, string(sess.State))
		return err
	}
	return nil
}

// On the first event seen for an existing session, recover its prior state
// from the event log. A note update while already waiting is not an entry.
func (s *Service) previousSessionState(ctx context.Context, current api.Event) (string, error) {
	var cursor, state string
	f := events.Filter{Project: current.Project, Subject: current.Subject, Types: []string{orchapi.EventSessionStarted, orchapi.EventSessionRegistered, orchapi.EventSessionStateChanged, orchapi.EventSessionEnded}}
	for {
		page, err := s.d.Events.Since(ctx, s.d.System(), cursor, f, 500)
		if err != nil {
			return "", err
		}
		for _, ev := range page {
			if ev.ID >= current.ID {
				return state, nil
			}
			var ss orchapi.Session
			if json.Unmarshal(ev.Data, &ss) == nil {
				state = string(ss.State)
			}
			cursor = ev.ID
		}
		if len(page) < 500 {
			return state, nil
		}
	}
}

// The immediately preceding ULID lets Since apply its normal scope, audience,
// and redaction checks to exactly this event, using the person's current cap.
func previousID(id string) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := []byte(id)
	for i := len(b) - 1; i >= 0; i-- {
		n := strings.IndexByte(alphabet, b[i])
		if n > 0 {
			b[i] = alphabet[n-1]
			break
		}
		b[i] = 'Z'
	}
	return string(b)
}
func (s *Service) pushVisible(ctx context.Context, sub *authz.Subject, e api.Event) (*api.Event, error) {
	es, err := s.d.Events.Since(ctx, sub, previousID(e.ID), events.Filter{Types: []string{e.Type}, Project: e.Project}, 1)
	if err != nil {
		return nil, err
	}
	if len(es) == 1 && es[0].ID == e.ID {
		return &es[0], nil
	}
	return nil, nil
}
func (s *Service) pushEventMessage(sub *authz.Subject, e api.Event) (pushMessage, bool) {
	switch e.Type {
	case orchapi.EventMailSent:
		var m orchapi.Mail
		if json.Unmarshal(e.Data, &m) != nil {
			return pushMessage{}, false
		}
		// News for a person (SPEC 9 "For you") buzzes once shown; a digest item
		// waits for 08:00.
		news := m.ForYou == orchapi.ForYouKnow && m.HeldUntil == nil
		if (m.Needs != orchapi.NeedsHuman && !news) || m.HeldFor != "" { // a held ask waits on its outside party
			return pushMessage{}, false
		}
		recipient := m.Recipient == sub.Address || m.Recipient == "owners" && sub.Scope.Relation(authz.Object{Project: e.Project, Area: e.Area}) == authz.Owner
		if sub.Address == s.d.Config.HumanAddress() && (m.Recipient == "machine:"+s.d.Config.Machine || strings.HasPrefix(m.Recipient, s.d.Config.Machine+"/")) {
			recipient = true
		}
		if !recipient {
			return pushMessage{}, false
		}
		msg := pushMessage{Title: "A decision needs you", Body: "Open zm to respond", URL: decisionsPage}
		if news {
			msg.Title, msg.Body = "News for you", "Open Decisions to read it"
		}
		return msg, true
	case orchapi.EventSessionStarted, orchapi.EventSessionRegistered, orchapi.EventSessionStateChanged, orchapi.EventSessionEnded:
		var ss orchapi.Session
		if json.Unmarshal(e.Data, &ss) != nil || ss.Address == "" || !s.pushOwnsSession(sub, ss) {
			return pushMessage{}, false
		}
		if e.Type == orchapi.EventSessionEnded && (ss.Outcome == nil || ss.Outcome.Kind == orchapi.OutcomeOK || ss.Outcome.Kind == orchapi.OutcomeKilled) {
			return pushMessage{}, false
		}
		title := "A session needs you"
		if e.Type == orchapi.EventSessionEnded {
			title = "A session ended unsuccessfully"
		}
		return pushMessage{Title: title, Body: "Open zm for details", URL: decisionsPage}, true
	}
	return pushMessage{}, false
}

// decisionsPage is where every alert opens: the Decisions page the device
// subscribed from (SPEC 16 "Phone alerts").
const decisionsPage = "/v0/approvals"

func (s *Service) pushOwnsSession(sub *authz.Subject, ss orchapi.Session) bool {
	if sub == nil || ss.Launcher == "" {
		return false
	}
	if ss.Launcher == sub.Identity.ID {
		return true
	}
	// Replica presence carries the joining identity's name, not its ID.
	return s.d != nil && s.d.Config != nil && ss.Machine != "" && ss.Machine != s.d.Config.Machine && sub.Identity.Kind == api.IdentityHuman && ss.Launcher == sub.Identity.Name
}

// Local owner tokens may use local knowledge of their own sessions even if
// joined-project grants live only on the authority.
func (s *Service) pushCanReadSession(sub *authz.Subject, ss orchapi.Session) bool {
	if s.d != nil && s.d.Projects != nil && s.d.Config != nil && sub.Scope.Machine() && sub.Identity.ID == s.d.Projects.LocalIdentity().ID && ss.Machine == s.d.Config.Machine && ss.Launcher == sub.Identity.ID {
		return true
	}
	return ss.Project == "" || ss.Project == api.ProjectNone || sub.Can(authz.Read, authz.Object{Project: ss.Project, Area: ss.Area})
}
