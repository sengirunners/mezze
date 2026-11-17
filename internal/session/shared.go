package session

// Shared sessions (SPEC 17.10): a managed session on a team project marked
// shared at spawn takes prompts from project members with operate, routed
// through the project's authority (which logs each one in session_prompts)
// to the session's machine: typed at once when the session is the
// authority's own, pulled by the replica's heartbeat when it is a replica's.

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Backlog and spam bounds (SPEC 17.10; tests shorten them).
var (
	promptPendingFor = 10 * time.Minute // a prompt not typed by then expires
	promptRateWindow = time.Minute      // per sender per session: at most promptRateMax in it
)

const (
	promptRateMax    = 10
	promptPendingMax = 20                  // a replica coming back types at most this many, oldest first
	promptKeep       = 30 * 24 * time.Hour // how long the log keeps a prompt
	promptListMax    = 200
	inUseTypedFor    = 5 * time.Minute  // a person typed lately: in_use for this long
	inUseTypedStep   = 30 * time.Second // typed_at's grain (presence changes at most this often)
)

var _ orchcore.SharedPrompts = (*Service)(nil)

// typed records that a person on this machine typed into the session
// (attach, zm session input): a shared session's in_use.
func (p *proc) typed() {
	p.mu.Lock()
	p.typedAt = time.Now()
	p.mu.Unlock()
}

// teamProject reports a team project: this machine is its authority, or it
// is one this replica joined (it has no local entry).
func (s *Service) teamProject(id string) bool {
	if id == "" || id == api.ProjectNone {
		return false
	}
	return s.d.Store.IsTeam(id) || s.d.Projects.Slug(id) == ""
}

// checkPromptText enforces SPEC 17.10's text rules: 1 to MaxPromptBytes of
// valid UTF-8, printable text with newlines and tabs only, so a prompt can
// never act as a terminal key (no ESC, no ^C).
func checkPromptText(text string) error {
	switch {
	case strings.TrimSpace(text) == "":
		return api.Errorf(api.CodeBadRequest, "text is required")
	case len(text) > orchapi.MaxPromptBytes:
		return api.Errorf(api.CodeBadRequest, "text is over %d bytes", orchapi.MaxPromptBytes)
	case !utf8.ValidString(text):
		return api.Errorf(api.CodeBadRequest, "text is not UTF-8")
	}
	for _, r := range text {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			return api.Errorf(api.CodeBadRequest, "text has a control character (U+%04X): only printable text, newlines and tabs", r)
		}
		if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x2028 || r == 0x2029 {
			// Bidi embeddings, overrides and isolates make the log and the
			// screen show other text than the agent reads; the Unicode line
			// and paragraph separators break lines no one sees coming.
			return api.Errorf(api.CodeBadRequest, "text has a bidi control or line separator (U+%04X)", r)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		// Claude Code runs a prompt starting with "!" as a shell command,
		// with no permission prompt, and one starting with "#" writes to the
		// owner's CLAUDE.md (memory mode): never from someone else's machine.
		// Any line counts: an early Enter (see typePrompt) makes it a prompt.
		line = strings.TrimLeftFunc(line, promptBlank)
		if strings.HasPrefix(line, "!") {
			return api.Errorf(api.CodeBadRequest, "no line of a prompt may start with \"!\" (a shell command in the agent)").
				WithHint("ask the agent in words; slash commands (/...) are allowed")
		}
		if strings.HasPrefix(line, "#") {
			return api.Errorf(api.CodeBadRequest, "no line of a prompt may start with \"#\" (a memory write in the agent)").
				WithHint("ask the agent in words; slash commands (/...) are allowed")
		}
	}
	return nil
}

// promptBlank reports a character that may come before a line's first
// visible one without counting: whitespace (Unicode's, so no-break and wide
// spaces too), a byte order mark and a zero-width space.
func promptBlank(r rune) bool {
	return unicode.IsSpace(r) || r == 0xfeff || r == 0x200b
}

// refuseAgentPrompt: for now only a person sends a prompt; a caller that says
// it is an agent session (Zm-As, a session's own token) is refused. A
// courtesy, not a boundary (SPEC 17.10): an agent on the owner's machine
// shares the owner's socket and can simply not say so.
func refuseAgentPrompt(sub *authz.Subject) error {
	if sub != nil && sub.Session != "" {
		return api.Errorf(api.CodeForbidden, "only a person sends a prompt to a shared session, not an agent session (%s)", sub.Session)
	}
	return nil
}

// promptTarget loads the session a prompt or a prompt-log read names, with
// the caller's checks: hidden or missing is not found; for a prompt
// (operate) a visible session the caller may not operate is forbidden.
func (s *Service) promptTarget(ctx context.Context, sub *authz.Subject, raw string, a authz.Action) (orchapi.Session, error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	var ss orchapi.Session
	var r row
	var ok bool
	far := s.remoteAddr(id)
	if far {
		ss, ok = s.loadPresence(ctx, id)
	} else {
		r, ok = s.loadRef(ctx, id)
		ss = r.Session
	}
	if !ok || !sub.Can(authz.Read, object(ss)) {
		return orchapi.Session{}, api.NotFound("session", raw) // as the caller wrote it: hidden answers like missing
	}
	if core.IsLocal(ctx) {
		// A capped process (found on this socket by its pid) operates and
		// reads only its own sessions, and none on another machine.
		if gate := s.capGate(sub); far && !gate.uncapped(ctx) || !far && !gate.allows(ctx, r, a, true) {
			return orchapi.Session{}, api.NotFound("session", raw)
		}
	}
	if a != authz.Read {
		if err := sub.Check(a, object(ss), "session", raw); err != nil {
			return orchapi.Session{}, err
		}
	}
	return ss, nil
}

// hPrompt is POST /v0/sessions/{id}/prompt.
func (s *Service) hPrompt(w http.ResponseWriter, r *http.Request) {
	var req orchapi.PromptRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	if err := refuseAgentPrompt(sub); err != nil {
		api.WriteError(w, err)
		return
	}
	if err := checkPromptText(req.Text); err != nil {
		api.WriteError(w, err)
		return
	}
	ss, err := s.promptTarget(ctx, sub, r.PathValue("id"), authz.Operate)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	pr, err := s.prompt(ctx, sub, ss, req.Text)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	pr.From = s.d.Redact(ctx, sub, pr.From)
	api.WriteJSON(w, http.StatusAccepted, pr)
}

// prompt logs a prompt to ss and types it at once when ss is this
// machine's (else its replica pulls it with its next heartbeat).
func (s *Service) prompt(ctx context.Context, sub *authz.Subject, ss orchapi.Session, text string) (orchapi.SessionPrompt, error) {
	switch {
	case !ss.Shared:
		return orchapi.SessionPrompt{}, api.Errorf(api.CodeConflict, "session %s is not shared: it takes no prompts from others", ss.Address).
			WithHint("its owner types into it (zm attach); a session is shared at spawn: zm run --shared")
	case !ss.Managed || ss.State == orchapi.StateEnded:
		return orchapi.SessionPrompt{}, api.Errorf(api.CodeConflict, "session %s is not a live managed session", ss.Address)
	case ss.Mode == orchapi.SessionModePipe:
		return orchapi.SessionPrompt{}, refusePipe(ss.Address, "prompts")
	case !s.teamProject(ss.Project):
		return orchapi.SessionPrompt{}, api.Errorf(api.CodeConflict, "session %s is not in a team project", ss.Address)
	}
	local := ss.Machine == s.d.Config.Machine
	if local && s.d.Projects.Slug(ss.Project) == "" {
		// A replica's own session in a joined project: the authority logs
		// and routes it (the team proxy forwards the call there).
		return orchapi.SessionPrompt{}, api.Errorf(api.CodeConflict, "prompts to %s go through its project's authority", ss.Address)
	}
	var p *proc
	if local {
		if p = s.proc(ss.ID); p == nil || p.isExited() {
			return orchapi.SessionPrompt{}, api.Errorf(api.CodeConflict, "session %s is not running", ss.Address)
		}
		if strings.Contains(text, "\n") && !p.bracketedPaste.Load() {
			return orchapi.SessionPrompt{}, api.Errorf(api.CodeBadRequest, "session %s's program takes no bracketed paste: send one line", ss.Address)
		}
	}
	now := storedTime(time.Now())
	from := sub.Address
	if sub.Session != "" {
		from = sub.Session
	}
	if err := s.promptRate(ctx, ss.Address, from, sub.Identity.Name, now); err != nil {
		return orchapi.SessionPrompt{}, err
	}
	pr := orchapi.SessionPrompt{ID: ids.New(), Session: ss.Address, From: from, Who: sub.Identity.Name, Text: text, At: now,
		State: orchapi.PromptPending}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO session_prompts (id, session, project, area, sender, who, text, at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, pr.ID, pr.Session, ss.Project, ss.Area, pr.From, pr.Who, pr.Text, store.Millis(now), pr.State); err != nil {
		return orchapi.SessionPrompt{}, err
	}
	if local {
		if err := typePrompt(p, text); err != nil {
			return orchapi.SessionPrompt{}, api.Errorf(api.CodeConflict, "session %s: %v", ss.Address, err)
		}
		// It was typed: record that even if the sender has gone meanwhile.
		s.markDelivered(context.WithoutCancel(ctx), ss.Project, ss.Address, []string{pr.ID})
		pr.State, pr.DeliveredAt = orchapi.PromptDelivered, &now
	}
	s.promptChanged(ctx, ss)
	return pr, nil
}

// typePrompt stages text without appending Enter or marking a permission
// prompt answered. The recipient's program still interprets printable input.
//
// Time of check, time of use (narrow, accepted): the caller checked that the
// program has bracketed paste on (DECSET 2004) before this types; a program
// that turns it off in between gets the paste markers as text, and the
// newlines inside may submit the lines one by one. The text itself holds no
// escape the markers could hide, and checkPromptText refuses a "!" or "#"
// first on any line, so the worst case is an early Enter: the lines arrive
// as separate prompts, none of them a shell command or a memory write.
func typePrompt(p *proc, text string) error {
	if strings.Contains(text, "\n") {
		text = "\x1b[200~" + text + "\x1b[201~"
	}
	return p.write([]byte(text))
}

// promptChanged publishes the session's updated event (its last_prompt).
func (s *Service) promptChanged(ctx context.Context, ss orchapi.Session) {
	if s.remoteAddr(ss.Address) {
		if cur, ok := s.loadPresence(ctx, ss.Address); ok {
			s.publish(orchapi.EventSessionUpdated, cur)
		}
		return
	}
	if r, ok := s.load(ctx, ss.Address); ok && r.State != orchapi.StateEnded {
		s.publish(orchapi.EventSessionUpdated, r.Session)
	}
}

// hPrompts is GET /v0/sessions/{id}/prompts.
func (s *Service) hPrompts(w http.ResponseWriter, r *http.Request) {
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	ss, err := s.promptTarget(ctx, sub, r.PathValue("id"), authz.Read)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if !sub.Scope.WholeProject(ss.Project) {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "a shared session's prompt log is for the project's members"))
		return
	}
	out, err := s.prompts(ctx, `WHERE session = ? ORDER BY at DESC, id DESC LIMIT ?`, ss.Address, promptListMax)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	for i := range out {
		out[i].From = s.d.Redact(ctx, sub, out[i].From)
	}
	api.WriteJSON(w, http.StatusOK, orchapi.PromptList{Prompts: out})
}

func (s *Service) prompts(ctx context.Context, where string, a ...any) ([]orchapi.SessionPrompt, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id, session, sender, who, text, at, state, delivered_at FROM session_prompts `+where, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.SessionPrompt{}
	for rows.Next() {
		var p orchapi.SessionPrompt
		var at int64
		var dl sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Session, &p.From, &p.Who, &p.Text, &at, &p.State, &dl); err != nil {
			return nil, err
		}
		p.At, p.DeliveredAt = store.FromMillis(at), store.NullMillis(dl)
		out = append(out, p)
	}
	return out, rows.Err()
}

// promptRate caps a sender's prompts to one session at promptRateMax per
// promptRateWindow (429 with the seconds until a slot frees).
func (s *Service) promptRate(ctx context.Context, session, from, who string, now time.Time) error {
	since := store.Millis(now.Add(-promptRateWindow))
	var n int
	var oldest sql.NullInt64
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*), min(at) FROM session_prompts
		WHERE session = ? AND sender = ? AND who = ? AND at > ?`, session, from, who, since).Scan(&n, &oldest); err != nil {
		return err
	}
	if n < promptRateMax {
		return nil
	}
	wait := 1
	if oldest.Valid {
		if d := store.FromMillis(oldest.Int64).Add(promptRateWindow).Sub(now); d > 0 {
			wait = int((d + time.Second - 1) / time.Second)
		}
	}
	e := api.Errorf(api.CodeRateLimited, "at most %d prompts a minute to one shared session", promptRateMax)
	e.RetryAfter = wait
	return e
}

// expirePending marks pending prompts older than promptPendingFor expired:
// a prompt that waited that long is no longer what its sender meant now.
func (s *Service) expirePending(ctx context.Context) {
	_, _ = s.d.Store.Registry().ExecContext(context.WithoutCancel(ctx), `UPDATE session_prompts SET state = ? WHERE state = ? AND at < ?`,
		orchapi.PromptExpired, orchapi.PromptPending, store.Millis(time.Now().Add(-promptPendingFor)))
}

// PendingPrompts implements orchcore.SharedPrompts (an authority, for a
// replica's heartbeat answer): the session's pending prompts, oldest first,
// at most promptPendingMax, none older than promptPendingFor.
func (s *Service) PendingPrompts(ctx context.Context, address string) []orchapi.SessionPrompt {
	s.expirePending(ctx)
	out, err := s.prompts(ctx, `WHERE session = ? AND state = ? ORDER BY at, id LIMIT ?`, address, orchapi.PromptPending, promptPendingMax)
	if err != nil {
		s.d.Log.Warn("shared session: pending prompts", "session", address, "err", err)
		return nil
	}
	return out
}

// MarkDelivered implements orchcore.SharedPrompts (an authority: the ids the
// replica on machine typed into address since its last beat). A prompt to
// another machine's session, or in another project, is left as it is.
func (s *Service) MarkDelivered(ctx context.Context, project, machine, address string, ids []string) {
	a, err := api.ParseAddress(address)
	if err != nil || a.Kind != api.KindSession || a.Machine != machine || machine == s.d.Config.Machine {
		return
	}
	if n := s.markDelivered(ctx, project, a.String(), ids); n > 0 {
		if ss, ok := s.loadPresence(ctx, address); ok {
			s.publish(orchapi.EventSessionUpdated, ss)
		}
	}
}

// markDelivered marks project's prompts ids to address (its machine is in
// the address) delivered. An ack that lands after a prompt expired still
// counts: it was typed.
func (s *Service) markDelivered(ctx context.Context, project, address string, ids []string) int64 {
	var n int64
	now := store.Millis(time.Now())
	for _, id := range ids {
		res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE session_prompts SET state = ?, delivered_at = ?
			WHERE id = ? AND session = ? AND project = ? AND state IN (?, ?)`, orchapi.PromptDelivered, now, id, address, project, orchapi.PromptPending, orchapi.PromptExpired)
		if err == nil {
			k, _ := res.RowsAffected()
			n += k
		}
	}
	return n
}

// DeliverPrompt implements orchcore.SharedPrompts (a replica: type a prompt its
// authority sent into a local shared session). typed is false when it was
// refused before anything was typed.
func (s *Service) DeliverPrompt(ctx context.Context, address string, pr orchapi.SessionPrompt) (bool, error) {
	r, ok := s.load(ctx, address)
	if !ok || !r.Shared || !r.Managed || r.State == orchapi.StateEnded {
		return false, api.Errorf(api.CodeConflict, "session %s takes no prompts", address)
	}
	p := s.proc(r.ID)
	if p == nil || p.isExited() {
		return false, api.Errorf(api.CodeConflict, "session %s is not running", address)
	}
	if checkPromptText(pr.Text) != nil { // the authority checked; a replica checks again before typing
		return false, api.Errorf(api.CodeBadRequest, "prompt %s: bad text", pr.ID)
	}
	if strings.Contains(pr.Text, "\n") && !p.bracketedPaste.Load() {
		return false, api.Errorf(api.CodeConflict, "session %s's program takes no bracketed paste", address)
	}
	if err := typePrompt(p, pr.Text); err != nil {
		return true, err // part of it may be on the terminal: never again
	}
	s.lastPrompts.Store(address, orchapi.SessionLastPrompt{Who: pr.Who, At: pr.At, Text: clipLine(pr.Text)})
	if r2, ok := s.load(ctx, address); ok {
		if r2.State == orchapi.StateEnded {
			s.lastPrompts.Delete(address) // it ended while this was typing
		}
		s.publish(orchapi.EventSessionUpdated, r2.Session)
	}
	return true, nil
}

// expirePrompts marks a session's pending prompts expired (it ended).
func (s *Service) expirePrompts(ctx context.Context, address string) {
	_, _ = s.d.Store.Registry().ExecContext(context.WithoutCancel(ctx), `UPDATE session_prompts SET state = ? WHERE session = ? AND state = ?`,
		orchapi.PromptExpired, address, orchapi.PromptPending)
}

// prunePrompts expires old pending prompts and drops prompts older than
// promptKeep.
func (s *Service) prunePrompts() {
	s.expirePending(s.ctx)
	_, _ = s.d.Store.Registry().ExecContext(s.ctx, `DELETE FROM session_prompts WHERE at < ?`, store.Millis(time.Now().Add(-promptKeep)))
}

// membersOnlySessionKeys are the Session fields only whole-project readers
// (members) see: who prompts a shared session and who is using it (SPEC
// 17.10). Events carry them as members-only keys.
var membersOnlySessionKeys = []string{"last_prompt", "in_use"}

// members reports a reader of the session's whole project (or no subject:
// an event, whose bus strips membersOnlySessionKeys per reader).
func members(ctx context.Context, project string) bool {
	sub := core.SubjectOf(ctx)
	return sub == nil || sub.Scope.WholeProject(project)
}

// dressShared adds a shared session's last_prompt and (this machine's
// managed session) in_use, for members only.
func (s *Service) dressShared(ctx context.Context, ss orchapi.Session) orchapi.Session {
	if !ss.Shared || !members(ctx, ss.Project) {
		ss.LastPrompt, ss.InUse = nil, nil
		return ss
	}
	ss.LastPrompt = nil
	if ps, err := s.prompts(ctx, `WHERE session = ? ORDER BY at DESC, id DESC LIMIT 1`, ss.Address); err == nil && len(ps) == 1 {
		ss.LastPrompt = &orchapi.SessionLastPrompt{Who: ps[0].Who, At: ps[0].At, Text: clipLine(ps[0].Text)}
	} else if v, ok := s.lastPrompts.Load(ss.Address); ok { // a replica: the last one it typed
		lp := v.(orchapi.SessionLastPrompt)
		ss.LastPrompt = &lp
	}
	if ss.Machine != s.d.Config.Machine {
		return ss // a replica's session: in_use as its presence said
	}
	ss.InUse = nil
	p := s.proc(ss.ID)
	if p == nil || ss.State == orchapi.StateEnded {
		return ss
	}
	p.mu.Lock()
	attached, typed := len(p.conns), p.typedAt
	p.mu.Unlock()
	recent := !typed.IsZero() && time.Since(typed) < inUseTypedFor
	if attached == 0 && !recent {
		return ss
	}
	u := orchapi.SessionInUse{By: s.d.Projects.LocalIdentity().Name, Attached: attached}
	if recent {
		t := storedTime(typed.Truncate(inUseTypedStep))
		u.TypedAt = &t
	}
	ss.InUse = &u
	return ss
}
