package team

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/rollup"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// copyTable is the copy's items, as rollup reads them.
var copyTable = rollup.Table{Name: "items", JSON: "json", Title: "json_extract(json, '$.title')", Labels: "json_extract(json, '$.labels')",
	Key: []string{"id"}, Ref: "id",
	Refs: func(source, refs string) (string, []any) {
		return "id IN (SELECT ? || ':' || value FROM json_each(?))", []any{source, refs}
	}}

// copyRollup is GET /v0/work/rollup from the copy, with no tailnet call. The
// copy holds what this member may read and its holders are redacted for it, so
// the only narrowing left is a named area. Counts follow the copy: a write
// shows once its change comes back, not while it waits in the outbox.
func (s *Service) copyRollup(ctx context.Context, rp *replica, snap api.Remote, me, area string) (orchapi.WorkRollupList, error) {
	src := rollup.SQL{DB: rp.db, Table: copyTable}
	if area != "" {
		src.Where, src.Args = "area = ?", []any{area}
	}
	live, err := rp.liveLeases(ctx)
	if err != nil {
		return orchapi.WorkRollupList{}, err
	}
	claims := make([]rollup.Claim, 0, len(live))
	for _, l := range live {
		if area == "" || l.Area == area {
			claims = append(claims, rollup.Claim{Lease: l, Mine: s.copyMine(ctx, l.Holder, me), Holder: s.copyHolder(ctx, l.Holder)})
		}
	}
	slices.SortFunc(claims, func(a, b rollup.Claim) int {
		if c := a.Lease.Claimed.Compare(b.Lease.Claimed); c != 0 {
			return c
		}
		return strings.Compare(a.Lease.Key, b.Lease.Key)
	})
	r, err := rollup.Build(ctx, src, snap.Project.ID, snap.Project.Slug, claims, time.Now())
	if err != nil {
		return orchapi.WorkRollupList{}, err
	}
	out := orchapi.WorkRollupList{Rollups: []orchapi.WorkRollup{r}}
	if note, synced := rp.staleNote(); note != "" {
		out.Behind = []orchapi.WorkBehind{{Project: snap.Project.ID, Source: "authority", State: orchapi.SyncStale, Synced: synced, Error: note}}
	}
	return out, nil
}

// copyHolder is a claim's holding session's state as this machine knows it:
// its own sessions, and another machine's only where it keeps presence. The
// copy's holders are already redacted for the member; one not known is "".
func (s *Service) copyHolder(ctx context.Context, holder string) orchapi.SessionState {
	if orchcore.Sessions(s.d) == nil {
		return ""
	}
	return rollup.HolderState(holder, func(addr string) (orchapi.Session, bool) { return orchcore.Sessions(s.d).Lookup(ctx, addr) })
}

// copyMine reports whether a lease's holder is the caller. For a person that is
// also a session they started on this machine; a session of theirs on another
// machine is not known here.
func (s *Service) copyMine(ctx context.Context, holder, me string) bool {
	if holder == "" {
		return false
	}
	if holder == me {
		return true
	}
	sub := core.SubjectOf(ctx)
	if sub == nil || sub.Session != "" || orchcore.Sessions(s.d) == nil {
		return false
	}
	if !strings.HasPrefix(me, "human:") {
		return false
	}
	ss, ok := orchcore.Sessions(s.d).Lookup(ctx, holder)
	return ok && ss.Launcher != "" && (ss.Launcher == sub.Identity.ID || ss.Launcher == sub.Identity.Name)
}
