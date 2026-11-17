package webhooks_test

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/webhooks"
	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
)

func stdHeaders(secret, id string, ts int64, body []byte) http.Header {
	h := http.Header{}
	h.Set(orchapi.HeaderStdWebhookID, id)
	h.Set(orchapi.HeaderStdWebhookTimestamp, strconv.FormatInt(ts, 10))
	h.Set(orchapi.HeaderStdWebhookSignature, webhooks.SignStd(secret, id, ts, body))
	return h
}

// The spec's own test vector (standard-webhooks spec, "Signature scheme").
func TestSignStdSpecVector(t *testing.T) {
	got := webhooks.SignStd("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestVerifyStd(t *testing.T) {
	now := time.Now()
	body := []byte(`{"type":"work.created"}`)
	// A daemon secret from before 2026-11-01 was "whsec_" + URL-safe base64:
	// no library decodes it, so it keys on its raw bytes.
	b := make([]byte, 32)
	rand.Read(b)
	legacy := "whsec_" + base64.RawURLEncoding.EncodeToString(b)
	for !strings.ContainsAny(legacy, "-_") {
		rand.Read(b)
		legacy = "whsec_" + base64.RawURLEncoding.EncodeToString(b)
	}
	if string(webhooks.StdKey(legacy)) != legacy {
		t.Fatal("a legacy URL-safe secret should key on its raw bytes")
	}
	if _, err := standardwebhooks.NewWebhook(webhooks.NewSecret()); err != nil {
		t.Fatalf("a new daemon secret is not library-ready: %v", err)
	}
	for _, secret := range []string{webhooks.NewSecret(), "a-callers-own-secret-0123", legacy} {
		h := stdHeaders(secret, "d1", now.Unix(), body)
		if err := webhooks.VerifyStd(secret, h, body, now, 0); err != nil {
			t.Fatalf("%s: %v", secret, err)
		}
		// The reference library agrees: NewWebhook for a whsec_ secret, raw
		// bytes for a caller's own.
		var wh *standardwebhooks.Webhook
		if secret != legacy && secret[:6] == "whsec_" {
			wh, _ = standardwebhooks.NewWebhook(secret)
		} else {
			wh, _ = standardwebhooks.NewWebhookRaw([]byte(secret))
		}
		if err := wh.Verify(body, h); err != nil {
			t.Fatalf("library on %s: %v", secret, err)
		}
		if webhooks.VerifyStd("wrong-secret-0123456789", h, body, now, 0) == nil {
			t.Fatal("a wrong secret verified")
		}
		if err := webhooks.VerifyStd(secret, h, body, now.Add(time.Hour), 0); err != webhooks.ErrStale {
			t.Fatalf("an hour-old delivery: %v", err)
		}
		bad := append([]byte(nil), body...)
		bad[2] ^= 1
		if webhooks.VerifyStd(secret, h, bad, now, 0) == nil {
			t.Fatal("a tampered body verified")
		}
		h2 := h.Clone()
		h2.Set(orchapi.HeaderStdWebhookID, "d2")
		if webhooks.VerifyStd(secret, h2, body, now, 0) == nil {
			t.Fatal("a changed id verified")
		}
		if err := webhooks.VerifyStd(secret, h, body, now.Add(-time.Hour), 0); err != webhooks.ErrStale {
			t.Fatalf("a delivery an hour in the future: %v", err)
		}
		for _, k := range []string{orchapi.HeaderStdWebhookID, orchapi.HeaderStdWebhookTimestamp, orchapi.HeaderStdWebhookSignature} {
			h3 := h.Clone()
			h3.Del(k)
			if webhooks.VerifyStd(secret, h3, body, now, 0) == nil {
				t.Fatalf("verified without %s", k)
			}
		}
		h4 := h.Clone()
		h4.Set(orchapi.HeaderStdWebhookSignature, "v2"+strings.TrimPrefix(h.Get(orchapi.HeaderStdWebhookSignature), "v1"))
		if webhooks.VerifyStd(secret, h4, body, now, 0) == nil {
			t.Fatal("a non-v1 signature verified")
		}
	}
}
