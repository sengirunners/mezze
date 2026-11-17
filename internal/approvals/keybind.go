package approvals

// Keys bound by a sign-in (SPEC 16 "Key approvers").
// A person's cockpit (the Mezze app) makes a signing key and binds it through a GitHub
// login of purpose "key" that this authority runs (the owners brick, through
// core.Deps.KeyLogin): the person is the identity that started the login,
// the account is GitHub's answer, the node is the starting credential's. No
// second device confirms it. What holds instead, on every answer:
//
//   - (a) the key's person still owns or administers the ask's project
//     (answersProject: the test that decides whose answer an ask takes);
//   - (b) a bound key's person is still linked to the account it was bound as;
//   - (c) a bound key answers only from the node it was bound on (this
//     daemon's own socket, or a member's node-bound token for that node);
//   - (d) the daemon whose socket took the call checked the caller runs in
//     no registered agent session (team's origin gate, orchapi.OriginChecked):
//     a member too old to mark it is refused, naming the member.
//
// One person has one active bound key per node: a new bind ends the others
// (replaced_by). Keys paired the old way end keyLegacyGrace after this code
// first ran here, unless a sign-in binds them; unconfirmed ones after
// keyUnconfirmedTTL.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var keyBindMigrations = []store.Migration{
	// A key bound by a sign-in: account, the node and machine it answers
	// from, the client's install id (display only), when, how, and when and
	// why it ended. Decisions record the answerer's account and where they
	// answered. approvals_marks.key_login is when this code first ran here:
	// legacy keys end keyLegacyGrace later.
	{Kind: store.KindRegistry, Name: "approvals/0020_key_login", Script: `
ALTER TABLE approvers ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE approvers ADD COLUMN bound_node TEXT NOT NULL DEFAULT '';
ALTER TABLE approvers ADD COLUMN machine TEXT NOT NULL DEFAULT '';
ALTER TABLE approvers ADD COLUMN install TEXT NOT NULL DEFAULT '';
ALTER TABLE approvers ADD COLUMN bound INTEGER;
ALTER TABLE approvers ADD COLUMN via TEXT NOT NULL DEFAULT '';
ALTER TABLE approvers ADD COLUMN ended INTEGER;
ALTER TABLE approvers ADD COLUMN ended_why TEXT NOT NULL DEFAULT '';
ALTER TABLE approvers ADD COLUMN replaced_by TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN surface TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN account_login TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN key_machine TEXT NOT NULL DEFAULT '';
CREATE TABLE approvals_marks (name TEXT PRIMARY KEY, at INTEGER NOT NULL);
INSERT INTO approvals_marks (name, at) VALUES ('key_login', CAST(strftime('%s', 'now') AS INTEGER) * 1000);
`},
}

// Tunables (tests read them).
var (
	// keyLegacyGrace is how long a key paired the old way keeps answering
	// after this code first ran at the authority.
	keyLegacyGrace = 7 * 24 * time.Hour
	// keyUnconfirmedTTL is how long an old-style pairing waits for a confirm.
	keyUnconfirmedTTL = 24 * time.Hour
)

// keyNonce is a client's single-use value in a signed key statement:
// printable, no separators, so the statement's lines cannot be shifted.
var keyNonce = regexp.MustCompile(`^[A-Za-z0-9_.:=+/-]{8,128}$`)

// KeyLogin is this module's core.KeyLogin (wiring sets core.Deps.KeyLogin).
func (s *Service) KeyLogin() core.KeyLogin { return keyLogin{s} }

type keyLogin struct{ s *Service }

type originCheckedKey struct{}

// GateKeyLogin is a request gate (core.RequestGate) that carries
// orchapi.OriginChecked on a GitHub login start into its context, where
// the key login's check reads it (the kit hands that check the headers
// only). Wiring adds it after team's origin gate, which sets the mark.
func (s *Service) GateKeyLogin(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v0/login/github/start" || r.URL.Query().Get(orchapi.OriginChecked) != "1" {
		return false
	}
	serve(w, r.WithContext(context.WithValue(r.Context(), originCheckedKey{}, true)))
	return true
}

// keyCallsWhat names a key's person-only calls in orchcore.OriginChecked's
// member-too-old refusal; anyone else unchecked gets keyRefused (rule d).
const keyCallsWhat = "its key sign-ins and key answers"

// callerNode is where sub calls from, as a bound key's node: this daemon's
// own socket owner (core.KeyNodeLocal) or a member's node-bound token (its
// tailnet node). Any other caller (a minted token, a tailnet login) has none.
func callerNode(sub *authz.Subject) (string, bool) {
	switch {
	case sub == nil:
		return "", false
	case sub.Via == authz.ViaLocal && sub.TokenID == "":
		return core.KeyNodeLocal, true
	case sub.Via == authz.ViaToken && sub.Node != "":
		return sub.Node, true
	}
	return "", false
}

// keyPerson is the identity id a key row answers as (” = this machine's
// owner, for a key paired without --for).
func (s *Service) keyPerson(me approverRow) string {
	if me.identity != "" {
		return me.identity
	}
	return s.d.Projects.LocalIdentity().ID
}

// answersProject reports whether identity (an id) owns or administers
// project now by its own grants (core.Deps.ProjectAdmin), as a permit's
// signer is judged: a person link or a shared GitHub account folds people
// for naming, never for rights. One function for a key login's start and
// bind, and every key answer and release.
func (s *Service) answersProject(ctx context.Context, identity, project string) (bool, error) {
	if identity == "" || project == "" {
		return false, nil
	}
	sub, err := s.d.Projects.IdentitySubject(ctx, identity)
	if api.IsCode(err, api.CodeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if sub.Identity.Kind != api.IdentityHuman {
		return false, nil
	}
	return s.d.ProjectAdmin(ctx, sub, project)
}

// keyStatement is a signed key statement's bytes: domain, then one field
// per line.
func keyStatement(domain string, fields ...string) []byte {
	return []byte(domain + "\n" + strings.Join(fields, "\n"))
}

// keyNonceCap is how many nonces one scope (a person's sign-ins, or one
// key's sign-outs) may hold at once: about 20 minutes' worth.
var keyNonceCap = 64

// nonceBook remembers the nonces of signed key statements in two
// generations of keyNonceSpan each, so a nonce is kept at least as long as
// its statement's time can pass fresh, and no call scans the whole book.
// Each scope holds at most keyNonceCap: a full scope is refused, so one
// caller fills only its own share. Nonces live in memory: after a restart
// a statement captured in its last minutes could be sent again, which for
// a bind still needs the person's GitHub approval and for a removal only
// signs the key out.
type nonceBook struct {
	mu        sync.Mutex
	start     time.Time
	cur, prev map[string]string // fp NUL nonce -> scope
	count     map[string]int    // scope -> nonces in cur and prev
}

// keyNonceSpan is one generation: a statement's time passes within
// keyAnswerSkew either side, so 2*keyAnswerSkew covers its whole window.
const keyNonceSpan = 2 * keyAnswerSkew

func (b *nonceBook) rotate(now time.Time) {
	switch d := now.Sub(b.start); {
	case b.cur == nil || d >= 2*keyNonceSpan:
		// First use, or a long gap: nothing kept is still fresh. (A clock
		// that went back keeps everything: never forget a nonce early.)
		b.cur, b.prev, b.count, b.start = map[string]string{}, map[string]string{}, map[string]int{}, now
	case d >= keyNonceSpan:
		for _, scope := range b.prev {
			if b.count[scope]--; b.count[scope] <= 0 {
				delete(b.count, scope)
			}
		}
		b.prev, b.cur, b.start = b.cur, map[string]string{}, b.start.Add(keyNonceSpan)
	}
}

// take records fp's nonce under scope; false if it was used or the scope
// is full.
func (b *nonceBook) take(now time.Time, scope, fp, nonce string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rotate(now)
	k := fp + "\x00" + nonce
	if _, used := b.cur[k]; used {
		return false
	}
	if _, used := b.prev[k]; used {
		return false
	}
	if b.count[scope] >= keyNonceCap {
		return false
	}
	b.cur[k] = scope
	b.count[scope]++
	return true
}

// fresh checks a signed statement's time and takes its nonce (once per key
// within the window, at most keyNonceCap per scope). It answers false for
// a stale time, a used nonce or a full scope. Callers run every other
// check first, so only a caller who passed them spends a nonce.
func (s *Service) fresh(scope, fp, nonce, at string) bool {
	if !s.timely(nonce, at) {
		return false
	}
	return s.nonces.take(s.now(), scope, fp, nonce)
}

// timely checks a signed statement's nonce shape and time, spending
// nothing (so a stale statement learns nothing from later checks).
func (s *Service) timely(nonce, at string) bool {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil || !keyNonce.MatchString(nonce) {
		return false
	}
	d := s.now().Sub(t)
	return d <= keyAnswerSkew && d >= -keyAnswerSkew
}

// legacyKeysOver reports whether keys paired the old way stopped
// answering: keyLegacyGrace after sign-in binding first ran here. It is
// checked at use (answer, pairing, confirm), so the deadline holds without
// the sweep; an unreadable mark is an error (the caller refuses).
func (s *Service) legacyKeysOver(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	var since int64
	if err := q.QueryRowContext(ctx, `SELECT at FROM approvals_marks WHERE name = 'key_login'`).Scan(&since); err != nil {
		return true, err
	}
	return !s.now().Before(store.FromMillis(since).Add(keyLegacyGrace)), nil
}

// keyConfirmable refuses confirming a key paired the old way once such
// keys stopped answering, or once its pairing waited past
// keyUnconfirmedTTL (the sweep drops those rows too, but late).
func (s *Service) keyConfirmable(ctx context.Context, tx *sql.Tx, a approverRow) error {
	if !strings.HasPrefix(a.Node, nodeKeyPrefix) || a.Via != "" {
		return nil
	}
	over, err := s.legacyKeysOver(ctx, tx)
	if err != nil {
		return err
	}
	if over {
		return errLegacyKeyOver()
	}
	if a.Paired == nil || s.now().Sub(*a.Paired) > keyUnconfirmedTTL {
		return api.Errorf(api.CodeConflict, "approver %s waited too long for a confirm; pair its key again or sign in from the cockpit", a.Name)
	}
	return nil
}

func errLegacyKeyOver() error {
	return api.Errorf(api.CodeForbidden, "keys are no longer paired with a code on this authority").
		WithHint("sign in from the cockpit: a GitHub sign-in binds its key")
}

// Check is core.KeyLogin's start check: the caller was checked (rule d),
// the proof is the key's over this authority, project, nonce and time, the
// nonce is new, the identity answers the project, and no other person holds
// the key.
func (k keyLogin) Check(ctx context.Context, sub *authz.Subject, _ http.Header, ks core.KeyStart) error {
	s := k.s
	marked, _ := ctx.Value(originCheckedKey{}).(bool)
	if err := orchcore.OriginChecked(sub, marked, keyCallsWhat, keyRefused()); err != nil {
		return err
	}
	pub, raw, err := parseKey(ks.Key.Key)
	if err != nil {
		return api.Errorf(api.CodeBadRequest, "key must be base64 of an uncompressed P-256 key")
	}
	daemon, err := s.PublicKey()
	if err != nil {
		return err
	}
	rk := KeyID(daemon)
	ok := false
	for _, p := range []string{ks.Project.Slug, ks.Project.ID} {
		if p != "" && verifySig(pub, keyStatement(orchapi.BindKeyVersion, rk, p, ks.Key.Key, ks.Key.Nonce, ks.Key.Time), ks.Key.Proof) {
			ok = true
			break
		}
	}
	refused := api.Errorf(api.CodeForbidden, "the key's proof does not hold (wrong key, authority, project or time, or a nonce used before)").
		WithHint("sign " + orchapi.BindKeyVersion + ", this authority's receipt key id, the project, the key, a new nonce and the time, one per line")
	if !ok || !s.timely(ks.Key.Nonce, ks.Key.Time) {
		return refused
	}
	fp := fingerprint(raw)
	// The project and the key's holder before the nonce: only a person who
	// answers the project, with a key that is no one else's, spends one.
	if yes, err := s.answersProject(ctx, sub.Identity.ID, ks.Project.ID); err != nil {
		return err
	} else if !yes {
		return keyNoProject(sub.Identity.Name, ks.Project.Slug)
	}
	held, err := scanApprover(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE node_id = ?`, nodeKeyPrefix+fp))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case s.keyPerson(held) != sub.Identity.ID:
		return api.Errorf(api.CodeConflict, "this key is already approver %s, another person's", held.Name)
	}
	if !s.fresh("bind\x00"+sub.Identity.ID, fp, ks.Key.Nonce, ks.Key.Time) {
		return refused
	}
	return nil
}

func keyNoProject(who, project string) error {
	return api.Errorf(api.CodeForbidden, "%s does not own or administer project %s, so a key of theirs would answer nothing there", who, project).
		WithHint("sign in for a project you answer; zm grant gives someone admin")
}

// Bind is core.KeyLogin's bind, inside the login's registry transaction
// (after the link): the starting identity's key, active now, bound to the
// account and node. It re-checks the project, ends the person's other
// bound key on that node and their keys paired the old way.
func (k keyLogin) Bind(ctx context.Context, tx *sql.Tx, b core.KeyBind) (api.KeyBinding, error) {
	s := k.s
	if yes, err := s.answersProject(ctx, b.Identity.ID, b.Project.ID); err != nil {
		return api.KeyBinding{}, api.Errorf(api.CodeUnavailable, "checking %s's grants: %v", b.Identity.Name, err)
	} else if !yes {
		return api.KeyBinding{}, keyNoProject(b.Identity.Name, b.Project.Slug)
	}
	_, raw, err := parseKey(b.Key.Key)
	if err != nil {
		return api.KeyBinding{}, api.Errorf(api.CodeBadRequest, "key must be base64 of an uncompressed P-256 key")
	}
	daemon, err := s.PublicKey()
	if err != nil {
		return api.KeyBinding{}, err
	}
	fp := fingerprint(raw)
	node, now := nodeKeyPrefix+fp, ms(s.now())
	device := cleanDevice(b.Key.Device)
	install := cleanDevice(b.Key.Install)
	local := s.d.Projects.LocalIdentity().ID
	held, err := scanApprover(tx.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE node_id = ?`, node))
	var name string
	switch {
	case err == nil:
		if s.keyPerson(held) != b.Identity.ID {
			return api.KeyBinding{}, api.Errorf(api.CodeConflict, "this key is already approver %s, another person's", held.Name)
		}
		// The same key again (or a key paired the old way, upgraded in
		// place): active, bound as now.
		name = held.Name
		if _, err := tx.ExecContext(ctx, `UPDATE approvers SET state = ?, identity = ?, account = ?, login = ?, bound_node = ?, machine = ?,
			install = ?, bound = ?, via = ?, ended = NULL, ended_why = '', replaced_by = '', node_key = ?, node_name = ?,
			code_hash = NULL, code_expires = NULL, paired = coalesce(paired, ?) WHERE name = ?`,
			orchapi.ApproverActive, b.Identity.ID, b.Account, b.Login, b.Node, b.Machine, install, now, orchapi.ViaLogin,
			b.Key.Key, device, now, name); err != nil {
			return api.KeyBinding{}, err
		}
	case errors.Is(err, sql.ErrNoRows):
		// Never take over a row found by name: only the node binds. A name
		// someone took first gets the next free suffix.
		base := keyApproverName(b.Machine, fp)
		name = ""
		for i := 1; i <= 9 && name == ""; i++ {
			try := base
			if i > 1 {
				try = fmt.Sprintf("%s-%d", strings.TrimSuffix(base[:min(len(base), 60)], "-"), i)
			}
			var other string
			switch err := tx.QueryRowContext(ctx, `SELECT name FROM approvers WHERE name = ? OR skeleton = ?`, try, skeleton(try)).Scan(&other); {
			case errors.Is(err, sql.ErrNoRows):
				name = try
			case err != nil:
				return api.KeyBinding{}, err
			}
		}
		if name == "" {
			return api.KeyBinding{}, api.Errorf(api.CodeConflict, "every approver name for this key is taken").
				WithHint("zm approver rm the key-* approvers you do not know on this authority, then sign in again")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approvers (name, skeleton, state, node_id, node_key, node_name, login, added_by,
			confirmed_by, created, paired, identity, account, bound_node, machine, install, bound, via)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			name, skeleton(name), orchapi.ApproverActive, node, b.Key.Key, device, b.Login, "github:@"+b.Login,
			"github sign-in", now, now, b.Identity.ID, b.Account, b.Node, b.Machine, install, now, orchapi.ViaLogin); err != nil {
			return api.KeyBinding{}, err
		}
	default:
		return api.KeyBinding{}, err
	}
	// One active bound key per person per node; their keys paired the old
	// way end too (an agent's key bound behind the person's back is ended by
	// the person's next sign-in, and theirs by its: either way it shows).
	mine := `identity = ?`
	if b.Identity.ID == local {
		mine = `(identity = ? OR identity = '')` // keys paired for nobody are this machine's owner's
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvers SET state = ?, ended = ?, ended_why = ?, replaced_by = ?
		WHERE `+mine+` AND node_id LIKE 'key:%' AND node_id != ? AND state != ? AND (
			(via = ? AND bound_node = ?) OR via = '')`,
		orchapi.ApproverRemoved, now, orchapi.EndedReplaced, fp, b.Identity.ID, node, orchapi.ApproverRemoved, orchapi.ViaLogin, b.Node); err != nil {
		return api.KeyBinding{}, err
	}
	s.d.Log.Warn("approvals: a key was bound by a GitHub sign-in", "approver", name, "key", fp, "identity", b.Identity.Name,
		"account", b.Account, "machine", b.Machine, "node", b.Node)
	return api.KeyBinding{Approver: name, Fingerprint: fp, State: orchapi.ApproverActive, ReceiptKey: KeyID(daemon),
		Account: b.Account, Login: b.Login, Machine: b.Machine}, nil
}

// cleanDevice is a client-given label made safe to show.
func cleanDevice(s string) string {
	d := strings.TrimSpace(textclean.Clean(s))
	if unsafePresentation(d) {
		return ""
	}
	if r := []rune(d); len(r) > maxDeviceName {
		d = string(r[:maxDeviceName])
	}
	return d
}

// keyApproverName names a bound key: key-<machine>-<the fingerprint's 16
// hex>, the machine cut to a valid name's characters and length.
func keyApproverName(machine, fp string) string {
	hexPart := strings.TrimPrefix(fp, keyFingerAlg)
	m := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return '-'
	}, machine)
	m = strings.Trim(m, "-")
	if max := 62 - len("key--") - len(hexPart); len(m) > max {
		m = strings.Trim(m[:max], "-")
	}
	if m == "" {
		return "key-" + hexPart
	}
	return "key-" + m + "-" + hexPart
}

// keyAnswerer is the checks a key's answer passes before its ask is read
// (rules b, c, d); rule a needs the ask (keyAnswersProject).
func (s *Service) keyAnswerer(r *http.Request, me approverRow) error {
	if me.Via != orchapi.ViaLogin {
		// Paired the old way: rule a only, until the grace period ends
		// (checked here, so the end holds even if the sweep never ran).
		over, err := s.legacyKeysOver(r.Context(), s.d.Store.Registry())
		if err != nil {
			return err
		}
		if over {
			return keyRefused()
		}
		return nil
	}
	sub := core.SubjectOf(r.Context())
	if err := orchcore.OriginChecked(sub, r.URL.Query().Get(orchapi.OriginChecked) == "1", keyCallsWhat, keyRefused()); err != nil {
		return err
	}
	if node, ok := callerNode(sub); !ok || node != me.boundNode {
		return keyRefused()
	}
	own := s.ownAccount.Load()
	if own == nil || *own == nil {
		return keyRefused()
	}
	if acc, _ := (*own)(r.Context(), me.identity); acc == "" || acc != me.Account {
		return keyRefused()
	}
	return nil
}

// keyStillActive re-reads a key approver in the decision's transaction: a
// sign-out, a replacing bind or the sweep that ended the key after its
// checks ran refuses the answer instead of recording it.
func (s *Service) keyStillActive(ctx context.Context, tx *sql.Tx, me approverRow) error {
	if me.owner || !strings.HasPrefix(me.Node, nodeKeyPrefix) {
		return nil
	}
	var state, node, via, bound string
	err := tx.QueryRowContext(ctx, `SELECT state, coalesce(node_id, ''), via, bound_node FROM approvers WHERE name = ?`, me.Name).Scan(&state, &node, &via, &bound)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (state != orchapi.ApproverActive || node != me.Node || via != me.Via || bound != me.boundNode) {
		return keyRefused()
	}
	if err != nil || via != "" {
		return err
	}
	over, err := s.legacyKeysOver(ctx, tx)
	if err == nil && over {
		return keyRefused()
	}
	return err
}

// keyAnswersProject is rule a for a key's answer to an ask of project.
func (s *Service) keyAnswersProject(ctx context.Context, me approverRow, project string) error {
	yes, err := s.answersProject(ctx, s.keyPerson(me), project)
	if err != nil {
		return err
	}
	if !yes {
		return keyRefused()
	}
	return nil
}

func (s *Service) handleUnbindKey(w http.ResponseWriter, r *http.Request) {
	var req orchapi.UnbindKeyRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.UnbindKey(r, req)
	respond(w, a, err)
}

// UnbindKey ends a key by its own signature (signing out): checked like an
// answer (rules c and d for a bound key), with a single-use nonce.
func (s *Service) UnbindKey(r *http.Request, req orchapi.UnbindKeyRequest) (orchapi.Approver, error) {
	ctx := r.Context()
	if err := orchcore.OriginChecked(core.SubjectOf(ctx), r.URL.Query().Get(orchapi.OriginChecked) == "1", keyCallsWhat, keyRefused()); err != nil {
		return orchapi.Approver{}, err
	}
	me, err := scanApprover(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE node_id = ?`, nodeKeyPrefix+req.Key))
	if err != nil || me.State == orchapi.ApproverRemoved || !strings.HasPrefix(req.Key, keyFingerAlg) {
		return orchapi.Approver{}, keyRefused()
	}
	pub, raw, err := parseKey(me.nodeKey)
	if err != nil || fingerprint(raw) != req.Key {
		return orchapi.Approver{}, keyRefused()
	}
	daemon, err := s.PublicKey()
	if err != nil {
		return orchapi.Approver{}, err
	}
	if !verifySig(pub, keyStatement(orchapi.UnbindKeyVersion, KeyID(daemon), req.Key, req.Nonce, req.Time), req.Proof) {
		return orchapi.Approver{}, keyRefused()
	}
	if me.Via == orchapi.ViaLogin {
		if node, ok := callerNode(core.SubjectOf(ctx)); !ok || node != me.boundNode {
			return orchapi.Approver{}, keyRefused()
		}
	}
	if !s.fresh("unbind\x00"+req.Key, req.Key, req.Nonce, req.Time) {
		return orchapi.Approver{}, keyRefused()
	}
	now := ms(s.now())
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE approvers SET state = ?, ended = ?, ended_why = ? WHERE name = ? AND state != ?`,
		orchapi.ApproverRemoved, now, orchapi.EndedSignedOut, me.Name, orchapi.ApproverRemoved); err != nil {
		return orchapi.Approver{}, err
	}
	me.State, me.Ended, me.EndedWhy = orchapi.ApproverRemoved, store.NullMillis(sql.NullInt64{Int64: now, Valid: true}), orchapi.EndedSignedOut
	s.d.Log.Warn("approvals: a key ended itself", "approver", me.Name, "key", req.Key)
	s.publish(ctx, orchapi.EventApproverRemoved, me.Approver)
	return me.Approver, nil
}

// sweepKeys ends keys paired the old way once the grace period is over and
// drops old-style pairings never confirmed (retry ticker).
func (s *Service) sweepKeys(ctx context.Context) {
	reg := s.d.Store.Registry()
	now := s.now()
	if _, err := reg.ExecContext(ctx, `DELETE FROM approvers WHERE state = ? AND node_id LIKE 'key:%' AND via = '' AND paired < ?`,
		orchapi.ApproverUnconfirmed, ms(now.Add(-keyUnconfirmedTTL))); err != nil && ctx.Err() == nil {
		s.d.Log.Warn("approvals: sweep unconfirmed keys", "err", err)
	}
	var since int64
	if err := reg.QueryRowContext(ctx, `SELECT at FROM approvals_marks WHERE name = 'key_login'`).Scan(&since); err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("approvals: read the key-login mark", "err", err)
		}
		return
	}
	if now.Before(store.FromMillis(since).Add(keyLegacyGrace)) {
		return
	}
	rows, err := reg.QueryContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE node_id LIKE 'key:%' AND via = '' AND state != ?`, orchapi.ApproverRemoved)
	if err != nil {
		if ctx.Err() == nil {
			s.d.Log.Warn("approvals: list legacy keys", "err", err)
		}
		return
	}
	var ending []approverRow
	for rows.Next() {
		a, err := scanApprover(rows)
		if err != nil {
			break
		}
		ending = append(ending, a)
	}
	if err := rows.Err(); err != nil && ctx.Err() == nil {
		s.d.Log.Warn("approvals: list legacy keys", "err", err) // the next tick ends the rest
	}
	_ = rows.Close()
	at := ms(now)
	for _, a := range ending {
		res, err := reg.ExecContext(ctx, `UPDATE approvers SET state = ?, ended = ?, ended_why = ? WHERE name = ? AND via = '' AND state != ?`,
			orchapi.ApproverRemoved, at, orchapi.EndedLegacy, a.Name, orchapi.ApproverRemoved)
		if err != nil {
			if ctx.Err() == nil {
				s.d.Log.Warn("approvals: end a legacy key", "approver", a.Name, "err", err)
			}
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			a.State, a.Ended, a.EndedWhy = orchapi.ApproverRemoved, store.NullMillis(sql.NullInt64{Int64: at, Valid: true}), orchapi.EndedLegacy
			s.d.Log.Warn("approvals: a key paired before sign-in binding ended", "approver", a.Name)
			s.publish(ctx, orchapi.EventApproverRemoved, a.Approver)
		}
	}
}

// answeredFromCols is askCols' last column: where a decision was made,
// as which account, and whose it was not (answeredWhere reads it).
const answeredFromCols = `json_object('surface', d.surface, 'account', d.account, 'login', d.account_login, 'machine', d.key_machine, 'not_theirs', d.not_theirs)`

// answeredWhere fills d's machine or device (device is the decisions
// row's) and, from answeredFromCols' JSON, its surface and account.
func answeredWhere(d *orchapi.Decision, device, from string) {
	if d.Method == orchapi.MethodOwner {
		d.Machine = device
	} else {
		d.Device = device
	}
	var f struct {
		Surface, Account, Login, Machine string
		NotTheirs                        string `json:"not_theirs"`
	}
	_ = json.Unmarshal([]byte(from), &f)
	d.Surface, d.Account, d.Login, d.NotTheirs = f.Surface, f.Account, f.Login, f.NotTheirs
	if f.Machine != "" {
		d.Machine = f.Machine
	}
}

// setAnsweredFrom stores what answeredFrom found, in the decision's
// transaction.
func setAnsweredFrom(ctx context.Context, tx *sql.Tx, d orchapi.Decision) error {
	machine := ""
	if d.Surface == orchapi.SurfaceKey {
		machine = d.Machine
	}
	_, err := tx.ExecContext(ctx, `UPDATE decisions SET surface = ?, account = ?, account_login = ?, key_machine = ?, not_theirs = ? WHERE ask = ?`,
		d.Surface, d.Account, d.Login, machine, d.NotTheirs, d.Ask)
	return err
}

// answeredFrom records on d where me answered and as which account: the
// owner's machine or the device; a sign-in-bound key's own account and
// machine; anyone else's verified link as it stands now
// (orchapi.Decision.Account). Never from a signed body.
func (s *Service) answeredFrom(ctx context.Context, me approverRow, d *orchapi.Decision) {
	if me.owner {
		d.Machine = me.NodeName
	} else {
		d.Device = first(me.NodeName, me.Node)
	}
	identity := ""
	switch {
	case me.owner:
		d.Surface = orchapi.SurfaceOwner
		if me.answerToken {
			d.Surface = orchapi.SurfaceAnswerToken
		}
		if id, err := s.d.Projects.Identity(ctx, me.Name); err == nil {
			identity = id.ID
		}
	case strings.HasPrefix(me.Node, nodeKeyPrefix):
		d.Surface = orchapi.SurfaceKey
		if me.Via == orchapi.ViaLogin {
			d.Account, d.Login, d.Machine = me.Account, me.Login, me.Machine
			return
		}
		identity = s.keyPerson(me)
	default:
		d.Surface = orchapi.SurfaceDevice
		identity = me.identity // a device paired for no one is no one's (approverAdmin)
	}
	if own := s.ownAccount.Load(); identity != "" && own != nil && *own != nil {
		d.Account, d.Login = (*own)(ctx, identity)
	}
}
