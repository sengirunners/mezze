package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// taskNoticeWaitMax bounds how long a completion notice waits out a rate_limited refusal (counted from the first
// one). A refusal whose retry_after would end past it fails the notice; the result stays in task_status and in
// the work comment.
const taskNoticeWaitMax = time.Hour

func (s *Service) deliverTask(id string) {
	s.taskDeliveryMu.Lock()
	defer s.taskDeliveryMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	t, err := s.getTask(ctx, s.d.System(), id)
	if err != nil || t.Result == nil || t.Continuation == "pending" {
		return
	}
	r, ok := s.load(ctx, id)
	if !ok {
		return
	}
	var heldAt int64
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT COALESCE(MIN(created),0) FROM session_task_holds WHERE task_id=?`, id).Scan(&heldAt)
	if heldAt > 0 && time.Since(time.UnixMilli(heldAt)) < 30*time.Second {
		return
	}
	// The task's session, within its chain's caps (its own, inherited from a
	// capped parent): its notice writes nothing outside them.
	sub := s.capChain(ctx, s.d.Projects.Local(r.Address))
	resultText := t.Result.Text
	if len(resultText) > 4000 {
		resultText = resultText[:4000]
		for !utf8.ValidString(resultText) {
			resultText = resultText[:len(resultText)-1]
		}
		resultText += "… (full result in task_status)"
	}
	if t.Result.TextWithheld {
		resultText = "withheld because redaction could not be verified"
	}
	body := fmt.Sprintf("Task %s finished %s; process %s (confidence %s). Evidence: %s. Branch: %s. Result: %s. Continuation: %s. Continuation error: %s", t.Session, t.Result.Finished, t.Result.Outcome.Kind, t.Result.Outcome.Confidence, t.Result.Outcome.Evidence, t.Result.Branch, resultText, t.Continuation, t.ContinuationError)
	db := s.d.Store.Registry()
	deliver := func(kind string, fn func() error) {
		column, errColumn, attempts := kind+"_done", kind+"_error", kind+"_attempts"
		retryAt, since := kind+"_retry_at", kind+"_limited_since"
		now := time.Now().UnixMilli()
		res, e := db.ExecContext(ctx, `UPDATE session_tasks SET `+column+`='sending',`+attempts+`=`+attempts+`+1 WHERE id=? AND `+column+`='pending' AND `+retryAt+`<=?`, id, now)
		if e != nil {
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return
		}
		e = fn()
		status, msg := "sent", ""
		sinceAt := int64(-1)
		var next int64 // when to try again, and when the waiting began, for a refusal that names a wait
		spent := 1     // attempts this try used: none for a wait
		if e != nil {
			status, msg = "failed", e.Error()
			var ae *api.Error
			if api.IsCode(e, api.CodeQueued) {
				status = "queued"
			} else if api.IsCode(e, api.CodeRateLimited) && errors.As(e, &ae) && ae.RetryAfter > 0 {
				var d struct {
					Limit string `json:"limit"`
				}
				if json.Unmarshal(ae.Data, &d) == nil && d.Limit == "repeat" {
					// The repeat rule refuses a mail already sent (an earlier try whose answer was lost): it is there.
					_, _ = db.ExecContext(context.Background(), `UPDATE session_tasks SET `+column+`='sent',`+errColumn+`='',`+retryAt+`=0 WHERE id=?`, id)
					return
				}
				// Nothing was sent: wait out the window instead of spending an attempt, within the bound.
				var first int64
				_ = db.QueryRowContext(ctx, `SELECT `+since+` FROM session_tasks WHERE id=?`, id).Scan(&first)
				if first == 0 {
					first = now
				}
				next = now + int64(ae.RetryAfter)*1000
				if next <= first+taskNoticeWaitMax.Milliseconds() {
					status, spent = "pending", 0
					sinceAt = first
				} else {
					next = 0
					msg += "; gave up: the limit did not clear within " + taskNoticeWaitMax.String() + " (the result is in task_status)"
				}
			} else {
				var n int
				_ = db.QueryRowContext(ctx, `SELECT `+attempts+` FROM session_tasks WHERE id=?`, id).Scan(&n)
				if n < 3 {
					status = "pending"
				}
			}
		}
		_, _ = db.ExecContext(context.Background(), `UPDATE session_tasks SET `+column+`=?,`+errColumn+`=?,`+retryAt+`=?,`+attempts+`=`+attempts+`-?,`+since+`=CASE WHEN ?<0 THEN `+since+` ELSE ? END WHERE id=?`, status, msg, next, 1-spent, sinceAt, sinceAt, id)
	}
	deliver("mail", func() error { return s.sendTaskNotice(ctx, sub, r.Session, "mail", t.Parent, t.Work, body) })
	if t.Work == "" {
		_, _ = db.ExecContext(ctx, `UPDATE session_tasks SET comment_done='skipped' WHERE id=? AND comment_done='pending'`, id)
	} else {
		deliver("comment", func() error { return s.sendTaskNotice(ctx, sub, r.Session, "comment", t.Parent, t.Work, body) })
	}

}

// sendTaskNotice crosses the team, mail, work and issue seams through core.
func (s *Service) sendTaskNotice(ctx context.Context, sub *authz.Subject, ss orchapi.Session, kind, to, work, body string) error {
	if s.taskDeliver != nil {
		return s.taskDeliver(ctx, sub, ss, kind, to, body)
	}
	if relay, ok := orchcore.Team(s.d).(orchcore.TaskRelay); ok {
		handled, err := relay.DeliverTask(ctx, sub, ss.Project, ss.Area, kind, to, work, body)
		if handled || err != nil {
			return err
		}
	}
	if kind == "mail" {
		if orchcore.Mail(s.d) == nil {
			return api.Errorf(api.CodeUnavailable, "mail module unavailable")
		}
		_, err := orchcore.Mail(s.d).Send(ctx, sub, api.ProjectRef{ID: ss.Project}, ss.Area, orchapi.SendMailRequest{To: to, Body: body, Area: ss.Area})
		return err
	}
	var err error = api.NotFound("work item", work)
	if w, ok := orchcore.Work(s.d).(orchcore.WorkCommenter); ok {
		err = w.CommentTask(ctx, sub, ss.Project, work, body)
	}
	if api.IsCode(err, api.CodeNotFound) && orchcore.Issues(s.d) != nil {
		id := strings.TrimPrefix(work, "bd:")
		if !strings.Contains(id, ":") {
			args, _ := json.Marshal(orchapi.AdapterCommentArgs{Body: body, Author: ss.Address})
			err = orchcore.Issues(s.d).NativeCall(core.WithSubject(ctx, sub), ss.Project, orchapi.AdapterRequest{Spec: api.AdapterProtocol, Verb: orchapi.VerbComment, ID: id, Args: args}, nil)
		}
	}
	return err
}
