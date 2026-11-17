package approvals

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// A person's step (SPEC 16 "Your step"): a card may name one step only the
// person it is for can do, such as a sign-in or a press in a service's own
// console, which no agent can do for them. It is the work of the card's
// verify choice: after the yes the ask is approved, not done and the
// person's turn (your_step) until the verify check, run by zm ask check,
// passes. The person's "I did it" (POST /v0/asks/{id}/step) asks the asker
// to run the check now and closes nothing; evidence the asker reports never
// closes it.

var stepMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0023_owner_step", Script: `
ALTER TABLE ask_actions ADD COLUMN step_done INTEGER;
ALTER TABLE ask_actions ADD COLUMN step_done_by TEXT NOT NULL DEFAULT '';
`},
}

// cleanStep checks and freezes a card's owner step: a card with a verify
// check and no action that runs something (the step is the work), plain
// numbered lines and an https link.
func cleanStep(in orchapi.AskStep, act *orchapi.AskAction, v *orchapi.AskVerify, prefixes []string) (*orchapi.AskStep, error) {
	bad := func(format string, a ...any) (*orchapi.AskStep, error) {
		return nil, api.Errorf(api.CodeBadRequest, format, a...).
			WithHint("zm ask new --to human:<name> --step-label '<what to do>' --step '<1st step>' --step '<2nd>' --step-link https://<the service's page> --verify '<check>'")
	}
	field := func(s string) string { return strings.TrimSpace(textclean.Clean(s)) }
	switch {
	case v == nil:
		return bad("a person's step needs --verify: the check zm runs to see it done")
	case act != nil:
		return bad("a person's step is the card's work: no action with it (one owner step per card, and nothing runs)")
	}
	out := orchapi.AskStep{Label: field(in.Label), Link: strings.TrimSpace(in.Link), Minutes: in.Minutes}
	lines := append([]string{in.Label}, in.Steps...)
	for _, l := range lines {
		if len(l) > orchapi.MaxStepLine || !utf8.ValidString(l) || unsafePresentation(l) {
			return bad("step lines are plain single lines of at most %d bytes", orchapi.MaxStepLine)
		}
	}
	for _, l := range in.Steps {
		if l = field(l); l != "" {
			out.Steps = append(out.Steps, l)
		}
	}
	switch {
	case out.Label == "":
		return bad("a person's step needs a label: what they do, in a few words")
	case len(out.Steps) == 0 || len(out.Steps) > orchapi.MaxStepLines:
		return bad("a person's step is 1 to %d numbered steps, got %d", orchapi.MaxStepLines, len(out.Steps))
	case out.Minutes < 0 || out.Minutes > orchapi.MaxStepMinutes:
		return bad("step minutes: 0 to %d", orchapi.MaxStepMinutes)
	}
	if hit := machineToken(strings.Join(append([]string{out.Label}, out.Steps...), "\n"), prefixes); hit != "" {
		return bad("a person reads the step: no ids, hashes or paths; it has %s. The link goes in --step-link", hit)
	}
	u, err := url.Parse(out.Link)
	switch {
	case len(out.Link) > orchapi.MaxStepLink || err != nil || strings.ContainsAny(out.Link, " \t\r\n"):
		return bad("step link: one https address of at most %d bytes", orchapi.MaxStepLink)
	case u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "":
		return bad("step link: an https address on the service's own site (https://...), with no user, password or #part in it")
	case u.Port() != "" && u.Port() != "443":
		return bad("step link: the service's own site on the usual https port")
	}
	host := strings.ToLower(u.Hostname())
	if !plainHost(host) {
		// What a person reads on the button must be what the browser opens:
		// no look-alike letters, no bare address.
		return bad("step link: a plain host name in ASCII letters, digits, dots and dashes (a non-ASCII name in its xn-- form), not an IP address")
	}
	u.Host = host
	out.Link, out.Domain = u.String(), strings.TrimPrefix(host, "www.")
	return &out, nil
}

// plainHost reports whether host is a DNS name of ASCII labels with at
// least one dot and a letter in its last label (so never an IP address).
func plainHost(host string) bool {
	labels := strings.Split(host, ".")
	if len(labels) < 2 || len(host) > 253 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	return strings.IndexFunc(tld, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
}

// stepOf is a's card owner step, or nil.
func stepOf(a orchapi.Ask) *orchapi.AskStep {
	if a.Card == nil {
		return nil
	}
	return a.Card.OwnerStep
}

// stepText is the owner step as the person reads it.
func stepText(st *orchapi.AskStep, v *orchapi.AskVerify) string {
	var b strings.Builder
	on := ""
	if v != nil {
		on = "If you choose " + strings.ToUpper(v.On) + ", "
	}
	fmt.Fprintf(&b, "%syour step", on)
	if st.Minutes > 0 {
		fmt.Fprintf(&b, ", about %d minute", st.Minutes)
		if st.Minutes > 1 {
			b.WriteString("s")
		}
	}
	fmt.Fprintf(&b, ": %s on %s (%s).", st.Label, st.Domain, st.Link)
	for i, l := range st.Steps {
		fmt.Fprintf(&b, "\n%d. %s", i+1, l)
	}
	b.WriteString("\nzm's check sees when it is done; nobody can mark it done for you.")
	return b.String()
}

// yourStep: a's approved work waits on its owner step: it was never said
// done, or the last check after the person said so did not pass.
func yourStep(a *orchapi.Ask) bool {
	x := a.ActionState
	if x == nil || x.State != orchapi.ActionApproved || stepOf(*a) == nil {
		return false
	}
	if x.StepDoneAt == nil {
		return true
	}
	return x.CheckedAt != nil && !x.CheckedAt.Before(*x.StepDoneAt) && x.CheckExit != nil && *x.CheckExit != 0
}

// stepAgain: a second "I did it" sooner than this records nothing new and
// mails nobody (a client's Check again comes after two minutes).
const stepAgain = time.Minute

// handleStepDone is POST /v0/asks/{id}/step: the person the step is for
// (never an agent session, never someone else for them) says it is done.
// zm records when and asks the asker to run the check now; only the check
// closes it.
func (s *Service) handleStepDone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.StepDoneRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(ctx)
	id := r.PathValue("id")
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	me := s.stepPerson(ctx, sub, a)
	if me == "" {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "only %s says their own step is done, never an agent or another person", first(strings.TrimPrefix(a.Owner, "human:"), "the person it is for")))
		return
	}
	switch x := a.ActionState; {
	case stepOf(a) == nil:
		err = api.Errorf(api.CodeConflict, "ask %s has no person's step", a.ID)
	case req.CardSHA != a.CardSHA || cardSHA(marshal(*a.Card)) != a.CardSHA:
		err = api.Errorf(api.CodeConflict, "ask %s: the card you read is not the one it carries", a.ID).WithHint("read it again")
	case x == nil || x.State != orchapi.ActionApproved:
		err = api.Errorf(api.CodeConflict, "ask %s is not waiting on its step", a.ID)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	now := s.now()
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE ask_actions SET step_done = ?, step_done_by = ? WHERE ask = ? AND state = 'approved' AND (step_done IS NULL OR step_done <= ?)`,
		ms(now), "human:"+me, a.ID, ms(now.Add(-stepAgain)))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.askForCheck(ctx, a, me)
	}
	a, err = s.ask(ctx, a.ID)
	respond(w, s.view(ctx, sub, a), err)
}

// stepPerson is the name of the person sub is, when that is the person a's
// step is for, by name or by the directory's person; else "". Never an
// agent session: a session on the owner's own socket is not the person.
func (s *Service) stepPerson(ctx context.Context, sub *authz.Subject, a orchapi.Ask) string {
	if sub == nil || sub.Session != "" || a.Owner == "" {
		return ""
	}
	name := sub.Identity.Name
	if name == "" && sub.Via == authz.ViaLocal {
		name = s.d.Config.Human // this machine's own person, on its socket
	}
	owner := strings.TrimPrefix(a.Owner, "human:")
	switch {
	case name == "":
		return ""
	case strings.EqualFold(name, owner):
		return owner
	case a.OwnerPerson != "" && s.personOf(ctx, a.Project, name) == a.OwnerPerson:
		return owner
	}
	return ""
}

// askForCheck mails a's asker that the person did their step: run the
// check now. Best effort: the not-done reminders still come.
func (s *Service) askForCheck(ctx context.Context, a orchapi.Ask, who string) {
	ctx = context.WithoutCancel(ctx)
	local := s.d.Projects.Local("")
	to := ""
	if orchapi.AgentSender(a.AskedBy) {
		to = a.AskedBy
	} else if a.Subject.Work != "" {
		to, _, _, _ = orchcore.Work(s.d).MailTarget(ctx, local, a.Project, a.Subject.Work)
	}
	if to == "" {
		return
	}
	text := fmt.Sprintf("STEP DONE: %s says they did their step on ask %s (%s). Run its check now: zm ask check %s",
		who, a.ID, quoted(a.Card.OwnerStep.Label), a.ID)
	if _, err := orchcore.Mail(s.d).Send(ctx, local, api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)}, a.Area,
		orchapi.SendMailRequest{ID: ids.New(), To: to, Body: text}); err != nil {
		s.d.Log.Warn("approvals: ask for an owner step's check", "ask", a.ID, "err", err)
	}
}
