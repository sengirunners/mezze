package orchclient

// Owned by the mail builder. One method per /v0/mail endpoint in API.md.

import (
	"context"
	"net/http"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func mailPath(id, suffix string) string { return "/v0/mail/" + client.Seg(id) + suffix }

// Send is POST /v0/mail.
func Send(ctx context.Context, c *client.Client, req orchapi.SendMailRequest) (orchapi.Mail, error) {
	var m orchapi.Mail
	return m, c.Do(ctx, http.MethodPost, "/v0/mail", req, &m)
}

// Mail is GET /v0/mail (a query over the mail the caller may read).
func Mail(ctx context.Context, c *client.Client, q orchapi.MailQuery) ([]orchapi.Mail, error) {
	var l orchapi.MailList
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/mail", "project", q.Project, "to", q.To, "from", q.From, "ref", q.Ref,
		"needs", string(q.Needs), "open", client.Bool(q.Open), "limit", client.Itoa(int64(q.Limit)),
		"since", timeParam(q.Since), "until", timeParam(q.Until)), nil, &l)
	return l.Mail, err
}

// Inbox is GET /v0/mail/inbox.
func Inbox(ctx context.Context, c *client.Client, q orchapi.InboxQuery) ([]orchapi.Mail, error) {
	return inbox(ctx, c, q, false)
}

// PeekInbox is GET /v0/mail/inbox/peek: the same mail, and the
// announcements in it stay unseen (a poll that must not eat them). A daemon
// older than the route answers not_found.
func PeekInbox(ctx context.Context, c *client.Client, q orchapi.InboxQuery) ([]orchapi.Mail, error) {
	return inbox(ctx, c, q, true)
}

func inbox(ctx context.Context, c *client.Client, q orchapi.InboxQuery, peek bool) ([]orchapi.Mail, error) {
	var l orchapi.MailList
	err := c.Do(ctx, http.MethodGet, client.Query(inboxPath(peek), "project", q.Project, "all", client.Bool(q.All), "limit", client.Itoa(int64(q.Limit)), "box", func() string {
		if q.Reports {
			return orchapi.InboxBoxReports
		}
		return ""
	}()), nil, &l)
	return l.Mail, err
}

// Sent is GET /v0/mail/sent.
func Sent(ctx context.Context, c *client.Client, project string, limit int) ([]orchapi.Mail, error) {
	var l orchapi.MailList
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/mail/sent", "project", project, "limit", client.Itoa(int64(limit))), nil, &l)
	return l.Mail, err
}

// Notice is GET /v0/mail/notice.
func Notice(ctx context.Context, c *client.Client) (orchapi.Notice, error) {
	var n orchapi.Notice
	return n, c.Do(ctx, http.MethodGet, "/v0/mail/notice", nil, &n)
}

// HookNotice shares per-session delivery records with the session hook route.
func HookNotice(ctx context.Context, c *client.Client) (orchapi.Notice, error) {
	var n orchapi.Notice
	return n, c.Do(ctx, http.MethodGet, "/v0/mail/notice?once=1", nil, &n)
}

// GetMail is GET /v0/mail/{id} (id or a unique suffix of 6+ chars).
func GetMail(ctx context.Context, c *client.Client, id string) (orchapi.Mail, error) {
	var m orchapi.Mail
	return m, c.Do(ctx, http.MethodGet, mailPath(id, ""), nil, &m)
}

// Ack is POST /v0/mail/{id}/ack.
func Ack(ctx context.Context, c *client.Client, id string) (orchapi.AckResponse, error) {
	var a orchapi.AckResponse
	return a, c.Do(ctx, http.MethodPost, mailPath(id, "/ack"), nil, &a)
}

// Reply is POST /v0/mail/{id}/reply.
func Reply(ctx context.Context, c *client.Client, id string, req orchapi.ReplyRequest) (orchapi.Mail, error) {
	var m orchapi.Mail
	return m, c.Do(ctx, http.MethodPost, mailPath(id, "/reply"), req, &m)
}

// Thread is GET /v0/mail/{id}/thread.
func Thread(ctx context.Context, c *client.Client, id string) ([]orchapi.Mail, error) {
	var l orchapi.MailList
	err := c.Do(ctx, http.MethodGet, mailPath(id, "/thread"), nil, &l)
	return l.Mail, err
}

// ImportLegacyMail submits a bounded full-body recovery artifact to the local daemon.
func ImportLegacyMail(ctx context.Context, c *client.Client, req orchapi.LegacyMailImportRequest) (orchapi.LegacyMailImportResult, error) {
	var out orchapi.LegacyMailImportResult
	err := c.Do(ctx, http.MethodPost, "/v0/mail/imports", req, &out)
	return out, err
}

func inboxPath(peek bool) string {
	if peek {
		return "/v0/mail/inbox/peek"
	}
	return "/v0/mail/inbox"
}

// MailTriage is the resolved project's triage settings (GET /v0/mail/triage).
func MailTriage(ctx context.Context, c *client.Client) (orchapi.MailTriage, error) {
	var t orchapi.MailTriage
	err := c.Do(ctx, http.MethodGet, "/v0/mail/triage", nil, &t)
	return t, err
}

// SetMailTriage changes them (PUT /v0/mail/triage; a project admin).
func SetMailTriage(ctx context.Context, c *client.Client, u orchapi.MailTriageUpdate) (orchapi.MailTriage, error) {
	var t orchapi.MailTriage
	err := c.Do(ctx, http.MethodPut, "/v0/mail/triage", u, &t)
	return t, err
}

func timeParam(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
