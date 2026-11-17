// Package mirrorquery builds scoped tracker-mirror list queries without I/O.
package mirrorquery

import (
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
)

// Columns is the mirror row shape consumed by the work service.
const Columns = `id, area, status, title, body, item`

// Query filters one source in one project. Empty Status selects active items;
// Owner also includes Leased candidates, rechecked after the lease overlay.
type Query struct {
	Scope                 authz.Scope
	Project, Source, Area string
	Status                orchapi.WorkStatus
	Text, Owner           string
	Leased                []string
	Limit                 int
}

// likeEscaper escapes LIKE's wildcards for ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// List returns a parameterized query, newest change first.
func List(q Query) (string, []any) {
	where, args := q.Scope.AreaSQL(q.Project, "area", authz.Read)
	conds := []string{"source = ?", where}
	args = append([]any{q.Source}, args...)
	if q.Status != "" {
		conds, args = append(conds, "status = ?"), append(args, string(q.Status))
	} else {
		conds = append(conds, "status IN ('open', 'claimed', 'in_review')")
	}
	if q.Area != "" {
		conds, args = append(conds, "area = ?"), append(args, q.Area)
	}
	if q.Text != "" {
		// LIKE is already case-insensitive (ASCII); lower() would copy every body.
		like := "%" + likeEscaper.Replace(q.Text) + "%"
		conds = append(conds, `(title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\' OR id LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if q.Owner != "" {
		// Both sides indexed (source, owner) and (source, ref): SQLite answers
		// the OR from the two indexes instead of walking every row.
		c := "owner = ?"
		args = append(args, q.Owner)
		if len(q.Leased) > 0 {
			c += " OR ref IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(q.Leased)), ", ") + ")"
			for _, k := range q.Leased {
				args = append(args, strings.TrimPrefix(k, q.Source+":"))
			}
		}
		conds = append(conds, "("+c+")")
	}
	args = append(args, q.Limit)
	// Without statistics SQLite walks the newest-first index and filters;
	// for a caller who sees a few areas (a guest) seeking by area is far
	// cheaper (11.5k rows: 14 ms -> under 1 ms).
	from := "work_mirror"
	switch {
	case q.Owner != "":
		from += " INDEXED BY work_mirror_owner"
	case where != "1=1" || q.Area != "":
		from += " INDEXED BY work_mirror_area"
	case q.Text != "": // skip other statuses before scanning text, especially done rows
		from += " INDEXED BY work_mirror_status"
	}
	return `SELECT ` + Columns + ` FROM ` + from + ` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY updated DESC, id LIMIT ?`, args
}
