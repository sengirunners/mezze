// Package wbcause marks the status event a team project's tracker write-back
// mints with the lease it was written for. The background writer
// runs after the claim has answered, so its move to claimed (or the release of
// the expired holder the claim replaced) lands after the claimer's base; the
// authority's base check (SPEC 17.6) skips an event marked with the writer's
// own live lease, and still counts every other status change.
package wbcause

import (
	"fmt"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Key is the event data key holding the cause. Events name it owner-only, so
// no replica or guest ever receives it; only the authority's check reads it.
const Key = "writeback_for"

// Of identifies a lease: its holder and claim time (to the millisecond, as
// stored).
func Of(holder string, claimed time.Time) string {
	return fmt.Sprintf("%s %d", holder, claimed.UnixMilli())
}

// Data is it as the write-back's status event carries it, marked with cause
// ("" leaves it unmarked).
func Data(it orchapi.WorkItem, cause string) any {
	return struct {
		orchapi.WorkItem
		Cause string `json:"writeback_for,omitempty"` // Key
	}{it, cause}
}

// NotOwn is the SQL condition, over the events table, keeping a row not
// marked with a cause (its two arguments; "" keeps every row).
const NotOwn = `(? = '' OR coalesce(json_extract(json, '$.data.` + Key + `'), '') != ?)`
