package orchapi

import (
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// WorkPageQuery is GET /v0/work/page's query string: a keyset-paginated,
// totally ordered read of every work item the caller may read, done items
// included. Repeated parameters (in, id, status, label) are ORed within
// themselves and ANDed with each other.
type WorkPageQuery struct {
	// In narrows to projects or areas: "slug", "ULID", "slug/area". An entry
	// naming a hidden or missing project or area matches nothing.
	In     []string
	IDs    []string // work ids ("local:k3x9qa"), optionally "<project ULID>/<id>"
	Status []WorkStatus
	Labels []string // items carrying any of these labels
	Source string
	Q      string // case-insensitive substring of title or body
	// Owner keeps items whose live lease owner holds (or, for a tracker item
	// with no lease, whose tracker owner is owner).
	Owner string
	// Created/updated ranges (inclusive bounds; zero = unbounded).
	CreatedAfter, CreatedBefore time.Time
	UpdatedAfter, UpdatedBefore time.Time
	Order                       string // "created" (default) or "updated"
	Asc                         bool   // oldest first (default newest first)
	After                       string // a WorkPage.Next cursor
	Limit                       int    // default 50, max 500
	Comments                    bool   // with=comments: fill each item's Comments
	Links                       bool   // with=links: fill each item's Links (hidden targets collapsed per rel)
	// Count (count=1) answers only Total: how many items match, no items.
	Count bool
	// Waiting narrows to items labelled or typed as waiting on a person.
	Waiting *WaitingFilter
	// Epic (epic=) keeps the items an open epic owns by the rollup's rule
	// (SPEC 10 "Rollup"): a work id, optionally "<project ULID>/<work id>".
	Epic string
}

// WaitingFilter keeps items carrying one of Labels, a label starting with one
// of Prefixes, or a tracker type in Types. In-process only (GET /v0/decisions).
type WaitingFilter struct {
	Labels, Prefixes, Types []string
	After                   string // internal waiting-page continuation
}

// WorkPage is GET /v0/work/page. Next is the cursor of the last item when
// more may follow ("" at the end). Skipped and Behind mean what they mean
// in WorkList: a partial page never looks complete.
type WorkPage struct {
	Items []WorkItem `json:"items"`
	// Cursors[i] is Items[i]'s position: pass it as after to continue
	// right after that item.
	Cursors []string `json:"cursors"`
	Next    string   `json:"next,omitempty"`
	Total   *int     `json:"total,omitempty"` // count=1 only
	// Epic echoes epic= when the page was filtered by it. An authority that
	// predates epic= ignores it and leaves this out: a caller that asked for
	// an epic refuses a page without the echo (never shows it unfiltered).
	Epic    string            `json:"epic,omitempty"`
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
	Behind  []WorkBehind      `json:"behind,omitempty"`
}

// WorkLabels is GET /v0/work/labels: every label on an item the caller may
// read (narrowed by in=), sorted.
type WorkLabels struct {
	Labels  []string          `json:"labels"`
	Skipped []api.WorkSkipped `json:"skipped,omitempty"`
}
