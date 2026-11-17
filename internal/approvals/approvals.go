// Package approvals is decisions on asks (SPEC section 16).
// Owned by the approvals builder.
//
// The project's owner or admin decides from their own machine (owner.go): its
// socket, or a member machine's socket forwarded to the authority. Every
// process of the owner's OS user reaches that socket, agents too; a paired
// approver is the opt-in for when that matters. An approver's right is bound
// to its tailnet node (WhoIs stable node ID + node key), never to a token, a
// name or the Tailscale login. Its routes are core.Device: the router refuses
// the socket and tokens before a handler runs; handlers then look the node up.
//
//   - Pairing: `zm approver add <name>` (local) reserves the name and
//     issues a one-time code. The device opens GET /v0/approvals on the
//     daemon's tailnet listener and enters it. Refused from this machine
//     (the daemon's own node or the host's Tailscale address), from tagged
//     nodes and other zm daemons, and from a node already bound. The first
//     approver is bootstrapped this way, once per daemon (BootstrapFile);
//     every later one must be confirmed on an existing approver's device.
//
//   - Asks: POST /v0/asks sends a needs=human mail and records its subject
//     {repo, pr, head_sha}. An approver answers it once (POST
//     /v0/asks/{id}/decide); a conflicting second answer is refused.
//
//   - Receipts: every decision is signed with the daemon's ed25519 key over
//     {project, ask, subject, decision, approver, node, time}; the public key
//     is published (GET /v0/receipts/key) so a CI gate can verify offline.
//
//   - Note cards: an ask's subject may also be a work item or a plain
//     question, carrying a card a person reads in one sitting (card.go). Its
//     answer lands on the work item whoever asked (writeback.go), and GET
//     /v0/decisions is the one inbox (inbox.go).
//
//   - Key approvers: a device that shares a machine with agents holds a key
//     instead of a node, and another device must confirm it (keys.go).
//
// Everything fails closed: unknown node, expired code, a node key that
// changed, an unverifiable receipt.
package approvals

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var approvalsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0001_init", Script: `
CREATE TABLE approvers (
	name         TEXT PRIMARY KEY,
	skeleton     TEXT NOT NULL UNIQUE,
	state        TEXT NOT NULL,
	code_hash    TEXT UNIQUE,
	code_expires INTEGER,
	node_id      TEXT UNIQUE,
	node_key     TEXT NOT NULL DEFAULT '',
	node_name    TEXT NOT NULL DEFAULT '',
	login        TEXT NOT NULL DEFAULT '',
	added_by     TEXT NOT NULL,
	confirmed_by TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	paired       INTEGER
);
CREATE TABLE asks (
	id       TEXT PRIMARY KEY,
	project  TEXT NOT NULL,
	area     TEXT NOT NULL DEFAULT '',
	repo     TEXT NOT NULL,
	pr       INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	options  TEXT NOT NULL,
	body     TEXT NOT NULL,
	asked_by TEXT NOT NULL,
	to_addr  TEXT NOT NULL,
	created  INTEGER NOT NULL
);
CREATE INDEX asks_project ON asks (project, area);
CREATE TABLE decisions (
	ask      TEXT PRIMARY KEY REFERENCES asks(id),
	decision TEXT NOT NULL,
	approver TEXT NOT NULL,
	node_id  TEXT NOT NULL,
	time     INTEGER NOT NULL,
	receipt  TEXT NOT NULL
);`},
	// Note cards, non-PR subjects and write-back. dedupe is empty when an ask
	// may repeat. A decision made before this migration already went where it
	// could, so it counts as recorded.
	{Kind: store.KindRegistry, Name: "approvals/0002_cards", Script: `
ALTER TABLE asks ADD COLUMN work TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN question TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN card TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN card_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN priority INTEGER NOT NULL DEFAULT 2;
ALTER TABLE asks ADD COLUMN topic TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN for_group TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN by_ms INTEGER;
ALTER TABLE asks ADD COLUMN withdrawn INTEGER;
ALTER TABLE asks ADD COLUMN dedupe TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX asks_once ON asks (project, dedupe) WHERE dedupe != '';
CREATE INDEX asks_created ON asks (project, created);
ALTER TABLE decisions ADD COLUMN text TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN answered_as TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN device TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN recorded INTEGER;
ALTER TABLE decisions ADD COLUMN write_error TEXT NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE decisions ADD COLUMN next_try INTEGER;
UPDATE decisions SET recorded = time;
CREATE INDEX decisions_pending ON decisions (next_try) WHERE recorded IS NULL;`},
}

var effectsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0003_effects", Script: `
ALTER TABLE decisions ADD COLUMN mailed INTEGER;
ALTER TABLE decisions ADD COLUMN mail_error TEXT NOT NULL DEFAULT '';
CREATE TABLE decision_effects (
 ask TEXT NOT NULL REFERENCES decisions(ask), kind TEXT NOT NULL, id TEXT NOT NULL UNIQUE,
 target TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, payload_sha TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(ask,kind)
);
UPDATE decisions SET next_try=NULL,write_error='unknown: pre-effects delivery outcome requires reconciliation',mail_error='unknown: pre-effects notification outcome requires reconciliation' WHERE recorded IS NULL;
CREATE INDEX asks_work ON asks(project,work,area) WHERE work != '';
DROP INDEX asks_once;
CREATE UNIQUE INDEX asks_once ON asks (project,area,dedupe) WHERE dedupe != '';
`},
}

// Tunables (tests change them).
var (
	// CodeTTL is how long a pairing code works.
	CodeTTL = 10 * time.Minute
	// maxPairFailures wrong codes per node (and 4x that overall) per CodeTTL.
	maxPairFailures = 5
)

// Service implements the approvals routes.
type Service struct {
	d *core.Deps
	// Publisher posts decisions to GitHub (default: GitHubBot with the
	// project's bot token, if any). Tests replace it.
	Publisher Publisher
	// The card judge (cardjudge.go): the model it asks (SetJudge; nil is
	// the configured provider CLI), its call slots, one held-ask sweep at a
	// time, and holdMu ordering a held ask's release or rejection against
	// its withdrawal.
	judgeMu   sync.Mutex
	judgeC    judge.Caller
	judging   chan struct{}
	rejudging atomic.Bool
	// defaulting: one due-default sweep at a time (it asks the irreversible
	// judge, so it runs off the retry ticker).
	defaulting atomic.Bool
	holdMu     sync.Mutex
	now        func() time.Time

	// groupMu orders a join's insert against an owner's group answer, so a
	// join either lands before the answer reaches its group or sees it.
	groupMu sync.Mutex
	// repeatLocks orders asks with one repeat key (ask_repeat.go), so two
	// first failures at once file one card.
	repeatLocks [64]sync.Mutex
	legacyMu    sync.Mutex
	legacyDone  bool
	deliveryMu  sync.Mutex
	keyMu       sync.Mutex
	key         ed25519.PrivateKey

	failMu sync.Mutex
	fails  map[string][]time.Time // node ID ("" = all) -> wrong pairing codes

	nonces nonceBook // single-use nonces of signed key statements (keybind.go)

	wg sync.WaitGroup // background publishes
	// ownAccount is the owners brick's verified LinkedAccount that
	// LinkedPerson wraps (person_links.go).
	ownAccount atomic.Pointer[func(ctx context.Context, identity string) (string, string)]
	// lanes starts fresh lanes (relane.go) and, in tests, actions; nil is
	// the team module.
	lanes laneHost
	// actionStarts holds one lock per ask whose action is being started.
	actionStartsMu sync.Mutex
	actionStarts   map[string]*startLock
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
)

// Migrations are this module's tables (core.Deps.AddMigrations); New adds them.
var Migrations = slices.Concat(approvalsMigrations, effectsMigrations, approversMigrations, notDoneMigrations, ownerMigrations, askActionsMigrations, askAttachMigrations, aliasesMigrations, cardjudgeMigrations, routingMigrations, repeatsMigrations, defaultsMigrations, peopleMigrations, askOriginMigrations, permitMigrations, personLinksMigrations, repeatMigrations, relaneMigrations, rulingMigrations, keyBindMigrations, notTheirsMigrations, actionRetryMigrations, stepMigrations)

// New returns the approvals service.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: func() time.Time { return time.Now().UTC() }, fails: map[string][]time.Time{}, judging: make(chan struct{}, judgeSlots),
		Publisher: &GitHubBot{Dir: filepath.Join(d.Config.Home, "approvals", "github")}}
	d.AddScrubber(s.scrub)
	d.AddHealthPart("card_judge", s.judgeHealth)
	orchcore.AddOwedSource(d, s.owedSource())
	return s
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Close waits for background publishes.
func (s *Service) Close() error {
	s.wg.Wait()
	return nil
}

// Routes registers /v0/approvers, /v0/approvals, /v0/asks, /v0/decisions,
// /v0/aliases, /v0/run-targets, /v0/steps, /v0/bundles and /v0/receipts.
func (s *Service) Routes(r *core.Router) {
	r = r.For("approvals")
	r.HandleFunc("POST /v0/approvers", core.Local, "local: reserve an exact, lookalike-free name; one-time pairing code", s.handleAdd)
	r.HandleFunc("POST /v0/approvers/self", core.Authed, "a human identity for its own phone only, never a session; the phone waits for an existing approver's confirm (never bootstraps)", s.handleAddSelf)
	r.HandleFunc("GET /v0/approvers", core.Local, "local", s.handleList)
	r.HandleFunc("GET /v0/approvers/self", core.Authed, "a human identity, never a session: only its own person's approvers (--for it; the machine owner's also those for nobody)", s.handleListSelf)
	r.HandleFunc("DELETE /v0/approvers/{name}", core.Local, "local: exact name only; takes effect at once", s.handleRemove)
	r.HandleFunc("POST /v0/approvers/{name}/confirm", core.Device, "device: the caller's node is an active approver; the named one is paired, unconfirmed", s.handleConfirm)
	r.HandleFunc("GET /v0/approvals", core.Device, "device: pairing page, or the approver's open asks", s.handlePage)
	for _, f := range []string{"app.js", "zm-decisions.js", "sw.js", "manifest.webmanifest", "icon.png"} {
		r.HandleFunc("GET /v0/approvals/"+f, core.Device, "device: the Decisions page's static file (installable to a home screen)", s.handleAsset)
	}
	r.HandleFunc("GET /v0/approvals/device", core.Device, "device: the caller's own pairing; open asks for active approvers only", s.handleDevice)
	r.HandleFunc("GET /v0/approvals/push-key", core.Device, "device: an active approver's node; the Web Push public key its browser subscribes with", s.handlePushKey)
	r.HandleFunc("POST /v0/approvals/push", core.Device, "device: an active approver's node; phone alerts for that device's person, while it stays active", s.handlePush)
	r.HandleFunc("POST /v0/approvals/push/off", core.Device, "device: an active approver's node; removes that node's own subscription", s.handlePush)
	r.HandleFunc("POST /v0/approvals/got-it", core.Device, "device: an active approver's node; marks read one for-you mail addressed to that device's person (its --for identity, else the machine owner), else not_found", s.handleGotIt)
	r.HandleFunc("POST /v0/approvals/pair", core.Device, "device: one-time code; refused from this machine, tagged nodes, zm nodes, bound nodes", s.handlePair)
	r.HandleFunc("POST /v0/approvals/pair-phone", core.Device, "device: no code; a phone (OS iOS/android) signed in to the tailnet as this daemon's own login, refused as pair is", s.handlePairPhone)
	r.HandleFunc("GET /v0/approvals/key", core.Authed, "none: the approver paired to ?key=p256:<16 hex>, else not_found (a member's way to see its own key's state)", s.handleKeyState)
	r.HandleFunc("DELETE /v0/approvals/key", core.Authed, "none from the caller: the body's signature by that key (zm-unbind-key/1, single-use nonce); a key bound by a sign-in only from its own node; a member's call must carry origin_checked (else 403 member-too-old)", s.handleUnbindKey)
	r.HandleFunc("POST /v0/approvals/pair-key", core.Authed, "one-time code and proof of the key; stays unconfirmed until an approver on another device confirms", s.handlePairKey)
	r.HandleFunc("POST /v0/asks", core.Authed, "write on the resolved project/area (WriteTarget), and on the work item; sends needs=human mail", s.requireReceiptKey(s.handleAsk))
	r.HandleFunc("GET /v0/asks", core.Authed, "read: ReadScope, and operate on the area or a party to the ask; admits an answer-only token", s.requireReceiptKey(s.handleAsks))
	r.HandleFunc("GET /v0/asks/{id}", core.Authed, "read on the ask's area and (operate or party), else not_found; admits an answer-only token", s.requireReceiptKey(s.handleGetAsk))
	r.HandleFunc("POST /v0/asks/{id}/decide", core.Device, "device: an active approver's node (stable ID + key); one decision per ask", s.handleDecide)
	r.HandleFunc("POST /v0/asks/{id}/choose", core.Authed, "read on the ask, then owner or admin of its project (core.Deps.ProjectAdmin: by scope, or a member machine's node-bound token by its identity's own grant); never a pull request's ask; admits an answer-only token, recorded as surface answer-token, never a choice with a confirm word", s.requireReceiptKey(s.handleOwnerAnswer))
	r.HandleFunc("POST /v0/asks/{id}/answer", core.Authed, "none from the caller: the body's signature by an active key approver whose person owns or administers the ask's project; a key bound by a sign-in only from its own node, while its person stays linked to its account, and a member's call must carry origin_checked (else 403 member-too-old); never a pull request's ask", s.handleKeyAnswer)
	r.HandleFunc("POST /v0/asks/{id}/undo", core.Authed, "read on the ask, then owner or admin of its project (as choose); only an applied default, not yet answered", s.requireReceiptKey(s.handleUndo))
	r.HandleFunc("POST /v0/asks/{id}/device-undo", core.Device, "device: an active approver's node (stable ID + key); only an applied default, not yet answered", s.handleDeviceUndo)
	r.HandleFunc("POST /v0/asks/{id}/action/retry", core.Authed, "read on the ask (else not_found), then its asker or owner or admin of its project (as choose); card_sha256 must be the ask's; only a run or command action that failed before it started, within an hour of the yes, at most 3 times; starts the identical approved action as its approver, checked again", s.requireReceiptKey(s.handleRetryAction))
	r.HandleFunc("POST /v0/asks/{id}/action/confirm", core.Authed, "read on the ask, then owner or admin of its project (as choose); only an action that is done or failed; changes nothing but where the ask is listed", s.requireReceiptKey(s.handleConfirmAction))
	r.HandleFunc("POST /v0/asks/{id}/object", core.Authed, "read on the ask (else not_found), then owner or admin of its project (as choose); only an answered ask; records the owner's objection and does nothing else", s.requireReceiptKey(s.handleObject))
	r.HandleFunc("POST /v0/asks/{id}/step", core.Authed, "read on the ask (else not_found), then only the person it is for (by name or directory person), never an agent session or another person; card_sha256 must be the ask's; only a card with a person's step, approved and not done; records it and mails the asker to run the check, closes nothing", s.requireReceiptKey(s.handleStepDone))
	r.HandleFunc("POST /v0/asks/{id}/check", core.Authed, "read on the ask (else not_found), then its asker or operate on its area (evidence: its asker or a person who owns or administers the project); only a card with a verify check that was approved", s.requireReceiptKey(s.handleCheck))
	r.HandleFunc("POST /v0/asks/{id}/withdraw", core.Authed, "read on the ask, and its asker or operate on its area", s.requireReceiptKey(s.handleWithdraw))
	r.HandleFunc("GET /v0/permits", core.Authed, "operate on the whole resolved project (WriteTarget): the permit kinds, the project's standing permissions and box marks", s.handlePermits)
	r.HandleFunc("GET /v0/permits/card", core.Authed, "none: the frozen card for a kind or a box class, from the daemon's closed list", s.handlePermitCard)
	r.HandleFunc("GET /v0/permits/dryrun", core.Authed, "operate on the whole resolved project (WriteTarget): per kind, cards a permit would have covered and how they were answered", s.handlePermitDryRun)
	r.HandleFunc("POST /v0/permits", core.Authed, "operate on the whole project of the named ask (else not_found); the grant itself is the ask's signed yes by an approver who owns or administers the project now", s.handleGrant)
	r.HandleFunc("POST /v0/permits/{id}/revoke", core.Authed, "operate on the permit's whole project (else not_found), then owner or admin of it (core.Deps.ProjectAdmin); unsigned", s.handleRevoke)
	r.HandleFunc("POST /v0/permits/boxes/unmark", core.Authed, "operate on the whole resolved project, then owner or admin of it (core.Deps.ProjectAdmin); unsigned", s.handleUnmark)
	r.HandleFunc("GET /v0/run-targets", core.Authed, "operate on the resolved project/area (WriteTarget); the project's machines an approved action may run on and whose they are (?person=)", s.handleRunTargets)
	r.HandleFunc("GET /v0/decisions", core.Authed, "ReadScope: asks as GET /v0/asks, work items by read on their area; admits an answer-only token, which gets asks only", s.requireReceiptKey(s.handleInbox))
	s.aliasRoutes(r)
	s.rulingRoutes(r)
	s.stepRoutes(r)
	s.personLinkRoutes(r)
	r.HandleFunc("GET /v0/receipts/key", core.Public, "none: the daemon's public receipt key", s.handleKey)
	r.HandleFunc("GET /v0/receipts/verify", core.Authed, "none: signature and expectations; ?project= must be visible; a person as ?approver= also needs the ask readable; admits an answer-only token", s.handleVerify)
	// An answer-only token (SPEC 16 "Answer-only tokens") reaches these and
	// nothing else: read the project's asks, answer card asks as its owner,
	// verify receipts. The kit refuses it on every other route.
	r.Admit(orchapi.AnswerLimit, "GET /v0/asks", "GET /v0/asks/{id}", "GET /v0/decisions", "POST /v0/asks/{id}/choose", "GET /v0/receipts/verify")
}

// signer returns the receipt key, creating it on first use.
func (s *Service) signer() (ed25519.PrivateKey, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.key == nil {
		k, err := loadOrCreateKey(filepath.Join(s.d.Config.Home, KeyFile))
		if err != nil {
			return nil, err
		}
		s.key = k
	}
	return s.key, nil
}

// PublicKey is the daemon's receipt public key.
func (s *Service) PublicKey() (ed25519.PublicKey, error) {
	k, err := s.signer()
	if err != nil {
		return nil, err
	}
	return k.Public().(ed25519.PublicKey), nil
}

// device is the tailnet caller of a core.Device route, after the request
// checks every device route shares.
func (s *Service) device(r *http.Request) (*core.Remote, error) {
	rm := core.RemoteOf(r.Context())
	if rm == nil || rm.NodeID == "" || rm.NodeKey == "" { // the router already refused this
		return nil, api.Errorf(api.CodeUnauthenticated, "unknown tailnet device")
	}
	if !s.ownHost(r.Context(), r.Host) {
		// DNS rebinding: a page on another origin that resolves to this
		// node would otherwise talk to it from the device's browser.
		return nil, api.Errorf(api.CodeForbidden, "open the approvals page by this daemon's tailnet name")
	}
	if r.Method != http.MethodGet {
		// Cross-site forms cannot set this header or a JSON content type
		// without a CORS preflight, which this daemon never allows.
		if api.GetHeader(r.Header, api.HeaderProtocol) == "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			return nil, api.Errorf(api.CodeForbidden, "approvals take JSON with the %s header (use the approvals page)", api.HeaderNew(api.HeaderProtocol))
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			return nil, api.Errorf(api.CodeForbidden, "cross-site request refused")
		}
	}
	return rm, nil
}

// ownHost reports whether a request's Host names this daemon on the
// tailnet, exactly (core.OwnTailnetHost): potluck-<machine>, zm-<machine> or the configured node
// name, the node's MagicDNS name, or a Tailscale IP. A prefix is not enough:
// zm-mini.attacker.example can be rebound to the node's address.
func (s *Service) ownHost(ctx context.Context, hostport string) bool {
	return s.d.OwnTailnetHost(ctx, hostport)
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, v)
}

func ms(t time.Time) int64 { return store.Millis(t) }

// Ask surfaces never advertise an empty authority identity or generate a fallback key.
func (s *Service) requireReceiptKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.migrateLegacySlots(r.Context()); err != nil {
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "legacy decision slots unavailable"))
			return
		}
		if _, err := s.PublicKey(); err != nil {
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "authority receipt key unavailable"))
			return
		}
		next(w, r)
	}
}
