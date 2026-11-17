package main

import (
	"reflect"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// The Go types each stable route reads and answers with (API.md rows
// labelled `stable`; TestRoutesAreAPIStableRows keeps the two lists equal).
// A route's 2xx body is the value its handler passes to api.WriteJSON.

type route struct {
	method, path string
	in           reflect.Type // the request body; nil: none
	// ok is the 2xx bodies by status: one type, or several a client tells
	// apart by their fields.
	ok     map[int][]reflect.Type
	stream bool   // 200 is also (GET /v0/events: only) text/event-stream
	note   string // what the types alone do not say
	// pending says when a 202 answers.
	pending string
	// refuse are documented refusals by status: the error envelope, its
	// data's Go type (nil: none or untyped) and when.
	refuse map[int]refusal
}

type refusal struct {
	data []reflect.Type // one, or several told apart by their fields
	why  string
}

func (r route) with(status int, why string, data ...reflect.Type) route {
	if r.refuse == nil {
		r.refuse = map[int]refusal{}
	}
	r.refuse[status] = refusal{data, why}
	return r
}

// queued is a lease write through a team replica while the authority is
// away or slow: 202 with the outbox entry (API.md, team replicas).
func (r route) queued() route {
	r.ok[202] = []reflect.Type{of[api.QueuedResponse]()}
	r.pending = "Queued: through a team replica while the project's authority is away or slow; the write runs when it answers"
	return r
}

// opPending is a session operation's 202: SessionOpResult with pending.
const opPending = "Pending: the agent's adapter (10 s) or the session's machine (15 s) has not answered yet; its events will say"

const rateLimited = "rate_limited: a send limit (SPEC 9 \"Send limits\"); nothing was sent. Wait retry_after seconds. " +
	"data says the limit broken"

const heldLease = "conflict: with data, someone else holds it and data is their lease, the holder redacted; " +
	"without, the item or write cannot be claimed now (the message says why)"

// askConflict and askForbidden are an answer's refusals (choose, and a key
// approver's signed answer).
const (
	askConflict = "conflict: data a Decision: already answered differently, the answer that stands; data a Refusal, " +
		"reason card-changed (card_sha256 is not the ask's card: read it again), held, joined-action-ask, withdrawn, " +
		"default-stands, or confirm-word (send the choice's confirm word as confirm); none: a forwarded write was lost, or the signed release of a held card was refused: read the ask again"
	askForbidden = "forbidden: with data, branch on data.reason (signed-only, member-too-old); without, not yours to " +
		"answer; docs/clients.md section 5"
)

func of[T any]() reflect.Type { return reflect.TypeFor[T]() }

func r200(method, path string, in, out reflect.Type) route {
	return route{method: method, path: path, in: in, ok: map[int][]reflect.Type{200: {out}}}
}

// opResult is a structured session operation's answer: 202 while it is
// pending.
func opResult(path string, in reflect.Type) route {
	r := r200("POST", path, in, of[orchapi.SessionOpResult]())
	r.ok[202], r.pending = []reflect.Type{of[orchapi.SessionOpResult]()}, opPending
	return r
}

var routes = []route{
	{method: "GET", path: "/v0/health", ok: map[int][]reflect.Type{200: {of[orchapi.Health]()}},
		note: "Remote and unauthenticated callers get only ok, protocol, version, commit and machine."},
	r200("GET", "/v0/whoami", nil, of[api.Whoami]()),
	{method: "GET", path: "/v0/events", ok: map[int][]reflect.Type{200: {of[api.Event]()}}, stream: true,
		note: "Server-sent events: each frame's data is one Event; its data is the components schema named by its type (dev.zm.*)."},
	r200("GET", "/v0/projects", nil, of[api.ProjectList]()),
	r200("GET", "/v0/projects/resolve", nil, of[api.Resolution]()),
	r200("GET", "/v0/projects/{project}", nil, of[api.Project]()),
	r200("GET", "/v0/projects/{project}/areas", nil, of[api.AreaList]()),
	r200("GET", "/v0/tokens", nil, of[api.TokenList]()),
	r200("POST", "/v0/tokens", of[api.MintTokenRequest](), of[api.MintTokenResponse]()),
	r200("DELETE", "/v0/tokens/{id}", nil, of[api.Token]()),

	r200("POST", "/v0/mail", of[orchapi.SendMailRequest](), of[orchapi.Mail]()).with(429, rateLimited, of[orchapi.SendLimit]()),
	r200("GET", "/v0/mail", nil, of[orchapi.MailList]()),
	r200("GET", "/v0/mail/inbox", nil, of[orchapi.MailList]()),
	r200("GET", "/v0/mail/sent", nil, of[orchapi.MailList]()),
	r200("GET", "/v0/mail/{id}", nil, of[orchapi.Mail]()),
	r200("POST", "/v0/mail/{id}/ack", nil, of[orchapi.AckResponse]()),
	r200("POST", "/v0/mail/{id}/reply", of[orchapi.ReplyRequest](), of[orchapi.Mail]()).with(429, rateLimited, of[orchapi.SendLimit]()),
	r200("GET", "/v0/mail/{id}/thread", nil, of[orchapi.MailList]()),

	r200("GET", "/v0/work", nil, of[orchapi.WorkList]()),
	r200("POST", "/v0/work", of[orchapi.CreateWorkRequest](), of[orchapi.WorkItem]()),
	r200("GET", "/v0/work/inflight", nil, of[orchapi.InflightList]()),
	r200("GET", "/v0/work/{id}", nil, of[orchapi.WorkItem]()),
	r200("GET", "/v0/work/{id}/holder", nil, of[orchapi.HolderResponse]()),
	r200("GET", "/v0/work/{id}/comments", nil, of[orchapi.CommentList]()),
	r200("POST", "/v0/work/{id}/claim", of[orchapi.ClaimRequest](), of[orchapi.ClaimResponse]()).queued().with(409, heldLease, of[orchapi.Lease]()),
	r200("POST", "/v0/work/{id}/release", of[orchapi.ReleaseRequest](), of[orchapi.ReleaseResponse]()).queued(),
	r200("POST", "/v0/work/{id}/comment", of[orchapi.CommentRequest](), of[orchapi.Comment]()),
	r200("POST", "/v0/work/{id}/status", of[orchapi.StatusRequest](), of[orchapi.WorkItem]()),
	r200("GET", "/v0/leases", nil, of[orchapi.LeaseList]()),
	r200("POST", "/v0/leases", of[orchapi.ClaimRequest](), of[orchapi.ClaimResponse]()).queued().with(409, heldLease, of[orchapi.Lease]()),
	r200("DELETE", "/v0/leases/{key}", of[orchapi.ReleaseRequest](), of[orchapi.ReleaseResponse]()).queued(),

	{method: "POST", path: "/v0/sessions", in: of[orchapi.SpawnRequest](), ok: map[int][]reflect.Type{201: {of[orchapi.Session]()}}},
	r200("GET", "/v0/sessions", nil, of[orchapi.SessionList]()),
	r200("GET", "/v0/machines", nil, of[api.MachineList]()),
	r200("GET", "/v0/sessions/{id}", nil, of[orchapi.Session]()),
	{method: "POST", path: "/v0/sessions/{id}/stop", in: of[orchapi.StopRequest](),
		ok:      map[int][]reflect.Type{200: {of[orchapi.Session](), of[orchapi.SessionOpResult]()}, 202: {of[orchapi.SessionOpResult]()}},
		pending: opPending,
		note:    "This machine's socket, for its own session: the Session. Otherwise the relayed operation's result."},
	{method: "GET", path: "/v0/sessions/{id}/events", ok: map[int][]reflect.Type{200: {of[orchapi.SessionEvents]()}}, stream: true,
		note: "Long-poll JSON, or server-sent events with Accept: text/event-stream (each frame's data is one event). " + agentLine},
	opResult("/v0/sessions/{id}/turn", of[orchapi.SessionTurnRequest]()),
	opResult("/v0/sessions/{id}/interrupt", of[orchapi.SessionInterruptRequest]()),
	opResult("/v0/sessions/{id}/answer", of[orchapi.SessionAnswerRequest]()),
	r200("GET", "/v0/people", nil, of[api.PeopleList]()),

	r200("POST", "/v0/asks", of[orchapi.NewAskRequest](), of[orchapi.Ask]()).
		with(409, "conflict: this was already asked; data is the earlier ask when you may read it", of[orchapi.Ask]()),
	r200("GET", "/v0/asks", nil, of[orchapi.AskList]()),
	r200("GET", "/v0/asks/{id}", nil, of[orchapi.Ask]()),
	route{method: "POST", path: "/v0/asks/{id}/answer", in: of[orchapi.SignedKeyAnswer](),
		ok:   map[int][]reflect.Type{200: {of[orchapi.Decision](), of[orchapi.Ask]()}},
		note: "The Decision; the Ask when the signed answer releases a held card."}.
		with(403, askForbidden, of[orchapi.Refusal]()).with(409, askConflict, of[orchapi.Decision](), of[orchapi.Refusal]()),
	r200("POST", "/v0/asks/{id}/choose", of[orchapi.DecideRequest](), of[orchapi.Decision]()).
		with(403, askForbidden, of[orchapi.Refusal]()).with(409, askConflict, of[orchapi.Decision](), of[orchapi.Refusal]()),
	r200("POST", "/v0/asks/{id}/withdraw", nil, of[orchapi.Ask]()),
	r200("GET", "/v0/decisions", nil, of[orchapi.Inbox]()),
	r200("GET", "/v0/receipts/key", nil, of[orchapi.ReceiptKey]()),
	r200("GET", "/v0/receipts/verify", nil, of[orchapi.ReceiptCheck]()),
}

// event is one dev.zm.* type and its data's Go type (nil: untyped, why says
// so). TestEventsCoverEveryType keeps this list equal to the dev.zm.*
// constants in orchapi, kit/api and computeapi.
type event struct {
	typ  string
	data reflect.Type
	why  string // for an untyped or left-out event
}

var events = []event{
	{api.EventDaemonStarted, of[orchapi.Health](), ""},
	{api.EventDaemonStopping, of[DaemonStopping](), "null when no request asked the stop"},
	{api.EventDaemonSwapped, of[api.SwapRecord](), ""},
	{api.EventDaemonUpdateChecked, of[api.SwapRecord](), ""},
	{api.EventDaemonSelfCheck, of[api.SelfCheck](), ""},
	{api.EventProjectCreated, of[api.Project](), ""},
	{api.EventProjectUpdated, of[api.Project](), ""},
	{api.EventAreaAdded, of[api.Area](), ""},
	{api.EventAreaUpdated, of[api.Area](), ""},
	{api.EventGranted, of[api.Grant](), ""},
	{api.EventRevoked, of[api.Grant](), ""},
	{api.EventTokenMinted, of[api.Token](), ""},
	{api.EventTokenRevoked, of[api.Token](), ""},
	{api.EventTailnetUp, of[api.TailnetStatus](), ""},
	{api.EventTailnetDown, of[api.TailnetStatus](), ""},
	{api.EventTeamJoined, of[api.TeamJoined](), ""},

	{orchapi.EventMailSent, of[orchapi.Mail](), ""},
	{orchapi.EventMailAcked, of[orchapi.Mail](), ""},

	{orchapi.EventMailImported, of[orchapi.Mail](), ""},
	{orchapi.EventWorkCreated, of[orchapi.WorkItem](), ""},
	{orchapi.EventWorkStatusChanged, of[orchapi.WorkItem](), ""},
	{orchapi.EventWorkLinked, of[orchapi.WorkItem](), ""},
	{orchapi.EventWorkChanged, of[orchapi.WorkItem](), ""},
	{orchapi.EventWorkCommented, of[orchapi.Comment](), ""},
	{orchapi.EventWorkClaimed, of[orchapi.Lease](), ""},
	{orchapi.EventWorkReleased, of[orchapi.Lease](), ""},
	{orchapi.EventWorkLeaseExpired, of[orchapi.Lease](), ""},
	{orchapi.EventWorkMoved, of[orchapi.WorkMoved](), ""},
	{orchapi.EventWorkRefreshed, of[orchapi.ProjectHint](), ""},
	{orchapi.EventWorkImported, of[orchapi.WorkImported](), "deprecated: nothing publishes it; older logs still carry it"},
	{orchapi.EventGitHubPRReady, of[orchapi.GitHubPREvent](), ""},
	{orchapi.EventGitHubPRHeadMoved, of[orchapi.GitHubPREvent](), ""},
	{orchapi.EventGitHubPRClosed, of[orchapi.GitHubPREvent](), ""},
	{orchapi.EventGitHubPRChecksSettled, of[orchapi.GitHubPREvent](), ""},
	{orchapi.EventSessionStarted, of[orchapi.Session](), ""},
	{orchapi.EventSessionRegistered, of[orchapi.Session](), ""},
	{orchapi.EventSessionStateChanged, of[orchapi.Session](), ""},
	{orchapi.EventSessionUpdated, of[orchapi.Session](), ""},
	{orchapi.EventSessionEnded, of[orchapi.Session](), ""},
	{orchapi.EventSessionLeft, of[orchapi.SessionLeft](), ""},
	{orchapi.EventSessionAdmissionRefused, of[orchapi.Admission](), ""},
	{orchapi.EventApproverAdded, of[orchapi.Approver](), ""},
	{orchapi.EventApproverRemoved, of[orchapi.Approver](), ""},
	{orchapi.EventAskCreated, of[orchapi.Ask](), ""},
	{orchapi.EventDefaultApplied, of[orchapi.Ask](), ""},
	{orchapi.EventDefaultUndone, of[orchapi.Ask](), ""},
	{orchapi.EventAskRepeated, of[orchapi.Ask](), ""},
	{orchapi.EventDecisionRecorded, of[orchapi.Decision](), ""},
	{orchapi.EventIssueCreated, of[orchapi.IssueEvent](), ""},
	{orchapi.EventIssueUpdated, of[orchapi.IssueEvent](), ""},
	{orchapi.EventIssueCommented, of[orchapi.IssueEvent](), ""},
	{orchapi.EventIssueCommentEdited, of[orchapi.IssueEvent](), ""},
	{orchapi.EventIssueCommentDeleted, of[orchapi.IssueEvent](), ""},
	{orchapi.EventIssueCommentRedacted, of[orchapi.IssueEvent](), ""},
	{orchapi.EventIssueMoved, of[orchapi.IssueEvent](), ""},
	{orchapi.EventAutomationCreated, of[orchapi.AutomationEvent](), ""},
	{orchapi.EventAutomationUpdated, of[orchapi.AutomationEvent](), ""},
	{orchapi.EventAutomationDeleted, of[orchapi.AutomationEvent](), ""},
	{orchapi.EventAutomationHealth, of[orchapi.AutomationEvent](), ""},
	{orchapi.EventSummaryCreated, of[orchapi.SummaryCreated](), ""},
	{orchapi.EventSummaryScheduleChanged, of[orchapi.ProjectHint](), ""},
	{orchapi.EventIssueImported, nil, "deprecated: nothing publishes it; older logs still carry it, its data untyped"},
	{orchapi.EventWebhookPing, of[orchapi.WebhookPing](), "sent only to the webhook being tested, never on the stream"},
	{orchapi.EventWebhookDisabled, of[orchapi.Webhook](), ""},

	{computeapi.EventRunTargetExpired, of[computeapi.RunTargetLease](), ""},
	{computeapi.EventSealingChanged, of[computeapi.SealingChanged](), ""},
	{computeapi.EventSecretGranted, of[computeapi.SecretEvent](), ""},
	{computeapi.EventSecretDelivered, of[computeapi.SecretEvent](), ""},
	{computeapi.EventSecretRefused, of[computeapi.SecretEvent](), ""},
	{computeapi.EventSecretRenewed, of[computeapi.SecretEvent](), ""},
	{computeapi.EventSecretRevoked, of[computeapi.SecretEvent](), ""},
	{computeapi.EventSecretGrantAdded, of[computeapi.SecretGrant](), ""},
	{computeapi.EventSecretGrantRemoved, of[computeapi.SecretGrant](), ""},
	{computeapi.EventSecretGrantDefault, of[computeapi.SecretGrant](), ""},
}

// DaemonStopping is dev.zm.daemon.stopping's data: who asked the daemon to
// stop. Its Go type is unexported in the Potluck module (kit/server.stopNote),
// so it is said here; TestDaemonStoppingMatchesPotluck keeps the two equal.
type DaemonStopping struct {
	By    string `json:"by"`
	What  string `json:"what,omitempty"` // why: restart, upgrade
	Force bool   `json:"force,omitempty"`
}

// nullData are the events whose data may be null (no payload to say).
var nullData = map[string]bool{api.EventDaemonStopping: true}

// notEvents are dev.zm.* constants that never reach a client as an event.
var notEvents = map[string]string{
	"dev.zm.sync.gone": "a team replica's sync tombstone (kit/events), not on GET /v0/events",
}

// nameOverrides name a component when two Go types share a name.
var nameOverrides = map[reflect.Type]string{}

// customType describes a type with its own MarshalJSON: adjust edits the
// schema made from its fields.
type customType struct {
	adjust func(schema)
}

var customTypes = map[reflect.Type]customType{
	// Its fields, then each of Parts (json:"-") at the top level: modules'
	// sections, most of which orchapi.Health names (any other is an extra
	// property, which no schema here forbids).
	of[api.Health](): {},
	// The base's fields, then each part that is set: never null.
	of[orchapi.Health](): {adjust: func(s schema) {
		props, _ := s["properties"].(schema)
		ht := of[orchapi.Health]()
		var keep []string
		parts := map[string]bool{}
		for i := range ht.NumField() {
			if f := ht.Field(i); !f.Anonymous {
				k, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				parts[k] = true
				if p, ok := props[k].(schema); ok {
					if alts, ok := p["anyOf"].([]schema); ok {
						props[k] = alts[0]
					}
				}
			}
		}
		req, _ := s["required"].([]string)
		for _, k := range req {
			if !parts[k] {
				keep = append(keep, k)
			}
		}
		if s["required"] = keep; len(keep) == 0 {
			delete(s, "required")
		}
	}},
}

// agentLine says what a structured session's event is: a line its agent's
// adapter wrote, passed through as read (a third-party adapter's too), so
// the schema allows any JSON. zm's own adapters write an AgentEvent
// (components.schemas.AgentEventData).
const agentLine = "Each event is a line the session's agent adapter wrote, passed through as read: any JSON. " +
	"zm's own adapters write an AgentEvent (components.schemas.AgentEventData)."
