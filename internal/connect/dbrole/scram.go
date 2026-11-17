// Package dbrole makes the zm_connect database role's setup SQL, holding a SCRAM
// verifier instead of a password.
package dbrole

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// scramIterations is Postgres's default for SCRAM-SHA-256.
const scramIterations = 4096

// SCRAMVerifier is the SCRAM-SHA-256 verifier Postgres stores for
// password with salt (Postgres accepts it in CREATE/ALTER ROLE ...
// PASSWORD and stores it as is). It cannot be turned back into the
// password, so the SQL that carries it holds no secret. The password must
// be ASCII without control characters (SASLprep is then the identity).
func SCRAMVerifier(password string, salt []byte) (string, error) {
	for _, r := range password {
		if r < 0x21 || r > 0x7e {
			return "", fmt.Errorf("dbrole: the database password must be printable ASCII")
		}
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := mac(salted, "Server Key")
	b := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramIterations, b(salt), b(stored[:]), b(serverKey)), nil
}

// NewSCRAMVerifier uses a fresh 16-byte salt.
func NewSCRAMVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	return SCRAMVerifier(password, salt)
}

// NewPassword is 32 random bytes, URL-safe base64 (printable ASCII).
func NewPassword() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
