package orchapi

import "time"

// Owed kinds: what a session or a person still owes, as zm's own records
// show it (SPEC 8 "What a session still owes").
const (
	OwedWork       = "work"          // a work item its lease holds
	OwedPR         = "pr"            // an open pull request linked to work it holds or last held
	OwedAskOpen    = "ask_open"      // a card it filed that no one has answered
	OwedAskNotDone = "ask_not_done"  // a card whose yes is not yet done
	OwedLane       = "lane"          // a session it started that is still running
	OwedGapSource  = "source_failed" // OwedGap.Kind: a source did not answer
	OwedGapUnseen  = "not_recorded"  // OwedGap.Kind: zm keeps no record of it
	OwedGapPartial = "partial"       // OwedGap.Kind: a source read only part of its records
	OwedForSession = "session"       // OwedList.For kinds
	OwedForPerson  = "person"
)

// OwedItem is one thing a session still owes, read from a record.
type OwedItem struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`   // work id, PR url, ask id or session address
	Title   string `json:"title"` // one line, as the record reads
	State   string `json:"state"` // the record's own state, in plain words
	Project string `json:"project,omitempty"`
	Area    string `json:"area,omitempty"`
	// Holder is the session that owes it (redacted for the reader).
	Holder string     `json:"holder,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

// OwedGap is something the list cannot fully show: a source that did not
// answer or read only part of its records, or a kind of work zm keeps no
// record of. The list shows each as unknown; it
// is never left out.
type OwedGap struct {
	Kind string `json:"kind"` // OwedGapSource, OwedGapPartial or OwedGapUnseen
	Name string `json:"name"` // the source or the kind of work
	Why  string `json:"why"`
}

// OwedList is GET /v0/owed.
type OwedList struct {
	// For is "session" (one session) or "person" (all that person's sessions).
	For      string     `json:"for"`
	Subject  string     `json:"subject"`  // the session address or the person's name
	Sessions []string   `json:"sessions"` // the sessions the list covers
	Items    []OwedItem `json:"items"`
	Unknown  []OwedGap  `json:"unknown"`
}
