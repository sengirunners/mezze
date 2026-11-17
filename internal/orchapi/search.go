package orchapi

// SearchMatch is one matching line of GET /v0/search.
type SearchMatch struct {
	File string `json:"file"` // absolute
	Line int    `json:"line"` // 1-based
	Col  int    `json:"col"`  // 1-based, in characters: the match's first
	Text string `json:"text"` // the line, at most SearchMaxLine characters
	// TextClipped: the line was longer and was cut (marked with "…").
	TextClipped bool `json:"text_clipped,omitempty"`
}

// SearchTruncated says which hard limit cut a search short.
type SearchTruncated struct {
	Matches bool `json:"matches,omitempty"` // SearchMaxMatches reached
	Files   bool `json:"files,omitempty"`   // SearchMaxFiles files with matches reached
	Time    bool `json:"time,omitempty"`    // the time limit passed
}

// SearchResult is GET /v0/search.
type SearchResult struct {
	Project   string           `json:"project"`
	Engine    string           `json:"engine"` // "ripgrep" or "walk"
	Roots     []string         `json:"roots"`
	Matches   []SearchMatch    `json:"matches"`
	Truncated *SearchTruncated `json:"truncated,omitempty"`
}

// Search limits (SPEC 21).
const (
	SearchMaxMatches = 2000
	SearchMaxFiles   = 200
	SearchMaxLine    = 200  // characters of text per match
	SearchMaxQuery   = 1000 // bytes
)
