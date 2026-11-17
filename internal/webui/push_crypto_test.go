package webui

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestPushEncryptionRoundTrip(t *testing.T) {
	receiver, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err = rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"title":"test"}`)
	encoded, err := encryptPush(receiver.PublicKey().Bytes(), auth, payload)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint32(encoded[16:20]) != 4096 || encoded[20] != 65 {
		t.Fatal("bad record header")
	}
	sender, err := ecdh.P256().NewPublicKey(encoded[21:86])
	if err != nil {
		t.Fatal(err)
	}
	shared, err := receiver.ECDH(sender)
	if err != nil {
		t.Fatal(err)
	}
	info := append([]byte("WebPush: info\x00"), receiver.PublicKey().Bytes()...)
	info = append(info, sender.Bytes()...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	cek, err := hkdf.Key(sha256.New, ikm, encoded[:16], "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := hkdf.Key(sha256.New, ikm, encoded[:16], "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	clear, err := gcm.Open(nil, nonce, encoded[86:], nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(clear) != string(append(payload, 2)) {
		t.Fatalf("unexpected decrypted content %q", clear)
	}
}
func TestVAPIDSignatureAndAudience(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token, err := vapidToken(key, "https://fcm.googleapis.com/a", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal(token)
	}
	body, _ := raw64.DecodeString(parts[1])
	var claims map[string]any
	if json.Unmarshal(body, &claims) != nil || claims["aud"] != "https://fcm.googleapis.com" {
		t.Fatalf("claims %s", body)
	}
	if claims["sub"] != defaultVAPIDSubject {
		t.Fatalf("VAPID contact subject: %v", claims["sub"])
	}
	sig, _ := raw64.DecodeString(parts[2])
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&key.PublicKey, hash[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("invalid signature")
	}
}

func TestVAPIDSubjectOverride(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := vapidToken(key, "https://fcm.googleapis.com/a", time.Now(), "https://example.org/contact")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	body, err := raw64.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err = json.Unmarshal(body, &claims); err != nil || claims["sub"] != "https://example.org/contact" {
		t.Fatalf("subject: %s %v", body, err)
	}
	if _, err = vapidToken(key, "https://fcm.googleapis.com/a", time.Now(), "http://example.org"); err == nil {
		t.Fatal("non-HTTPS subject accepted")
	}
}

func TestVAPIDTokenReusedPerOrigin(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{}
	now := time.Now()
	first, err := s.signedVAPID(key, "https://web.push.apple.com/a", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.signedVAPID(key, "https://web.push.apple.com/b", now.Add(59*time.Minute))
	if err != nil || first != second {
		t.Fatal("token was not reused for the origin", err)
	}
	third, err := s.signedVAPID(key, "https://web.push.apple.com/b", now.Add(time.Hour))
	if err != nil || third == first {
		t.Fatal("token did not rotate", err)
	}
}
func TestPushEndpointRestrictions(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{{"https://web.push.apple.com/a", true}, {"https://fcm.googleapis.com/a", true}, {"https://updates.push.services.mozilla.com/a", true}, {"https://web.push.apple.com.evil.example/a", false}, {"https://user@web.push.apple.com/a", false}, {"http://web.push.apple.com/a", false}, {"https://127.0.0.1/a", false}, {"https://web.push.apple.com:443/a", false}} {
		if got := pushEndpoint(tc.url); got != tc.want {
			t.Errorf("%s = %v", tc.url, got)
		}
	}
}
