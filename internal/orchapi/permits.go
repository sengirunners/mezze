package orchapi

import "time"

// Standing permissions (SPEC 16 "Standing permissions"). A permit lets zm do
// one kind of act it performs itself (a task start, a machine choice)
// without a card each time, while every condition of its kind holds from
// zm's own records. Kinds are a closed list in the daemon, each with frozen
// text an owner reads on the card that grants it. Granting, renewing and
// widening take a signed answer; revoking does not. While a kind is in its
// dry run zm skips no card: it only records whether a permit of that kind
// would have covered each card with an action.

// PermitCondition is one condition of a kind, as an owner reads it.
type PermitCondition struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// PermitKind is one kind of standing permission. SHA covers its name, text
// and conditions (with their versions): a permit granted under another SHA
// is void.
type PermitKind struct {
	Name       string            `json:"name"`
	Text       string            `json:"text"`
	Conditions []PermitCondition `json:"conditions"`
	SHA        string            `json:"sha256"`
	// DryRun: zm records would-have-covered for this kind's cards and skips
	// none.
	DryRun bool `json:"dry_run,omitempty"`
}

// BoxClass is a class an owner may put on a machine (a fact about the box,
// never a claim on a job), with its frozen text.
type BoxClass struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// Permit states.
const (
	PermitLive     = "live"
	PermitExpired  = "expired"
	PermitRevoked  = "revoked"
	PermitReplaced = "replaced" // renewed or widened by a later grant
	PermitVoid     = "void"     // its kind changed after the grant
)

// Permit is one granted standing permission. Ask is the card whose signed
// answer granted it; Approver who answered (Method is always approver).
type Permit struct {
	ID         string     `json:"id"`
	Project    string     `json:"project"`
	Kind       string     `json:"kind"`
	KindSHA    string     `json:"kind_sha256"`
	Ask        string     `json:"ask"`
	Approver   string     `json:"approver"`
	GrantedBy  string     `json:"granted_by"` // who sent the grant (any operator; the signature is what counts)
	Created    time.Time  `json:"created"`
	Expires    time.Time  `json:"expires"`
	Revoked    *time.Time `json:"revoked,omitempty"`
	RevokedBy  string     `json:"revoked_by,omitempty"`
	ReplacedBy string     `json:"replaced_by,omitempty"`
	State      string     `json:"state"`
}

// BoxMark is a class an owner put on a machine with a signed answer.
type BoxMark struct {
	Project  string    `json:"project"`
	Machine  string    `json:"machine"`
	Class    string    `json:"class"`
	Ask      string    `json:"ask"`
	Approver string    `json:"approver"`
	Set      time.Time `json:"set"`
}

// PermitList is GET /v0/permits: the kinds, the project's permits (newest
// first) and its box marks.
type PermitList struct {
	Project string       `json:"project"`
	Kinds   []PermitKind `json:"kinds"`
	Classes []BoxClass   `json:"classes"`
	Permits []Permit     `json:"permits"`
	Boxes   []BoxMark    `json:"boxes"`
}

// PermitCardRequest asks the daemon for the card that grants a kind for Days
// (Kind and Days), or that puts Class on Machine (Machine and Class). File it
// with POST /v0/asks unchanged but for recommend, why and searched; once a
// person answered it with the yes choice on a paired device or key, POST
// /v0/permits with the same fields and the ask's id.
type PermitCardRequest struct {
	Kind    string `json:"kind,omitempty"`
	Days    int    `json:"days,omitempty"`
	Machine string `json:"machine,omitempty"`
	Class   string `json:"class,omitempty"`
}

// PermitGrantRequest is POST /v0/permits.
type PermitGrantRequest struct {
	PermitCardRequest
	Ask string `json:"ask"`
}

// PermitGrant is POST /v0/permits' answer: the permit or the box mark made.
type PermitGrant struct {
	Permit *Permit  `json:"permit,omitempty"`
	Box    *BoxMark `json:"box,omitempty"`
}

// BoxUnmarkRequest is POST /v0/permits/boxes/unmark (unsigned: taking a
// class off only narrows).
type BoxUnmarkRequest struct {
	Machine string `json:"machine"`
	Class   string `json:"class"`
}

// PermitDryRunRow is one kind's line in the weekly report: of the cards
// with an action asked in the window, how many a permit of the kind would
// have covered; of those, how many a person answered with the choice the
// permit would have taken, the action's (Saved), answered otherwise (WronglyCovered: no, another choice, free
// text or sent back), or has not answered (Open; withdrawn ones are
// Withdrawn).
type PermitDryRunRow struct {
	Kind           string         `json:"kind"`
	Checked        int            `json:"checked"`
	WouldCover     int            `json:"would_cover"`
	Saved          int            `json:"saved"`
	WronglyCovered int            `json:"wrongly_covered"`
	Open           int            `json:"open"`
	Withdrawn      int            `json:"withdrawn"`
	NotCovered     map[string]int `json:"not_covered,omitempty"` // by failed condition id
}

// PermitDryRunReport is GET /v0/permits/dryrun.
type PermitDryRunReport struct {
	Project string            `json:"project"`
	From    time.Time         `json:"from"`
	To      time.Time         `json:"to"`
	Rows    []PermitDryRunRow `json:"rows"`
}
