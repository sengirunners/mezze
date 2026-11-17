package team

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Serving a joined project from its copy (SPEC 17.6 "Replica reads" and
// "Writes"). Nothing here waits on the tailnet except a claim, release or
// lease while the authority is reachable (one round trip, SPEC: claims are
// a synchronous compare-and-set at the authority), bounded by ClaimWait.

// copyKind says what a call does on the copy: "read", "write", "" (not
// served here: forward as round B does).
func copyKind(method, path string) string {
	switch method {
	case http.MethodGet:
		switch {
		case path == "/v0/work" || path == "/v0/work/sources" || path == "/v0/work/inflight" || path == "/v0/work/rollup" || path == "/v0/leases",
			path == "/v0/mail" || path == "/v0/mail/inbox" || path == "/v0/mail/sent":
			return "read"
		case strings.HasPrefix(path, "/v0/work/") || strings.HasPrefix(path, "/v0/mail/") && path != "/v0/mail/read-marks": // no event carries a read mark
			return "read"
		}
	case http.MethodPost:
		switch {
		case path == "/v0/leases" || path == "/v0/mail":
			return "write"
		case strings.HasPrefix(path, "/v0/work/"):
			for _, v := range []string{"/claim", "/release", "/comment", "/status", "/link"} {
				if strings.HasSuffix(path, v) {
					return "write"
				}
			}
		case strings.HasPrefix(path, "/v0/mail/"):
			if strings.HasSuffix(path, "/ack") || strings.HasSuffix(path, "/reply") {
				return "write"
			}
		}
	case http.MethodDelete:
		if strings.HasPrefix(path, "/v0/leases/") {
			return "write"
		}
	}
	return ""
}

// segs splits /v0/<kind>/<id>[/<verb>] into id and verb (id unescaped).
func segs(path, prefix string) (id, verb string) {
	rest := strings.TrimPrefix(path, prefix)
	id, verb, _ = strings.Cut(rest, "/")
	if u, err := url.PathUnescape(id); err == nil {
		id = u
	}
	return id, verb
}

// caller is who acts on the replica, as the authority will see it: a
// session or machine address on this machine (sent as Zm-As), else the
// replica's identity.
func (s *Service) callerAddr(r *http.Request, snap api.Remote) (as, me string) {
	if sub := core.SubjectOf(r.Context()); sub != nil && sub.Address != "" {
		if a, err := api.ParseAddress(sub.Address); err == nil && (a.Kind == api.KindSession || a.Kind == api.KindMachine) && a.Machine == s.d.Config.Machine {
			return a.String(), a.String()
		}
	}
	return "", "human:" + snap.Identity
}

// holds reports whether the copy can answer an item route for path: the
// item (or mail) is in it, or the route needs none.
func (rp *replica) holds(ctx context.Context, path string) bool {
	switch {
	case strings.HasPrefix(path, "/v0/work/"):
		id, _ := segs(path, "/v0/work/")
		_, _, ok := rp.findItem(ctx, id)
		return ok
	case strings.HasPrefix(path, "/v0/mail/"):
		id, _ := segs(path, "/v0/mail/")
		_, ok := rp.findMail(ctx, id)
		return ok
	case strings.HasPrefix(path, "/v0/leases/"):
		key, _ := segs(path, "/v0/leases/")
		var n int
		_ = rp.db.QueryRowContext(ctx, `SELECT count(*) FROM leases WHERE key = ?`, key).Scan(&n)
		return n > 0
	}
	return false
}

// serveCopy answers r from x's copy and reports true, or reports false (the
// caller forwards it). area narrows a named project.
func (s *Service) serveCopy(w http.ResponseWriter, r *http.Request, x *remote, rp *replica, area string, body []byte) bool {
	kind := copyKind(r.Method, r.URL.Path)
	if kind == "" || !rp.isReady() {
		return false
	}
	snap := x.Snapshot()
	if err := teamlink.Ended(snap); err != nil {
		api.WriteError(w, err)
		return true
	}
	if body == nil {
		var err error
		if body, err = teamlink.ReadBody(r); err != nil {
			api.WriteError(w, err)
			return true
		}
		r.Body = io.NopCloser(bytes.NewReader(body)) // a false answer forwards r
	}
	s.d.Log.Debug("request served from the replica copy", "method", r.Method, "path", r.URL.Path, "project", snap.Project.Slug, "kind", kind)
	if kind == "read" {
		v, ok, err := s.copyRead(r, x, rp, area)
		if !ok {
			return false
		}
		if err != nil {
			api.WriteError(w, err)
		} else {
			api.WriteJSON(w, http.StatusOK, v)
		}
		return true
	}
	return s.copyWrite(w, r, x, rp, area, body)
}

// copyRead answers a read from the copy. ok=false: not answerable here
// (an item the copy does not hold: the caller forwards).
func (s *Service) copyRead(r *http.Request, x *remote, rp *replica, area string) (any, bool, error) {
	ctx := r.Context()
	snap := x.Snapshot()
	_, me := s.callerAddr(r, snap)
	q := r.URL.Query()
	path := r.URL.Path
	if area == "" {
		area = q.Get("area")
	}
	switch path {
	case "/v0/work":
		limit, _ := strconv.Atoi(q.Get("limit"))
		v, err := rp.listItems(ctx, snap.Project.ID, orchapi.WorkQuery{Area: area, Source: q.Get("source"), Status: orchapi.WorkStatus(q.Get("status")),
			Owner: q.Get("owner"), Q: textclean.Clean(q.Get("q")), Limit: limit})
		return v, true, err
	case "/v0/work/sources":
		return orchapi.WorkSourceList{Sources: []orchapi.WorkSource{rp.source(ctx, snap.Project.ID)}}, true, nil
	case "/v0/work/inflight":
		v, err := rp.inflight(ctx, snap.Project.ID, area)
		return v, true, err
	case "/v0/work/rollup":
		v, err := s.copyRollup(ctx, rp, snap, me, area)
		return v, true, err
	case "/v0/leases":
		ls, err := rp.leases(ctx, area, q.Get("holder"))
		return orchapi.LeaseList{Leases: ls}, true, err
	case "/v0/mail/inbox", "/v0/mail/inbox/peek":
		limit, _ := strconv.Atoi(q.Get("limit"))
		box := q.Get("box")
		if box != "" && box != orchapi.InboxBoxReports {
			return nil, true, api.Errorf(api.CodeBadRequest, "box %q: want reports, or none for the inbox", box)
		}
		mark := path == "/v0/mail/inbox" // peek: a poll leaves announcements unseen
		ms, err := s.inbox(ctx, rp, snap, me, q.Get("all") == "1", box == orchapi.InboxBoxReports, limit, mark)
		return orchapi.MailList{Mail: ms}, true, err
	case "/v0/mail/sent":
		limit, _ := strconv.Atoi(q.Get("limit"))
		ms, err := rp.mailWhere(ctx, snap, me, true, "sender = ?", []any{me}, limit)
		return orchapi.MailList{Mail: ms}, true, err
	case "/v0/mail":
		limit, _ := strconv.Atoi(q.Get("limit"))
		ms, err := rp.mailQuery(ctx, snap, me, q, limit)
		return orchapi.MailList{Mail: ms}, true, err
	}
	if strings.HasPrefix(path, "/v0/work/") {
		id, verb := segs(path, "/v0/work/")
		it, _, ok := rp.findItem(ctx, id)
		if !ok {
			return nil, false, nil
		}
		switch verb {
		case "":
			v, err := rp.whole(ctx, it, true)
			return v, true, err
		case "comments":
			v, err := rp.whole(ctx, it, true)
			return orchapi.CommentList{Comments: nonNil(v.Comments)}, true, err
		case "holder":
			// zm who shows the holder's presence, which the copy does not
			// keep: asked of the authority while it answers, else the copy.
			if rp.isReachable() {
				return nil, false, nil
			}
			v, err := rp.holder(ctx, it)
			return v, true, err
		}
		return nil, false, nil
	}
	if strings.HasPrefix(path, "/v0/mail/") {
		id, verb := segs(path, "/v0/mail/")
		m, ok := rp.findMail(ctx, id, me)
		if !ok {
			return nil, false, nil
		}
		switch verb {
		case "":
			return m, true, nil
		case "thread":
			ms, err := rp.thread(ctx, m, me)
			return orchapi.MailList{Mail: ms}, true, err
		}
	}
	return nil, false, nil
}

// ---- items ----

// findItem finds an item by its id, or by its ref alone when one item has
// it ("k3x9qa" for local:k3x9qa).
func (rp *replica) findItem(ctx context.Context, id string) (orchapi.WorkItem, string, bool) {
	var js, base string
	err := rp.db.QueryRowContext(ctx, `SELECT json, base FROM items WHERE id = ?`, id).Scan(&js, &base)
	if err == sql.ErrNoRows && id != "" && !strings.Contains(id, ":") {
		rows, qerr := rp.db.QueryContext(ctx, `SELECT json, base FROM items WHERE id LIKE ? ESCAPE '\' LIMIT 2`, "%:"+likeEscape(id))
		if qerr == nil {
			n := 0
			for rows.Next() {
				n++
				_ = rows.Scan(&js, &base)
			}
			rows.Close()
			if n == 1 {
				err = nil
			}
		}
	}
	if err != nil {
		return orchapi.WorkItem{}, "", false
	}
	var it orchapi.WorkItem
	if json.Unmarshal([]byte(js), &it) != nil {
		return orchapi.WorkItem{}, "", false
	}
	return it, base, true
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// liveLeases is the copy's leases still live by the authority's clock (a
// bound lease is renewed there without an event, so it counts until its
// release or expiry arrives, and the deadline the copy has for it is only the
// claim's: it is served with none, never one that reads as expired).
func (rp *replica) liveLeases(ctx context.Context) (map[string]orchapi.Lease, error) {
	now := store.Millis(rp.authorityNow())
	rows, err := rp.db.QueryContext(ctx, `SELECT json FROM leases WHERE bound = 1 OR expires > ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]orchapi.Lease{}
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			return nil, err
		}
		var l orchapi.Lease
		if json.Unmarshal([]byte(js), &l) == nil {
			if l.Bound {
				l.Expires = nil
			}
			out[l.Key] = l
		}
	}
	return out, rows.Err()
}

// statusOverlay is the status each item's unsettled status writes leave,
// by item.
func (rp *replica) statusOverlay(ctx context.Context, item string) (map[string]orchapi.WorkStatus, error) {
	where, args := "kind = 'status'", []any{}
	if item != "" {
		where += " AND item = ?"
		args = append(args, item)
	}
	es, err := rp.overlays(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]orchapi.WorkStatus{}
	for _, e := range es {
		if e.state == api.OutboxAcked && rp.caughtUp(ctx, e) {
			continue
		}
		var req struct {
			Status orchapi.WorkStatus `json:"status"`
		}
		if json.Unmarshal([]byte(e.body), &req) == nil {
			out[e.item] = req.Status
		}
	}
	return out, nil
}

// caughtUp: the copy already shows what an acked write did.
func (rp *replica) caughtUp(ctx context.Context, e outEntry) bool {
	if e.seen == "" || e.item == "" {
		return false
	}
	var base string
	_ = rp.db.QueryRowContext(ctx, `SELECT base FROM items WHERE id = ?`, e.item).Scan(&base)
	return base >= e.seen
}

// finish sets an item's owner from live leases (a local item with none has
// no owner; a tracker item keeps its tracker's) and its overlaid status.
func finish(it *orchapi.WorkItem, ls map[string]orchapi.Lease, st map[string]orchapi.WorkStatus) {
	if l, ok := ls[it.ID]; ok {
		it.Owner = l.Holder
	} else if it.Source == "local" {
		it.Owner = ""
	}
	if v, ok := st[it.ID]; ok {
		it.Status = v
	}
}

// appendLowerASCII appends s with A-Z folded.
func appendLowerASCII[T ~string | ~[]byte](dst []byte, s T) []byte {
	n := len(dst)
	dst = append(dst, s...)
	for i, c := range dst[n:] {
		if 'A' <= c && c <= 'Z' {
			dst[n+i] = c + 'a' - 'A'
		}
	}
	return dst
}

// jsonKeeps reports whether encoding/json writes s byte for byte, so that a
// field containing s has s in its JSON text.
func jsonKeeps(s string) bool {
	if !utf8.ValidString(s) || strings.ContainsAny(s, "\"\\<>&\u2028\u2029") {
		return false
	}
	for _, c := range []byte(s) {
		if c < 0x20 {
			return false
		}
	}
	return true
}

// listItems is GET /v0/work from the copy.
func (rp *replica) listItems(ctx context.Context, project string, q orchapi.WorkQuery) (orchapi.WorkList, error) {
	if q.Status != "" && !orchapi.ValidStatus(q.Status) {
		return orchapi.WorkList{}, api.Errorf(api.CodeBadRequest, "bad status %q (open, claimed, in_review, done)", q.Status)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 5000)
	ls, err := rp.liveLeases(ctx)
	if err != nil {
		return orchapi.WorkList{}, err
	}
	st, err := rp.statusOverlay(ctx, "")
	if err != nil {
		return orchapi.WorkList{}, err
	}
	conds, args := []string{"1=1"}, []any{}
	// The status goes down to SQL: a copy is mostly done items, and decoding
	// them is what a list or search spends. An item with an unsettled status
	// write always passes, for the checks below to judge.
	overlaid, _ := json.Marshal(slices.AppendSeq([]string{}, maps.Keys(st)))
	switch {
	case q.Status != "":
		conds, args = append(conds, "(status = ? OR id IN (SELECT value FROM json_each(?)))"), append(args, string(q.Status), string(overlaid))
	default:
		conds, args = append(conds, "(status != ? OR source = 'local' OR id IN (SELECT value FROM json_each(?)))"), append(args, string(orchapi.WorkDone), string(overlaid))
	}
	if q.Area != "" {
		conds, args = append(conds, "area = ?"), append(args, q.Area)
	}
	if src := strings.ToLower(strings.TrimSpace(q.Source)); src != "" {
		conds, args = append(conds, "source = ?"), append(args, src)
	}
	owner := q.Owner
	if owner != "" {
		if a, err := api.ParseAddress(owner); err == nil {
			owner = a.String()
		}
	}
	rows, err := rp.db.QueryContext(ctx, `SELECT json FROM items WHERE `+strings.Join(conds, " AND ")+` ORDER BY created DESC, id`, args...)
	if err != nil {
		return orchapi.WorkList{}, err
	}
	defer rows.Close()
	// Search is here, not in SQL: Go's byte search is far faster than LIKE, and
	// text JSON keeps as written can be looked for in the raw row, so only
	// likely rows are decoded. Case folds for ASCII only, like LIKE did.
	needle := appendLowerASCII(nil, q.Q)
	inRaw := q.Q != "" && jsonKeeps(q.Q)
	has := func(s string) bool { return bytes.Contains(appendLowerASCII(nil, s), needle) }
	var lower []byte
	var js sql.RawBytes // reused: a row search rejects costs no allocation
	out := []orchapi.WorkItem{}
	for rows.Next() && len(out) < limit {
		if err := rows.Scan(&js); err != nil {
			return orchapi.WorkList{}, err
		}
		if inRaw {
			if lower = appendLowerASCII(lower[:0], js); !bytes.Contains(lower, needle) {
				continue
			}
		}
		var it orchapi.WorkItem
		if json.Unmarshal(js, &it) != nil {
			continue
		}
		if q.Q != "" && !has(it.Title) && !has(it.Body) && !has(it.ID) {
			continue
		}
		finish(&it, ls, st)
		switch {
		case q.Status != "" && it.Status != q.Status:
			continue
		case q.Status == "" && it.Source != "local" && it.Status == orchapi.WorkDone:
			continue // tracker items without ?status= exclude done, as at the authority
		case owner != "" && it.Owner != owner:
			continue
		}
		it.Links, it.Comments = nil, nil
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return orchapi.WorkList{}, err
	}
	wl := orchapi.WorkList{Items: out}
	if note, synced := rp.staleNote(); note != "" {
		wl.Behind = []orchapi.WorkBehind{{Project: project, Source: "authority", State: orchapi.SyncStale, Synced: synced, Error: note}}
	}
	return wl, nil
}

// whole is one item with its comments, overlaid with unsettled writes.
func (rp *replica) whole(ctx context.Context, it orchapi.WorkItem, comments bool) (orchapi.WorkItem, error) {
	ls, err := rp.liveLeases(ctx)
	if err != nil {
		return it, err
	}
	st, err := rp.statusOverlay(ctx, it.ID)
	if err != nil {
		return it, err
	}
	finish(&it, ls, st)
	es, err := rp.overlays(ctx, "item = ? AND kind IN ('comment', 'link')", it.ID)
	if err != nil {
		return it, err
	}
	if comments {
		rows, err := rp.db.QueryContext(ctx, `SELECT json FROM comments WHERE item = ? ORDER BY id`, it.ID)
		if err != nil {
			return it, err
		}
		have := map[string]bool{}
		it.Comments = []orchapi.Comment{}
		for rows.Next() {
			var js string
			if rows.Scan(&js) != nil {
				continue
			}
			var c orchapi.Comment
			if json.Unmarshal([]byte(js), &c) == nil {
				it.Comments = append(it.Comments, c)
				have[c.ID] = true
			}
		}
		rows.Close()
		for _, e := range es {
			if e.kind != "comment" || have[e.id] {
				continue
			}
			var req orchapi.CommentRequest
			_ = json.Unmarshal([]byte(e.body), &req)
			it.Comments = append(it.Comments, orchapi.Comment{ID: e.id, Item: it.ID, Author: e.authorOr(""), Body: req.Body, Created: e.created})
		}
	}
	for _, e := range es {
		if e.kind != "link" {
			continue
		}
		var l orchapi.Link
		if json.Unmarshal([]byte(e.body), &l) == nil && !hasLink(it.Links, l) {
			it.Links = append(it.Links, orchapi.Link{Rel: l.Rel, Target: l.Target})
		}
	}
	if note, _ := rp.staleNote(); note != "" {
		it.Stale = note
	}
	return it, nil
}

func hasLink(ls []orchapi.Link, l orchapi.Link) bool {
	for _, x := range ls {
		if x.Rel == l.Rel && x.Target == l.Target {
			return true
		}
	}
	return false
}

// authorOr is who a queued write is by (its Zm-As, else def).
func (e outEntry) authorOr(def string) string {
	if e.as != "" {
		return e.as
	}
	return def
}

func (rp *replica) leases(ctx context.Context, area, holder string) ([]orchapi.Lease, error) {
	m, err := rp.liveLeases(ctx)
	if err != nil {
		return nil, err
	}
	if holder != "" {
		if a, err := api.ParseAddress(holder); err == nil {
			holder = a.String()
		}
	}
	out := []orchapi.Lease{}
	for _, l := range m {
		if (area != "" && l.Area != area) || (holder != "" && l.Holder != holder) {
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Claimed.Equal(out[j].Claimed) {
			return out[i].Claimed.Before(out[j].Claimed)
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (rp *replica) holder(ctx context.Context, it orchapi.WorkItem) (orchapi.HolderResponse, error) {
	m, err := rp.liveLeases(ctx)
	if err != nil {
		return orchapi.HolderResponse{}, err
	}
	out := orchapi.HolderResponse{ID: it.ID}
	if l, ok := m[it.ID]; ok {
		out.Holder, out.Lease, out.MailTo, out.Via = l.Holder, &l, l.Holder, "holder of "+it.ID
	} else if it.CreatedBy != "" {
		out.MailTo, out.Via = it.CreatedBy, "creator of "+it.ID
		if it.Source != "local" && it.Owner != "" {
			out.TrackerOwner = it.Owner
		}
	}
	return out, nil
}

// source is the authority's row in GET /v0/work/sources: the copy's
// freshness (lag_seconds is an age), its last error, sanitised, and how many
// items it holds. The copy holds only what this machine's token reads (the
// snapshot and sync follow its scope), so its count shows nothing hidden.
func (rp *replica) source(ctx context.Context, project string) orchapi.WorkSource {
	v := rp.view()
	ws := orchapi.WorkSource{Project: project, Name: "authority", Prefix: "authority", Verbs: []string{}, OK: v.State == "ok",
		Sync: &orchapi.WorkSync{State: orchapi.SyncOK, LagSeconds: v.AgeSecs, Error: v.LastError}}
	if v.Synced != nil {
		ws.Sync.Synced = *v.Synced
	}
	switch v.State {
	case "loading":
		ws.Sync.State, ws.Error = orchapi.SyncLoading, "first snapshot in progress"
	case "stale":
		ws.Sync.State = orchapi.SyncStale
		ws.Error = "replica copy is stale"
		if v.LastError != "" {
			ws.Error += ": " + v.LastError
		}
	}
	if v.Pending > 0 {
		ws.Sync.Mode = "outbox " + strconv.Itoa(v.Pending)
	}
	rp.mu.Lock()
	ws.Backup, ws.Started = rp.backup, rp.started
	rp.mu.Unlock()
	if v.State != "loading" {
		var n int
		if rp.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&n) == nil {
			ws.Sync.Items = n
		}
	}
	return ws
}

// ---- mail ----

// A machine's copy can contain its owner's mail; its agents are distinct readers.
const personalCopySQL = "(COALESCE(json_extract(mail.json,'$.audience'),'') = '' OR (json_extract(mail.json,'$.audience') = 'participants' AND EXISTS (SELECT 1 FROM json_each(json_extract(mail.json,'$.participants')) WHERE value = ?)))"

func (rp *replica) findMail(ctx context.Context, raw string, reader ...string) (orchapi.Mail, bool) {
	id := strings.ToUpper(strings.TrimSpace(raw))
	if len(id) < 6 || len(id) > 26 {
		return orchapi.Mail{}, false
	}
	var js string
	filter, args := "", []any{id}
	if len(reader) > 0 {
		filter = " AND " + personalCopySQL
		args = append(args, reader[0])
	}
	err := rp.db.QueryRowContext(ctx, `SELECT json FROM mail WHERE id = ?`+filter, args...).Scan(&js)
	if err == sql.ErrNoRows && len(id) < 26 {
		rows, qerr := rp.db.QueryContext(ctx, `SELECT json FROM mail WHERE id LIKE ? ESCAPE '\'`+filter+` LIMIT 2`, append([]any{"%" + likeEscape(id)}, args[1:]...)...)
		if qerr == nil {
			n := 0
			for rows.Next() {
				n++
				_ = rows.Scan(&js)
			}
			rows.Close()
			if n == 1 {
				err = nil
			}
		}
	}
	if err != nil {
		return orchapi.Mail{}, false
	}
	var m orchapi.Mail
	return m, json.Unmarshal([]byte(js), &m) == nil
}

// mailWhere lists mail matching where, newest first; withQueued adds the
// caller's queued mail the copy does not show yet (its sent list).
func (rp *replica) mailWhere(ctx context.Context, snap api.Remote, me string, withQueued bool, where string, args []any, limit int) ([]orchapi.Mail, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 5000)
	rows, err := rp.db.QueryContext(ctx, `SELECT json FROM mail WHERE (`+where+`) AND `+personalCopySQL+` ORDER BY created DESC, id DESC LIMIT ?`, append(args, me, limit)...)
	if err != nil {
		return nil, err
	}
	out := []orchapi.Mail{}
	have := map[string]bool{}
	for rows.Next() {
		var js string
		if rows.Scan(&js) != nil {
			continue
		}
		var m orchapi.Mail
		if json.Unmarshal([]byte(js), &m) == nil {
			out = append(out, m)
			have[m.ID] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !withQueued {
		return out, nil
	}
	es, err := rp.overlays(ctx, "kind IN ('mail', 'reply')")
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if have[e.id] || e.authorOr("human:"+snap.Identity) != me {
			continue
		}
		var req orchapi.SendMailRequest
		_ = json.Unmarshal([]byte(e.body), &req)
		if e.kind == "reply" {
			var rr orchapi.ReplyRequest
			_ = json.Unmarshal([]byte(e.body), &rr)
			req = orchapi.SendMailRequest{Body: rr.Body, Needs: rr.Needs, ReplyTo: e.item}
			if p, ok := rp.findMail(ctx, e.item); ok {
				req.To, req.Area, req.Ref, req.Audience = p.From, p.Area, p.Ref, p.Audience
				if p.From == me {
					req.To = p.To
				}
			}
		}
		m := orchapi.Mail{ID: e.id, Project: snap.Project.ID, Area: req.Area, From: me, To: req.To, Via: "queued", Ref: req.Ref,
			Kind: req.Kind, Needs: req.Needs, Body: textclean.Clean(req.Body), ReplyTo: req.ReplyTo, Created: e.created, Audience: req.Audience}
		if req.Audience == orchapi.MailAudienceParticipants {
			m.Participants = []string{me, req.To}
		}
		if m.Kind == "" {
			m.Kind = orchapi.MailKindNote
		}
		if m.Needs == "" {
			m.Needs = orchapi.NeedsAny
		}
		out = append([]orchapi.Mail{m}, out...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// announceWindow is how long an announcement stays in a session's inbox
// (the mail module's AnnounceWindow).
const announceWindow = 24 * time.Hour

// inbox is the caller's mail in the copy, as the authority would answer it
// (the mail module's boxWhere): a session gets its direct mail, its
// machine's group mail for the project and unseen announcements, never
// needs=human mail; the replica's identity (its machine's human) gets its
// own mail and needs=human mail sent to this machine's sessions and group.
func (s *Service) inbox(ctx context.Context, rp *replica, snap api.Remote, me string, all, reports bool, limit int, mark bool) ([]orchapi.Mail, error) {
	if reports {
		return s.reports(ctx, rp, snap, me, all, limit)
	}
	return s.inboxMark(ctx, rp, snap, me, all, limit, mark)
}

// inboxMark is inbox; mark records the announcements shown as seen (a
// notice only counts them).
func (s *Service) inboxMark(ctx context.Context, rp *replica, snap api.Remote, me string, all bool, limit int, mark bool) ([]orchapi.Mail, error) {
	open := "acked = 0"
	if all {
		open = "1=1"
	}
	a, err := api.ParseAddress(me)
	if err != nil {
		return []orchapi.Mail{}, nil
	}
	m := s.d.Config.Machine
	var where string
	var args []any
	switch a.Kind {
	case api.KindSession:
		parts := []string{"(recipient = ? AND " + open + ")"}
		args = append(args, me)
		if ss, ok := orchcore.Sessions(s.d).Lookup(ctx, me); ok && ss.Project == snap.Project.ID {
			parts = append(parts, "(recipient = ? AND "+open+")")
			args = append(args, "machine:"+m)
			ann := "(recipient = 'all' AND created >= ? AND NOT EXISTS (SELECT 1 FROM mail_seen WHERE mail_id = mail.id AND address = ?))"
			if all {
				ann = "(recipient = 'all' AND created >= ? AND ? != '')"
			}
			parts = append(parts, ann)
			args = append(args, store.Millis(rp.authorityNow().Add(-announceWindow)), me)
		}
		where = "needs != 'human' AND " + notReport + " AND (" + strings.Join(parts, " OR ") + ")"
	case api.KindHuman:
		where = notReport + " AND ((recipient = ? AND " + open + ") OR (needs = 'human' AND " + open + " AND (recipient = ? OR (recipient >= ? AND recipient < ?))))"
		args = []any{me, "machine:" + m, m + "/", m + "0"}
	case api.KindMachine:
		where, args = notReport+" AND recipient = ? AND "+open, []any{me}
	default:
		return []orchapi.Mail{}, nil
	}
	ms, err := rp.mailWhere(ctx, snap, me, false, "("+where+")", args, limit)
	if err != nil {
		return nil, err
	}
	if !all {
		ms = s.dropTaken(ctx, rp, snap, me, ms)
	}
	if mark && a.Kind == api.KindSession {
		for _, x := range ms {
			if x.Recipient == "all" {
				_ = rp.exec(ctx, `INSERT OR IGNORE INTO mail_seen (mail_id, address) VALUES (?, ?)`, x.ID, me)
			}
		}
	}
	return ms, nil
}

// mailQuery is GET /v0/mail from the copy.
func (rp *replica) mailQuery(ctx context.Context, snap api.Remote, me string, q url.Values, limit int) ([]orchapi.Mail, error) {
	conds, args := []string{"1=1"}, []any{}
	for _, f := range []struct{ key, col string }{{"to", "json_extract(json, '$.to')"}, {"from", "sender"}, {"ref", "json_extract(json, '$.ref')"}, {"needs", "needs"}} {
		v := strings.TrimSpace(q.Get(f.key))
		if v == "" {
			continue
		}
		if f.key != "needs" {
			if a, err := api.ParseAddress(v); err == nil {
				v = a.String()
			}
		}
		conds, args = append(conds, f.col+" = ?"), append(args, v)
	}
	if q.Get("open") == "1" {
		conds = append(conds, "acked = 0")
	}
	return rp.mailWhere(ctx, snap, me, false, strings.Join(conds, " AND "), args, limit)
}

// thread is m's thread, oldest first (by reply_to).
func (rp *replica) thread(ctx context.Context, m orchapi.Mail, me string) ([]orchapi.Mail, error) {
	root := m
	for i := 0; root.ReplyTo != "" && i < 500; i++ {
		p, ok := rp.findMail(ctx, root.ReplyTo, me)
		if !ok {
			break
		}
		root = p
	}
	out := []orchapi.Mail{root}
	queue := []string{root.ID}
	for len(queue) > 0 && len(out) < 500 {
		id := queue[0]
		queue = queue[1:]
		rows, err := rp.db.QueryContext(ctx, `SELECT json FROM mail WHERE reply_to = ? AND `+personalCopySQL+` ORDER BY created, id`, id, me)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var js string
			var c orchapi.Mail
			if rows.Scan(&js) == nil && json.Unmarshal([]byte(js), &c) == nil {
				out = append(out, c)
				queue = append(queue, c.ID)
			}
		}
		rows.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ---- writes ----

// copyWrite queues a write and answers at once (comments, status, links,
// mail, acks: optimistic, overlaid on the copy), or, for a claim, release
// or lease, waits up to ClaimWait for the authority's answer while it is
// reachable and otherwise answers 202 queued.
func (s *Service) copyWrite(w http.ResponseWriter, r *http.Request, x *remote, rp *replica, area string, body []byte) bool {
	ctx := r.Context()
	snap := x.Snapshot()
	as, me := s.callerAddr(r, snap)
	path := r.URL.Path
	fail := func(err error) bool { api.WriteError(w, err); return true }
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v0/work/"):
		id, verb := segs(path, "/v0/work/")
		it, base, ok := rp.findItem(ctx, id)
		if !ok {
			return false // not in the copy (yet): the authority answers
		}
		wpath := "/v0/work/" + url.PathEscape(it.ID) + "/" + verb
		switch verb {
		case "comment":
			var req orchapi.CommentRequest
			if err := json.Unmarshal(body, &req); err != nil {
				return fail(api.Errorf(api.CodeBadRequest, "bad JSON: %v", err))
			}
			req.Body = textclean.Clean(req.Body)
			body, _ = json.Marshal(req)
			if textclean.Blank(req.Body) {
				return fail(api.Errorf(api.CodeBadRequest, "empty comment"))
			}
			if len(req.Body) > 64<<10 {
				return fail(api.Errorf(api.CodeBadRequest, "comment is over %d bytes", 64<<10))
			}
			e, err := rp.enqueue(ctx, "comment", http.MethodPost, wpath, body, as, it.ID, "")
			if err != nil {
				return fail(err)
			}
			api.WriteJSON(w, http.StatusOK, orchapi.Comment{ID: e.id, Item: it.ID, Author: me, Body: req.Body, Created: e.created})
			return true
		case "status":
			var req orchapi.StatusRequest
			if err := json.Unmarshal(body, &req); err != nil {
				return fail(api.Errorf(api.CodeBadRequest, "bad JSON: %v", err))
			}
			if !orchapi.ValidStatus(req.Status) {
				return fail(api.Errorf(api.CodeBadRequest, "bad status %q (open, claimed, in_review, done)", req.Status))
			}
			if it.Source == "local" && req.Status == orchapi.WorkClaimed {
				return fail(api.Errorf(api.CodeBadRequest, "an item is claimed by taking a lease").WithHint("zm claim " + it.ID))
			}
			_ = base // read again by enqueue, in the transaction that queues it
			e, err := rp.enqueue(ctx, "status", http.MethodPost, wpath, body, as, it.ID, "")
			if err != nil {
				return fail(err)
			}
			v, err := rp.whole(ctx, it, false)
			if err != nil {
				return fail(err)
			}
			v.Comments, v.Write = nil, e.id
			api.WriteJSON(w, http.StatusOK, v)
			return true
		case "link":
			return s.copyLink(w, r, rp, it, wpath, body, as)
		case "claim":
			return s.leaseWrite(w, r, x, rp, "claim", http.MethodPost, wpath, body, as, it.ID)
		case "release":
			if forced(body) {
				return fail(api.Errorf(api.CodeForbidden, "release --force is the authority's: it needs operate there, from its own socket").
					WithHint("ask an owner on " + snap.Authority + " to run zm release --force"))
			}
			return s.leaseWrite(w, r, x, rp, "release", http.MethodPost, wpath, body, as, it.ID)
		}
		return false
	case r.Method == http.MethodPost && path == "/v0/leases":
		var req orchapi.ClaimRequest
		if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Key) == "" {
			return fail(api.Errorf(api.CodeBadRequest, "a lease needs a key"))
		}
		return s.leaseWrite(w, r, x, rp, "lease", http.MethodPost, path, body, as, strings.TrimSpace(req.Key))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v0/leases/"):
		key, _ := segs(path, "/v0/leases/")
		if forced(body) {
			return fail(api.Errorf(api.CodeForbidden, "release --force is the authority's: it needs operate there, from its own socket").
				WithHint("ask an owner on " + snap.Authority + " to run zm release --force"))
		}
		return s.leaseWrite(w, r, x, rp, "release", http.MethodDelete, "/v0/leases/"+url.PathEscape(key), body, as, key)
	case r.Method == http.MethodPost && path == "/v0/mail":
		var req orchapi.SendMailRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return fail(api.Errorf(api.CodeBadRequest, "bad JSON: %v", err))
		}
		reply := false // a reply_to that answers its parent's other party, as the authority judges it
		if req.ReplyTo != "" {
			if parent, ok := rp.findMail(ctx, req.ReplyTo, me); ok {
				if parent.Origin != nil {
					return fail(api.Errorf(api.CodeConflict, "imported mail has no verified reply transport"))
				}
				reply = replyParty(parent.From, parent.To, me) == strings.TrimSpace(req.To)
			}
		}
		if req.ClientRequestID == "" {
			req.Body = textclean.Clean(req.Body)
		}
		body, _ = json.Marshal(req)
		if textclean.Blank(req.Body) {
			return fail(api.Errorf(api.CodeBadRequest, "empty mail body"))
		}
		if strings.EqualFold(strings.TrimSpace(req.Kind), orchapi.MailKindNotice) {
			return fail(api.Errorf(api.CodeBadRequest, "kind %q: want note or announce", req.Kind)) // only a daemon sends one
		}
		to, toAddr, members, err := mailTo(me, req.To)
		if err != nil {
			return fail(err)
		}
		if !reply && members == nil {
			// Check the recipient the authority will use. A queued write would
			// already have told the agent it was sent.
			recipient := to.String()
			if to.Kind == api.KindWork {
				if it, _, ok := rp.findItem(ctx, to.String()); ok {
					if h, herr := rp.holder(ctx, it); herr == nil {
						recipient = itemMailTo(it, h, recipient)
					}
				}
			}
			needs := orchapi.Needs(strings.ToLower(strings.TrimSpace(string(req.Needs))))
			if err := orchapi.RefusePersonMail(me, req.Kind, recipient, needs); err != nil {
				return fail(err) // the authority refuses it too: never queue it
			}
			if err := orchapi.RefuseChoreMail(ctx, s.judgeCaller(), me, req.Kind, recipient, needs, textclean.Clean(req.Body)); err != nil {
				return fail(err) // a report asking the reader to do a step: refused there too
			}
		}
		if req.Area == "" && area != "" {
			req.Area = area
			body, _ = json.Marshal(req)
		}
		s.mailQueued(as, req.Needs)
		return s.syncWrite(w, r, rp, "mail", http.MethodPost, "/v0/mail", body, as, "", func(e outEntry, why string) (int, any) {
			m := orchapi.Mail{ID: e.id, Project: snap.Project.ID, Area: req.Area, From: me, To: toAddr, Via: "queued (" + why + ")", Ref: req.Ref,
				Kind: req.Kind, Needs: req.Needs, Body: textclean.Clean(req.Body), ReplyTo: req.ReplyTo, Created: e.created, Audience: req.Audience}
			if req.Audience == orchapi.MailAudienceParticipants {
				m.Participants = []string{me, toAddr}
			}
			if members != nil {
				m.Audience, m.Participants, m.Members = orchapi.MailAudienceParticipants, members, members
			}
			if req.ReplyTo != "" {
				if parent, ok := rp.findMail(ctx, req.ReplyTo, me); ok {
					m.Audience, m.Participants = parent.Audience, parent.Participants
				} else {
					// Unresolved inherited audience must not expose an ID in state.
					m.Audience = orchapi.MailAudienceParticipants
				}
			}
			if m.Kind == "" {
				m.Kind = orchapi.MailKindNote
			}
			if m.Needs == "" {
				m.Needs = orchapi.NeedsAny
			}
			fileReport(&m)
			s.waitingOn(as, m)
			return http.StatusOK, m
		}, me)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v0/mail/"):
		id, verb := segs(path, "/v0/mail/")
		parent, ok := rp.findMail(ctx, id, me)
		if !ok {
			return false
		}
		wpath := "/v0/mail/" + parent.ID + "/" + verb
		switch verb {
		case "ack":
			return s.syncWrite(w, r, rp, "ack", http.MethodPost, wpath, body, as, parent.ID, func(e outEntry, why string) (int, any) {
				t := e.created
				return http.StatusOK, orchapi.AckResponse{ID: parent.ID, Acked: true, AckedBy: me, AckedAt: &t}
			}, me)
		case "reply":
			if parent.Origin != nil {
				return fail(api.Errorf(api.CodeConflict, "imported mail has no verified reply transport"))
			}
			if parent.Kind == orchapi.MailKindNotice {
				return fail(api.Errorf(api.CodeBadRequest, "mail %s is a notice from zm: nobody reads a reply", parent.ID).WithHint("zm ack " + parent.ID))
			}
			var req orchapi.ReplyRequest
			if err := json.Unmarshal(body, &req); err != nil || textclean.Blank(req.Body) {
				return fail(api.Errorf(api.CodeBadRequest, "empty mail body"))
			}
			if req.ClientRequestID == "" {
				req.Body = textclean.Clean(req.Body)
			}
			body, _ = json.Marshal(req)
			to := parent.From
			if to == me {
				to = parent.To
			}
			s.mailQueued(as, req.Needs)
			return s.syncWrite(w, r, rp, "reply", http.MethodPost, wpath, body, as, parent.ID, func(e outEntry, why string) (int, any) {
				m := orchapi.Mail{ID: e.id, Project: snap.Project.ID, Area: parent.Area, From: me, To: to, Via: "queued (" + why + ")", Ref: parent.Ref,
					Kind: orchapi.MailKindNote, Needs: req.Needs, Body: textclean.Clean(req.Body), ReplyTo: parent.ID, Created: e.created, Audience: parent.Audience, Participants: parent.Participants}
				if m.Needs == "" {
					m.Needs = orchapi.NeedsAny
				}
				fileReport(&m)
				s.waitingOn(as, m)
				return http.StatusOK, m
			}, me)
		}
	}
	return false
}

// cursorNow is the copy's cursor (a base for an item it has no change of).
func (rp *replica) cursorNow() string {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.cursor
}

// leaseWrite queues a claim, release or lease and, while the authority is
// reachable, waits up to ClaimWait for its answer (claims stay an atomic
// compare-and-set at the authority). Unreachable, or no answer in time: 202
// with the write's id; its outcome shows in the outbox, and a claim that
// loses there comes back as a conflict (the session waits for a human).
func (s *Service) leaseWrite(w http.ResponseWriter, r *http.Request, x *remote, rp *replica, kind, method, path string, body []byte, as, item string) bool {
	snap := x.Snapshot()
	if kind == "claim" || kind == "lease" {
		var req orchapi.ClaimRequest
		if json.Unmarshal(body, &req) == nil {
			req.Note = textclean.Clean(req.Note)
			body, _ = json.Marshal(req)
		}
	}
	if noQueue(r) {
		return s.leaseNoQueue(w, r, x, rp, kind, method, path, body, as)
	}
	return s.syncWrite(w, r, rp, kind, method, path, body, as, item, func(e outEntry, why string) (int, any) {
		return http.StatusAccepted, api.QueuedResponse{Queued: true, Write: e.id, Project: snap.Project.ID, Item: item,
			Note: why + ": queued; it runs when the authority answers (zm team outbox)"}
	})
}

// syncWrite queues a write and, while the authority is reachable, waits up
// to ClaimWait for the authority's answer and relays it (so a claim's
// conflict, a mail's routing or a refusal reach the caller as they would
// directly). Otherwise it answers queued(entry, why) at once.
func (s *Service) syncWrite(w http.ResponseWriter, r *http.Request, rp *replica, kind, method, path string, body []byte, as, item string,
	queued func(e outEntry, why string) (int, any), principal ...string) bool {
	actor := as
	if len(principal) > 0 {
		actor = principal[0]
	}
	id, err := rp.reserveMailRequest(r.Context(), kind, method, path, body, actor)
	if err != nil {
		api.WriteError(w, err)
		return true
	}
	ch := rp.waitFor(id) // before the drain can see the entry
	e, err := rp.enqueueID(r.Context(), id, kind, method, path, body, as, item, "", rp.writeVisibility(r.Context(), kind, path, body, actor))
	if err != nil {
		rp.stopWaiting(id, ch)
		api.WriteError(w, err)
		return true
	}
	if e.state != api.OutboxPending && e.result != "" {
		rp.stopWaiting(id, ch)
		var er api.ErrorResponse
		if json.Unmarshal([]byte(e.result), &er) == nil && er.Error != nil {
			api.WriteError(w, er.Error)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(e.result))
		}
		return true
	}
	answerQueued := func(why string) bool {
		code, v := queued(e, why)
		api.WriteJSON(w, code, v)
		return true
	}
	if !rp.isReachable() {
		rp.stopWaiting(id, ch)
		select {
		case res := <-ch: // answered already (it came back while we looked)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(res.Status)
			_, _ = w.Write(res.Body)
			return true
		default:
		}
		return answerQueued("the authority is unreachable")
	}
	t := time.NewTimer(ClaimWait)
	defer t.Stop()
	select {
	case res := <-ch:
		rp.stopWaiting(e.id, ch)
		var er api.ErrorResponse
		if res.Status/100 == 2 || (json.Unmarshal(res.Body, &er) == nil && er.Error != nil) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(res.Status)
			_, _ = w.Write(res.Body)
			return true
		}
		api.WriteError(w, api.Errorf(api.CodeInternal, "the authority answered %d", res.Status))
		return true
	case <-t.C:
	case <-r.Context().Done():
	}
	// Stop waiting, then look once more: the answer may have been handed
	// over just now (answer sends under the same lock), and then it is
	// this caller's, not a queued write's.
	rp.stopWaiting(e.id, ch)
	select {
	case res := <-ch:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
		return true
	default:
	}
	if r.Context().Err() != nil {
		return true
	}
	return answerQueued("no answer from the authority within " + ClaimWait.String())
}
