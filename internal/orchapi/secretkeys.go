package orchapi

import "time"

// Secret key states (SPEC 17.12).
const (
	SecretKeyActive   = "active"   // a sender may seal to it
	SecretKeyReplaced = "replaced" // the machine published a newer key
	SecretKeyRevoked  = "revoked"  // its machine or an admin ended it
)

// SecretKey is one machine's published seal key in a project's key
// directory (SPEC 17.12): what a sender seals a secret to. KeyID and
// SealKey are the inputs sealbox.Seal takes; Fingerprint names the pair,
// and a sender pins it per machine.
type SecretKey struct {
	Project     string     `json:"project"`
	Machine     string     `json:"machine"`
	Owner       string     `json:"owner"` // the person whose machine it is (redacted when hidden)
	KeyID       string     `json:"key_id"`
	SealKey     string     `json:"seal_key"` // X25519 public key, base64url
	Fingerprint string     `json:"fingerprint"`
	State       string     `json:"state"`
	Published   time.Time  `json:"published"`
	Ended       *time.Time `json:"ended,omitempty"`
	EndedBy     string     `json:"ended_by,omitempty"`
}

// SecretKeyList is GET /v0/secret-keys.
type SecretKeyList struct {
	Keys []SecretKey `json:"keys"`
}
