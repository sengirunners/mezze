// Package unheld finds work that is still in flight with nobody holding it:
// an item claimed or in review, with a pr link, whose lease has
// lapsed while its PR is open (an agent waiting on CI lets it run out). GET
// /v0/work/inflight lists such items as rows marked unheld, so in-flight work
// does not look abandoned. The item's status stands for the PR's: GitHub
// linking (SPEC 18) moves it to done on merge and to open on an unmerged close.
package unheld

import (
	"context"
	"database/sql"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// project.db: PRLinked's rel = 'pr' would otherwise scan every link.
// Migrations are this package's indexes; work.New adds them (core.Deps.AddMigrations).
var Migrations = []store.Migration{
	{Kind: store.KindProject, Name: "work/0204_links_rel", Script: `CREATE INDEX work_links_rel ON work_links (rel, item);`},
}

// PRLinked lists the items of project's db that carry a pr link, are claimed
// or in review, and sit in an area sc reads (the item's own area, from its
// mirror or local row), in link order. Status and area are filtered in SQL
// through the mirror's (source, ref) and work_items' ref keys, so the many
// done items with PRs are never loaded.
func PRLinked(ctx context.Context, db *sql.DB, sc authz.Scope, project string) ([]string, error) {
	where, args := sc.AreaSQL(project, "coalesce(m.area, i.area)", authz.Read)
	rows, err := db.QueryContext(ctx, `SELECT l.item FROM work_links l
		LEFT JOIN work_mirror m ON m.source = substr(l.item, 1, instr(l.item, ':') - 1) AND m.ref = substr(l.item, instr(l.item, ':') + 1)
		LEFT JOIN work_items i ON l.item LIKE 'local:%' AND i.ref = substr(l.item, 7)
		WHERE l.rel = 'pr' AND coalesce(m.status, i.status) IN ('claimed', 'in_review') AND `+where+`
		GROUP BY l.item ORDER BY min(l.created), l.item`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Rows are the unheld rows for project: each PR-linked item that no live
// lease holds (leased: project+"\x00"+key) and item says is claimed or in
// review. item answers nil for an item the caller cannot see.
func Rows(project string, ids []string, leased map[string]bool, item func(key string) *orchapi.WorkItem) []orchapi.Inflight {
	var out []orchapi.Inflight
	for _, id := range ids {
		if leased[project+"\x00"+id] {
			continue
		}
		it := item(id)
		if it == nil || it.Status != orchapi.WorkClaimed && it.Status != orchapi.WorkInReview {
			continue
		}
		out = append(out, orchapi.Inflight{Lease: orchapi.Lease{Project: project, Area: it.Area, Key: id}, Item: it, Unheld: true})
	}
	return out
}
