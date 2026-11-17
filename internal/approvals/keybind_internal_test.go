package approvals

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

// A nonce is used once while it can be fresh, each scope holds at most
// keyNonceCap, and old generations drop without a scan per call.
func TestNonceBook(t *testing.T) {
	var b nonceBook
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if !b.take(t0, "s", "fp", "n1") || b.take(t0, "s", "fp", "n1") {
		t.Fatal("a nonce taken twice")
	}
	if !b.take(t0, "s", "other-fp", "n1") {
		t.Fatal("the same nonce for another key")
	}
	// One generation later the nonce is still remembered.
	if b.take(t0.Add(keyNonceSpan+time.Second), "s", "fp", "n1") {
		t.Fatal("forgotten after one generation")
	}
	// Two generations later it can no longer be fresh, and is dropped.
	if !b.take(t0.Add(2*keyNonceSpan+2*time.Second), "s", "fp", "n1") {
		t.Fatal("kept after two generations")
	}
	var c nonceBook
	for i := range keyNonceCap {
		if !c.take(t0, "full", "fp", string(rune('a'+i%26))+time.Duration(i).String()) {
			t.Fatalf("refused at %d", i)
		}
	}
	if c.take(t0, "full", "fp", "one-more") {
		t.Fatal("a full scope took another")
	}
	if !c.take(t0, "another", "fp", "one-more") {
		t.Fatal("a full scope blocked another scope")
	}
	if !c.take(t0.Add(2*keyNonceSpan), "full", "fp", "later") {
		t.Fatal("a scope stayed full after its nonces aged out")
	}
}

// A key has one spelling: base64 that does not re-encode the same is
// refused.
func TestParseKeyCanonical(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	raw, _ := k.PublicKey.Bytes()
	good := base64.StdEncoding.EncodeToString(raw)
	if _, _, err := parseKey(good); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseKey(good[:10] + "\n" + good[10:]); err == nil {
		t.Fatal("a key with a newline parsed")
	}
}
