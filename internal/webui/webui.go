// Package webui is phone push (SPEC 16 "Phone alerts"): Web Push to the
// devices paired on the Decisions page. internal/approvals owns that page,
// its pairing and its subscribe routes; it reaches this package only through
// orchcore.DevicePush. This package keeps the subscriptions and the VAPID key,
// follows the event log and delivers. Every alert opens the Decisions page.
package webui

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"sync"

	"github.com/sengirunners/potluck/kit/core"
)

// Service is the push module.
type Service struct {
	d *core.Deps

	pushMu          sync.Mutex
	pushKey         *ecdsa.PrivateKey
	pushTokenMu     sync.Mutex
	pushTokens      map[string]cachedPushToken
	pushHTTP        *http.Client
	pushCtx         context.Context
	pushCancel      context.CancelFunc
	pushDone        chan struct{}
	pushDrainDone   chan struct{}
	pushPersonMu    sync.Mutex
	pushPersonLocks map[string]chan struct{}
	pushWorkMu      sync.Mutex
	pushActive      map[string]bool
	pushWorkers     sync.WaitGroup
}

// New makes the push module.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	d.AddBackupScrubber(scrubPushBackup)
	return &Service{d: d, pushHTTP: publicPushClient()}
}

// Routes registers the push test (zm web test-push).
func (s *Service) Routes(r *core.Router) {
	r = r.For("web")
	r.HandleFunc("POST /v0/web/push/test", core.Local, "local: send a test to the machine owner's subscriptions", s.pushTest)
}

// Start follows the event log for alerts.
func (s *Service) Start(ctx context.Context) error {
	return s.startPush(ctx)
}

// Close stops the follower and the delivery loop.
func (s *Service) Close() error {
	if s.pushCancel != nil {
		s.pushCancel()
		<-s.pushDone
		<-s.pushDrainDone
		s.pushWorkers.Wait()
	}
	return nil
}
