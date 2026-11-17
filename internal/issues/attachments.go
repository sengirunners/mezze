package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/issues/imgsafe"
	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// Attachments (SPEC "Native issues", attachments): a few images or HTML mockups on a work
// item, a decision's brief. The bytes live at the authority only, in the
// store's Attacher (never in the issue rows, events, a move or an export);
// a history row carries each one's metadata so the change feed, events and
// the work mirror see it come and go. They expire (default 14 days) or are
// removed by their attacher or an area admin; both are hard deletes.

// sweepEvery is how often expired attachments are removed.
var sweepEvery = 5 * time.Minute

// cleanSlots bounds images decoded at once (each at most imgsafe.MaxDecodeBytes).
var cleanSlots = make(chan struct{}, 2)

func (s *Service) attachmentRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/issues/{id}/attachments", core.Authed, "read on the issue's area: its live attachments' metadata", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		ats, err := s.issueAttachments(r.Context(), sub, l.project, l.id)
		return orchapi.IssueAttachmentList{Attachments: ats}, err
	}))
	r.HandleFunc("GET /v0/issues/{id}/attachments/{aid}", core.Authed, "read on the issue's area: the image's bytes (nosniff, inline); expired answers not_found", s.attachmentBytes)
	r.HandleFunc("POST /v0/issues/{id}/attachments", core.Authed, "whoever may comment (write, not guest, on the issue's area): the body is a PNG, JPEG or WebP image or an HTML page (kept verbatim, at most 1 MiB, UTF-8); ?expires= (default 14d, at most 90d); at most 8 live", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		return s.attach(r.Context(), sub, l, r.Body, r.URL.Query().Get("expires"))
	}))
	r.HandleFunc("DELETE /v0/issues/{id}/attachments/{aid}", core.Authed, "its attacher (any session of their identity) or an admin of the issue's area, with write there: a hard delete", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		return s.removeAttachment(r.Context(), sub, l, r.PathValue("aid"))
	}))
}

func (s *Service) attacher() (istore.Attacher, error) {
	at, ok := s.st.(istore.Attacher)
	if !ok {
		return nil, api.Errorf(api.CodeNotImplemented, "this issue store keeps no attachments")
	}
	return at, nil
}

// parseExpiry reads an attachment's lifetime: "" is the default (14 days),
// "<n>d" days, else a Go duration (36h); at most 90 days.
func parseExpiry(v string) (time.Duration, error) {
	if v == "" {
		return orchapi.IssueAttachmentTTL, nil
	}
	var d time.Duration
	var err error
	if n, ok := strings.CutSuffix(v, "d"); ok {
		var days int
		days, err = strconv.Atoi(n)
		d = time.Duration(min(max(days, -1), 91)) * 24 * time.Hour // no overflow
	} else {
		d, err = time.ParseDuration(v)
	}
	switch {
	case err != nil:
		return 0, api.Errorf(api.CodeBadRequest, "bad expiry %q (e.g. 14d or 36h)", v)
	case d <= 0:
		return 0, api.Errorf(api.CodeBadRequest, "an expiry is after now")
	case d > orchapi.MaxIssueAttachmentTTL:
		return 0, api.Errorf(api.CodeBadRequest, "an attachment lives at most 90 days (%s asked)", v)
	}
	return d, nil
}

// cleanImage re-encodes an image upload (imgsafe), answering zm's errors; an
// HTML page is kept as it came (it is only ever served sandboxed).
func cleanImage(ctx context.Context, body []byte) ([]byte, string, error) {
	if imgsafe.Sniff(body) == "" && imgsafe.IsHTML(body) {
		if len(body) > imgsafe.MaxBytes {
			return nil, "", api.Errorf(api.CodeTooLarge, "an HTML page is at most %d bytes", imgsafe.MaxBytes)
		}
		return body, imgsafe.HTML, nil
	}
	select {
	case cleanSlots <- struct{}{}:
		defer func() { <-cleanSlots }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	data, typ, err := imgsafe.Clean(body)
	switch {
	case errors.Is(err, imgsafe.ErrType):
		return nil, "", api.Errorf(api.CodeBadRequest, "an attachment is a PNG, JPEG or WebP image or an HTML page (UTF-8), told by its bytes (no SVG)")
	case errors.Is(err, imgsafe.ErrTooLarge):
		return nil, "", api.Errorf(api.CodeTooLarge, "%v", err)
	case err != nil:
		return nil, "", api.Errorf(api.CodeBadRequest, "%v", err)
	}
	return data, typ, nil
}

func (s *Service) attach(ctx context.Context, sub *authz.Subject, l located, in io.Reader, expires string) (orchapi.IssueAttachment, error) {
	at, err := s.attacher()
	if err != nil {
		return orchapi.IssueAttachment{}, err
	}
	ttl, err := parseExpiry(expires)
	if err != nil {
		return orchapi.IssueAttachment{}, err
	}
	it, err := l.ps.Get(ctx, l.id, l.vis)
	if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	// Refused before the body is read or decoded: a reader who may not
	// comment costs nothing and learns nothing more.
	if err := s.canWrite(sub, l.project, it.Area, l.id); err != nil {
		return orchapi.IssueAttachment{}, err
	}
	body, err := io.ReadAll(io.LimitReader(in, orchapi.MaxIssueAttachmentUpload+1))
	if err != nil {
		return orchapi.IssueAttachment{}, api.Errorf(api.CodeBadRequest, "reading the image: %v", err)
	}
	if len(body) > orchapi.MaxIssueAttachmentUpload {
		return orchapi.IssueAttachment{}, api.Errorf(api.CodeTooLarge, "an image is at most %d bytes before re-encoding", orchapi.MaxIssueAttachmentUpload)
	}
	data, typ, err := cleanImage(ctx, body)
	if err != nil {
		return orchapi.IssueAttachment{}, err
	}
	sum := sha256.Sum256(data)
	now := s.now().UTC().Truncate(time.Millisecond)
	a := istore.Attachment{IssueID: l.id, Identity: sub.Identity.ID, IssueAttachment: orchapi.IssueAttachment{
		ID: ids.New(), Type: typ, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		Created: now, CreatedBy: sub.Address, Expires: now.Add(ttl)}}
	as, err := at.Attachments(ctx, l.project, true)
	if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	if err := as.Add(ctx, a, data, orchapi.MaxIssueAttachments, now); errors.Is(err, istore.ErrTooMany) {
		return orchapi.IssueAttachment{}, api.Errorf(api.CodeConflict, "%s already has %d attachments, the most an item keeps", l.id, orchapi.MaxIssueAttachments).
			WithHint("remove one first: zm issues attachments rm " + l.id + " <attachment-id>")
	} else if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	note := istore.Mutation{Op: istore.OpAttachmentNote, Note: &istore.AttachmentNote{Kind: istore.KindAttached, Attachment: a.IssueAttachment}}
	if _, _, err := s.write(ctx, sub, l, note); err != nil {
		if _, derr := as.Delete(context.WithoutCancel(ctx), []string{a.ID}); derr != nil {
			s.warn("attachment not taken back", "issue", l.id, "attachment", a.ID, "err", derr)
		}
		return orchapi.IssueAttachment{}, err
	}
	out := a.IssueAttachment
	out.CreatedBy = s.d.RedactFor(ctx, sub, out.CreatedBy)
	return out, nil
}

// issueAttachments is id's live attachments as sub sees them (addresses
// redacted), [] when none.
func (s *Service) issueAttachments(ctx context.Context, sub *authz.Subject, project, id string) ([]orchapi.IssueAttachment, error) {
	m, err := s.attachmentsOf(ctx, project, []string{id})
	if err != nil {
		return nil, err
	}
	out := m[id]
	if out == nil {
		out = []orchapi.IssueAttachment{}
	}
	for i := range out {
		out[i].CreatedBy = s.d.RedactFor(ctx, sub, out[i].CreatedBy)
	}
	return out, nil
}

// attachmentsOf is the live attachments of each issue in ids (unredacted).
// A project without any answers none, without opening anything.
func (s *Service) attachmentsOf(ctx context.Context, project string, issueIDs []string) (map[string][]orchapi.IssueAttachment, error) {
	at, ok := s.st.(istore.Attacher)
	if !ok || len(issueIDs) == 0 {
		return nil, nil
	}
	as, err := at.Attachments(ctx, project, false)
	if errors.Is(err, istore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, s.storeErr(err, "")
	}
	m, err := as.List(ctx, issueIDs, s.now())
	if err != nil {
		return nil, s.storeErr(err, "")
	}
	out := make(map[string][]orchapi.IssueAttachment, len(m))
	for id, ats := range m {
		for _, a := range ats {
			out[id] = append(out[id], a.IssueAttachment)
		}
	}
	return out, nil
}

// fillAttachments sets each issue's Attachments (a read that cannot reach
// them leaves them out and logs: an issue read never fails on its images).
// redact names the caller (nil: the work mirror, shared by every reader, so
// created_by is left empty there).
func (s *Service) fillAttachments(ctx context.Context, sub *authz.Subject, project string, its []orchapi.Issue) {
	if len(its) == 0 {
		return
	}
	idl := make([]string, len(its))
	for i := range its {
		idl[i] = its[i].ID
	}
	m, err := s.attachmentsOf(ctx, project, idl)
	if err != nil {
		s.warn("attachments unreadable", "project", project, "err", err)
		return
	}
	for i := range its {
		ats := m[its[i].ID]
		for j := range ats {
			if sub == nil {
				ats[j].CreatedBy = ""
			} else {
				ats[j].CreatedBy = s.d.RedactFor(ctx, sub, ats[j].CreatedBy)
			}
		}
		its[i].Attachments = ats
	}
}

// attachmentBytes serves one image: the issue's read check, then its bytes
// with a type the browser may not second-guess.
func (s *Service) attachmentBytes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	sc, err := s.d.ReadScope(ctx, r.URL.Query().Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	l, err := s.locateFor(ctx, sub, sc, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	aid := r.PathValue("aid")
	nf := api.NotFound("attachment", aid)
	at, err := s.attacher()
	if err != nil {
		api.WriteError(w, err)
		return
	}
	as, err := at.Attachments(ctx, l.project, false)
	if errors.Is(err, istore.ErrNotFound) {
		api.WriteError(w, nf)
		return
	}
	if err != nil {
		api.WriteError(w, s.storeErr(err, l.id))
		return
	}
	a, data, err := as.Get(ctx, l.id, aid, s.now())
	if errors.Is(err, istore.ErrNotFound) {
		api.WriteError(w, nf)
		return
	}
	if err != nil {
		api.WriteError(w, s.storeErr(err, l.id))
		return
	}
	h := w.Header()
	ct, csp := a.Type, "default-src 'none'; sandbox"
	if ct == imgsafe.HTML {
		// A static mockup: its inline styles, data: images and fonts render;
		// scripts and every fetch stay off.
		ct += "; charset=utf-8"
		csp = htmlCSP
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", `inline; filename="`+a.ID+extOf(a.Type)+`"`)
	h.Set("Content-Security-Policy", csp)
	h.Set("Cache-Control", "no-store") // a removed or expired image is gone at once
	h.Set("ETag", `"`+a.SHA256+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

const htmlCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:; sandbox"

func extOf(typ string) string {
	switch typ {
	case imgsafe.PNG:
		return ".png"
	case imgsafe.JPEG:
		return ".jpg"
	case imgsafe.WebP:
		return ".webp"
	case imgsafe.HTML:
		return ".html"
	}
	return ""
}

func (s *Service) removeAttachment(ctx context.Context, sub *authz.Subject, l located, aid string) (orchapi.IssueAttachment, error) {
	nf := api.NotFound("attachment", aid)
	at, err := s.attacher()
	if err != nil {
		return orchapi.IssueAttachment{}, err
	}
	as, err := at.Attachments(ctx, l.project, false)
	if errors.Is(err, istore.ErrNotFound) {
		return orchapi.IssueAttachment{}, nf
	}
	if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	all, err := as.Of(ctx, l.id)
	if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	var a *istore.Attachment
	for i := range all {
		if all[i].ID == aid {
			a = &all[i]
		}
	}
	if a == nil {
		return orchapi.IssueAttachment{}, nf
	}
	it, err := l.ps.Get(ctx, l.id, l.vis)
	if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	if err := s.canWrite(sub, l.project, it.Area, l.id); err != nil {
		return orchapi.IssueAttachment{}, err
	}
	// The attacher is an identity; a row without one (none today) falls
	// back to its address, never to an empty one.
	mine := a.Identity != "" && a.Identity == sub.Identity.ID
	if a.Identity == "" {
		mine = a.CreatedBy != "" && a.CreatedBy == sub.Address
	}
	if !mine && !moderates(sub, l.project, it.Area) {
		return orchapi.IssueAttachment{}, api.Errorf(api.CodeForbidden, "only its attacher, or an admin of the item's area, removes an attachment")
	}
	// Removed first, noted after: history never says removed for an image
	// still served, and a removal racing another (or the sweep) notes once.
	gone, err := as.Delete(ctx, []string{a.ID})
	if err != nil {
		return orchapi.IssueAttachment{}, s.storeErr(err, l.id)
	}
	if len(gone) == 0 {
		return orchapi.IssueAttachment{}, nf
	}
	note := istore.Mutation{Op: istore.OpAttachmentNote, Note: &istore.AttachmentNote{Kind: istore.KindAttachmentRemoved, Attachment: a.IssueAttachment}}
	if _, _, err := s.write(ctx, sub, l, note); err != nil {
		s.warn("attachment removal not noted", "issue", l.id, "attachment", a.ID, "err", err)
	}
	out := a.IssueAttachment
	out.CreatedBy = s.d.RedactFor(ctx, sub, out.CreatedBy)
	return out, nil
}

// sweepAttachments removes every expired attachment for good, then notes
// each one it removed on its issue (as zm). Rows a failed removal left are
// expired still, so the next sweep tries again; a log truncation another
// reader held up is settled on every sweep.
func (s *Service) sweepAttachments(ctx context.Context) {
	at, ok := s.st.(istore.Attacher)
	if !ok {
		return
	}
	projects, err := at.AttachmentProjects(ctx)
	if err != nil {
		s.warn("attachment sweep", "err", err)
		return
	}
	for _, p := range projects {
		as, err := at.Attachments(ctx, p, false)
		if err != nil {
			continue
		}
		if err := as.Settle(ctx); err != nil {
			s.warn("attachment sweep", "project", p, "err", err)
		}
		exp, err := as.Expired(ctx, s.now())
		if err != nil || len(exp) == 0 {
			continue
		}
		idl := make([]string, len(exp))
		for i, a := range exp {
			idl[i] = a.ID
		}
		gone, err := as.Delete(ctx, idl)
		if err != nil {
			s.warn("attachment sweep", "project", p, "err", err)
			continue
		}
		removed := map[string]bool{}
		for _, id := range gone {
			removed[id] = true
		}
		ps, err := s.st.Project(ctx, p, false)
		if err != nil {
			continue
		}
		for _, a := range exp {
			if !removed[a.ID] {
				continue // removed meanwhile, and noted there
			}
			m := istore.Mutation{Op: istore.OpAttachmentNote, ID: a.IssueID,
				Note: &istore.AttachmentNote{Kind: istore.KindAttachmentExpired, Attachment: a.IssueAttachment}}
			if _, _, err := s.mutate(ctx, s.d.System(), p, ps, m); err != nil && !api.IsCode(err, api.CodeNotFound) {
				s.warn("attachment expiry not noted", "issue", a.IssueID, "err", err)
			}
		}
	}
}

// dropAttachments removes an issue's attachments when it leaves the project
// (a move): they do not travel with it.
func (s *Service) dropAttachments(ctx context.Context, project, id string) {
	at, ok := s.st.(istore.Attacher)
	if !ok {
		return
	}
	as, err := at.Attachments(ctx, project, false)
	if err != nil {
		return
	}
	all, err := as.Of(ctx, id)
	if err != nil || len(all) == 0 {
		return
	}
	idl := make([]string, len(all))
	for i, a := range all {
		idl[i] = a.ID
	}
	if _, err := as.Delete(ctx, idl); err != nil {
		s.warn("moved issue's attachments not removed (they expire)", "issue", id, "err", err)
	}
}
