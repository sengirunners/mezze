package mail

import (
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// GET /v0/mail?since=&until= (zm measures pages a long history by moving
// until back): both bounds are inclusive times, a bad one is refused.
func TestMailTimeBounds(t *testing.T) {
	f := newFixture(t)
	f.sess.add("mini/aaaaaaaa", f.harbor.ID, "", true, orchapi.StateRunning)
	f.sess.add("mini/bbbbbbbb", f.harbor.ID, "", true, orchapi.StateRunning)
	sub := f.d.Projects.Local("mini/aaaaaaaa")
	var sent orchapi.Mail
	for _, body := range []string{"one", "two", "three"} {
		sent = must(f.s.Send(ctx, sub, f.harbor.Ref(), "", orchapi.SendMailRequest{To: "mini/bbbbbbbb", Body: body}))
	}
	owner := f.as("", "")
	count := func(q orchapi.MailQuery) int { return len(must(orchclient.Mail(ctx, owner, q))) }

	past, future := sent.Created.Add(-time.Hour), sent.Created.Add(time.Hour)
	if n := count(orchapi.MailQuery{}); n != 3 {
		t.Fatalf("unbounded: %d", n)
	}
	for name, tc := range map[string]struct {
		q    orchapi.MailQuery
		want int
	}{
		"inside":             {orchapi.MailQuery{Since: past, Until: future}, 3},
		"all before":         {orchapi.MailQuery{Until: past}, 0},
		"all after":          {orchapi.MailQuery{Since: future}, 0},
		"until is inclusive": {orchapi.MailQuery{Until: sent.Created}, 3},
		"since is inclusive": {orchapi.MailQuery{Since: sent.Created}, 1},
	} {
		if n := count(tc.q); n < tc.want || (name != "since is inclusive" && n != tc.want) {
			t.Errorf("%s: %d mails, want %d", name, n, tc.want)
		}
	}
	// A bad time is refused, not ignored.
	var l orchapi.MailList
	err := owner.Do(ctx, "GET", "/v0/mail?since=yesterday", nil, &l)
	wantCode(t, err, api.CodeBadRequest)
}
