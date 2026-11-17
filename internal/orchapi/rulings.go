package orchapi

// Owner rulings (SPEC 16 "Card judge"): the project's standing owner
// answers, kept in [[rulings]] in its project.toml at its authority.
// GET, POST /v0/rulings and DELETE /v0/rulings/{name} read and change them.
// Not generated: zm's own shape.

// Ruling is one standing owner answer. Ask is the approved ask whose card
// carried Text word for word (set by POST /v0/rulings; a ruling written by
// hand may have none).
type Ruling struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Ask  string `json:"ask,omitempty"`
}

// RulingList is GET /v0/rulings and what a change answers: the resolved
// project's rulings, in file order.
type RulingList struct {
	Project string   `json:"project"`
	Rulings []Ruling `json:"rulings"`
}

// RulingAddRequest is POST /v0/rulings: add Name with Text, on the strength
// of Ask, an ask of the project an owner approved on a paired device or
// with a key, whose card carries Text exactly.
type RulingAddRequest struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Ask  string `json:"ask"`
}
