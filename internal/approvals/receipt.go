package approvals

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// KeyFile holds the daemon's ed25519 receipt key (base64 seed, mode 0600)
// in $ZM_HOME. Losing it only means new receipts carry a new key id; old
// receipts still verify against the public key published for them.
const KeyFile = "receipt.key"

// TimeFormat is the receipt time: RFC 3339 UTC with milliseconds.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// loadOrCreateKey reads the receipt key at path, creating it on first use.
func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return loadOrCreateKey(path) // another caller won the race
		}
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(enc); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return priv, nil
	}
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a receipt key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// KeyID names a public key: "zm:" + the first 16 hex chars of its sha256.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "zm:" + hex.EncodeToString(h[:8])
}

// PublicKey describes pub as GET /v0/receipts/key does.
func PublicKey(pub ed25519.PublicKey) orchapi.ReceiptKey {
	b := base64.StdEncoding.EncodeToString(pub)
	return orchapi.ReceiptKey{Alg: "ed25519", KeyID: KeyID(pub), PublicKey: b, Line: "ed25519:" + b}
}

// ParsePublicKey accepts the forms `zm receipt key` prints: the line
// "ed25519:<base64>", bare base64, or the JSON of orchapi.ReceiptKey.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") {
		var k orchapi.ReceiptKey
		if err := json.Unmarshal([]byte(s), &k); err != nil {
			return nil, fmt.Errorf("bad receipt key JSON: %v", err)
		}
		if k.Alg != "" && k.Alg != "ed25519" {
			return nil, fmt.Errorf("receipt key algorithm %q is not ed25519", k.Alg)
		}
		s = k.PublicKey
	}
	s = strings.TrimPrefix(s, "ed25519:")
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not an ed25519 public key (want ed25519:<base64 of 32 bytes>)")
	}
	return ed25519.PublicKey(b), nil
}

// signedPart is exactly what the signature covers, in this field order.
type signedPart struct {
	V        string             `json:"v"`
	Project  string             `json:"project"`
	Ask      string             `json:"ask"`
	Subject  orchapi.AskSubject `json:"subject"`
	Decision string             `json:"decision"`
	Approver string             `json:"approver"`
	Node     string             `json:"node"`
	Time     string             `json:"time"`
	// Version 2 only; absent, version 1's bytes are unchanged.
	CardSHA    string `json:"card_sha256,omitempty"`
	TextSHA    string `json:"text_sha256,omitempty"`
	AnsweredAs string `json:"answered_as,omitempty"`
	Action     string `json:"action,omitempty"`
}

// SigningBytes is the message a receipt's signature covers: the version
// line, a newline, then the compact JSON of {v, project, ask, subject {repo,
// pr, head_sha}, decision, approver, node, time} in that order. Version 2's
// line and subject differ, and card_sha256, text_sha256 and answered_as follow.
func SigningBytes(r orchapi.Receipt) []byte {
	b, _ := json.Marshal(signedPart{V: r.V, Project: r.Project, Ask: r.Ask, Subject: r.Subject,
		Decision: r.Decision, Approver: r.Approver, Node: r.Node, Time: r.Time,
		CardSHA: r.CardSHA, TextSHA: r.TextSHA, AnsweredAs: r.AnsweredAs, Action: r.Action})
	domain := orchapi.ReceiptVersion
	if r.V == orchapi.ReceiptVersion2 {
		domain = orchapi.ReceiptVersion2
	}
	return append([]byte(domain+"\n"), b...)
}

// Sign fills KeyID, Sig and, when empty, V (version 1).
func Sign(priv ed25519.PrivateKey, r orchapi.Receipt) orchapi.Receipt {
	if r.V == "" {
		r.V = orchapi.ReceiptVersion
	}
	r.KeyID = KeyID(priv.Public().(ed25519.PublicKey))
	r.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SigningBytes(r)))
	return r
}

var (
	shaRe     = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ghOwnerRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38})$`)
	repoSegRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
	hostRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)
)

// CanonRepo returns the canonical repo name host/owner/name (lower case;
// host github.com when only owner/name is given; a URL's scheme and a
// trailing .git dropped). Anything else is refused.
func CanonRepo(s string) (string, error) {
	in := s
	s = strings.ToLower(strings.TrimSpace(s))
	if rest, ok := strings.CutPrefix(s, "git@"); ok {
		s = strings.Replace(rest, ":", "/", 1) // git@github.com:owner/name
	}
	for _, p := range []string{"https://", "http://", "ssh://git@"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) == 2 {
		if !ghOwnerRe.MatchString(parts[0]) { // "github.com/x" is not owner/name
			return "", api.Errorf(api.CodeBadRequest, "repo %q is not owner/name or host/owner/name", in)
		}
		parts = append([]string{"github.com"}, parts...)
	}
	if len(parts) != 3 || !hostRe.MatchString(parts[0]) || !repoSegRe.MatchString(parts[1]) || !repoSegRe.MatchString(parts[2]) {
		return "", api.Errorf(api.CodeBadRequest, "repo %q is not owner/name or host/owner/name", in)
	}
	return strings.Join(parts, "/"), nil
}

// CanonSubject validates s and returns its canonical form.
func CanonSubject(s orchapi.AskSubject) (orchapi.AskSubject, error) {
	repo, err := CanonRepo(s.Repo)
	if err != nil {
		return s, err
	}
	sha := strings.ToLower(strings.TrimSpace(s.HeadSHA))
	if !shaRe.MatchString(sha) {
		return s, api.Errorf(api.CodeBadRequest, "head_sha must be the full commit id (40 or 64 hex characters), not %q", s.HeadSHA).
			WithHint("a prefix never matches: pass git rev-parse HEAD")
	}
	if s.PR <= 0 {
		return s, api.Errorf(api.CodeBadRequest, "pr must be a pull request number")
	}
	return orchapi.AskSubject{Repo: repo, PR: s.PR, HeadSHA: sha}, nil
}

// wellFormedV2 checks the parts only a version 2 receipt has: exactly one of
// a work item and a question as its subject, never a pull request's fields.
func wellFormedV2(r orchapi.Receipt) bool {
	s := r.Subject
	if s.Repo != "" || s.PR != 0 || s.HeadSHA != "" || (s.Work == "") == (s.Question == "") {
		return false
	}
	if s.Question != "" && !sha256Re.MatchString(s.Question) {
		return false
	}
	if !sha256Re.MatchString(r.CardSHA) || (r.TextSHA != "" && !sha256Re.MatchString(r.TextSHA)) {
		return false
	}
	return r.AnsweredAs == orchapi.AnsweredAsOwner || r.AnsweredAs == orchapi.AnsweredAsContact
}

// Verify checks r's signature with pub and every non-empty expectation.
// It fails closed: anything malformed, signed by another key, or for
// another project, ask, repo, PR, head SHA, work item, question, decision or
// approver is invalid, and so is an owner's unsigned answer when exp.Signed.
func Verify(r orchapi.Receipt, pub ed25519.PublicKey, exp orchapi.ReceiptExpect) orchapi.ReceiptCheck {
	bad := func(format string, a ...any) orchapi.ReceiptCheck {
		return orchapi.ReceiptCheck{Valid: false, Reason: fmt.Sprintf(format, a...), KeyID: r.KeyID}
	}
	if len(pub) != ed25519.PublicKeySize {
		return bad("no verification key")
	}
	if r.V != orchapi.ReceiptVersion && r.V != orchapi.ReceiptVersion2 {
		return bad("not a %s receipt", orchapi.ReceiptVersion)
	}
	if r.KeyID != KeyID(pub) {
		return bad("signed by key %s, not %s", r.KeyID, KeyID(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return bad("malformed signature")
	}
	if !ed25519.Verify(pub, SigningBytes(r), sig) {
		return bad("signature does not verify")
	}
	// Signed is not enough: the fields must also be well formed.
	if r.V == orchapi.ReceiptVersion2 {
		if !wellFormedV2(r) {
			return bad("malformed subject")
		}
	} else if cs, err := CanonSubject(r.Subject); err != nil || cs != r.Subject || r.CardSHA != "" || r.TextSHA != "" || r.AnsweredAs != "" || r.Action != "" {
		return bad("malformed subject")
	}
	if _, err := time.Parse(time.RFC3339, r.Time); err != nil || r.Project == "" || r.Ask == "" || r.Node == "" ||
		!api.ValidName(r.Decision) || !api.ValidName(r.Approver) {
		return bad("malformed receipt")
	}
	if exp.Work != "" && exp.Ask == "" && exp.Card == "" {
		return bad("a work decision requires an ask or card expectation")
	}
	if exp.Question != "" && r.Subject.Question == r.CardSHA && exp.Ask == "" {
		return bad("a legacy question requires an ask expectation")
	}
	var checked []string
	eq := func(what, want, got string) bool {
		checked = append(checked, what)
		return want == got
	}
	if exp.Card != "" && !eq("card", exp.Card, r.CardSHA) {
		return bad("receipt is for another card")
	}
	if exp.Project != "" && !eq("project", exp.Project, r.Project) {
		return bad("receipt is for another project")
	}
	if exp.Ask != "" && !eq("ask", strings.ToUpper(strings.TrimSpace(exp.Ask)), r.Ask) {
		return bad("receipt is for another ask")
	}
	if exp.Repo != "" {
		repo, err := CanonRepo(exp.Repo)
		if err != nil || !eq("repo", repo, r.Subject.Repo) {
			return bad("receipt is for another repo")
		}
	}
	if exp.PR != 0 && !eq("pr", fmt.Sprint(exp.PR), fmt.Sprint(r.Subject.PR)) {
		return bad("receipt is for another pull request")
	}
	if exp.HeadSHA != "" {
		sha := strings.ToLower(strings.TrimSpace(exp.HeadSHA))
		if !shaRe.MatchString(sha) {
			return bad("expected head SHA must be a full commit id (a prefix never matches)")
		}
		if !eq("head_sha", sha, r.Subject.HeadSHA) {
			return bad("receipt is for another head commit")
		}
	}
	if exp.Work != "" && !eq("work", strings.TrimSpace(exp.Work), r.Subject.Work) {
		return bad("receipt is for another work item")
	}
	if exp.Question != "" && !eq("question", strings.ToLower(strings.TrimSpace(exp.Question)), r.Subject.Question) {
		return bad("receipt is for another question")
	}
	if exp.Decision != "" && !eq("decision", exp.Decision, r.Decision) {
		return bad("decision is %s, not %s", r.Decision, exp.Decision)
	}
	if exp.Approver != "" && !eq("approver", exp.Approver, r.Approver) {
		return bad("receipt is from another approver")
	}
	if exp.Signed {
		checked = append(checked, "signed")
		if strings.HasPrefix(r.Node, orchapi.OwnerNodePrefix) {
			return bad("receipt is the owner's unsigned answer (node %s), not a signed one", r.Node)
		}
	}
	return orchapi.ReceiptCheck{Valid: true, KeyID: r.KeyID, Checked: checked}
}

// ParseReceipt decodes a receipt (JSON), refusing unknown fields.
func ParseReceipt(b []byte) (orchapi.Receipt, error) {
	var r orchapi.Receipt
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, api.Errorf(api.CodeBadRequest, "not a receipt: %v", err)
	}
	return r, nil
}
