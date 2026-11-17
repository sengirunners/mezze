// Package secretkeys is the key directory (SPEC 17.12): each project's
// authority lists the seal key every member machine published, so a sender
// can seal a secret to a machine before it leaves the sender (sealed drops,
// SPEC 17.12, build after this). The keys are the daemons' existing X25519
// seal keys (potluck kit/tunnel), certified by their ed25519 identity keys;
// the directory never takes a key from a request body: a member machine's
// key is the one its own transport certified on the publishing request
// (core.Remote.SealKey), and the authority's own is its Sealer's. A machine
// is the one its node-bound token names, so no caller can publish a key
// under another machine's name.
//
// Interface: the routes (REST) and Pins, the sender's trust-on-first-use
// record. Driver: the registry SQLite table secret_keys (a Postgres
// authority is the second, with the store topology).
package secretkeys

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

const schema = `
CREATE TABLE secret_keys (
	project     TEXT NOT NULL,
	area        TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL,
	key_id      TEXT NOT NULL,
	seal_key    BLOB NOT NULL,
	machine     TEXT NOT NULL,
	owner       TEXT NOT NULL,
	node        TEXT NOT NULL DEFAULT '',
	state       TEXT NOT NULL,
	published   INTEGER NOT NULL,
	ended       INTEGER,
	ended_by    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (project, machine, fingerprint)
);
CREATE INDEX secret_keys_machine ON secret_keys (project, machine, state);`

// Migrations are this module's tables; New adds them.
var Migrations = []store.Migration{{Kind: store.KindRegistry, Name: "secretkeys/0001_init", Script: schema}}

const (
	// maxActive bounds one project's machines with a key.
	maxActive = 5000
	// keepReplaced is how many replaced keys a machine's history keeps.
	keepReplaced = 20
	// maxRevoked bounds a machine's revoked keys (kept for good).
	maxRevoked = 100
)

var fpEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Fingerprint names a published key: 26 base32 characters (130 bits) of
// sha256 over the key id and the seal key, so a sender's pin covers both.
func Fingerprint(keyID string, sealKey []byte) string {
	h := sha256.Sum256([]byte("zm-secret-key/1\n" + keyID + "\n" + base64.RawURLEncoding.EncodeToString(sealKey)))
	return fpEnc.EncodeToString(h[:17])[:26]
}

// validFingerprint reports whether s is spelled as Fingerprint spells one.
func validFingerprint(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// Service is the key directory module.
type Service struct {
	d   *core.Deps
	now func() time.Time
}

var _ core.Module = (*Service)(nil)

// New returns the module. Construct it after teamlink is provided: on a
// member's machine its routes go to the project's authority, where the
// directory lives, and the member's transport certifies its key there.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	if link := teamlink.From(d); link != nil {
		link.RegisterRoutes("secretkeys",
			teamlink.RouteRule{Pattern: "GET /v0/secret-keys", Kind: teamlink.KindWrite, Hedge: teamlink.Hedged},
			teamlink.RouteRule{Pattern: "PUT /v0/secret-keys", Kind: teamlink.KindWrite, Hedge: teamlink.Write},
			teamlink.RouteRule{Pattern: "POST /v0/secret-keys/{fingerprint}/revoke", Kind: teamlink.KindWrite, Hedge: teamlink.Write})
	}
	return &Service{d: d, now: time.Now}
}

// Routes registers /v0/secret-keys.
func (s *Service) Routes(r *core.Router) {
	r = r.For("secretkeys")
	r.HandleFunc("GET /v0/secret-keys", core.Authed, "read on the key's project/area; ?project= like ReadScope", s.h(s.handleList))
	r.HandleFunc("PUT /v0/secret-keys", core.Authed, "read on the resolved project/area; only the calling machine's own key: the authority's owner on its socket, or a member's node-bound token whose transport certified the key", s.h(s.handlePublish))
	r.HandleFunc("POST /v0/secret-keys/{fingerprint}/revoke", core.Authed, "the key's own machine, or admin on the whole project; a key the caller cannot read answers not_found", s.h(s.handleRevoke))
}

func (s *Service) h(fn func(r *http.Request, sub *authz.Subject) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r, core.SubjectOf(r.Context()))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

func (s *Service) db() *sql.DB { return s.d.Store.Registry() }

type row struct {
	Project, Area, Fingerprint, KeyID string
	SealKey                           []byte
	Machine, Owner, Node, State       string
	Published                         int64
	Ended                             sql.NullInt64
	EndedBy                           string
}

const cols = `project, area, fingerprint, key_id, seal_key, machine, owner, node, state, published, ended, ended_by`

func scan(sc interface{ Scan(...any) error }) (row, error) {
	var k row
	err := sc.Scan(&k.Project, &k.Area, &k.Fingerprint, &k.KeyID, &k.SealKey, &k.Machine, &k.Owner, &k.Node, &k.State,
		&k.Published, &k.Ended, &k.EndedBy)
	return k, err
}

func (k row) object() authz.Object { return authz.Object{Project: k.Project, Area: k.Area} }

func (s *Service) view(ctx context.Context, sub *authz.Subject, k row) orchapi.SecretKey {
	v := orchapi.SecretKey{Project: s.d.Projects.Slug(k.Project), Machine: k.Machine, Owner: s.d.RedactFor(ctx, sub, k.Owner),
		KeyID: k.KeyID, SealKey: base64.RawURLEncoding.EncodeToString(k.SealKey), Fingerprint: k.Fingerprint, State: k.State,
		Published: store.FromMillis(k.Published), Ended: store.NullMillis(k.Ended)}
	if k.EndedBy != "" {
		v.EndedBy = s.d.RedactFor(ctx, sub, k.EndedBy)
	}
	return v
}

// machineOwner is the OS user on the socket without a token.
func machineOwner(sub *authz.Subject) bool {
	return sub != nil && sub.Via == authz.ViaLocal && !sub.Remote && sub.TokenID == ""
}

// member is a member machine's daemon (or a session on it) calling with its
// node-bound token.
func member(sub *authz.Subject) bool {
	return sub != nil && sub.Remote && sub.Node != "" && sub.Machine != ""
}

func address(id api.Identity) string { return id.Kind + ":" + id.Name }

// caller is the machine publishing and the key its transport certified.
type caller struct {
	machine, node, keyID string
	sealKey              []byte
}

func (s *Service) callerKey(ctx context.Context, sub *authz.Subject) (caller, error) {
	switch {
	case machineOwner(sub):
		sl, ok := s.d.Transport(core.ViaTunnel).(core.Sealer)
		if !ok {
			return caller{}, api.Errorf(api.CodeUnavailable, "this daemon has no seal key module")
		}
		idB64, cert, err := sl.SealIdentity()
		if err != nil {
			return caller{}, api.Errorf(api.CodeUnavailable, "this machine cannot read its seal key: %v", err)
		}
		if idB64 == "" {
			return caller{}, api.Errorf(api.CodeUnavailable, "sealing is off on this machine, so it has no key to publish")
		}
		id, pub, ok := sealbox.Peer(idB64, cert)
		if !ok {
			return caller{}, api.Errorf(api.CodeUnavailable, "this machine's own seal certificate does not verify")
		}
		return caller{machine: s.d.Config.Machine, keyID: id, sealKey: pub}, nil
	case member(sub):
		rm := core.RemoteOf(ctx)
		if rm == nil || rm.SealID == "" || len(rm.SealKey) != 32 {
			return caller{}, api.Errorf(api.CodeConflict, "machine %s named no seal key on this call", sub.Machine).
				WithHint("update zm on that machine; a build from before sealing has no key to publish")
		}
		if rm.Via == core.ViaTunnel && rm.SealID != sub.Node {
			// The door certified it as the signing key's; anything else is a bug upstream.
			return caller{}, api.Errorf(api.CodeForbidden, "the seal key on this call is not the calling machine's")
		}
		return caller{machine: sub.Machine, node: sub.Node, keyID: rm.SealID, sealKey: append([]byte(nil), rm.SealKey...)}, nil
	}
	return caller{}, api.Errorf(api.CodeForbidden, "only a machine publishes its own seal key").
		WithHint("run zm secret keys publish on that machine: its daemon publishes the key its transport certifies")
}

func (s *Service) handlePublish(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return nil, err
	}
	c, err := s.callerKey(ctx, sub)
	if err != nil {
		return nil, err
	}
	now := store.Millis(s.now())
	k := row{Project: t.Project.ID, Area: t.Area, Fingerprint: Fingerprint(c.keyID, c.sealKey), KeyID: c.keyID, SealKey: c.sealKey,
		Machine: c.machine, Owner: address(sub.Identity), Node: c.node, State: orchapi.SecretKeyActive, Published: now}
	err = store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		// Rows are per machine: a member that names another machine's key
		// gets a row of its own, never in that machine's way.
		old, err := scan(tx.QueryRowContext(ctx, `SELECT `+cols+` FROM secret_keys WHERE project = ? AND machine = ? AND fingerprint = ?`,
			k.Project, k.Machine, k.Fingerprint))
		switch {
		case err == nil && old.State == orchapi.SecretKeyActive:
			k = old // published before: nothing changes
			return nil
		case err == nil:
			return api.Errorf(api.CodeConflict, "this machine's seal key was %s; an ended key is never taken back", old.State).
				WithHint("make a new seal key on that machine (remove $ZM_HOME/tunnel/seal and restart its daemon), then publish again")
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var n, mine int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM secret_keys WHERE project = ? AND state = ?`,
			k.Project, orchapi.SecretKeyActive).Scan(&n); err != nil {
			return err
		}
		if n >= maxActive {
			return api.Errorf(api.CodeConflict, "project %s already lists %d machines' keys", t.Project.Slug, n)
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM secret_keys WHERE project = ? AND machine = ? AND state = ?`,
			k.Project, k.Machine, orchapi.SecretKeyRevoked).Scan(&mine); err != nil {
			return err
		}
		if mine >= maxRevoked {
			return api.Errorf(api.CodeConflict, "machine %s already has %d revoked keys in %s", k.Machine, mine, t.Project.Slug).
				WithHint("an admin of the project looks into why before this machine publishes again")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE secret_keys SET state = ?, ended = ?, ended_by = ?
			WHERE project = ? AND machine = ? AND state = ?`,
			orchapi.SecretKeyReplaced, now, k.Owner, k.Project, k.Machine, orchapi.SecretKeyActive); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_keys (`+cols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '')`,
			k.Project, k.Area, k.Fingerprint, k.KeyID, k.SealKey, k.Machine, k.Owner, k.Node, k.State, k.Published); err != nil {
			return err
		}
		// A machine's history keeps its newest replaced keys only, so no
		// caller grows the table by publishing key after key. Revoked keys
		// stay (bounded by maxRevoked), so a revoked key never comes back.
		_, err = tx.ExecContext(ctx, `DELETE FROM secret_keys WHERE project = ? AND machine = ? AND state = ? AND fingerprint NOT IN
			(SELECT fingerprint FROM secret_keys WHERE project = ? AND machine = ? AND state = ? ORDER BY ended DESC LIMIT ?)`,
			k.Project, k.Machine, orchapi.SecretKeyReplaced, k.Project, k.Machine, orchapi.SecretKeyReplaced, keepReplaced)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.view(ctx, sub, k), nil
}

func (s *Service) handleList(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	sc, err := s.d.ReadScope(ctx, q.Get("project"))
	if err != nil {
		return nil, err
	}
	where, args := sc.SQL("project", "area", authz.Read)
	if q.Get("all") == "" {
		where += " AND state = ?"
		args = append(args, orchapi.SecretKeyActive)
	}
	owner := ""
	if to := strings.TrimSpace(q.Get("to")); to != "" {
		kind, name, _ := strings.Cut(to, ":")
		switch {
		case name == "":
			return nil, api.Errorf(api.CodeBadRequest, "to must be machine:<name> or human:<name>")
		case kind == "machine":
			where += " AND machine = ?"
			args = append(args, name)
		case kind == api.IdentityHuman:
			owner = api.IdentityHuman + ":" + name // matched after redaction: a hidden owner matches nothing
		default:
			return nil, api.Errorf(api.CodeBadRequest, "to must be machine:<name> or human:<name>")
		}
	}
	rows, err := s.db().QueryContext(ctx, `SELECT `+cols+` FROM secret_keys WHERE `+where+` ORDER BY machine, published DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := orchapi.SecretKeyList{Keys: []orchapi.SecretKey{}}
	for rows.Next() {
		k, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if !sub.Can(authz.Read, k.object()) {
			continue
		}
		v := s.view(ctx, sub, k)
		if owner != "" && v.Owner != owner {
			continue
		}
		out.Keys = append(out.Keys, v)
	}
	return out, rows.Err()
}

// self reports whether sub is the machine that published k.
func (s *Service) self(sub *authz.Subject, k row) bool {
	switch {
	case machineOwner(sub):
		return k.Node == "" && k.Machine == s.d.Config.Machine
	case member(sub):
		return k.Node == sub.Node && k.Machine == sub.Machine
	}
	return false
}

func (s *Service) handleRevoke(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	fp := r.PathValue("fingerprint")
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return nil, err
	}
	if !validFingerprint(fp) {
		return nil, api.NotFound("secret key", fp)
	}
	var k row
	var revoked int
	err = store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+cols+` FROM secret_keys WHERE project = ? AND fingerprint = ? ORDER BY machine`, t.Project.ID, fp)
		if err != nil {
			return err
		}
		var seen []row
		for rows.Next() {
			x, err := scan(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if sub.Can(authz.Read, x.object()) {
				seen = append(seen, x)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(seen) == 0 {
			return api.NotFound("secret key", fp)
		}
		// The machine revokes its own row; an admin of the whole project
		// revokes the key wherever it is listed (one key, one holder).
		var targets []row
		for _, x := range seen {
			if s.self(sub, x) {
				targets = []row{x}
				break
			}
		}
		if targets == nil {
			if !sub.Can(authz.Admin, authz.Object{Project: t.Project.ID}) {
				return api.Errorf(api.CodeForbidden, "only the key's own machine or an admin of the whole project revokes it")
			}
			targets = seen
		}
		k = targets[0]
		now := store.Millis(s.now())
		for i, x := range targets {
			if x.State != orchapi.SecretKeyActive {
				continue // ended already: as it was
			}
			x.State, x.Ended, x.EndedBy = orchapi.SecretKeyRevoked, sql.NullInt64{Int64: now, Valid: true}, sub.Address
			if _, err := tx.ExecContext(ctx, `UPDATE secret_keys SET state = ?, ended = ?, ended_by = ? WHERE project = ? AND machine = ? AND fingerprint = ?`,
				x.State, now, x.EndedBy, x.Project, x.Machine, x.Fingerprint); err != nil {
				return err
			}
			revoked++
			if i == 0 {
				k = x
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if revoked > 0 {
		s.d.Log.Warn("secretkeys: a seal key was revoked; senders refuse it from now on", "machine", k.Machine, "fingerprint", k.Fingerprint, "rows", revoked, "by", sub.Address)
	}
	return s.view(ctx, sub, k), nil
}
