package orchapi

import (
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Not generated: Mail moved here ahead of potluck deleting its copy (nothing
// in potluck uses it), so zm's mail can carry thread, members and batch.
// MailList and DeviceView hold Mail, so they moved with it.

// Mail is one message. It lives in its project's project.db.
type Mail struct {
	Audience       string          `json:"audience,omitempty"`
	Participants   []string        `json:"participants,omitempty"`
	Origin         *MailOrigin     `json:"origin,omitempty"`
	LegacyDelivery *LegacyDelivery `json:"legacy_delivery,omitempty"`
	// Imported mail has no verified return transport in this version.
	ReplyUnavailable string `json:"reply_unavailable,omitempty"`
	ID               string `json:"id"` // ULID; the CLI accepts any unique suffix of 6+ chars
	Project          string `json:"project"`
	Area             string `json:"area,omitempty"`
	From             string `json:"from"` // the sender's address
	To               string `json:"to"`   // what the sender wrote, canonicalised
	// Recipient is where it was delivered: a session address, machine:<host>
	// (the project's group mailbox on that machine), human:<name>, owners, or all.
	Recipient string `json:"recipient"`
	// Via explains the routing: "direct", "group", "holder of bd:x",
	// "creator of local:3", "orphan of <addr>", "announce", "owners".
	Via     string `json:"via"`
	Ref     string `json:"ref,omitempty"` // a work item id in the same project
	Kind    string `json:"kind"`
	Needs   Needs  `json:"needs"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
	// Thread is the id of the thread's first mail (zm thread <id> lists it).
	Thread string `json:"thread,omitempty"`
	// Members is set on each copy of mail to several people (SPEC 9 "Mail
	// to several people"): the sender and every recipient, sorted. Batch is
	// the id of the send's first copy, the same on every copy.
	Members []string   `json:"members,omitempty"`
	Batch   string     `json:"batch,omitempty"`
	Created time.Time  `json:"created"`
	AckedBy string     `json:"acked_by,omitempty"` // "someone" when the caller cannot see the taker
	AckedAt *time.Time `json:"acked_at,omitempty"`
	// ForYou is set on mail addressed to a person (SPEC 9 "For you"): "know"
	// (news; the person's Got it marks it read) or "digest" (held until
	// HeldUntil, the next 08:00 Pacific, then a know item).
	ForYou    string     `json:"for_you,omitempty"`
	HeldUntil *time.Time `json:"held_until,omitempty"`
	// HeldFor names the outside party an open ask's mail waits on (SPEC 16,
	// ask.for): needs=human, but not waiting on anyone here, so it holds no
	// one at needs_human, never pushes and is not counted as unread.
	HeldFor string `json:"held_for,omitempty"`
	// Note is set on a send's answer only: what the daemon did that the
	// sender did not ask for (ReportNote when it filed a report).
	Note string `json:"note,omitempty"`
}

// MailList is the response of every mail listing.
type MailList struct {
	Mail []Mail `json:"mail"`
	// Skipped is a team replica's merged listing (as WorkList's): an authority
	// that did not answer.
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
}

// SendLimit is the data of a 429 rate_limited refusal of a mail send (SPEC 9
// "Send limits"): which rule broke, its maximum and its window.
type SendLimit struct {
	Limit   string `json:"limit"` // repeat, pair or sender
	Max     int    `json:"max,omitempty"`
	WindowS int    `json:"window_s"`
}

// DeviceView is GET /v0/approvals/device: what the calling device may see.
type DeviceView struct {
	Node     string    `json:"node"`
	NodeName string    `json:"node_name,omitempty"`
	Approver *Approver `json:"approver,omitempty"` // nil: not paired
	// OneTap is true when this unpaired device may pair with one tap (POST
	// /v0/approvals/pair-phone); OneTapWhy says why a phone may not.
	OneTap    bool   `json:"one_tap,omitempty"`
	OneTapWhy string `json:"one_tap_why,omitempty"`
	// Asks are the open asks (active approvers only), every project.
	Asks []Ask `json:"asks,omitempty"`
	// Confirm lists paired devices waiting for an existing approver.
	Confirm []Approver `json:"confirm,omitempty"`
	// Identity is the person an active approver decides for: this daemon's
	// machine owner, the only one who can add an approver. GitHub is the
	// login that identity is linked to (zm login github), when it is.
	Identity string `json:"identity,omitempty"`
	GitHub   string `json:"github,omitempty"`
	// Decisions is what GET /v0/decisions answers that person (active
	// approvers only): open and answered asks, and work waiting on them.
	Decisions []InboxItem       `json:"decisions,omitempty"`
	Skipped   []api.WorkSkipped `json:"skipped,omitempty"`
	// ForYou is the device's person's unread mail (SPEC 9 "For you"), newest first:
	// each a know item the device's Got it (POST /v0/approvals/got-it) marks read.
	ForYou []Mail `json:"for_you,omitempty"`
}
