package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

var _ orchcore.PresenceSource = (*Service)(nil)

// PresenceSessions is a daemon-internal complete local live inventory, with
// separately bounded recent outcomes. It does not apply a request's scope:
// team heartbeats need sessions in other projects and none for mail routing.
func (s *Service) PresenceSessions(ctx context.Context, liveLimit, endedLimit int) ([]orchapi.Session, error) {
	return s.presenceSessionsAt(ctx, liveLimit, endedLimit, time.Now())
}

func (s *Service) presenceSessionsAt(ctx context.Context, liveLimit, endedLimit int, now time.Time) ([]orchapi.Session, error) {
	if liveLimit < 1 || liveLimit > 10000 || endedLimit < 0 || endedLimit > 500 {
		return nil, api.Errorf(api.CodeBadRequest, "presence inventory limits must be live 1..10000 and ended 0..500")
	}
	if s == nil || s.d == nil || s.d.Store == nil {
		return nil, api.Errorf(api.CodeUnavailable, "presence inventory store is unavailable")
	}
	db := s.d.Store.Registry()
	if db == nil {
		return nil, api.Errorf(api.CodeUnavailable, "presence inventory registry is closed")
	}
	// One SELECT keeps live/ended membership and lineage in one read snapshot.
	// Project only argv[0] and outcome.kind: full argv and evidence can be large
	// and private. An invalid or oversized argv[0] suppresses terminal titles.
	rows, err := db.QueryContext(ctx, `WITH live AS (
 SELECT id FROM sessions WHERE machine=? AND state!='ended' ORDER BY id LIMIT ?
), recent AS (
 SELECT id FROM sessions WHERE machine=? AND state='ended' AND ended>?
 AND CASE WHEN json_valid(outcome) THEN json_type(outcome)='object' ELSE 0 END
 ORDER BY ended DESC, id DESC LIMIT ?
), inventory AS (SELECT id FROM live UNION ALL SELECT id FROM recent)
SELECT s.id,s.address,s.machine,s.project,s.area,s.agent,s.title,s.state,s.note,s.managed,
 s.started,s.updated,s.ended,COALESCE(json_extract(NULLIF(s.outcome,''),'$.kind'),''),
 s.activity,s.activity_at,s.term_title,s.shared,COALESCE(l.parent,''),s.mode,s.caps,
 CASE WHEN s.command='' THEN NULL
 WHEN NOT json_valid(s.command) THEN '__invalid_command__'
 WHEN json_type(s.command)!='array' THEN '__invalid_command__'
 WHEN json_array_length(s.command)=0 THEN NULL
 WHEN json_type(s.command,'$[0]')!='text' OR length(json_extract(s.command,'$[0]'))>4096 THEN '__invalid_command__'
 ELSE json_extract(s.command,'$[0]') END,
 s.work,s.account,s.for_id,s.job_kind,s.tokens
FROM inventory i JOIN sessions s ON s.id=i.id
LEFT JOIN session_lineage l ON l.id=s.id
ORDER BY CASE WHEN s.state='ended' THEN 1 ELSE 0 END,s.ended DESC,s.id DESC`,
		s.d.Config.Machine, liveLimit+1, s.d.Config.Machine, store.Millis(now.Add(-24*time.Hour)), endedLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]orchapi.Session, 0)
	live := 0
	for rows.Next() {
		var ss orchapi.Session
		var state, outcome string
		var command sql.NullString
		var caps string
		var managed, shared int
		var started, updated int64
		var ended, activityAt sql.NullInt64
		var tokens string
		if err := rows.Scan(&ss.ID, &ss.Address, &ss.Machine, &ss.Project, &ss.Area, &ss.Agent,
			&ss.Title, &state, &ss.Note, &managed, &started, &updated, &ended, &outcome,
			&ss.Activity, &activityAt, &ss.TermTitle, &shared, &ss.Parent, &ss.Mode, &caps, &command,
			&ss.Work, &ss.Account, &ss.For, &ss.JobKind, &tokens); err != nil {
			return nil, err
		}
		ss.Tokens = parseTokens(tokens)
		if caps != "" {
			var c orchapi.AgentCapabilities
			if json.Unmarshal([]byte(caps), &c) == nil {
				ss.Capabilities = &c
			}
		}
		ss.State, ss.Managed, ss.Shared = orchapi.SessionState(state), managed != 0, shared != 0
		ss.Started, ss.Updated, ss.Ended = store.FromMillis(started), store.FromMillis(updated), store.NullMillis(ended)
		ss.ActivityAt = store.NullMillis(activityAt)
		if ss.State != orchapi.StateEnded {
			live++
			if live > liveLimit {
				return nil, api.Errorf(api.CodeUnavailable, "local presence inventory exceeds %d live sessions; refusing an incomplete heartbeat", liveLimit)
			}
		} else {
			ss.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeKind(outcome)}
		}
		if command.Valid {
			ss.Command = []string{command.String}
		}
		if !orchapi.SharesTermTitle(ss.Agent, ss.Command) {
			ss.TermTitle = ""
		}
		out = append(out, ss)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Release the database read before consulting live state. This overlay is
	// approximate, as ordinary reads are, and never fetches shared prompt text.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	who := s.d.Projects.LocalIdentity().Name
	for i := range out {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ss := &out[i]
		if !ss.Shared || !ss.Managed || ss.State == orchapi.StateEnded {
			continue
		}
		p := s.proc(ss.ID)
		if p == nil {
			continue
		}
		p.mu.Lock()
		attached, typed := len(p.conns), p.typedAt
		p.mu.Unlock()
		recent := !typed.IsZero() && now.Sub(typed) < inUseTypedFor
		if attached == 0 && !recent {
			continue
		}
		ss.InUse = &orchapi.SessionInUse{By: who, Attached: attached}
		if recent {
			t := storedTime(typed.Truncate(inUseTypedStep))
			ss.InUse.TypedAt = &t
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
