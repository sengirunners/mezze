package team

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// DeliverTask sends a session's completion notice through the same replica
// copy/outbox and authority forwarding as ordinary mail and comments. No
// handler or transport is marked local: the caller is an authenticated subject.
func (s *Service) DeliverTask(ctx context.Context, sub *authz.Subject, project, area, kind, to, work, body string) (bool, error) {
	x := s.ByName(project)
	if x == nil {
		return false, nil
	}
	if sub == nil || sub.Via != authz.ViaLocal {
		return true, api.Errorf(api.CodeForbidden, "task delivery needs the local session owner")
	}
	var path string
	var payload any
	switch kind {
	case "mail":
		path = "/v0/mail"
		payload = orchapi.SendMailRequest{To: to, Body: body, Area: area}
	case "comment":
		path = "/v0/work/" + url.PathEscape(work) + "/comment"
		payload = orchapi.CommentRequest{Body: body}
	default:
		return true, api.Errorf(api.CodeBadRequest, "unknown task delivery kind")
	}
	do := func(path string, payload any, issue bool) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(core.WithSubject(ctx, sub), http.MethodPost, "http://zm"+path, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		api.SetHeader(req.Header, api.HeaderProject, project)
		rec := httptest.NewRecorder()
		rp := rep(x)
		copied := false
		if issue {
			s.forwardIssue(rec, req, x, area)
		} else {
			if rp != nil {
				copied = s.serveCopy(rec, req, x, rp, area, b)
			}
			if !copied {
				s.Forward(rec, req, x, area, true)
			}
		}
		// Copy comments always enqueue; a mail can also return 200 while queued.
		if copied && rec.Code >= 200 && rec.Code < 300 {
			queued := kind == "comment"
			var m orchapi.Mail
			if kind == "mail" && json.Unmarshal(rec.Body.Bytes(), &m) == nil && strings.HasPrefix(m.Via, "queued") {
				queued = true
			}
			if queued {
				return api.Errorf(api.CodeQueued, "queued for the project authority")
			}
		}
		if rec.Code == http.StatusAccepted {
			var q api.QueuedResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &q)
			return api.Errorf(api.CodeQueued, "%s", q.Note).WithData(q)
		}
		if rec.Code >= 200 && rec.Code < 300 {
			return nil
		}
		var v struct {
			Error *api.Error `json:"error"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &v) == nil && v.Error != nil {
			return v.Error
		}
		return api.Errorf(api.CodeUnavailable, "team task delivery failed (HTTP %d)", rec.Code)
	}
	err := do(path, payload, false)
	if kind == "comment" && api.IsCode(err, api.CodeNotFound) {
		id := strings.TrimPrefix(work, "bd:")
		if !strings.Contains(id, ":") {
			err = do("/v0/issues/"+url.PathEscape(id)+"/comments", orchapi.IssueCommentRequest{Text: body}, true)
		}
	}
	return true, err
}
