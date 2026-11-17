// Package query parses bounded work-list filters without storage or routing.
package query

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/ids"
)

// Epic splits epic=: an optional "<project ULID>/" ("" = any project) and a
// tracker item's work id (a local item is never an epic).
func Epic(raw string) (project string, a api.Address, err error) {
	id := raw
	if p, rest, ok := strings.Cut(raw, "/"); ok && ids.Valid(p) {
		project, id = p, rest
	}
	a, err = api.ParseAddress(id)
	if err != nil || a.Kind != api.KindWork || a.Source == "local" {
		return "", api.Address{}, api.Errorf(api.CodeBadRequest, "bad epic %q (a tracker item's work id, optionally <project ULID>/<id>)", raw)
	}
	return project, a, nil
}

// Page preserves the work API validation before any source is queried.
func Page(v url.Values, maxList int) (orchapi.WorkPageQuery, error) {
	q := orchapi.WorkPageQuery{In: v["in"], IDs: v["id"], Labels: v["label"], Source: strings.ToLower(strings.TrimSpace(v.Get("source"))),
		Q: textclean.Clean(v.Get("q")), Order: v.Get("order"), After: v.Get("after"), Owner: v.Get("owner"), Epic: strings.TrimSpace(v.Get("epic"))}
	if q.Owner != "" {
		if a, err := api.ParseAddress(q.Owner); err == nil {
			q.Owner = a.String()
		}
	}
	for _, st := range v["status"] {
		if !orchapi.ValidStatus(orchapi.WorkStatus(st)) {
			return q, api.Errorf(api.CodeBadRequest, "bad status %q (open, claimed, in_review, done)", st)
		}
		q.Status = append(q.Status, orchapi.WorkStatus(st))
	}
	switch q.Order {
	case "", "created", "updated":
	default:
		return q, api.Errorf(api.CodeBadRequest, "bad order %q (created, updated)", q.Order)
	}
	switch v.Get("dir") {
	case "", "desc":
	case "asc":
		q.Asc = true
	default:
		return q, api.Errorf(api.CodeBadRequest, "bad dir %q (asc, desc)", v.Get("dir"))
	}
	for k, dst := range map[string]*time.Time{"created_after": &q.CreatedAfter, "created_before": &q.CreatedBefore,
		"updated_after": &q.UpdatedAfter, "updated_before": &q.UpdatedBefore} {
		if t := v.Get(k); t != "" {
			tt, err := time.Parse(time.RFC3339Nano, t)
			if err != nil {
				return q, api.Errorf(api.CodeBadRequest, "bad %s %q (RFC 3339)", k, t)
			}
			*dst = tt
		}
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			return q, api.Errorf(api.CodeBadRequest, "bad limit %q", l)
		}
		q.Limit = n
	}
	if q.Epic != "" {
		if _, _, err := Epic(q.Epic); err != nil {
			return q, err
		}
	}
	q.Count = v.Get("count") == "1"
	if q.Count && q.Owner != "" {
		return q, api.Errorf(api.CodeBadRequest, "count=1 cannot be combined with owner= (the owner is decided after the lease overlay)")
	}
	for name, n := range map[string]int{"in": len(q.In), "id": len(q.IDs), "label": len(q.Labels), "status": len(q.Status)} {
		if n > maxList {
			return q, api.Errorf(api.CodeBadRequest, "at most %d %s= values", maxList, name)
		}
	}
	for _, w := range strings.Split(v.Get("with"), ",") {
		switch strings.TrimSpace(w) {
		case "":
		case "comments":
			q.Comments = true
		case "links":
			q.Links = true
		default:
			return q, api.Errorf(api.CodeBadRequest, "bad with %q (comments, links)", w)
		}
	}
	return q, nil
}
