package dbrole

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// RFC 7677's SCRAM-SHA-256 exchange (user "user", password "pencil"): the
// verifier's StoredKey must check the client's proof, and its ServerKey
// must give the server signature the RFC shows.
func TestSCRAMVerifierMatchesRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	v, err := SCRAMVerifier("pencil", salt)
	if err != nil {
		t.Fatal(err)
	}
	head, keys, ok := strings.Cut(v, "$")
	if !ok || head != "SCRAM-SHA-256" {
		t.Fatalf("verifier %q", v)
	}
	params, keyPair, _ := strings.Cut(keys, "$")
	if params != "4096:W22ZaJ0SNY7soEsUEjb6gQ==" {
		t.Fatalf("params %q", params)
	}
	storedB, serverB, _ := strings.Cut(keyPair, ":")
	stored, _ := base64.StdEncoding.DecodeString(storedB)
	server, _ := base64.StdEncoding.DecodeString(serverB)

	auth := "n=user,r=rOprNGfwEbeRWgbNEkqO," +
		"r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096," +
		"c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	if got := base64.StdEncoding.EncodeToString(mac(server, auth)); got != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Fatalf("server signature %s", got)
	}
	proof, _ := base64.StdEncoding.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")
	sig := mac(stored, auth)
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ sig[i]
	}
	h := sha256.Sum256(clientKey)
	if !hmac.Equal(h[:], stored) {
		t.Fatal("the client proof does not check against StoredKey")
	}
}

func TestPasswordIsPrintableAndUnique(t *testing.T) {
	a, b := NewPassword(), NewPassword()
	if a == b || len(a) != 43 {
		t.Fatalf("%d %v", len(a), a == b)
	}
	if _, err := NewSCRAMVerifier(a); err != nil {
		t.Fatal(err)
	}
	if _, err := SCRAMVerifier("has space", []byte("salt")); err == nil {
		t.Fatal("want an error")
	}
}
