package webhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
)

// Signing follows Standard Webhooks (https://www.standardwebhooks.com) with
// its reference Go library: webhook-id, webhook-timestamp and
// webhook-signature "v1,<base64 HMAC-SHA256(key, id.timestamp.body)>".
// Zm-Webhook-Signature (below) and GitHub's X-Hub-Signature-256 are sent too;
// the Zm one is deprecated (SPEC 18.1 gives the removal date).

// StdKey is the HMAC key Standard Webhooks signs with, chosen by the
// secret's form, not by who made it: "whsec_" + valid standard base64 (every
// secret the daemon makes since 2026-11-01) keys on the decoded bytes, and
// receivers pass the secret as given to a library's NewWebhook; any other
// secret (a caller's own, or a daemon-made one from before 2026-11-01, which
// was URL-safe base64) keys on its bytes as given (NewWebhookRaw).
func StdKey(secret string) []byte {
	if rest, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if k, err := base64.StdEncoding.DecodeString(rest); err == nil && len(k) > 0 {
			return k
		}
	}
	return []byte(secret)
}

// SignStd is the webhook-signature value for message id at ts.
func SignStd(secret, id string, ts int64, body []byte) string {
	wh, _ := standardwebhooks.NewWebhookRaw(StdKey(secret))
	sig, _ := wh.Sign(id, time.Unix(ts, 0), body)
	return sig
}

// VerifyStd checks the Standard Webhooks headers as a receiver should: a v1
// signature over webhook-id, webhook-timestamp and the raw body (the
// library compares in constant time), and the timestamp within tolerance of
// now (0 = DefaultTolerance). Dedupe on webhook-id: retries repeat it.
func VerifyStd(secret string, h http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return ErrBadSignature
	}
	wh, _ := standardwebhooks.NewWebhookRaw(StdKey(secret))
	if err := wh.VerifyIgnoringTimestamp(body, h); err != nil {
		return ErrBadSignature
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	n, _ := strconv.ParseInt(h.Get(standardwebhooks.HeaderWebhookTimestamp), 10, 64)
	if d := now.Sub(time.Unix(n, 0)); d > tolerance || d < -tolerance {
		return ErrStale
	}
	return nil
}

// Sign is the deprecated Zm-Webhook-Signature value for one attempt: "sha256=" + hex
// HMAC-SHA256(secret, "<unix seconds>." + body). The timestamp is inside the
// MAC, so a captured delivery cannot be replayed later with a fresh one.
func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte{'.'})
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// SignHub is GitHub's X-Hub-Signature-256 value: "sha256=" + hex
// HMAC-SHA256(secret, body). It carries no timestamp: receivers using it
// should dedupe on Zm-Webhook-Delivery.
func SignHub(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verification errors.
var (
	ErrBadSignature = errors.New("webhook signature does not match")
	ErrStale        = errors.New("webhook timestamp outside the tolerance")
)

// DefaultTolerance is how far a delivery's timestamp may be from now.
const DefaultTolerance = 5 * time.Minute

// Verify checks the deprecated Zm-Webhook-Signature header (new receivers
// use VerifyStd) over the timestamp header and the raw body, in
// constant time, and the timestamp within tolerance of now (0 =
// DefaultTolerance). Dedupe on Zm-Webhook-Delivery as well: retries carry the
// same delivery id with a new timestamp.
func Verify(secret []byte, sig, ts string, body []byte, now time.Time, tolerance time.Duration) error {
	if len(secret) == 0 {
		return ErrBadSignature
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	n, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if !hmac.Equal([]byte(strings.TrimSpace(sig)), []byte(Sign(secret, n, body))) {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(n, 0)); d > tolerance || d < -tolerance {
		return ErrStale
	}
	return nil
}

// VerifyHub checks a GitHub-style X-Hub-Signature-256 header in constant
// time (the GitHub receiver uses it on GitHub's deliveries).
func VerifyHub(secret []byte, sig string, body []byte) bool {
	if len(secret) == 0 {
		return false
	}
	return hmac.Equal([]byte(strings.TrimSpace(sig)), []byte(SignHub(secret, body)))
}

// NewSecret returns a random signing secret in the Standard Webhooks form:
// "whsec_" + standard base64 of 32 random bytes.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "whsec_" + base64.StdEncoding.EncodeToString(b)
}

// MinSecret is the shortest secret a caller may supply.
const MinSecret = 16
