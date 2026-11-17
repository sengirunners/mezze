package orchclient

// Owned by the approvals builder. One method per /v0/approvers, /v0/approvals,
// /v0/asks and /v0/receipts endpoint in API.md.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// AddApprover is POST /v0/approvers (local): reserve a name, get a pairing code.
func AddApprover(ctx context.Context, c *client.Client, name string) (orchapi.NewApproverResponse, error) {
	return AddApproverFor(ctx, c, name, "")
}

// AddApproverFor is AddApprover for a device of person (an identity's name).
func AddApproverFor(ctx context.Context, c *client.Client, name, person string) (orchapi.NewApproverResponse, error) {
	var r orchapi.NewApproverResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/approvers", orchapi.NewApproverRequest{Name: name, For: person}, &r)
}

// AddMyPhone is POST /v0/approvers/self: a pairing code for a phone of the
// caller's own person, at the project's authority when it is a joined one.
func AddMyPhone(ctx context.Context, c *client.Client, name string) (orchapi.NewApproverResponse, error) {
	var r orchapi.NewApproverResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/approvers/self", orchapi.NewApproverRequest{Name: name}, &r)
}

// Approvers is GET /v0/approvers (local).
func Approvers(ctx context.Context, c *client.Client) ([]orchapi.Approver, error) {
	var l orchapi.ApproverList
	return l.Approvers, c.Do(ctx, http.MethodGet, "/v0/approvers", nil, &l)
}

// ApproversHere is GET /v0/approvers (local) with the daemon they are paired on.
func ApproversHere(ctx context.Context, c *client.Client) (orchapi.ApproversAt, error) {
	var l orchapi.ApproversAt
	return l, c.Do(ctx, http.MethodGet, "/v0/approvers", nil, &l)
}

// MyApprovers is GET /v0/approvers/self: the caller's own person's
// approvers, at the project's authority when it is a joined one.
func MyApprovers(ctx context.Context, c *client.Client) (orchapi.ApproversAt, error) {
	var l orchapi.ApproversAt
	return l, c.Do(ctx, http.MethodGet, "/v0/approvers/self", nil, &l)
}

// RemoveApprover is DELETE /v0/approvers/{name} (local).
func RemoveApprover(ctx context.Context, c *client.Client, name string) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodDelete, "/v0/approvers/"+client.Seg(name), nil, &a)
}

// ConfirmApprover is POST /v0/approvers/{name}/confirm (from an approver device).
func ConfirmApprover(ctx context.Context, c *client.Client, name string) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodPost, "/v0/approvers/"+client.Seg(name)+"/confirm", struct{}{}, &a)
}

// Pair is POST /v0/approvals/pair (from the device being paired).
func Pair(ctx context.Context, c *client.Client, code string) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodPost, "/v0/approvals/pair", orchapi.PairRequest{Code: code}, &a)
}

// PairPhone is POST /v0/approvals/pair-phone (from a phone; no code).
func PairPhone(ctx context.Context, c *client.Client) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodPost, "/v0/approvals/pair-phone", struct{}{}, &a)
}

// DeviceView is GET /v0/approvals/device (from a device).
func DeviceView(ctx context.Context, c *client.Client) (orchapi.DeviceView, error) {
	var v orchapi.DeviceView
	return v, c.Do(ctx, http.MethodGet, "/v0/approvals/device", nil, &v)
}

// NewAsk is POST /v0/asks: a needs=human mail with a subject.
//
// A daemon that predates thread_title and pr_url refuses the whole ask as an
// unknown field (team projects proxy the body to their authority): the ask is
// sent again once without them, since they are only shown beside the card.
func NewAsk(ctx context.Context, c *client.Client, req orchapi.NewAskRequest) (orchapi.Ask, error) {
	var a orchapi.Ask
	err := c.Do(ctx, http.MethodPost, "/v0/asks", req, &a)
	if (req.ThreadTitle != "" || req.PRURL != "") && (orchapi.UnknownField(err, "thread_title") || orchapi.UnknownField(err, "pr_url")) {
		req.ThreadTitle, req.PRURL = "", ""
		a = orchapi.Ask{}
		err = c.Do(ctx, http.MethodPost, "/v0/asks", req, &a)
	}
	return a, err
}

// NewAskWithFiles attaches each file (an image or an HTML page, as bytes) to
// req's work item, then asks with their ids; if any attach or the ask fails,
// the ones already attached are removed and nothing is created. names label
// the files in errors.
func NewAskWithFiles(ctx context.Context, c *client.Client, req orchapi.NewAskRequest, files [][]byte, names []string) (orchapi.Ask, error) {
	if len(files) == 0 {
		return NewAsk(ctx, c, req)
	}
	item := req.Subject.Work
	if _, rest, ok := strings.Cut(item, ":"); ok {
		item = rest
	}
	var done []string
	undo := func() {
		for _, id := range done {
			_, _ = RemoveIssueAttachment(context.WithoutCancel(ctx), c, item, id)
		}
	}
	for i, data := range files {
		at, err := AttachIssueImage(ctx, c, item, data, "")
		if err != nil {
			undo()
			return orchapi.Ask{}, fmt.Errorf("attach %s: %w", names[i], err)
		}
		done = append(done, at.ID)
	}
	req.Attachments = done
	a, err := NewAsk(ctx, c, req)
	if err != nil {
		undo()
	}
	return a, err
}

// Asks is GET /v0/asks (open: only undecided ones).
func Asks(ctx context.Context, c *client.Client, project string, open bool) ([]orchapi.Ask, error) {
	var l orchapi.AskList
	return l.Asks, c.Do(ctx, http.MethodGet, client.Query("/v0/asks", "project", project, "open", client.Bool(open)), nil, &l)
}

// AskPage is one page of GET /v0/asks, newest first: beforeID is the last
// page's Next ("" for the first), limit 0 the server's default. Next is "" on
// the last page.
func AskPage(ctx context.Context, c *client.Client, project, beforeID string, limit int) (asks []orchapi.Ask, next string, err error) {
	var l orchapi.AskList
	err = c.Do(ctx, http.MethodGet, client.Query("/v0/asks", "project", project, "before_id", beforeID, "limit", client.Itoa(int64(limit))), nil, &l)
	return l.Asks, l.Next, err
}

// ObjectAsk is POST /v0/asks/{id}/object: an owner's press against an
// answered card's outcome (the measures' harm record).
func ObjectAsk(ctx context.Context, c *client.Client, id, note string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/object", orchapi.ObjectRequest{Note: note}, &a)
}

// SimilarAsks is GET /v0/asks?q=: the readable card asks, answered or open,
// that share the most words with q, best first. Which one answers the
// question is the caller's call.
func SimilarAsks(ctx context.Context, c *client.Client, project, q string) ([]orchapi.Ask, error) {
	var l orchapi.AskList
	return l.Asks, c.Do(ctx, http.MethodGet, client.Query("/v0/asks", "project", project, "q", q), nil, &l)
}

// GetAsk is GET /v0/asks/{id}.
func GetAsk(ctx context.Context, c *client.Client, id string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodGet, "/v0/asks/"+client.Seg(id), nil, &a)
}

// Decide is POST /v0/asks/{id}/decide (from an approver device).
func Decide(ctx context.Context, c *client.Client, id, decision string) (orchapi.Decision, error) {
	var d orchapi.Decision
	return d, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/decide", orchapi.DecideRequest{Decision: decision}, &d)
}

// DecideWith is Decide with a free-text answer or who it speaks for.
func DecideWith(ctx context.Context, c *client.Client, id string, req orchapi.DecideRequest) (orchapi.Decision, error) {
	var d orchapi.Decision
	return d, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/decide", req, &d)
}

// KeyApprover is GET /v0/approvals/key: the approver paired to a key
// fingerprint (p256:<16 hex>). On a member it is answered by the authority.
func KeyApprover(ctx context.Context, c *client.Client, project, key string) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodGet, client.Query("/v0/approvals/key", "project", project, "key", key), nil, &a)
}

// UnbindKey is DELETE /v0/approvals/key: a key ends itself (signing out)
// at project's authority.
func UnbindKey(ctx context.Context, c *client.Client, project string, in orchapi.UnbindKeyRequest) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodDelete, client.Query("/v0/approvals/key", "project", project), in, &a)
}

// AnswerSigned is POST /v0/asks/{id}/answer: a key approver's signed answer.
// project names the ask's project (a joined one's answer goes to its authority).
func AnswerSigned(ctx context.Context, c *client.Client, project, id string, in orchapi.SignedKeyAnswer) (orchapi.Decision, error) {
	var d orchapi.Decision
	return d, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/answer", "project", project), in, &d)
}

// ReleaseHeldOnDevice is POST /v0/asks/{id}/decide with the release token: a
// paired approver device releases a card the card judge could not check
// (Ask.NotChecked), as it is. The answer is the Ask.
func ReleaseHeldOnDevice(ctx context.Context, c *client.Client, id string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/decide", orchapi.DecideRequest{Decision: orchapi.DecisionReleaseUnchecked}, &a)
}

// ReleaseHeldSigned is POST /v0/asks/{id}/answer with a key approver's signed
// release (KeyAnswer.Decision orchapi.DecisionReleaseUnchecked). The answer is
// the Ask.
func ReleaseHeldSigned(ctx context.Context, c *client.Client, project, id string, in orchapi.SignedKeyAnswer) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/answer", "project", project), in, &a)
}

// Choose is POST /v0/asks/{id}/choose: the project's owner answers a card
// ask. project, when set, names whose authority (else each joined one is
// tried while the ask is not found).
func Choose(ctx context.Context, c *client.Client, project, id string, req orchapi.DecideRequest) (orchapi.Decision, error) {
	var d orchapi.Decision
	return d, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/choose", "project", project), req, &d)
}

// PairKey is POST /v0/approvals/pair-key. project names whose authority.
func PairKey(ctx context.Context, c *client.Client, project string, req orchapi.PairKeyRequest) (orchapi.Approver, error) {
	var a orchapi.Approver
	return a, c.Do(ctx, http.MethodPost, client.Query("/v0/approvals/pair-key", "project", project), req, &a)
}

// WithdrawAsk is POST /v0/asks/{id}/withdraw.
func WithdrawAsk(ctx context.Context, c *client.Client, id string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/withdraw", struct{}{}, &a)
}

// ConfirmAction is POST /v0/asks/{id}/action/confirm: a project owner saw
// how an answered ask's action went. project, when set, names whose authority.
func ConfirmAction(ctx context.Context, c *client.Client, project, id string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/action/confirm", "project", project), orchapi.ActionConfirmRequest{}, &a)
}

// StepDone is POST /v0/asks/{id}/step: the person did the card's owner
// step (cardSHA the card read); zm asks the asker to run the check.
// project, when set, names whose authority.
func StepDone(ctx context.Context, c *client.Client, project, id, cardSHA string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/step", "project", project), orchapi.StepDoneRequest{CardSHA: cardSHA}, &a)
}

// RetryAction is POST /v0/asks/{id}/action/retry: start the identical
// approved action of the card read (cardSHA) again, after it failed before
// it started. project, when set, names whose authority.
func RetryAction(ctx context.Context, c *client.Client, project, id, cardSHA string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/action/retry", "project", project), orchapi.ActionRetryRequest{CardSHA: cardSHA}, &a)
}

// CheckAsk is POST /v0/asks/{id}/check: the asker's run of an approved
// ask's verify check, or its giving up.
func CheckAsk(ctx context.Context, c *client.Client, id string, req orchapi.CheckRequest) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/check", req, &a)
}

// Decisions is GET /v0/decisions. status is a comma list ("" = open,answered).
func Decisions(ctx context.Context, c *client.Client, project, status string) (orchapi.Inbox, error) {
	return DecisionsFor(ctx, c, project, status, "")
}

// DecisionsFor is GET /v0/decisions?for=: "" or "me" (work waiting on the
// caller), "everyone" (work waiting on anyone), "agents" (questions no ask
// covers yet, which an agent writes up or decides).
func DecisionsFor(ctx context.Context, c *client.Client, project, status, forWho string) (orchapi.Inbox, error) {
	var in orchapi.Inbox
	return in, c.Do(ctx, http.MethodGet, client.Query("/v0/decisions", "project", project, "status", status, "for", forWho), nil, &in)
}

// Aliases is GET /v0/aliases (identity "" = the caller's own).
func Aliases(ctx context.Context, c *client.Client, identity string) (orchapi.Aliases, error) {
	var out orchapi.Aliases
	return out, c.Do(ctx, http.MethodGet, client.Query("/v0/aliases", "identity", identity), nil, &out)
}

// AddAlias is POST /v0/aliases.
func AddAlias(ctx context.Context, c *client.Client, req orchapi.AliasRequest) (orchapi.Aliases, error) {
	var out orchapi.Aliases
	return out, c.Do(ctx, http.MethodPost, "/v0/aliases", req, &out)
}

// RemoveAlias is DELETE /v0/aliases/{alias}.
func RemoveAlias(ctx context.Context, c *client.Client, alias string) (orchapi.Aliases, error) {
	var out orchapi.Aliases
	return out, c.Do(ctx, http.MethodDelete, "/v0/aliases/"+client.Seg(alias), nil, &out)
}

// ReceiptKey is GET /v0/receipts/key.
func ReceiptKey(ctx context.Context, c *client.Client) (orchapi.ReceiptKey, error) {
	var k orchapi.ReceiptKey
	return k, c.Do(ctx, http.MethodGet, "/v0/receipts/key", nil, &k)
}

// VerifyReceipt is GET /v0/receipts/verify for a receipt's JSON.
func VerifyReceipt(ctx context.Context, c *client.Client, receipt []byte, exp orchapi.ReceiptExpect) (orchapi.ReceiptCheck, error) {
	var r orchapi.ReceiptCheck
	pr := ""
	if exp.PR != 0 {
		pr = strconv.Itoa(exp.PR)
	}
	if exp.Project == "" {
		var rc orchapi.Receipt
		if json.Unmarshal(receipt, &rc) == nil {
			exp.Project = rc.Project
		}
	}
	signed := ""
	if exp.Signed {
		signed = "1"
	}
	path := client.Query("/v0/receipts/verify", "receipt", base64.RawURLEncoding.EncodeToString(receipt), "project", exp.Project,
		"ask", exp.Ask, "repo", exp.Repo, "pr", pr, "head_sha", exp.HeadSHA, "decision", exp.Decision, "approver", exp.Approver, "work", exp.Work, "question", exp.Question, "card", exp.Card,
		"signed", signed)
	if err := c.Do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return r, err
	}
	if exp.Signed && r.Valid && !slices.Contains(r.Checked, "signed") {
		// A daemon from before signed=1 ignores it: never take its yes.
		r.Valid, r.Reason = false, "the daemon did not check that the answer is signed (it predates signed=1)"
	}
	return r, nil
}

// UndoDefault is POST /v0/asks/{id}/undo: the project's owner takes back a
// default zm applied at the ask's deadline.
func UndoDefault(ctx context.Context, c *client.Client, project, id string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, client.Query("/v0/asks/"+client.Seg(id)+"/undo", "project", project), struct{}{}, &a)
}

// UndoDefaultOnDevice is POST /v0/asks/{id}/device-undo: a paired approver
// device takes back an applied default.
func UndoDefaultOnDevice(ctx context.Context, c *client.Client, id string) (orchapi.Ask, error) {
	var a orchapi.Ask
	return a, c.Do(ctx, http.MethodPost, "/v0/asks/"+client.Seg(id)+"/device-undo", struct{}{}, &a)
}

// PersonLinks is GET /v0/person-links.
func PersonLinks(ctx context.Context, c *client.Client) (orchapi.PersonLinks, error) {
	var out orchapi.PersonLinks
	return out, c.Do(ctx, http.MethodGet, "/v0/person-links", nil, &out)
}

// AddPersonLink is POST /v0/person-links.
func AddPersonLink(ctx context.Context, c *client.Client, req orchapi.PersonLinkRequest) (orchapi.PersonLinks, error) {
	var out orchapi.PersonLinks
	return out, c.Do(ctx, http.MethodPost, "/v0/person-links", req, &out)
}

// RemovePersonLink is DELETE /v0/person-links/{identity}.
func RemovePersonLink(ctx context.Context, c *client.Client, identity string) (orchapi.PersonLinks, error) {
	var out orchapi.PersonLinks
	return out, c.Do(ctx, http.MethodDelete, "/v0/person-links/"+client.Seg(identity), nil, &out)
}
