package webui

// Web Push's VAPID signature (RFC 8292) and aes128gcm content coding
// (RFC 8291). Only the small, single-record payload used by zm is needed.
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"time"
)

var raw64 = base64.RawURLEncoding

const defaultVAPIDSubject = "https://github.com/sengirunners/mezze"

func vapidToken(key *ecdsa.PrivateKey, endpoint string, now time.Time, configured ...string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	header := raw64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	subject := defaultVAPIDSubject
	if len(configured) > 0 && configured[0] != "" {
		subject = configured[0]
	}
	contact, err := url.Parse(subject)
	if err != nil || contact.Scheme != "https" || contact.Hostname() == "" || contact.User != nil || contact.Fragment != "" {
		return "", errors.New("web.vapid_subject must be a public HTTPS URL")
	}
	claims, err := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	if err != nil {
		return "", err
	}
	input := header + "." + raw64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + raw64.EncodeToString(sig), nil
}

func encryptPush(peerKey, auth, payload []byte) ([]byte, error) {
	if len(auth) != 16 || len(payload) > 3800 {
		return nil, errors.New("invalid push authentication secret or payload size")
	}
	curve := ecdh.P256()
	peer, err := curve.NewPublicKey(peerKey)
	if err != nil {
		return nil, err
	}
	sender, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := sender.ECDH(peer)
	if err != nil {
		return nil, err
	}
	pub := sender.PublicKey().Bytes()
	info := append([]byte("WebPush: info\x00"), peerKey...)
	info = append(info, pub...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain := append(append([]byte(nil), payload...), 2)
	out := append([]byte(nil), salt...)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], 4096)
	out = append(out, rs[:]...)
	out = append(out, byte(len(pub)))
	out = append(out, pub...)
	out = aead.Seal(out, nonce, plain, nil)
	return out, nil
}

func parseVAPID(raw string) (*ecdsa.PrivateKey, error) {
	b, err := raw64.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	k, err := ecdh.P256().NewPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("invalid VAPID key: %w", err)
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), k.PublicKey().Bytes())
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, D: new(big.Int).SetBytes(b)}, nil
}
