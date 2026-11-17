package orchapi

import "time"

// Person steps (SPEC 16 "Person steps"): a manual step for one person, with
// no real choice in it (install the newest build, accept an invite), that an
// agent hands to that person's own agent. It waits at the project's
// authority until one daily steps card per person carries every pending step
// at once (a hand ask whose one press preapproves the person's agent for all
// of them), so a person gets one card a day instead of a stream of requests.
// The step judge (stepjudge.md) reads each step before it is stored.

// Person step states. A step moves draft -> pending -> carded -> reported ->
// done; failed and dropped end it early, and a passing check makes any
// pending, carded, reported or failed step done.
const (
	// StepDraft is a step saved from an agent's report to one person (the mail
	// redirect); its adder completes it with zm ask bundle add --draft.
	StepDraft = "draft"
	// StepPending is judged fit and waits for the person's next steps card.
	StepPending = "pending"
	// StepCarded is on a filed steps card: Bundle, Number and Ask say which.
	StepCarded = "carded"
	// StepReported is said done by the person's agent: its adder runs the check.
	StepReported = "reported"
	// StepDone is a step whose check passed.
	StepDone = "done"
	// StepFailed is said failed by the person's agent, or a done report whose
	// check then failed.
	StepFailed = "failed"
	// StepDropped is taken back by its adder or an operator.
	StepDropped = "dropped"
)

// Bounds on a person step, in bytes.
const (
	MaxPersonStepTitle = 80
	MaxPersonStepBrief = 900
	MaxPersonStepCheck = 200
	MaxPersonStepNote  = 500
)

// PersonStep is one step for one person (GET /v0/steps/{id}).
type PersonStep struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Area    string `json:"area,omitempty"`
	// Person is the identity name the step is for (as human:<name>).
	Person  string `json:"person"`
	AddedBy string `json:"added_by"`
	// Work is the work item the step serves (bd:zm-1, local:k3x9qa).
	Work string `json:"work"`
	// Title is one plain line; Brief is the numbered steps to done, which the
	// person's agent works from; Check is the shell line its adder runs to
	// see it done (zm ask bundle step-check).
	Title string `json:"title"`
	Brief string `json:"brief"`
	Check string `json:"check"`
	// Risk is the step judge's: none, irreversible or spending.
	Risk  string `json:"risk"`
	State string `json:"state"`
	// Bundle, Number and Ask: the steps card it is on (carded and after).
	Bundle  string    `json:"bundle,omitempty"`
	Number  int       `json:"number,omitempty"`
	Ask     string    `json:"ask,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// The person's agent's last report (zm ask bundle report).
	ReportNote string     `json:"report_note,omitempty"`
	ReportedBy string     `json:"reported_by,omitempty"`
	ReportedAt *time.Time `json:"reported_at,omitempty"`
	// The adder's last check (zm ask bundle step-check).
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	CheckExit   *int       `json:"check_exit,omitempty"`
	CheckOutput string     `json:"check_output,omitempty"`
	CheckedBy   string     `json:"checked_by,omitempty"`
}

// StepAddRequest is POST /v0/steps. A new step names To (human:<name>, a
// member of the work item's project), Work, Title, Brief and Check. Draft
// completes a draft step instead (its person and work item stay); FromAsk
// copies an open, unanswered hand ask's brief and verify line (its person
// and work item too; Title defaults to its card's) and withdraws that ask.
type StepAddRequest struct {
	To      string `json:"to,omitempty"`
	Work    string `json:"work,omitempty"`
	Title   string `json:"title,omitempty"`
	Brief   string `json:"brief,omitempty"`
	Check   string `json:"check,omitempty"`
	Draft   string `json:"draft,omitempty"`
	FromAsk string `json:"from_ask,omitempty"`
}

// StepList is GET /v0/steps.
type StepList struct {
	Steps []PersonStep `json:"steps"`
}

// StepReportRequest is POST /v0/bundles/{id}/steps/{n}/report: the person's
// agent says step n of the card is done or failed (Result), with a note.
type StepReportRequest struct {
	Result string `json:"result"` // "done" or "failed"
	Note   string `json:"note"`
}

// StepCheckRequest is POST /v0/steps/{id}/check: the exit of the step's
// check line and its output's tail, as its adder ran it.
type StepCheckRequest struct {
	Exit   *int   `json:"exit"`
	Output string `json:"output,omitempty"`
}

// Bundle is GET /v0/bundles/{id}: one steps card's steps in card order.
// Done is true when it has steps and every one is done or dropped.
type Bundle struct {
	ID    string       `json:"id"`
	Ask   string       `json:"ask,omitempty"`
	Steps []PersonStep `json:"steps"`
	Done  bool         `json:"done"`
}
