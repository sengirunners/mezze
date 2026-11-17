package webui

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

// Migrations are this module's tables; its New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "webui/0002_push_dismissed", Script: `CREATE TABLE web_push_dismissed (identity TEXT NOT NULL, address TEXT NOT NULL, ended INTEGER NOT NULL, PRIMARY KEY(identity,address,ended));`},
	{Kind: store.KindRegistry, Name: "webui/0001_push", Script: `
CREATE TABLE web_push_subscriptions (
 endpoint_hash TEXT PRIMARY KEY, endpoint TEXT NOT NULL, p256dh TEXT NOT NULL, auth TEXT NOT NULL,
 identity TEXT NOT NULL, address TEXT NOT NULL, token TEXT NOT NULL, cap_project TEXT NOT NULL, cap_area TEXT NOT NULL,
 cap_role TEXT NOT NULL, created_cursor TEXT NOT NULL
);
CREATE INDEX web_push_person ON web_push_subscriptions(identity);
CREATE TABLE web_push_state (id INTEGER PRIMARY KEY CHECK (id = 1), cursor TEXT NOT NULL);
CREATE TABLE web_push_session_state (address TEXT PRIMARY KEY, state TEXT NOT NULL);
CREATE TABLE web_push_queue (
 event_id TEXT NOT NULL, endpoint_hash TEXT NOT NULL, project TEXT NOT NULL, type TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(event_id, endpoint_hash)
);
CREATE INDEX web_push_due ON web_push_queue(next_at);
CREATE TABLE web_push_sent (endpoint_hash TEXT NOT NULL, item TEXT NOT NULL, sent_at INTEGER NOT NULL, PRIMARY KEY(endpoint_hash,item));
CREATE TABLE web_push_rate (endpoint_hash TEXT PRIMARY KEY, since INTEGER NOT NULL, sent INTEGER NOT NULL);
CREATE TABLE web_push_rate_event (identity TEXT NOT NULL, event_id TEXT NOT NULL, sent_at INTEGER NOT NULL, PRIMARY KEY(identity,event_id));
CREATE TABLE web_push_mail_link (endpoint_hash TEXT NOT NULL, session TEXT NOT NULL, mail_id TEXT NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(endpoint_hash,mail_id));
`},
}

type pushSubscription = orchapi.DevicePushRequest

// scrubPushBackup (core.BackupScrubber) drops every subscription from a
// staged backup copy: its endpoint is a capability URL and auth/p256dh are
// its encryption keys, so none goes off-site. A restored home sends no alert
// until each device turns alerts on again (it subscribes anew).
func scrubPushBackup(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{`DELETE FROM web_push_subscriptions`, `DELETE FROM web_push_queue`, `DELETE FROM web_push_mail_link`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// deviceToken marks a subscription made by a paired device (SPEC 16 "Phone
// alerts"): no bearer token, the device's person while it stays an active
// approver.
const deviceToken = "device:"

type pushRecord struct{ hash, endpoint, p256dh, auth, identity, address, token, cursor string }
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

var errPushExpired = errors.New("push subscription expired")

type pushHTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *pushHTTPError) Error() string { return fmt.Sprintf("push service returned HTTP %d", e.Status) }

const maxPushSubscriptionsPerPerson = 5

type cachedPushToken struct {
	value   string
	created time.Time
}

// Apple asks application servers not to rotate provider tokens more than
// once an hour. Cache per push-service origin, never per subscription URL.
func (s *Service) signedVAPID(key *ecdsa.PrivateKey, endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	origin := u.Scheme + "://" + u.Host
	s.pushTokenMu.Lock()
	defer s.pushTokenMu.Unlock()
	if cached, ok := s.pushTokens[origin]; ok && now.Sub(cached.created) < time.Hour && !now.Before(cached.created) {
		return cached.value, nil
	}
	subject := ""
	if s.d != nil && s.d.Config != nil {
		subject = s.d.Config.File.Web.VAPIDSubject
	}
	token, err := vapidToken(key, endpoint, now, subject)
	if err != nil {
		return "", err
	}
	if s.pushTokens == nil {
		s.pushTokens = make(map[string]cachedPushToken)
	}
	s.pushTokens[origin] = cachedPushToken{token, now}
	return token, nil
}

func (s *Service) vapid() (*ecdsa.PrivateKey, error) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.pushKey != nil {
		return s.pushKey, nil
	}
	path := s.d.Config.Path("secrets/web-push-vapid")
	raw, err := secrets.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		k, e := ecdh.P256().GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		raw = raw64.EncodeToString(k.Bytes())
		if e = secrets.WriteFile(path, raw); e != nil {
			return nil, e
		}
	}
	key, err := parseVAPID(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	s.pushKey = key
	return key, nil
}

// Browser push endpoints are capability URLs. Restrict destinations to browser
// push providers, then validate the resolved address again at dial time.
func pushEndpoint(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 2048 || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "fcm.googleapis.com" || h == "updates.push.services.mozilla.com" || h == "web.push.apple.com" || strings.HasSuffix(h, ".push.apple.com")
}
func pushHash(endpoint string) string {
	h := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(h[:])
}

// subscribe stores req for identity, made by a paired device (deviceToken +
// its node ID).
func (s *Service) subscribe(ctx context.Context, identity, address, token string, req pushSubscription) error {
	pub, e := raw64.DecodeString(req.Keys.P256DH)
	auth, e2 := raw64.DecodeString(req.Keys.Auth)
	if !pushEndpoint(req.Endpoint) || e != nil || e2 != nil || len(auth) != 16 {
		return api.Errorf(api.CodeBadRequest, "invalid push subscription")
	}
	if _, e = ecdh.P256().NewPublicKey(pub); e != nil {
		return api.Errorf(api.CodeBadRequest, "invalid push subscription key")
	}
	h := pushHash(req.Endpoint)
	// An endpoint belongs to the person who first registered it; another
	// identity cannot steal it by presenting the same capability URL.
	return store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO web_push_subscriptions(endpoint_hash,endpoint,p256dh,auth,identity,address,token,cap_project,cap_area,cap_role,created_cursor)
 VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(endpoint_hash) DO UPDATE SET p256dh=excluded.p256dh,auth=excluded.auth,token=excluded.token,cap_project=excluded.cap_project,cap_area=excluded.cap_area,cap_role=excluded.cap_role,created_cursor=excluded.created_cursor WHERE identity=excluded.identity`, h, req.Endpoint, req.Keys.P256DH, req.Keys.Auth, identity, address, token, "", "", "", ids.New())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return api.NotFound("subscription", "")
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE identity=? AND endpoint_hash NOT IN (SELECT endpoint_hash FROM web_push_subscriptions WHERE identity=? ORDER BY created_cursor DESC LIMIT ?)`, identity, identity, maxPushSubscriptionsPerPerson)
		return err
	})
}

// PushPublicKey is the VAPID public key a browser subscribes with (core.DevicePush).
func (s *Service) PushPublicKey(context.Context) (string, error) {
	key, err := s.vapid()
	if err != nil {
		return "", err
	}
	return raw64.EncodeToString(elliptic.Marshal(key.Curve, key.X, key.Y)), nil
}

// SubscribeDevice stores a paired device's subscription for its person
// (core.DevicePush); the approvals brick checked the device first.
func (s *Service) SubscribeDevice(ctx context.Context, person *authz.Subject, node string, req orchapi.DevicePushRequest) error {
	if person == nil || person.Identity.ID == "" || node == "" {
		return api.Errorf(api.CodeForbidden, "push needs a paired device's person")
	}
	return s.subscribe(ctx, person.Identity.ID, person.Address, deviceToken+node, req)
}

// UnsubscribeDevice removes node's subscription for endpoint (core.DevicePush).
func (s *Service) UnsubscribeDevice(ctx context.Context, node, endpoint string) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE endpoint_hash=? AND token=?`, pushHash(endpoint), deviceToken+node)
	return err
}
func (s *Service) pushRecords(ctx context.Context, person string) ([]pushRecord, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT endpoint_hash,endpoint,p256dh,auth,identity,address,token,created_cursor FROM web_push_subscriptions WHERE (?='' OR identity=?)`, person, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pushRecord
	for rows.Next() {
		var p pushRecord
		if err := rows.Scan(&p.hash, &p.endpoint, &p.p256dh, &p.auth, &p.identity, &p.address, &p.token, &p.cursor); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pushSubject is a paired device's person now, only while the device is an
// active approver for that same person; nil for anything else.
func (s *Service) pushSubject(ctx context.Context, p pushRecord) *authz.Subject {
	node, ok := strings.CutPrefix(p.token, deviceToken)
	if !ok || orchcore.DevicePerson(s.d) == nil {
		return nil
	}
	sub, ok := orchcore.DevicePerson(s.d)(ctx, node)
	if !ok || sub == nil || sub.Identity.ID != p.identity || sub.Address != p.address {
		return nil
	}
	return sub
}
func publicPushClient() *http.Client {
	blocked := []netip.Prefix{}
	for _, x := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16"} {
		blocked = append(blocked, netip.MustParsePrefix(x))
	}
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, addr string, _ syscall.RawConn) error {
		h, _, e := net.SplitHostPort(addr)
		if e != nil {
			return errors.New("invalid push address")
		}
		ip, e := netip.ParseAddr(h)
		if e != nil {
			return e
		}
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return errors.New("private push address")
		}
		for _, p := range blocked {
			if p.Contains(ip) {
				return errors.New("reserved push address")
			}
		}
		return nil
	}}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (s *Service) sendPush(ctx context.Context, p pushRecord, msg pushMessage) error {
	if !pushEndpoint(p.endpoint) {
		return errors.New("invalid stored push endpoint")
	}
	key, err := s.vapid()
	if err != nil {
		return err
	}
	pub, err := raw64.DecodeString(p.p256dh)
	if err != nil {
		return err
	}
	auth, err := raw64.DecodeString(p.auth)
	if err != nil {
		return err
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	encrypted, err := encryptPush(pub, auth, body)
	if err != nil {
		return err
	}
	token, err := s.signedVAPID(key, p.endpoint, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(encrypted))
	if err != nil {
		return err
	}
	req.Header.Set("TTL", "3600")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "vapid t="+token+", k="+raw64.EncodeToString(elliptic.Marshal(key.Curve, key.X, key.Y)))
	resp, err := s.pushHTTP.Do(req)
	if err != nil {
		// net/http includes the full URL in *url.Error; a push endpoint is a
		// capability and must not be copied into logs or test-command errors.
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			err = requestErr.Err
		}
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		_, err = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE endpoint_hash=?`, p.hash)
		if err != nil {
			return err
		}
		return errPushExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retry := time.Duration(0)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			retry = time.Duration(min(seconds, 3600)) * time.Second
		} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			retry = min(time.Until(date), time.Hour)
		}
		return &pushHTTPError{Status: resp.StatusCode, RetryAfter: retry}
	}
	return nil
}
func (s *Service) pushTest(w http.ResponseWriter, r *http.Request) {
	ps, err := s.pushRecords(r.Context(), s.d.Projects.LocalIdentity().ID)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	sent := 0
	for _, p := range ps {
		if s.pushSubject(r.Context(), p) == nil {
			continue
		}
		if err = s.sendPush(r.Context(), p, pushMessage{Title: "zm test", Body: "Notifications are working", URL: decisionsPage}); err != nil {
			if errors.Is(err, errPushExpired) {
				continue
			}
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "test push failed: %v", err))
			return
		}
		sent++
	}
	if sent == 0 {
		api.WriteError(w, api.Errorf(api.CodeNotFound, "no active phone subscription for this person"))
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]int{"sent": sent})
}
