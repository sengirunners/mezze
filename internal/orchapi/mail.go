package orchapi

import (
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Needs says who must act on a mail.
type Needs string

const (
	NeedsAny   Needs = "any"   // default
	NeedsAgent Needs = "agent" // a human recipient is refused
	NeedsHuman Needs = "human" // a decision only a person can make; only a human may ack it
)

// Mail kinds. "announce" is the only kind allowed to "all".
const (
	MailKindNote     = "note"
	MailKindAnnounce = "announce"
	// MailKindNotice is zm telling a session something about itself: it shows
	// at the next prompt, wakes nobody, is never copied to a group mailbox and
	// takes no reply. Only the daemon sends one.
	MailKindNotice = "notice"
	// MailKindNudge is a rare, urgent note to a person (human:<name> or owners):
	// the one agent mail people's team chat shows.
	MailKindNudge = "nudge"
	// MailKindReport is mail for a person that is not a decision (SPEC 9
	// "Reports"): an agent's note that would reach a person is filed as one.
	// It is stored and readable (GET /v0/mail/inbox?box=reports) but never
	// in an inbox, Decisions, a push or the notice count: people take
	// decisions only (zm ask new).
	MailKindReport = "report"
)

// ReportNote is what a sender is told when its mail to a person was filed
// as a report.
const ReportNote = "people take decisions only: use zm ask new. This mail was filed as a report (zm inbox --reports), not delivered to an inbox"

// InboxBoxReports is GET /v0/mail/inbox?box=reports.
const InboxBoxReports = "reports"

// MaxNoticeLen bounds the one-line notice hooks print on every prompt
// (part of the agent-facing byte budget, internal/budget).
const MaxNoticeLen = 160

// MailAudienceParticipants restricts mail to exact authenticated participants.
const MailAudienceParticipants = "participants"

// MailOrigin is a source claim, never authenticated native authorship.
type MailOrigin struct {
	System     string    `json:"system"`
	Instance   string    `json:"instance"`
	ID         string    `json:"id"`
	ThreadID   string    `json:"thread_id,omitempty"`
	ParentID   string    `json:"parent_id,omitempty"`
	SourceKind string    `json:"source_kind,omitempty"`
	SourceRef  string    `json:"source_ref,omitempty"`
	Sender     string    `json:"sender"`
	Recipient  string    `json:"recipient"`
	SentAt     time.Time `json:"sent_at"`
	ImportedAt time.Time `json:"imported_at"`
	ImportedBy string    `json:"imported_by"`
	Trust      string    `json:"trust"`
}

type LegacyDelivery struct {
	LastDeliveredAt *time.Time `json:"last_delivered_at,omitempty"`
	DeadAt          *time.Time `json:"dead_at,omitempty"`
	State           string     `json:"state"`
	AckBy           string     `json:"ack_by,omitempty"`
	AckAt           *time.Time `json:"ack_at,omitempty"`
	DigestAt        *time.Time `json:"digest_at,omitempty"`
	DeliveredCount  int        `json:"delivered_count"`
}

// For-you kinds (SPEC 9 "For you"). A decision for a person is an ask
// ("decide", SPEC 16), never mail.
const (
	ForYouKnow   = "know"
	ForYouDigest = "digest"
)

// SendMailRequest is POST /v0/mail. The project comes from resolution
// (Zm-Project, cwd, session); Area defaults to the ref's area, else the
// resolved area.
type SendMailRequest struct {
	Audience        string `json:"audience,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	To              string `json:"to"`
	Body            string `json:"body"`
	Area            string `json:"area,omitempty"`
	Ref             string `json:"ref,omitempty"`
	Kind            string `json:"kind,omitempty"`  // default "note"
	Needs           Needs  `json:"needs,omitempty"` // default "any" ("human" when To is human:<name> or owners)
	ReplyTo         string `json:"reply_to,omitempty"`
	// Digest holds mail to a person for their 08:00 Pacific digest instead
	// of showing it now (SPEC 9 "For you"); only for human:<name>.
	Digest bool `json:"digest,omitempty"`
	// ID is an in-process caller's own mail id (never read from JSON): the
	// approvals module records an ask under it before the mail exists, so
	// the mail is never open without its ask.
	ID string `json:"-"`
}

// ReplyRequest is POST /v0/mail/{id}/reply.
type ReplyRequest struct {
	ClientRequestID string `json:"client_request_id,omitempty"`
	Body            string `json:"body"`
	Needs           Needs  `json:"needs,omitempty"`
	// ID is the reply's id when a team replica's write id names it (never
	// read from JSON; SPEC 17.6).
	ID string `json:"-"`
	// Silent (in process only, never read from JSON) takes the parent
	// without sending anything: the approvals module's notice would only
	// tell a person what they just did themselves.
	Silent bool `json:"-"`
}

// InboxQuery is GET /v0/mail/inbox's query string. Without a project
// (header or ?project=) it is the union of every project the caller sees.
type InboxQuery struct {
	Project string
	All     bool // include acked mail
	Limit   int  // default 50
	Reports bool // ?box=reports: the reports filed for the caller instead of its inbox
}

// MailQuery is GET /v0/mail's query string (any filter may be empty).
type MailQuery struct {
	Project string
	To      string
	From    string
	Ref     string
	Needs   Needs
	Open    bool // only unacked
	Limit   int
	// Since and Until bound when it was sent (inclusive; zero = no bound):
	// a client pages through a long history by moving Until back.
	Since, Until time.Time
}

// AckResponse is POST /v0/mail/{id}/ack. Acked=false with AckedBy set means
// someone else took this group mail first (AckedBy is "someone" when the
// caller cannot see that session).
type AckResponse struct {
	ID      string     `json:"id"`
	Acked   bool       `json:"acked"`
	AckedBy string     `json:"acked_by,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// Notice is GET /v0/mail/notice: one line for a hook, "" when nothing is
// unread or no project resolves. Never an error on the prompt path.
type Notice struct {
	Line   string `json:"line"`
	Unread int    `json:"unread"`
}

// NoticePart is one project's share of a notice (unread counts and the
// senders, redacted for the reader). A team authority sends these to a
// replica with every presence answer, so a hook on the replica builds its
// notice line without a tailnet call (SPEC 17.3).
type NoticePart struct {
	Project  string `json:"project"`
	Slug     string `json:"slug"`
	Unread   int    `json:"unread"`
	Group    int    `json:"group,omitempty"`
	Announce int    `json:"announce,omitempty"`
	Owners   int    `json:"owners,omitempty"`
	// DirectWakeable counts direct mail excluding notices, without overlapping
	// group/quiet subtraction. Nil means an older peer did not report it.
	DirectWakeable *int `json:"direct_wakeable,omitempty"`
	// Quiet counts the notices among Unread: they wake nobody.
	Quiet int      `json:"quiet,omitempty"`
	From  []string `json:"from,omitempty"`
}

// Mail event types. Data is the Mail (acked: the Mail after ack).
const (
	EventMailImported = "dev.zm.mail.imported"
	EventMailSent     = "dev.zm.mail.sent"
	EventMailAcked    = "dev.zm.mail.acked"
)

// ToPerson reports whether mail delivered to recipient with needs reaches a
// person: human:<name>, owners, or needs=human mail (which the machine's
// human reads for its sessions and group mailbox).
func ToPerson(recipient string, needs Needs) bool {
	return recipient == "owners" || strings.HasPrefix(recipient, "human:") || needs == NeedsHuman
}

// AgentSender reports whether from is an agent's address (a session or a
// machine's group, zm's own included), not a person's.
func AgentSender(from string) bool {
	a, err := api.ParseAddress(from)
	return err == nil && (a.Kind == api.KindSession || a.Kind == api.KindMachine)
}
