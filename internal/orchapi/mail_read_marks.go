package orchapi

// Read marks (SPEC 9 "Read marks"): a person's "read up to here" per
// conversation, kept by zm so every device shows the same unread.
const (
	// MaxReadMarkConv bounds a conversation key, in bytes.
	MaxReadMarkConv = 200
	// MaxReadMarksPut bounds the marks one PUT carries.
	MaxReadMarksPut = 100
)

// ReadMark is one conversation's mark for the caller: mail created at or
// before UptoMs (ms) is read. Conv is the client's key; zm never reads it.
type ReadMark struct {
	Project string `json:"project"` // the project's slug
	Conv    string `json:"conv"`
	UptoMs  int64  `json:"upto_ms"`
}

// ReadMarkList is GET and PUT /v0/mail/read-marks's answer.
type ReadMarkList struct {
	Marks []ReadMark `json:"marks"`
}

// ReadMarkSet is one mark in a PUT.
type ReadMarkSet struct {
	Conv   string `json:"conv"`
	UptoMs int64  `json:"upto_ms"`
}

// ReadMarksRequest is PUT /v0/mail/read-marks?project=<slug>'s body.
type ReadMarksRequest struct {
	Marks []ReadMarkSet `json:"marks"`
}
