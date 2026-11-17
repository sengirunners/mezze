package approvals

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Key approvers (SPEC 16 "Key approvers"). A cockpit (the Mezze app) runs on the machine its agents
// run on and speaks over the socket, where every caller is the owner. So the
// right cannot come from the caller: it comes from a signature by a key. The
// daemon cannot tell where a key lives, so pairing proves nothing by itself:
// a key gets the right when a GitHub sign-in this authority runs binds it to
// the person who started it (keybind.go), or, the old way, when an approver
// on another device (Device routes, which refuse the socket) confirms its
// fingerprint. A key approver cannot confirm anyone, and cannot decide a
// pull request's ask.

const (
	// keyAnswerSkew is how far a signed answer's time may be from the clock.
	keyAnswerSkew = 5 * time.Minute
	pairKeyDomain = "zm-pair-key/1\n"
	nodeKeyPrefix = "key:"
	keyFingerAlg  = "p256:"
	maxDeviceName = 60
)

// parseKey reads a P-256 public key in uncompressed X9.63 form.
func parseKey(b64 string) (*ecdsa.PublicKey, []byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, nil, err
	}
	if base64.StdEncoding.EncodeToString(raw) != b64 {
		// One spelling per key: what is signed and stored is canonical.
		return nil, nil, errors.New("key is not canonical base64")
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return nil, nil, err
	}
	return pub, raw, nil
}

// fingerprint names a key: p256: and the first 16 hex chars of its sha256.
func fingerprint(raw []byte) string {
	h := sha256.Sum256(raw)
	return keyFingerAlg + hex.EncodeToString(h[:8])
}

// verifySig checks an ASN.1 ECDSA SHA-256 signature over msg.
func verifySig(pub *ecdsa.PublicKey, msg []byte, sigB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	h := sha256.Sum256(msg)
	return ecdsa.VerifyASN1(pub, h[:], sig)
}

func (s *Service) handleKeyState(w http.ResponseWriter, r *http.Request) {
	a, err := s.KeyState(r.Context(), r.URL.Query().Get("key"))
	respond(w, a, err)
}

// KeyState is the approver paired to a key, by its fingerprint. GET /v0/approvers
// is local to the authority, so a key paired from a member machine (the Mezze app)
// learns here whether a person has confirmed it. A fingerprint is public: the
// answer is the name and state the confirming phone already shows.
func (s *Service) KeyState(ctx context.Context, key string) (orchapi.Approver, error) {
	hexPart, ok := strings.CutPrefix(key, keyFingerAlg)
	if _, err := hex.DecodeString(hexPart); !ok || err != nil || len(hexPart) != 16 {
		return orchapi.Approver{}, api.Errorf(api.CodeBadRequest, "key must be p256: and 16 hex characters")
	}
	a, err := scanApprover(s.d.Store.Registry().QueryRowContext(ctx,
		`SELECT `+approverCols+` FROM approvers WHERE node_id = ?`, nodeKeyPrefix+key))
	if errors.Is(err, sql.ErrNoRows) {
		return orchapi.Approver{}, api.NotFound("approver key", key)
	}
	return a.Approver, err
}

func (s *Service) handlePairKey(w http.ResponseWriter, r *http.Request) {
	var req orchapi.PairKeyRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.PairKey(r.Context(), core.SubjectOf(r.Context()).Address, req)
	respond(w, a, err)
}

// PairKey binds the approver whose code this is to a key. Never active: it
// waits for an approver on another device, even when it would be the first
// (bootstrapping stays a tailnet device's, which needs a browser sign-in).
func (s *Service) PairKey(ctx context.Context, caller string, req orchapi.PairKeyRequest) (orchapi.Approver, error) {
	tries := "key\x00" + caller
	if !s.mayTry(tries) {
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "too many wrong pairing codes; wait %s and ask for a new one", CodeTTL)
	}
	if over, err := s.legacyKeysOver(ctx, s.d.Store.Registry()); err != nil {
		return orchapi.Approver{}, err
	} else if over {
		return orchapi.Approver{}, errLegacyKeyOver()
	}
	pub, raw, err := parseKey(req.PublicKey)
	if err != nil {
		return orchapi.Approver{}, api.Errorf(api.CodeBadRequest, "public_key must be base64 of an uncompressed P-256 key")
	}
	if !verifySig(pub, []byte(pairKeyDomain+normCode(req.Code)+"\n"+req.PublicKey), req.Proof) {
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "the proof does not match this key and code")
	}
	device := strings.TrimSpace(textclean.Clean(req.Device))
	if unsafePresentation(device) {
		device = "a key"
	}
	if r := []rune(device); len(r) > maxDeviceName {
		device = string(r[:maxDeviceName])
	}
	node, now := nodeKeyPrefix+fingerprint(raw), s.now()
	var a approverRow
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM approvers WHERE code_hash = ? AND state = ? AND code_expires > ?`,
			hashCode(req.Code), orchapi.ApproverPending, ms(now)).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) || normCode(req.Code) == "" {
			s.failed(tries)
			return api.Errorf(api.CodeForbidden, "invalid or expired pairing code").
				WithHint("run zm approver add <name> on the project's authority for a new one")
		}
		if err != nil {
			return err
		}
		var bound string
		err = tx.QueryRowContext(ctx, `SELECT name FROM approvers WHERE node_id = ?`, node).Scan(&bound)
		switch {
		case err == nil:
			return api.Errorf(api.CodeConflict, "this key is already approver %s", bound)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvers SET state = ?, code_hash = NULL, code_expires = NULL,
			node_id = ?, node_key = ?, node_name = ?, paired = ? WHERE name = ?`,
			orchapi.ApproverUnconfirmed, node, req.PublicKey, first(device, "a key"), ms(now), name); err != nil {
			return err
		}
		a, err = scanApprover(tx.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE name = ?`, name))
		return err
	})
	if err != nil {
		return orchapi.Approver{}, err
	}
	s.d.Log.Warn("approvals: a key paired; it holds no right until an approver confirms it on another device",
		"approver", a.Name, "key", a.Key, "by", caller)
	var active int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM approvers WHERE `+canConfirm, orchapi.ApproverActive).Scan(&active); err != nil {
		return a.Approver, err
	}
	if active == 0 {
		a.Note = "nobody can confirm this key yet: pair a phone first (zm approver add, on the project's authority), then confirm it there"
	}
	return a.Approver, nil
}

// keyRefused is the one answer for every signature that does not hold up, so
// it says nothing about which approvers or asks exist.
func keyRefused() error {
	return api.Errorf(api.CodeForbidden, "this answer is not signed by a paired approver's key").
		WithHint("no socket caller or token holds the approver right; the project's owner answers with zm ask answer, or answer on a paired device")
}

// keyApprover checks a signed answer before anything else is read: an active
// key approver signed exactly these bytes, for this ask, this daemon, now.
func (s *Service) keyApprover(ctx context.Context, id string, in orchapi.SignedKeyAnswer) (approverRow, orchapi.KeyAnswer, error) {
	var ans orchapi.KeyAnswer
	name, err := checkName(in.Approver)
	if err != nil {
		return approverRow{}, ans, keyRefused()
	}
	me, err := scanApprover(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+approverCols+` FROM approvers WHERE name = ?`, name))
	if err != nil || me.State != orchapi.ApproverActive || !strings.HasPrefix(me.Node, nodeKeyPrefix) {
		return approverRow{}, ans, keyRefused()
	}
	pub, raw, err := parseKey(me.nodeKey)
	if err != nil || nodeKeyPrefix+fingerprint(raw) != me.Node {
		return approverRow{}, ans, keyRefused()
	}
	payload, err := base64.StdEncoding.DecodeString(in.Payload)
	if err != nil || !verifySig(pub, append([]byte(orchapi.KeyAnswerVersion+"\n"), payload...), in.Sig) {
		return approverRow{}, ans, keyRefused()
	}
	if err := json.Unmarshal(payload, &ans); err != nil || ans.V != orchapi.KeyAnswerVersion {
		return approverRow{}, ans, keyRefused()
	}
	daemon, err := s.PublicKey()
	if err != nil {
		return approverRow{}, ans, err
	}
	if ans.Daemon != KeyID(daemon) || ans.Ask != strings.ToUpper(strings.TrimSpace(id)) {
		return approverRow{}, ans, keyRefused()
	}
	at, err := time.Parse(time.RFC3339, ans.Time)
	if err != nil {
		return approverRow{}, ans, keyRefused()
	}
	if d := s.now().Sub(at); d > keyAnswerSkew || d < -keyAnswerSkew {
		return approverRow{}, ans, keyRefused()
	}
	return me, ans, nil
}

func (s *Service) handleKeyAnswer(w http.ResponseWriter, r *http.Request) {
	var in orchapi.SignedKeyAnswer
	if err := api.DecodeJSON(r, &in); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	me, ans, err := s.keyApprover(ctx, r.PathValue("id"), in)
	if err == nil {
		err = s.keyAnswerer(r, me)
	}
	if err == nil {
		// Rule a before anything else about the ask: a key whose person
		// does not answer its project learns nothing (missing, held, a pull
		// request's): the same 403.
		var a orchapi.Ask
		if a, err = s.ask(ctx, ans.Ask); err != nil {
			if api.IsCode(err, api.CodeNotFound) {
				err = keyRefused()
			}
		} else {
			err = s.keyAnswersProject(ctx, me, a.Project)
		}
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if s.isRelease(ctx, ans.Ask, ans.Decision) {
		a, err := s.ask(ctx, ans.Ask)
		if err == nil {
			err = s.keyAnswersProject(ctx, me, a.Project)
		}
		if err == nil && (ans.Project != a.Project || ans.CardSHA != a.CardSHA) {
			err = api.Errorf(api.CodeConflict, "this answer was signed for another card; read the ask again").WithData(orchapi.Refusal{Reason: orchapi.ReasonCardChanged})
		}
		if err == nil {
			a, err = s.releaseUnchecked(ctx, ans.Ask, me)
		}
		respond(w, a, err)
		return
	}
	d, err := s.signedAnswer(ctx, me, ans.Ask, orchapi.DecideRequest{Decision: ans.Decision, Text: ans.Text, As: ans.As, Confirm: ans.Confirm}, func(a orchapi.Ask) error {
		if a.Subject.IsPR() {
			return api.Errorf(api.CodeForbidden, "a pull request's ask is decided on a paired device over the tailnet, never by a key on a machine that runs agents")
		}
		if err := s.keyAnswersProject(ctx, me, a.Project); err != nil {
			return err
		}
		if ans.Project != a.Project || ans.CardSHA != a.CardSHA {
			return api.Errorf(api.CodeConflict, "this answer was signed for another card; read the ask again").WithData(orchapi.Refusal{Reason: orchapi.ReasonCardChanged})
		}
		return nil
	})
	respond(w, d, err)
}
