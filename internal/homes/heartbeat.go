package homes

// The watchdog heartbeat (SPEC 17.9 "Off-site home backups"): every minute
// the home POSTs a small signed note to a watchdog URL: its id, how many
// trackers it has and how many fail to sync, its off-site backup's status
// (no error text), the zm version. No name (project, tracker, path), no issue
// data and no secret leaves the home. It is an outbound call only, so a home on a private
// network is watched from outside without opening a port. Signed per
// Standard Webhooks (webhook-id, webhook-timestamp, webhook-signature) with
// the reference library, as zm's webhooks are (SPEC 18.1), with a whsec_
// secret read through the secrets brick on every send (a rotated secret is
// picked up at once).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/secrets"
)

// heartbeatEvery is how often the home reports (tests shorten it).
var heartbeatEvery = time.Minute

// syncFailingAfter: a tracker counts as failing in the heartbeat once its
// mirror syncs have failed this long (a blip is not news).
const syncFailingAfter = 10 * time.Minute

// heartbeat is the body (version 1).
type heartbeat struct {
	V       int                 `json:"v"`
	Home    string              `json:"home"`
	Sent    time.Time           `json:"sent"`
	Version string              `json:"version"`
	Commit  string              `json:"commit,omitempty"`
	Sync    heartbeatSync       `json:"sync"`
	Backup  *orchapi.HomeBackup `json:"backup"`
}

type heartbeatSync struct {
	OK      bool `json:"ok"`
	Sources int  `json:"sources"`
	Failing int  `json:"failing"` // trackers whose syncs have failed for 10 minutes
}

// heartbeats is the last send's outcome (in memory: it changes every minute).
type heartbeats struct {
	mu       sync.Mutex
	lastSent *time.Time
	status   int
	err      string
	failures int
}

func (h *heartbeats) record(at time.Time, status int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSent, h.status = &at, status
	if err != nil {
		h.err = clip(err.Error(), 300)
		h.failures++
		return
	}
	h.err, h.failures = "", 0
}

// brief is the last send's outcome for GET /v0/health (nil: none sent yet).
func (h *heartbeats) brief() *orchapi.HeartbeatBrief {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastSent == nil {
		return nil
	}
	return &orchapi.HeartbeatBrief{LastSent: h.lastSent, LastStatus: h.status, Failing: h.failures > 0, Failures: h.failures, Error: h.err}
}

// Heartbeat is the watchdog heartbeat's last send (nil: no watchdog set up,
// or none sent since the daemon started), for GET /v0/health
// (orchcore.BackupReporter).
func (s *Service) Heartbeat() *orchapi.HeartbeatBrief {
	st, err := s.loadOffsite()
	if err != nil || st.Watchdog == nil {
		return nil
	}
	return s.hb.brief()
}

func (h *heartbeats) view(w *watchdogRec) *orchapi.WatchdogView {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &orchapi.WatchdogView{URL: w.URL, Home: w.Home, Secret: w.Secret, LastSent: h.lastSent,
		LastStatus: h.status, LastError: h.err, Failures: h.failures}
}

var heartbeatClient = &http.Client{Timeout: 10 * time.Second}

func checkWatchdogURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return api.Errorf(api.CodeBadRequest, "watchdog URL %q is not an https URL", raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return api.Errorf(api.CodeBadRequest, "watchdog URL %q: https only", raw)
	}
	return nil
}

// setWatchdog is PUT /v0/backup/watchdog: it saves the setting and sends
// one heartbeat at once, whose outcome the answer shows.
func (s *Service) setWatchdog(ctx context.Context, req orchapi.WatchdogRequest) (orchapi.OffsiteView, error) {
	if err := checkWatchdogURL(req.URL); err != nil {
		return orchapi.OffsiteView{}, err
	}
	if err := offsite.CheckHome(req.Home); err != nil {
		return orchapi.OffsiteView{}, err
	}
	if _, err := secrets.Parse(req.Secret); err != nil {
		return orchapi.OffsiteView{}, err
	}
	w := &watchdogRec{URL: req.URL, Secret: req.Secret, Home: req.Home}
	if _, err := s.signer(ctx, w); err != nil {
		return orchapi.OffsiteView{}, secrets.Recode(err, api.CodeBadRequest)
	}
	if _, err := s.updateOffsite(func(st *offsiteState) error { st.Watchdog = w; return nil }); err != nil {
		return orchapi.OffsiteView{}, err
	}
	s.beat(ctx, w)
	return s.offsiteView()
}

func (s *Service) watchdogOff() (orchapi.OffsiteView, error) {
	v, err := s.offsiteView()
	if err != nil {
		return v, err
	}
	if v.Watchdog == nil {
		return v, api.Errorf(api.CodeNotFound, "no watchdog is set up")
	}
	_, err = s.updateOffsite(func(st *offsiteState) error { st.Watchdog = nil; return nil })
	return v, err
}

func (s *Service) signer(ctx context.Context, w *watchdogRec) (*standardwebhooks.Webhook, error) {
	ref, err := secrets.Parse(w.Secret)
	if err != nil {
		return nil, err
	}
	v, err := secrets.Default().Get(ctx, ref)
	if err != nil {
		return nil, secrets.Prefix(err, "the watchdog secret: ")
	}
	if !strings.HasPrefix(v.Reveal(), "whsec_") {
		return nil, api.Errorf(api.CodeUnavailable, "the watchdog secret (%s) is not a whsec_ secret", ref)
	}
	wh, err := standardwebhooks.NewWebhook(v.Reveal())
	if err != nil {
		return nil, api.Errorf(api.CodeUnavailable, "the watchdog secret (%s) is not whsec_ and base64", ref)
	}
	return wh, nil
}

// heartbeatBody is what this home says about itself now.
func (s *Service) heartbeatBody(home string) heartbeat {
	hb := heartbeat{V: 1, Home: home, Sent: s.now().UTC().Truncate(time.Second), Version: api.Version, Commit: api.Commit,
		Sync: heartbeatSync{OK: true}, Backup: s.HomeBackup()}
	if hb.Backup != nil {
		hb.Backup.Error = "" // it can name a project or a path; failing and failures say enough
	}
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		if f, ok := s.d.Projects.File(p.ID); ok {
			hb.Sync.Sources += len(orchcore.ProjectTrackers(f))
		}
	}
	if th, ok := orchcore.Work(s.d).(orchcore.TrackerHealth); ok {
		hb.Sync.Failing = len(th.FailingTrackers(authz.All(), syncFailingAfter))
	}
	hb.Sync.OK = hb.Sync.Failing == 0
	return hb
}

// beat sends one heartbeat and records the outcome.
func (s *Service) beat(ctx context.Context, w *watchdogRec) {
	at := s.now().UTC()
	status, err := s.sendHeartbeat(ctx, w)
	s.hb.record(at, status, err)
	if err != nil {
		s.warn("watchdog heartbeat failed", "err", err)
	}
}

func (s *Service) sendHeartbeat(ctx context.Context, w *watchdogRec) (int, error) {
	wh, err := s.signer(ctx, w)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(s.heartbeatBody(w.Home))
	if err != nil {
		return 0, err
	}
	var idb [12]byte
	_, _ = rand.Read(idb[:])
	id := "hb_" + hex.EncodeToString(idb[:])
	now := time.Now()
	sig, err := wh.Sign(id, now, body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zm/"+api.Version)
	req.Header.Set(standardwebhooks.HeaderWebhookID, id)
	req.Header.Set(standardwebhooks.HeaderWebhookTimestamp, fmt.Sprint(now.Unix()))
	req.Header.Set(standardwebhooks.HeaderWebhookSignature, sig)
	resp, err := heartbeatClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("the watchdog answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp.StatusCode, nil
}

// heartbeatLoop sends a heartbeat every heartbeatEvery while one is set up.
func (s *Service) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := s.loadOffsite()
		if err != nil || st.Watchdog == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.beat(cctx, st.Watchdog)
		cancel()
	}
}

// offsiteRoutes are the off-site backup's and the watchdog's routes: the
// machine's owner only (they name secrets, by reference).
func (s *Service) offsiteRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/backup", core.Local, "local: the machine's owner; the off-site home backup (target, secret references, last set, failures) and the watchdog heartbeat", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.offsiteView()
	}))
	r.HandleFunc("PUT /v0/backup", core.Local, "local: the machine's owner; sets the off-site home backup after checking the target answers (run_now runs it)", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.OffsiteRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		v, err := s.setOffsite(r.Context(), req)
		if err != nil || !req.RunNow {
			return v, err
		}
		if _, err := s.runOffsite(r.Context()); err != nil {
			return nil, err
		}
		return s.offsiteView()
	}))
	r.HandleFunc("DELETE /v0/backup", core.Local, "local: the machine's owner; turns the off-site backup off (its sets stay)", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.offsiteOff()
	}))
	r.HandleFunc("POST /v0/backup/run", core.Local, "local: the machine's owner; takes and sends a set now", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		res, err := s.runOffsite(r.Context())
		if err != nil && !res.Ran && res.Error == "" {
			return nil, err
		}
		if err != nil {
			return nil, api.Errorf(api.CodeUnavailable, "%s", res.Error)
		}
		return res, nil
	}))
	r.HandleFunc("GET /v0/backup/sets", core.Local, "local: the machine's owner; the sets on the off-site target", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.offsiteSets(r.Context())
	}))
	r.HandleFunc("PUT /v0/backup/watchdog", core.Local, "local: the machine's owner; sets the watchdog heartbeat and sends one now", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.WatchdogRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.setWatchdog(r.Context(), req)
	}))
	r.HandleFunc("DELETE /v0/backup/watchdog", core.Local, "local: the machine's owner; stops the heartbeat", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.watchdogOff()
	}))
}
