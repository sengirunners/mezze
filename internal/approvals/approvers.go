package approvals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

// codeAlphabet has no 0/O, 1/I/L or U: a code read off a terminal is typed right.
const codeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

func newCode() string {
	c, err := codeFrom(rand.Reader)
	if err != nil {
		panic(err)
	}
	return c
}

// codeFrom draws 8 symbols from r by rejection sampling: a byte at or above
// the largest multiple of the alphabet's size is skipped, so every symbol is
// equally likely (b % 30 alone favours 16 of them).
func codeFrom(r io.Reader) (string, error) {
	n := len(codeAlphabet)
	limit := 256 - 256%n
	out := make([]byte, 0, 8)
	buf := make([]byte, 16)
	for len(out) < 8 {
		k, err := io.ReadAtLeast(r, buf, 1)
		if err != nil {
			return "", err
		}
		for _, b := range buf[:k] {
			if int(b) < limit && len(out) < 8 {
				out = append(out, codeAlphabet[int(b)%n])
			}
		}
	}
	return string(out[:4]) + "-" + string(out[4:]), nil
}

// BootstrapFile (under $ZM_HOME) records that this daemon's first approver
// was bootstrapped. It is created with that approver and never removed by
// the daemon: removing every approver does not reopen bootstrap, so a socket
// caller (any agent on the Mac) cannot rm, add and pair its own node into
// the decide right. Recovery when no approver is left is a manual step the
// human performs on the Mac, outside the API: delete this file by hand.
const BootstrapFile = "approvals/bootstrapped"

func (s *Service) bootstrapPath() string {
	return filepath.Join(s.d.Config.Home, filepath.FromSlash(BootstrapFile))
}

// bootstrapped reports whether the first approver was ever bootstrapped.
// An unreadable marker counts as bootstrapped (fail closed).
func (s *Service) bootstrapped() bool {
	_, err := os.Stat(s.bootstrapPath())
	return !errors.Is(err, os.ErrNotExist)
}

// markBootstrapped creates the marker. created is false when it already
// existed (another pairing won the race): that pairing must not bootstrap.
func (s *Service) markBootstrapped(why string) (created bool, err error) {
	p := s.bootstrapPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, werr := fmt.Fprintf(f, "%s %s\ndelete this file by hand only to let the next pairing bootstrap again (SPEC section 16)\n",
		why, s.now().Format(time.RFC3339))
	if err := f.Close(); werr == nil {
		werr = err
	}
	return true, werr
}

// stuck reports whether a new pairing could never be confirmed: no approver
// is active and the one bootstrap is used up (the marker exists).
func (s *Service) stuck(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	if !s.bootstrapped() {
		return false, nil
	}
	var active int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM approvers WHERE `+canConfirm, orchapi.ApproverActive).Scan(&active)
	return active == 0, err
}

// canConfirm selects the approvers who can confirm another: active, and bound
// to a tailnet device (the confirm route refuses a key approver's socket).
const canConfirm = `state = ? AND node_id NOT LIKE 'key:%'`

// stuckNote explains a pairing nobody can confirm and the manual recovery.
// where names the marker: the full path for local callers, a path under
// $ZM_HOME on this machine for a device.
func (s *Service) stuckNote(name, where string) string {
	return "nobody can confirm this device: no approver is active and this daemon already bootstrapped its first one. " +
		"Recovery is by hand, by the Mac's human: delete " + where + ", then run zm approver rm " + name +
		" and zm approver add " + name + " and pair again; that pairing bootstraps."
}

func (s *Service) markerForDevice() string {
	return "$ZM_HOME/" + BootstrapFile + " on " + s.d.Config.Machine
}

// annotate sets Note on pending and unconfirmed approvers when stuck.
func (s *Service) annotate(as []orchapi.Approver, stuck bool, where string) {
	if !stuck {
		return
	}
	for i := range as {
		if as[i].State == orchapi.ApproverPending || as[i].State == orchapi.ApproverUnconfirmed {
			as[i].Note = s.stuckNote(as[i].Name, where)
		}
	}
}

// normCode upper-cases a typed code and drops dashes and spaces. A code is a
// secret, not an identity, so it may be typed loosely.
func normCode(c string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, strings.TrimSpace(c))
}

func hashCode(c string) string {
	h := sha256.Sum256([]byte("zm-pair\n" + normCode(c)))
	return hex.EncodeToString(h[:])
}

const approverCols = `name, state, coalesce(node_id, ''), node_key, node_name, login, added_by, confirmed_by, created, paired, code_expires, identity,
	account, bound_node, machine, via, bound, ended, ended_why, replaced_by`

var approversMigrations = []store.Migration{
	// identity: whose device (zm approver add --for); '' is the machine owner.
	{Kind: store.KindRegistry, Name: "approvals/0007_approver_identity", Script: `
ALTER TABLE approvers ADD COLUMN identity TEXT NOT NULL DEFAULT '';
`},
}

type approverRow struct {
	orchapi.Approver
	nodeKey string
	// identity is the ID of the person whose device this is ('' = the
	// machine owner).
	identity string
	// boundNode is where a sign-in-bound key answers from: a member's
	// tailnet node, or core.KeyNodeLocal for this daemon's own socket.
	boundNode string
	// owner is set when the project's owner decides from their own machine
	// (owner.go): Name is the identity, NodeName the machine.
	owner bool
	// answerToken is set, with owner, when the owner's answer came with an
	// answer-only token (orchapi.SurfaceAnswerToken).
	answerToken bool
}

// method is how a decision by r was made (orchapi.MethodOwner or MethodApprover).
func (r approverRow) method() string {
	if r.owner {
		return orchapi.MethodOwner
	}
	return orchapi.MethodApprover
}

func scanApprover(sc interface{ Scan(...any) error }) (approverRow, error) {
	var a approverRow
	var created int64
	var paired, exp, bound, ended sql.NullInt64
	err := sc.Scan(&a.Name, &a.State, &a.Node, &a.nodeKey, &a.NodeName, &a.Login, &a.AddedBy, &a.ConfirmedBy, &created, &paired, &exp, &a.identity,
		&a.Account, &a.boundNode, &a.Machine, &a.Via, &bound, &ended, &a.EndedWhy, &a.ReplacedBy)
	a.Created, a.Paired = store.FromMillis(created), store.NullMillis(paired)
	a.Bound, a.Ended = store.NullMillis(bound), store.NullMillis(ended)
	if k, ok := strings.CutPrefix(a.Node, nodeKeyPrefix); ok {
		a.Key = k
	}
	if a.State == orchapi.ApproverPending {
		a.CodeExpires = store.NullMillis(exp)
	}
	return a, err
}

func (s *Service) approvers(ctx context.Context) ([]orchapi.Approver, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+approverCols+` FROM approvers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.Approver{}
	for rows.Next() {
		a, err := scanApprover(rows)
		if err != nil {
			return nil, err
		}
		a.Identity = a.identity // resolved to a name below, after the rows close
		out = append(out, a.Approver)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Identity != "" {
			if who, err := s.d.Projects.Identity(ctx, out[i].Identity); err == nil {
				out[i].Identity = who.Name
			}
		}
	}
	return out, nil
}

// approverOf returns the approver bound to the calling device. ok is false
// unless the stable node ID AND the node key both match (a re-keyed or
// re-registered node must pair again).
func (s *Service) approverOf(ctx context.Context, rm *core.Remote) (approverRow, bool) {
	if rm == nil || rm.NodeID == "" || rm.NodeKey == "" {
		return approverRow{}, false
	}
	a, err := scanApprover(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE node_id = ?`, rm.NodeID))
	if err != nil || a.nodeKey != rm.NodeKey {
		return approverRow{}, false
	}
	return a, true
}

// activeApprover is the decide check: an active approver bound to this
// exact node, and never this machine.
func (s *Service) activeApprover(ctx context.Context, rm *core.Remote) (approverRow, error) {
	a, ok := s.approverOf(ctx, rm)
	if !ok || a.State != orchapi.ApproverActive || rm.Self {
		return approverRow{}, api.Errorf(api.CodeForbidden, "this device is not a paired approver").
			WithHint("pair it: run zm approver add <name> on the daemon's machine, then open the approvals page on the device")
	}
	return a, nil
}

// ---- local: add, ls, rm ----

func (s *Service) handleAdd(w http.ResponseWriter, r *http.Request) {
	var req orchapi.NewApproverRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	out, err := s.AddFor(r.Context(), core.SubjectOf(r.Context()).Address, req.Name, req.For)
	respond(w, out, err)
}

// Add reserves name for a device and issues a one-time pairing code. Adding
// a pending name may issue a new code only after the old code expires.
// Reply attribution is not an authenticated process identity.
func (s *Service) Add(ctx context.Context, by, raw string) (orchapi.NewApproverResponse, error) {
	return s.AddFor(ctx, by, raw, "")
}

// selfAdded prefixes added_by for a phone its person added (AddSelf).
const selfAdded = "self:"

// AddSelf reserves name for a phone of the caller's own person and issues a
// pairing code (SPEC 16 "A person pairs their own phone"): a human identity
// on this daemon, never a session, and only for itself. The phone then waits
// for an existing approver's tap, like any later approver.
func (s *Service) AddSelf(ctx context.Context, sub *authz.Subject, raw string) (orchapi.NewApproverResponse, error) {
	if !aPerson(sub) {
		return orchapi.NewApproverResponse{}, api.Errorf(api.CodeForbidden, "a person pairs their own phone; an agent session cannot").
			WithHint("run zm approver pair-me in your own terminal, not in an agent's")
	}
	if strings.TrimSpace(raw) == "" {
		raw = sub.Identity.Name + "-phone"
	}
	return s.AddFor(ctx, selfAdded+sub.Address, raw, sub.Identity.Name)
}

func (s *Service) handleAddSelf(w http.ResponseWriter, r *http.Request) {
	var req orchapi.NewApproverRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	out, err := s.AddSelf(r.Context(), core.SubjectOf(r.Context()), req.Name)
	respond(w, out, err)
}

// AddFor is Add for a device of person (an identity's name; "" is this
// machine's owner): a human identity on this daemon, else 404.
func (s *Service) AddFor(ctx context.Context, by, raw, person string) (orchapi.NewApproverResponse, error) {
	name, err := checkName(raw)
	if err != nil {
		return orchapi.NewApproverResponse{}, err
	}
	forID := ""
	if person = strings.TrimSpace(person); person != "" {
		who, err := s.d.Projects.Identity(ctx, person)
		if err != nil || who.Kind != api.IdentityHuman {
			return orchapi.NewApproverResponse{}, api.NotFound("identity", person)
		}
		forID = who.ID
	}
	sk := skeleton(name)
	// An approver must not look like another approver or an identity.
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return orchapi.NewApproverResponse{}, err
	}
	for _, id := range ids {
		if id.Name != name && skeleton(id.Name) == sk {
			return orchapi.NewApproverResponse{}, api.Errorf(api.CodeConflict, "approver name %q looks like identity %q", name, id.Name).
				WithHint("use the identity's exact name, or a name that looks like no one else's")
		}
	}
	code := newCode()
	now := s.now()
	exp := now.Add(CodeTTL)
	var resp orchapi.NewApproverResponse
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var other, state string
		var expires sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT name, state, code_expires FROM approvers WHERE skeleton = ? OR name = ?`, sk, name).Scan(&other, &state, &expires)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO approvers (name, skeleton, state, code_hash, code_expires, added_by, created, identity)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, name, sk, orchapi.ApproverPending, hashCode(code), ms(exp), by, ms(now), forID)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		case other != name:
			return api.Errorf(api.CodeConflict, "approver name %q looks like approver %q", name, other)
		case state == orchapi.ApproverPending && expires.Valid && expires.Int64 > ms(now):
			return api.Errorf(api.CodeConflict, "a live pairing is already pending")
		case state != orchapi.ApproverPending:
			return api.Errorf(api.CodeConflict, "%s is already a paired approver (%s)", name, state).
				WithHint("zm approver rm " + name + " first to pair another device")
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE approvers SET code_hash = ?, code_expires = ?, added_by = ?, identity = ? WHERE name = ?`,
				hashCode(code), ms(exp), by, forID, name); err != nil {
				return err
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM approvers WHERE `+canConfirm, orchapi.ApproverActive).Scan(&active); err != nil {
			return err
		}
		a, err := scanApprover(tx.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE name = ?`, name))
		if err != nil {
			return err
		}
		resp = orchapi.NewApproverResponse{Approver: a.Approver, Code: code, Expires: store.FromMillis(ms(exp)),
			PairPath: "/v0/approvals?code=" + strings.ReplaceAll(code, "-", ""), Confirm: active > 0 || s.bootstrapped()}
		if active == 0 && s.bootstrapped() {
			resp.Note = s.stuckNote(name, s.bootstrapPath())
			resp.Approver.Note = resp.Note
		}
		return nil
	})
	return resp, err
}

// IdentityNameFree refuses a new identity name that looks like an approver
// (shares its confusables skeleton) without being that exact name: the
// other half of the rule Add enforces, so no approver and identity look
// alike whichever came first. It also refuses a name that is, or looks
// like, another identity's inbox alias (aliases.go), the other half of the
// rule addAlias enforces: one name never answers for two identities.
func (s *Service) IdentityNameFree(ctx context.Context, name string) error {
	var other string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT name FROM approvers WHERE skeleton = ? AND name != ?`,
		skeleton(name), name).Scan(&other)
	switch {
	case err == nil:
		return api.Errorf(api.CodeConflict, "identity name %q looks like approver %q", name, other).
			WithHint("use the approver's exact name, or a name that looks like no one else's")
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var alias, holder string
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT a.alias, coalesce(i.name, a.identity) FROM identity_aliases a
		LEFT JOIN identities i ON i.id = a.identity WHERE a.skeleton = ?`, skeleton(name)).Scan(&alias, &holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return api.Errorf(api.CodeConflict, "identity name %q is (or looks like) %s's inbox alias %q", name, holder, alias).
		WithHint("pick another name, or remove the alias first: zm identity alias rm " + alias)
}

// aPerson reports whether sub is a human identity acting for itself (never
// a session): the only caller of the self routes.
func aPerson(sub *authz.Subject) bool {
	return sub != nil && sub.Session == "" && sub.Identity.ID != "" && sub.Identity.Kind == api.IdentityHuman &&
		strings.HasPrefix(sub.Address, "human:") && sub.Address == "human:"+sub.Identity.Name
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	as, err := s.List(r.Context())
	respond(w, orchapi.ApproversAt{Daemon: s.d.Config.Machine, Approvers: as}, err)
}

func (s *Service) handleListSelf(w http.ResponseWriter, r *http.Request) {
	as, err := s.ListSelf(r.Context(), core.SubjectOf(r.Context()))
	respond(w, orchapi.ApproversAt{Daemon: s.d.Config.Machine, Approvers: as}, err)
}

// ListSelf is the caller's own person's approvers on this daemon:
// those added for its identity (--for, pair-me), and, for this machine's
// owner, those added for nobody. A member reaches its project's authority
// this way, where GET /v0/approvers is local. Only a person, never a session.
func (s *Service) ListSelf(ctx context.Context, sub *authz.Subject) ([]orchapi.Approver, error) {
	if !aPerson(sub) {
		return nil, api.Errorf(api.CodeForbidden, "a person lists their own approvers; an agent session cannot").
			WithHint("run zm approver ls in your own terminal, not in an agent's")
	}
	as, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	owner := s.d.Projects.LocalIdentity()
	isOwner := owner.ID != "" && owner.ID == sub.Identity.ID
	out := []orchapi.Approver{}
	for _, a := range as {
		if a.Identity != sub.Identity.Name && (a.Identity != "" || !isOwner) {
			continue
		}
		// Who added it: a session's address only to a caller who may see it.
		by, self := strings.CutPrefix(a.AddedBy, selfAdded)
		a.AddedBy = s.d.RedactFor(ctx, sub, by)
		if self {
			a.AddedBy = selfAdded + a.AddedBy
		}
		out = append(out, a)
	}
	return out, nil
}

// List is every approver (local callers); a pending or unconfirmed one that
// nobody can confirm carries a Note saying how to recover.
func (s *Service) List(ctx context.Context) ([]orchapi.Approver, error) {
	as, err := s.approvers(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.stuck(ctx, s.d.Store.Registry())
	s.annotate(as, st, s.bootstrapPath())
	return as, err
}

func (s *Service) handleRemove(w http.ResponseWriter, r *http.Request) {
	a, err := s.Remove(r.Context(), r.PathValue("name"))
	respond(w, a, err)
}

// Remove deletes an approver by its exact name. The right ends at once:
// every decide looks the node up again.
func (s *Service) Remove(ctx context.Context, raw string) (orchapi.Approver, error) {
	name, err := checkName(raw)
	if err != nil {
		return orchapi.Approver{}, err
	}
	var a approverRow
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var err error
		a, err = scanApprover(tx.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE name = ?`, name))
		if errors.Is(err, sql.ErrNoRows) {
			return api.NotFound("approver", name)
		}
		if err != nil {
			return err
		}
		if a.State == orchapi.ApproverActive {
			// An active approver means bootstrap is over (a marker lost or
			// predating this rule is restored before the right goes away).
			if _, err := s.markBootstrapped("restored when active approver " + a.Name + " was removed"); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM approvers WHERE name = ?`, name)
		return err
	})
	if err != nil {
		return orchapi.Approver{}, err
	}
	s.publish(ctx, orchapi.EventApproverRemoved, a.Approver)
	return a.Approver, nil
}

// ---- device: pair, confirm ----

func (s *Service) handlePair(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	var req orchapi.PairRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.Pair(r.Context(), rm, req.Code)
	respond(w, a, err)
}

// Pair binds the approver whose code this is to the calling node.
func (s *Service) Pair(ctx context.Context, rm *core.Remote, code string) (orchapi.Approver, error) {
	switch {
	case rm.Self:
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "pairing refused: this request comes from the daemon's own machine").
			WithHint("pair a separate device (a phone) that joined the tailnet with its own sign-in")
	case rm.Tagged:
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "pairing refused: tagged nodes (servers, CI) are never a person's device")
	case rm.Zm || api.IsDaemonNode(rm.Node):
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "pairing refused: %s runs a zm daemon (its agents could answer)", rm.Node)
	}
	if !s.mayTry(rm.NodeID) {
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "too many wrong pairing codes; wait %s and ask for a new one", CodeTTL)
	}
	if prev, ok := s.approverOf(ctx, rm); ok {
		return orchapi.Approver{}, api.Errorf(api.CodeConflict, "this device is already approver %s", prev.Name)
	}
	now := s.now()
	var a approverRow
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var name, addedBy string
		err := tx.QueryRowContext(ctx, `SELECT name, added_by FROM approvers WHERE code_hash = ? AND state = ? AND code_expires > ?`,
			hashCode(code), orchapi.ApproverPending, ms(now)).Scan(&name, &addedBy)
		if errors.Is(err, sql.ErrNoRows) || normCode(code) == "" {
			s.failed(rm.NodeID)
			return api.Errorf(api.CodeForbidden, "invalid or expired pairing code").
				WithHint("run zm approver add <name> on the daemon's machine for a new one")
		}
		if err != nil {
			return err
		}
		var bound string
		err = tx.QueryRowContext(ctx, `SELECT name FROM approvers WHERE node_id = ?`, rm.NodeID).Scan(&bound)
		switch {
		case err == nil:
			// approverOf failed above, so the node key changed (a re-keyed
			// or re-registered node): its old pairing never works again.
			return api.Errorf(api.CodeConflict, "this device's node key changed since it paired as approver %s; it must pair again", bound).
				WithHint("on the daemon's machine: zm approver rm " + bound + ", then zm approver add " + bound +
					" (an existing approver confirms it)")
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// The first approver of this daemon is bootstrapped by pairing
		// alone, once (BootstrapFile); every later one, even after every
		// approver was removed, waits for an existing approver's confirmation.
		// A key (one bound by a sign-in is active with no device) confirms
		// nobody, so it neither blocks nor uses up the bootstrap.
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM approvers WHERE `+canConfirm, orchapi.ApproverActive).Scan(&active); err != nil {
			return err
		}
		state, by := orchapi.ApproverUnconfirmed, ""
		// A phone its person added for themselves (AddSelf) never bootstraps:
		// someone else's device confirms it.
		if active == 0 && !s.bootstrapped() && !strings.HasPrefix(addedBy, selfAdded) {
			created, err := s.markBootstrapped("first approver " + name + " bootstrapped")
			if err != nil {
				return err
			}
			if created {
				state, by = orchapi.ApproverActive, "bootstrap"
			}
		} else if active > 0 {
			if _, err := s.markBootstrapped("restored: approvers were active when " + name + " paired"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvers SET state = ?, confirmed_by = ?, code_hash = NULL, code_expires = NULL,
			node_id = ?, node_key = ?, node_name = ?, login = ?, paired = ? WHERE name = ?`,
			state, by, rm.NodeID, rm.NodeKey, rm.Node, rm.Login, ms(now), name); err != nil {
			return err
		}
		a, err = scanApprover(tx.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE name = ?`, name))
		return err
	})
	if err != nil {
		return orchapi.Approver{}, err
	}
	if a.State == orchapi.ApproverActive {
		s.d.Log.Warn("approvals: first approver bootstrapped (no confirmation); later pairings need one", "approver", a.Name,
			"node", a.NodeName, "marker", s.bootstrapPath())
		s.publish(ctx, orchapi.EventApproverAdded, a.Approver)
	} else if st, err := s.stuck(ctx, s.d.Store.Registry()); err == nil && st {
		a.Note = s.stuckNote(a.Name, s.markerForDevice())
		s.d.Log.Warn("approvals: a device paired but no active approver can confirm it", "approver", a.Name,
			"node", a.NodeName, "recovery", "delete "+s.bootstrapPath()+" by hand, rm and add the approver, pair again")
	}
	return a.Approver, nil
}

func pairBudget(node string) string {
	if strings.HasPrefix(node, "key\x00") {
		return "key-budget"
	}
	return ""
}

// mayTry reports whether node may try another pairing code.
func (s *Service) mayTry(node string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	cut := s.now().Add(-CodeTTL)
	for k, ts := range s.fails {
		keep := ts[:0]
		for _, t := range ts {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		s.fails[k] = keep
	}
	return len(s.fails[node]) < maxPairFailures && len(s.fails[pairBudget(node)]) < 4*maxPairFailures
}

func (s *Service) failed(node string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	now := s.now()
	s.fails[node] = append(s.fails[node], now)
	budget := pairBudget(node)
	s.fails[budget] = append(s.fails[budget], now)
}

func (s *Service) handleConfirm(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	var req orchapi.ConfirmRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.ConfirmNode(r.Context(), rm, r.PathValue("name"), req.Node)
	respond(w, a, err)
}

// Confirm activates a paired, unconfirmed approver bound to a tailnet device.
func (s *Service) Confirm(ctx context.Context, rm *core.Remote, raw string) (orchapi.Approver, error) {
	return s.ConfirmNode(ctx, rm, raw, "")
}

// ConfirmNode activates a paired, unconfirmed approver; the caller must be an
// active approver on its own device (never the one it confirms). node is what
// the person compared: a name can be removed, added and paired to another
// device between their look and their tap, so a different node is a conflict.
func (s *Service) ConfirmNode(ctx context.Context, rm *core.Remote, raw, node string) (orchapi.Approver, error) {
	me, err := s.activeApprover(ctx, rm)
	if err != nil {
		return orchapi.Approver{}, err
	}
	name, err := checkName(raw)
	if err != nil {
		return orchapi.Approver{}, err
	}
	var a approverRow
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var err error
		a, err = scanApprover(tx.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE name = ?`, name))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return api.NotFound("approver", name)
		case err != nil:
			return err
		case a.State != orchapi.ApproverUnconfirmed:
			return api.Errorf(api.CodeConflict, "approver %s is %s, not waiting for confirmation", name, a.State)
		case a.Node == me.Node:
			return api.Errorf(api.CodeForbidden, "a device cannot confirm itself")
		case node == "" && a.Key != "":
			return api.Errorf(api.CodeBadRequest, "confirming a key names the key you compared")
		case node != "" && node != a.Node:
			return api.Errorf(api.CodeConflict, "approver %s is now another device; look again before confirming", name)
		}
		if err := s.keyConfirmable(ctx, tx, a); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvers SET state = ?, confirmed_by = ? WHERE name = ? AND state = ?`,
			orchapi.ApproverActive, me.Name, name, orchapi.ApproverUnconfirmed); err != nil {
			return err
		}
		a.State, a.ConfirmedBy = orchapi.ApproverActive, me.Name
		return nil
	})
	if err != nil {
		return orchapi.Approver{}, err
	}
	s.publish(ctx, orchapi.EventApproverAdded, a.Approver)
	return a.Approver, nil
}

// publish appends a machine-level approver event (owner only).
func (s *Service) publish(ctx context.Context, typ string, a orchapi.Approver) {
	if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: typ, Subject: a.Name, Data: a}); err != nil {
		s.d.Log.Warn("approvals: publish", "type", typ, "err", err)
	}
}

// unconfirmed lists paired devices waiting for an approver's confirmation.
func (s *Service) unconfirmed(ctx context.Context) ([]orchapi.Approver, error) {
	all, err := s.approvers(ctx)
	if err != nil {
		return nil, err
	}
	var out []orchapi.Approver
	for _, a := range all {
		if a.State == orchapi.ApproverUnconfirmed {
			out = append(out, a)
		}
	}
	return out, nil
}
