package orchapi

import (
	"time"
)

// SessionsBeat is a heartbeat's "sessions" part: every live session of the
// project on the replica; the authority marks the replica's other sessions
// in the project ended.
type SessionsBeat struct {
	Sessions []PresenceSession `json:"sessions"`
	Ended    []PresenceSession `json:"ended,omitempty"`
	// Elsewhere are the addresses of the replica's live sessions registered
	// outside this project (another project or none): addresses only, so mail
	// in this project reaches them (SPEC 9 "Across projects"). Absent: the
	// authority leaves the machine's set alone.
	Elsewhere []string `json:"elsewhere"`
}

// T3EnsureRequest is POST /v0/team/machines/{machine}/t3 (SPEC 8 "Mezze app on a
// machine"): start or reuse the Mezze app on machine and mint a one-time pairing
// credential. Project is the team project both machines are in (slug or
// the authority's ULID; on this machine's socket it may be left out when
// one team project is joined or hosted). Scopes are T3 scope names, none
// for T3's standard client scopes.
type T3EnsureRequest struct {
	Project string   `json:"project,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

// T3Environment answers it: the machine's Mezze app server and a pairing
// credential for it, single use and good for 5 minutes. The authority
// hands it out once (the first answer to the requester) and never again.
// A client spends it with T3's connectPairing over each URL in turn,
// checking EnvironmentID.
type T3Environment struct {
	Machine       string    `json:"machine"`
	EnvironmentID string    `json:"environment_id"`
	Version       string    `json:"version"` // the server's version
	Commit        string    `json:"commit"`  // the checkout commit it runs
	Port          int       `json:"port"`
	URLs          []string  `json:"urls"`              // http://<ip>:<port> per private or tailnet IPv4 there
	Started       bool      `json:"started,omitempty"` // this ensure started it (else it already ran)
	Credential    string    `json:"credential"`
	Scopes        []string  `json:"scopes,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// PresenceSession is one session in a heartbeat. Address must be on the
// machine name the replica's token was redeemed for.
type PresenceSession struct {
	Address string `json:"address"`
	Area    string `json:"area,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Title   string `json:"title,omitempty"`
	// Parent is the session that started it (api.Session), on the same machine.
	Parent      string       `json:"parent,omitempty"`
	State       SessionState `json:"state"`
	OutcomeKind OutcomeKind  `json:"outcome_kind,omitempty"`
	EndedAt     *time.Time   `json:"ended_at,omitempty"`
	Note        string       `json:"note,omitempty"`
	Managed     bool         `json:"managed,omitempty"`
	Started     time.Time    `json:"started"`
	// Work is what the replica believes the session holds (keys it claimed
	// through the proxy, and its spawn --work item).
	Work []string `json:"work,omitempty"`
	// Shared and InUse are the session's (SPEC 17.10); Delivered are the
	// ids of prompts the replica typed into it since its last beat.
	Shared    bool          `json:"shared,omitempty"`
	InUse     *SessionInUse `json:"in_use,omitempty"`
	Delivered []string      `json:"delivered,omitempty"`
	// Mode and Capabilities are a shared structured session's (api.Session),
	// so the authority relays its events and operations.
	Mode         string             `json:"mode,omitempty"`
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`
	// Activity, ActivityAt and TermTitle are the session's (api.Session).
	Activity   string     `json:"activity,omitempty"`
	ActivityAt *time.Time `json:"activity_at,omitempty"`
	TermTitle  string     `json:"term_title,omitempty"`
	// Elsewhere: the session is live and in another project's heartbeat to
	// this authority (one row per session there); only its address is sent.
	// This heartbeat does not take it for gone: its row moves when the other
	// project's heartbeat lands, never through ended (SPEC 10).
	Elsewhere bool `json:"elsewhere,omitempty"`
	// Usage attribution (api.Session), live and ended sessions alike.
	Account string         `json:"account,omitempty"`
	For     string         `json:"for,omitempty"`
	JobKind string         `json:"job_kind,omitempty"`
	Tokens  *SessionTokens `json:"tokens,omitempty"`
}

// SessionsBeatAnswer is the "sessions" part of a PresenceResponse: per
// session, what the authority holds for it.
type SessionsBeatAnswer struct {
	Sessions []PresenceAnswer `json:"sessions"`
	// Human is the unread mail of the replica's identity (human:<name>) and
	// of the machine's needs=human mail, for its notice.
	Human []NoticePart `json:"human,omitempty"`
	// Elsewhere answers the request's elsewhere addresses the authority took:
	// each one's unread mail in the project (notice and waiting only).
	Elsewhere []PresenceAnswer `json:"elsewhere,omitempty"`
}

// PresenceAnswer is one session's part of a PresenceResponse.
// IdentityGitHub is the identity the GitHub link acts as (internal/ghlink).
const IdentityGitHub = "zm-github"

type PresenceAnswer struct {
	Address string `json:"address"`
	// Error is why the session was not recorded (an area the replica may
	// not write in answers like a missing one).
	Error string `json:"error,omitempty"`
	// Held is every key of the project with a live lease held by Address.
	Held []string `json:"held"`
	// Finished lists keys the session reported holding whose lease ended
	// because the GitHub link closed the work (a merged PR names it): not a lost lease.
	Finished []string `json:"finished,omitempty"`
	// Notice is the session's unread mail in the project.
	Notice []NoticePart `json:"notice,omitempty"`
	// Waiting counts needs=human mail it sent that is still open.
	Waiting int `json:"waiting,omitempty"`
	// Prompts are the prompts pending for this shared session, oldest first:
	// the replica types each once (by id) and reports it in Delivered.
	Prompts []SessionPrompt `json:"prompts,omitempty"`
	// Stream, for a shared structured session a member reads through the
	// authority: the replica uploads its events after this seq (POST
	// /v0/team/session-events) soon after the answer.
	Stream *int64 `json:"stream,omitempty"`
}

// SessionEventsUpload is POST /v0/team/session-events: a page of a shared
// structured session's events after After, as its machine read them for the
// authority's readers.
type SessionEventsUpload struct {
	Project string        `json:"project"` // the authority's ULID
	Session string        `json:"session"` // its full address, on the token's machine
	After   int64         `json:"after"`
	Page    SessionEvents `json:"page"`
}
