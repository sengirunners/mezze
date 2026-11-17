package approvals

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func sshBlob(parts ...[]byte) string {
	var b []byte
	for _, p := range parts {
		b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
		b = append(b, p...)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func rsaLine(t *testing.T, bits int) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return "ssh-rsa " + sshBlob([]byte("ssh-rsa"), big.NewInt(int64(k.E)).Bytes(), append([]byte{0}, k.N.Bytes()...)) + " rsa@host"
}

// An authorize-key action takes one OpenSSH public key line of a strong
// type, nothing else: what the card shows (its fingerprint and comment) is
// exactly what the target appends.
func TestParseAuthorizedKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	ed := "ssh-ed25519 " + sshBlob([]byte("ssh-ed25519"), pub)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	q, _ := ek.PublicKey.Bytes()
	ec := "ecdsa-sha2-nistp256 " + sshBlob([]byte("ecdsa-sha2-nistp256"), []byte("nistp256"), q) + " ec@host"

	key, comment, fp, err := parseAuthorizedKey("  " + ed + "   Ivo's   MacBook \n")
	if err != nil || key != ed+" Ivo's MacBook" || comment != "Ivo's MacBook" || !strings.HasPrefix(fp, "SHA256:") || len(fp) != 7+43 {
		t.Fatalf("ed25519: %q %q %q %v", key, comment, fp, err)
	}
	for _, ok := range []string{ec, rsaLine(t, 3072)} {
		if _, _, _, err := parseAuthorizedKey(ok); err != nil {
			t.Errorf("%.30s: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"rsa 2048":   rsaLine(t, 2048),
		"options":    `command="sh" ` + ed,
		"two lines":  ed + "\n" + ed,
		"dsa":        "ssh-dss " + sshBlob([]byte("ssh-dss"), []byte("x")),
		"type lies":  "ssh-ed25519 " + sshBlob([]byte("ssh-rsa"), pub),
		"short":      "ssh-ed25519 " + sshBlob([]byte("ssh-ed25519"), pub[:16]),
		"trailing":   "ssh-ed25519 " + sshBlob([]byte("ssh-ed25519"), pub, []byte("x")),
		"no key":     "ssh-ed25519",
		"not base64": "ssh-ed25519 !!!",
		"reserved":   ed + " OWNER DECISION",
	} {
		if _, _, _, err := parseAuthorizedKey(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The receipt binds a running action by its whole frozen JSON; ack-mail's
// line stays byte for byte.
func TestActionLine(t *testing.T) {
	act := &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, On: "a", Machine: "box", User: "ivo", Key: "ssh-ed25519 AAAA x"}
	got := actionLine(&orchapi.AskCard{Action: act})
	if got != "authorize-key sha256="+textSHA(marshal(*act))+" on=a" {
		t.Fatalf("line %q", got)
	}
	ack := &orchapi.AskAction{Kind: orchapi.ActionAckMail, IDs: []string{"A", "B"}, On: "b", Count: 2}
	if got := actionLine(&orchapi.AskCard{Action: ack}); got != "ack-mail count=2 ids_sha256="+idsSHA(ack.IDs)+" on=b" {
		t.Fatalf("ack line %q", got)
	}
	if b := marshal(*ack); b != `{"kind":"ack-mail","ids":["A","B"],"on":"b","count":2}` {
		t.Fatalf("an ack-mail action's JSON changed (its stored cards' hashes with it): %s", b)
	}
}

// An answer-only token never picks a choice that carries a confirm word,
// whatever the action's kind (today only signed-only kinds carry one, so
// this is the rule for a kind that ever runs nothing but takes a word); the
// other choice, a card with no word, and every other caller are untouched.
func TestAnswerTokenConfirmWord(t *testing.T) {
	tok, sock := approverRow{owner: true, answerToken: true}, approverRow{owner: true}
	worded := &orchapi.AskAction{Kind: orchapi.ActionAckMail, On: "a", ConfirmWord: "harbor"}
	plain := &orchapi.AskAction{Kind: orchapi.ActionAckMail, On: "a"}
	for _, c := range []struct {
		name     string
		me       approverRow
		act      *orchapi.AskAction
		decision string
		refused  bool
	}{
		{"the token, the worded choice", tok, worded, "a", true},
		{"the token, the worded choice spelled loosely", tok, worded, " A ", true},
		{"the token, the other choice", tok, worded, "b", false},
		{"the token, no word", tok, plain, "a", false},
		{"the token, no action", tok, nil, "a", false},
		{"the socket, the worded choice", sock, worded, "a", false},
	} {
		err := answerTokenConfirmWord(c.me, c.act, c.decision)
		var e *api.Error
		got := errors.As(err, &e) && e.Code == api.CodeForbidden && strings.Contains(string(e.Data), orchapi.ReasonSignedOnly)
		if got != c.refused || (err != nil) != c.refused {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
