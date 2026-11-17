package orchclient

// Owned by the issues builder. One method per /v0/issues endpoint in API.md.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func issuePath(id, suffix string) string { return "/v0/issues/" + client.Seg(id) + suffix }

// IssueWrite is a write's answer: the issue as the caller sees it, and
// whether nothing changed.
type IssueWrite struct {
	orchapi.Issue
	Noop       bool  `json:"noop,omitempty"`
	LeaseFence int64 `json:"lease_fence,omitempty"` // a lease-backed claim's fence
}

// Issues is GET /v0/issues.
func Issues(ctx context.Context, c *client.Client, q orchapi.IssueQuery) (orchapi.IssueList, error) {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("project", q.Project)
	set("status", q.Status)
	if q.Priority != nil {
		v.Set("priority", strconv.Itoa(*q.Priority))
	}
	set("type", q.Type)
	set("assignee", q.Assignee)
	set("no_assignee", client.Bool(q.NoAssignee))
	for _, l := range q.Labels {
		v.Add("label", l)
	}
	for _, l := range q.LabelAny {
		v.Add("label_any", l)
	}
	set("parent", q.Parent)
	for _, id := range q.IDs {
		v.Add("id", id)
	}
	set("title_contains", q.TitleContains)
	set("desc_contains", q.DescContains)
	set("notes_contains", q.NotesContains)
	set("q", q.Q)
	set("substr", q.Substr)
	set("ready", client.Bool(q.Ready))
	set("sort", q.Sort)
	set("reverse", client.Bool(q.Reverse))
	set("limit", client.Itoa(int64(q.Limit)))
	set("all", client.Bool(q.All))
	set("deps", client.Bool(q.Deps))
	set("count", client.Bool(q.Count))
	set("blocked", client.Bool(q.Blocked))
	set("incoming", client.Bool(q.Incoming))
	set("comments", client.Bool(q.Comments))
	if q.Number != nil {
		v.Set("number", strconv.Itoa(*q.Number))
	}
	for k, t := range map[string]*time.Time{"updated_after": q.UpdatedAfter, "updated_before": q.UpdatedBefore,
		"created_after": q.CreatedAfter, "created_before": q.CreatedBefore} {
		if t != nil {
			v.Set(k, t.UTC().Format(time.RFC3339Nano))
		}
	}
	for _, a := range q.Areas {
		v.Add("area", a)
	}
	set("contains", q.Contains)
	set("claimed_by", q.ClaimedBy)
	set("after", q.After)
	set("id_after", q.IDAfter)
	path := "/v0/issues"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var l orchapi.IssueList
	return l, c.Do(ctx, http.MethodGet, path, nil, &l)
}

// CreateIssue is POST /v0/issues.
func CreateIssue(ctx context.Context, c *client.Client, req orchapi.NewIssueRequest) (orchapi.Issue, error) {
	var it orchapi.Issue
	return it, c.Do(ctx, http.MethodPost, "/v0/issues", req, &it)
}

// IssueStats is GET /v0/issues/stats.
func IssueStats(ctx context.Context, c *client.Client, project string) (orchapi.IssueStats, error) {
	var s orchapi.IssueStats
	return s, c.Do(ctx, http.MethodGet, client.Query("/v0/issues/stats", "project", project), nil, &s)
}

// IssueProjects is GET /v0/issues/projects.
func IssueProjects(ctx context.Context, c *client.Client) (orchapi.IssueProjects, error) {
	var p orchapi.IssueProjects
	return p, c.Do(ctx, http.MethodGet, "/v0/issues/projects", nil, &p)
}

// SetupIssues is POST /v0/issues/setup.
func SetupIssues(ctx context.Context, c *client.Client, prefix string) (orchapi.IssueProjectInfo, error) {
	var p orchapi.IssueProjectInfo
	return p, c.Do(ctx, http.MethodPost, "/v0/issues/setup", map[string]string{"prefix": prefix}, &p)
}

// MoveIssues is POST /v0/issues/move (from the resolved project).
func MoveIssues(ctx context.Context, c *client.Client, req orchapi.IssueMoveRequest) (orchapi.IssueMoveResult, error) {
	var r orchapi.IssueMoveResult
	return r, c.Do(ctx, http.MethodPost, "/v0/issues/move", req, &r)
}

// RestoreIssues is POST /v0/issues/restore.
func RestoreIssues(ctx context.Context, c *client.Client, req orchapi.IssueRestoreRequest) (orchapi.IssueRestoreResult, error) {
	var r orchapi.IssueRestoreResult
	return r, c.Do(ctx, http.MethodPost, "/v0/issues/restore", req, &r)
}

// Issue is GET /v0/issues/{id}.
func Issue(ctx context.Context, c *client.Client, id string) (orchapi.IssueDetail, error) {
	var d orchapi.IssueDetail
	return d, c.Do(ctx, http.MethodGet, issuePath(id, ""), nil, &d)
}

// UpdateIssue is PATCH /v0/issues/{id}.
func UpdateIssue(ctx context.Context, c *client.Client, id string, req orchapi.UpdateIssueRequest) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodPatch, issuePath(id, ""), req, &w)
}

// ClaimIssue is POST /v0/issues/{id}/claim.
func ClaimIssue(ctx context.Context, c *client.Client, id string) (IssueWrite, error) {
	return ClaimIssueAs(ctx, c, id, "")
}

// ClaimIssueAs claims id with a routing label as its assignee ("" = the
// caller's name; api.ClaimIssueRequest.Assignee).
func ClaimIssueAs(ctx context.Context, c *client.Client, id, assignee string) (IssueWrite, error) {
	return ClaimIssueWith(ctx, c, id, orchapi.ClaimIssueRequest{Assignee: assignee})
}

// ClaimIssueWith is POST /v0/issues/{id}/claim with every field of req.
func ClaimIssueWith(ctx context.Context, c *client.Client, id string, req orchapi.ClaimIssueRequest) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodPost, issuePath(id, "/claim"), req, &w)
}

// CloseIssue is POST /v0/issues/{id}/close.
func CloseIssue(ctx context.Context, c *client.Client, id string, req orchapi.CloseIssueRequest) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodPost, issuePath(id, "/close"), req, &w)
}

// ReopenIssue is POST /v0/issues/{id}/reopen.
func ReopenIssue(ctx context.Context, c *client.Client, id string, req orchapi.ReopenIssueRequest) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodPost, issuePath(id, "/reopen"), req, &w)
}

// IssueComments is GET /v0/issues/{id}/comments.
func IssueComments(ctx context.Context, c *client.Client, id string) ([]orchapi.IssueComment, error) {
	var out struct {
		Comments []orchapi.IssueComment `json:"comments"`
	}
	return out.Comments, c.Do(ctx, http.MethodGet, issuePath(id, "/comments"), nil, &out)
}

// CommentIssue is POST /v0/issues/{id}/comments.
func CommentIssue(ctx context.Context, c *client.Client, id string, req orchapi.IssueCommentRequest) (orchapi.IssueComment, error) {
	var cm orchapi.IssueComment
	return cm, c.Do(ctx, http.MethodPost, issuePath(id, "/comments"), req, &cm)
}

// EditComment is PATCH /v0/issues/{id}/comments/{cid} (the author).
func EditComment(ctx context.Context, c *client.Client, id, cid string, req orchapi.EditCommentRequest) (orchapi.IssueComment, error) {
	var cm orchapi.IssueComment
	return cm, c.Do(ctx, http.MethodPatch, issuePath(id, "/comments/"+url.PathEscape(cid)), req, &cm)
}

// DeleteComment is DELETE /v0/issues/{id}/comments/{cid} (the author, or a
// moderator): a tombstone.
func DeleteComment(ctx context.Context, c *client.Client, id, cid string) (orchapi.IssueComment, error) {
	var cm orchapi.IssueComment
	return cm, c.Do(ctx, http.MethodDelete, issuePath(id, "/comments/"+url.PathEscape(cid)), nil, &cm)
}

// RedactComment is POST /v0/issues/{id}/comments/{cid}/redact (an admin of
// the issue's area, or the machine owner): cannot be undone.
func RedactComment(ctx context.Context, c *client.Client, id, cid string) (orchapi.IssueComment, error) {
	var cm orchapi.IssueComment
	return cm, c.Do(ctx, http.MethodPost, issuePath(id, "/comments/"+url.PathEscape(cid)+"/redact"), struct{}{}, &cm)
}

// AckRedactionGap is POST /v0/issues/{id}/comments/{cid}/ack-gap: clears
// one redaction gap, named as the caller's redaction conflict names it.
func AckRedactionGap(ctx context.Context, c *client.Client, id, cid, stop string) (orchapi.RedactionGapAck, error) {
	var a orchapi.RedactionGapAck
	return a, c.Do(ctx, http.MethodPost, issuePath(id, "/comments/"+url.PathEscape(cid)+"/ack-gap"), orchapi.AckRedactionGapRequest{Stop: stop}, &a)
}

// IssueHistory is GET /v0/issues/{id}/history.
func IssueHistory(ctx context.Context, c *client.Client, id string) ([]orchapi.IssueChange, error) {
	var out struct {
		History []orchapi.IssueChange `json:"history"`
	}
	return out.History, c.Do(ctx, http.MethodGet, issuePath(id, "/history"), nil, &out)
}

// AddIssueDep is POST /v0/issues/{id}/deps.
func AddIssueDep(ctx context.Context, c *client.Client, id string, req orchapi.IssueDepRequest) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodPost, issuePath(id, "/deps"), req, &w)
}

// RemoveIssueDep is DELETE /v0/issues/{id}/deps/{target}.
func RemoveIssueDep(ctx context.Context, c *client.Client, id, target string) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodDelete, issuePath(id, "/deps/"+client.Seg(target)), nil, &w)
}

// AddIssueLabel is POST /v0/issues/{id}/labels.
func AddIssueLabel(ctx context.Context, c *client.Client, id, label string) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodPost, issuePath(id, "/labels"), orchapi.IssueLabelRequest{Label: label}, &w)
}

// RemoveIssueLabel is DELETE /v0/issues/{id}/labels/{label}.
func RemoveIssueLabel(ctx context.Context, c *client.Client, id, label string) (IssueWrite, error) {
	var w IssueWrite
	return w, c.Do(ctx, http.MethodDelete, issuePath(id, "/labels/"+client.Seg(label)), nil, &w)
}

// IssueLabels is GET /v0/issues/labels (areas nil: every area).
func IssueLabels(ctx context.Context, c *client.Client, project string, areas []string) ([]string, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	for _, a := range areas {
		v.Add("area", a)
	}
	path := "/v0/issues/labels"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out struct {
		Labels []string `json:"labels"`
	}
	return out.Labels, c.Do(ctx, http.MethodGet, path, nil, &out)
}

// Memories lists project's bd memories (user key -> text).
func Memories(ctx context.Context, c *client.Client, project string) (map[string]string, error) {
	var l orchapi.MemoryList
	return l.Memories, c.Do(ctx, http.MethodGet, client.Query("/v0/issues/memories", "project", project), nil, &l)
}

// Remember stores a bd memory in the resolved project.
func Remember(ctx context.Context, c *client.Client, key, value string) (orchapi.Memory, error) {
	var m orchapi.Memory
	return m, c.Do(ctx, http.MethodPut, "/v0/issues/memories/"+url.PathEscape(key), orchapi.Memory{Value: value}, &m)
}

// Forget deletes a bd memory from the resolved project.
func Forget(ctx context.Context, c *client.Client, key string) (orchapi.Memory, error) {
	var m orchapi.Memory
	return m, c.Do(ctx, http.MethodDelete, "/v0/issues/memories/"+url.PathEscape(key), nil, &m)
}

// IssueSQL runs one read-only SELECT over project's bd-shaped views.
func IssueSQL(ctx context.Context, c *client.Client, project, query string) (orchapi.SQLResult, error) {
	var r orchapi.SQLResult
	return r, c.Do(ctx, http.MethodPost, client.Query("/v0/issues/sql", "project", project), orchapi.SQLRequest{Query: query}, &r)
}

// AttachIssueImage is POST /v0/issues/{id}/attachments: data is a PNG, JPEG
// or WebP image; expires is how long it lives ("" = 14 days; e.g. 36h, 30d).
func AttachIssueImage(ctx context.Context, c *client.Client, id string, data []byte, expires string) (orchapi.IssueAttachment, error) {
	var out orchapi.IssueAttachment
	resp, err := c.Stream(ctx, http.MethodPost, client.Query(issuePath(id, "/attachments"), "expires", expires),
		client.RawBody{Data: data, Type: "application/octet-stream"})
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// IssueAttachments is GET /v0/issues/{id}/attachments (metadata only).
func IssueAttachments(ctx context.Context, c *client.Client, id string) (orchapi.IssueAttachmentList, error) {
	var out orchapi.IssueAttachmentList
	return out, c.Do(ctx, http.MethodGet, issuePath(id, "/attachments"), nil, &out)
}

// IssueAttachment is GET /v0/issues/{id}/attachments/{aid}: the image's
// bytes, for the caller to read and close.
func IssueAttachment(ctx context.Context, c *client.Client, id, aid string) (*http.Response, error) {
	return c.Stream(ctx, http.MethodGet, issuePath(id, "/attachments/"+client.Seg(aid)), nil)
}

// RemoveIssueAttachment is DELETE /v0/issues/{id}/attachments/{aid}.
func RemoveIssueAttachment(ctx context.Context, c *client.Client, id, aid string) (orchapi.IssueAttachment, error) {
	var out orchapi.IssueAttachment
	return out, c.Do(ctx, http.MethodDelete, issuePath(id, "/attachments/"+client.Seg(aid)), nil, &out)
}
