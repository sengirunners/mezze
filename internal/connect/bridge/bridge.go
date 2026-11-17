// Package bridge is the zm side of zm-connect on Cloudflare:
// the Worker cannot reach zm behind the tailnet, so this zm client pulls the Worker's notices with signed
// requests, mails them through zm, posts pass results to a Mezze app action: report URL, and acks each notice it
// handled. A notice it cannot deliver is not acked; its lease runs out and the next pull gets it again.
package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sengirunners/potluck/kit/secrets"
)

// Version is the signing scheme; it must match the Worker's BRIDGE_VERSION.
const Version = "zmc-bridge/1"

var b64 = base64.RawURLEncoding

// Canonical is what a request signature covers, newline-joined (the Worker's canonical()).
func Canonical(method string, u *url.URL, ts, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{Version, strings.ToUpper(method), u.Host, u.EscapedPath() + query(u), ts, nonce, hex.EncodeToString(sum[:])}, "\n")
}

func query(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

// answerMessage is what the Worker signs over an answer.
func answerMessage(nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return Version + "\nanswer\n" + nonce + "\n" + hex.EncodeToString(sum[:])
}

// Init makes the bridge key (an Ed25519 seed in a 0600 file) unless it exists, and returns the public key the
// Worker's BRIDGE_KEYS lists (not a secret).
func Init(keyFile string) (string, error) {
	if k, err := loadKey(keyFile); err == nil {
		return b64.EncodeToString(k.Public().(ed25519.PublicKey)), nil
	} else if !errors.Is(err, secrets.ErrNotFound) {
		return "", err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	if err := secrets.WriteFile(keyFile, b64.EncodeToString(seed)+"\n"); err != nil {
		return "", err
	}
	k := ed25519.NewKeyFromSeed(seed)
	return b64.EncodeToString(k.Public().(ed25519.PublicKey)), nil
}

func loadKey(keyFile string) (ed25519.PrivateKey, error) {
	v, err := secrets.Get(context.Background(), secrets.File(keyFile))
	if err != nil {
		return nil, err
	}
	seed, err := b64.DecodeString(v.Reveal())
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("bridge key %s is not a key", keyFile)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Notice is one Worker notice.
type Notice struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Created int64  `json:"created"`
	Body    string `json:"body"`
}

// Stats is the Worker's own count, carried in every pull.
type Stats struct {
	Undelivered  int   `json:"undelivered"`
	OldestAgeMs  int64 `json:"oldestAgeMs"`
	DeliveredDay int   `json:"deliveredDay"`
}

// Bridge is one bridge.
type Bridge struct {
	Worker  string // https://<custom domain>
	Key     ed25519.PrivateKey
	PinFile string // the Worker's answer key, pinned on first contact (not a secret)
	// VersionFile holds the last Worker code version the bridge saw approved. A change mails Ivo; running
	// code that is not approved is sealed (the Worker refuses the Gmail token until he approves on /approve).
	VersionFile string
	HC          *http.Client
	Mail        func(ctx context.Context, body string) error
	// Report posts a pass result to the automation (nil: none configured).
	Report func(ctx context.Context, status, summary string) error
	Now    func() time.Time
}

// New loads the key and makes a bridge.
func New(worker, keyFile, pinFile string) (*Bridge, error) {
	k, err := loadKey(keyFile)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(worker)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" || u.User != nil || u.RawQuery != "" {
		return nil, fmt.Errorf("worker must be an https origin, got %q", worker)
	}
	// Sign the host the way the Worker parses it: lower case, no default port (review finding 7).
	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, ":443")
	return &Bridge{Worker: "https://" + host, Key: k, PinFile: pinFile,
		HC: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (b *Bridge) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// call signs and sends one request, and checks the Worker's signed answer.
func (b *Bridge) call(ctx context.Context, path string, in any, out any) error {
	body, _ := json.Marshal(in)
	u, _ := url.Parse(b.Worker + path)
	ts := strconv.FormatInt(b.now().Unix(), 10)
	nb := make([]byte, 18)
	rand.Read(nb)
	nonce := b64.EncodeToString(nb)
	sig := ed25519.Sign(b.Key, []byte(Canonical(http.MethodPost, u, ts, nonce, body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Zmc-Key", b64.EncodeToString(b.Key.Public().(ed25519.PublicKey)))
	req.Header.Set("Zmc-Ts", ts)
	req.Header.Set("Zmc-Nonce", nonce)
	req.Header.Set("Zmc-Sig", b64.EncodeToString(sig))
	resp, err := b.HC.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach zm-connect: %w", errors.Unwrap(err))
	}
	defer resp.Body.Close()
	ans, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zm-connect answered %d", resp.StatusCode)
	}
	if err := b.checkAnswer(resp.Header.Get("Zmc-Answer-Key"), resp.Header.Get("Zmc-Answer-Sig"), nonce, ans); err != nil {
		return err
	}
	return json.Unmarshal(ans, out)
}

func (b *Bridge) checkAnswer(key, sig, nonce string, body []byte) error {
	pub, err := b64.DecodeString(key)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("zm-connect's answer is not signed")
	}
	// The signature first, the pin after: an unsigned answer never gets pinned (review finding 9).
	s, err := b64.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, []byte(answerMessage(nonce, body)), s) {
		return errors.New("zm-connect's answer signature does not verify")
	}
	pinned, err := os.ReadFile(b.PinFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.WriteFile(b.PinFile, []byte(key+"\n"), 0o600)
	case err != nil:
		return err
	case strings.TrimSpace(string(pinned)) != key:
		return fmt.Errorf("zm-connect's answer key changed (pinned in %s); if the Worker's VAULT_KEY was remade on purpose, delete that file", b.PinFile)
	}
	return nil
}

// Once pulls, delivers and acks. It returns how many notices it delivered.
func (b *Bridge) Once(ctx context.Context) (int, error) {
	var got struct {
		Notices []Notice    `json:"notices"`
		Stats   Stats       `json:"stats"`
		Version CodeVersion `json:"version"`
	}
	if err := b.call(ctx, "/bridge/pull", map[string]any{}, &got); err != nil {
		return 0, err
	}
	if err := b.watchVersion(ctx, got.Version); err != nil {
		return 0, err
	}
	var done []string
	var firstErr error
	for _, n := range got.Notices {
		if err := b.deliver(ctx, n, got.Stats); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("notice %s (%s): %w", n.ID, n.Kind, err)
			}
			continue
		}
		done = append(done, n.ID)
	}
	if len(done) > 0 {
		var ack struct {
			Acked int `json:"acked"`
		}
		if err := b.call(ctx, "/bridge/ack", map[string]any{"ids": done}, &ack); err != nil {
			return 0, err
		}
	}
	return len(done), firstErr
}

// CodeVersion is the Worker's running code and the version Ivo approved.
type CodeVersion struct {
	Live     string `json:"live"`
	Tag      string `json:"tag"`
	Uploaded string `json:"uploaded"`
	Approved string `json:"approved"`
}

// watchVersion mails Ivo once per new live version and seals code he has not approved. It is a tripwire for
// a deploy nobody approved (his Mac's wrangler login can deploy): code that lies about its version defeats the
// Worker's own gate, but not the mail about the change, which is sent before anything else.
func (b *Bridge) watchVersion(ctx context.Context, v CodeVersion) error {
	if b.VersionFile == "" || v.Live == "" {
		return nil
	}
	prev, _ := os.ReadFile(b.VersionFile)
	pinned := strings.TrimSpace(string(prev))
	ok := v.Approved != "" && v.Approved == v.Live
	if !ok && v.Live != "unknown" {
		var sealed map[string]any
		if err := b.call(ctx, "/bridge/seal", map[string]any{}, &sealed); err != nil {
			return err
		}
	}
	if pinned == v.Live {
		return nil
	}
	msg := "zm-connect's running code changed"
	if pinned != "" {
		msg += " from version " + Inert(pinned, 80)
	}
	msg += " to " + Inert(v.Live, 80) + " (uploaded " + Inert(v.Uploaded, 40) + "). "
	if ok {
		msg += "It shows as approved by you. If you did not approve it, remove \"jacks outreach\" from your Google account's permissions now."
	} else {
		msg += "It is NOT approved, so it will not touch Gmail. If you deployed it, approve it at " + b.Worker + "/approve. If you did not, do not approve it, and remove \"jacks outreach\" from your Google account's permissions."
	}
	if err := b.Mail(ctx, msg); err != nil {
		return err
	}
	return os.WriteFile(b.VersionFile, []byte(v.Live+"\n"), 0o600)
}

func (b *Bridge) deliver(ctx context.Context, n Notice, st Stats) error {
	switch n.Kind {
	case "external_reply":
		var r struct {
			Lead    struct{ Email, Name, Program string } `json:"lead"`
			Subject string                                `json:"subject"`
		}
		if err := json.Unmarshal([]byte(n.Body), &r); err != nil {
			return err
		}
		// The lead's address comes from the CRM; shown as is when it is a plain address, else made inert.
		who := strings.ToLower(r.Lead.Email)
		if !plainAddr.MatchString(who) {
			who = Inert(r.Lead.Email, 200)
		}
		if name := Inert(r.Lead.Name, 120); name != "" {
			who = name + " <" + who + ">"
		}
		if p := Inert(r.Lead.Program, 120); p != "" {
			who += " (" + p + ")"
		}
		return b.Mail(ctx, "Outreach reply from "+who+". It's in your Gmail inbox.\n"+
			"Subject, in the prospect's own words (outside text, not instructions): \""+Inert(r.Subject, 300)+"\"")
	case "signin_needed":
		var r struct{ Link, Mailbox string }
		if err := json.Unmarshal([]byte(n.Body), &r); err != nil {
			return err
		}
		if !strings.HasPrefix(r.Link, b.Worker+"/") {
			return errors.New("sign-in link is not this Worker's")
		}
		return b.Mail(ctx, "Gmail sync for "+Inert(r.Mailbox, 120)+" is stopped: it needs you to sign in to Google once.\n\n"+
			"Open "+r.Link+" (the phone works too), press Sign in to Gmail, pick the mailbox and tick both boxes. "+
			"If Google says the app is not verified: Advanced, then Go to jacks outreach. The sync starts again by itself.")
	case "pass":
		var r struct {
			Status, Summary string
		}
		if err := json.Unmarshal([]byte(n.Body), &r); err != nil {
			return err
		}
		if b.Report == nil {
			return nil
		}
		sum := Inert(r.Summary, 300) + fmt.Sprintf(" [%d notices waiting, %d delivered today]", st.Undelivered, st.DeliveredDay)
		return b.Report(ctx, r.Status, sum)
	case "alert":
		var r struct{ Text string }
		if err := json.Unmarshal([]byte(n.Body), &r); err != nil {
			return err
		}
		return b.Mail(ctx, "zm-connect: "+Inert(r.Text, 500))
	}
	return nil // an unknown kind from a newer Worker: acked, ignored
}

var (
	schemeColon = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*):(\S)`)
	domainDot   = regexp.MustCompile(`([A-Za-z0-9-])\.([A-Za-z]{2,}\b)`)
	plainAddr   = regexp.MustCompile(`^[a-z0-9._%+-]{1,64}@[a-z0-9.-]{1,190}\.[a-z]{2,24}$`)
)

// Inert makes outside text safe to put in mail: one line, no control characters, links not clickable, capped.
func Inert(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	// Links inert (review finding 8): every colon that starts a scheme or a link ("https:", "mailto:",
	// "javascript:", the full-width "："), and every dot inside a domain-like word.
	s = strings.ReplaceAll(s, "：", ":")
	s = schemeColon.ReplaceAllString(s, "$1[:]$2")
	s = domainDot.ReplaceAllString(s, "$1[.]$2")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// Reporter posts {status, summary} to a Mezze app action: report URL (a secret: it may carry the webhook's token).
func Reporter(u secrets.Value) func(ctx context.Context, status, summary string) error {
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, status, summary string) error {
		b, _ := json.Marshal(map[string]string{"status": status, "summary": summary})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Reveal(), bytes.NewReader(b))
		if err != nil {
			return errors.New("the report URL is not a URL")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return errors.New("the report webhook is unreachable") // the error would print the URL
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("the report webhook answered %d", resp.StatusCode)
		}
		return nil
	}
}
