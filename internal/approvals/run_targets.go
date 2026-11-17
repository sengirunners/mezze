package approvals

// Run targets for asks (SPEC 16 "Asks that carry an action"): an
// agent whose choice needs the approver to act finds the approver's machine
// that takes approved actions, so it attaches the step as an action instead
// of a plain ask or a report that gets lost. GET /v0/run-targets lists them
// (zm ask machines); an ask whose run action names no machine runs on the
// approver's only ready one; a card the judge refuses names their machine
// and the flags; the judge reads them (cardjudge.go). The machines and whose they are come from the team module
// (orchcore.AskRunTargets).

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// handleRunTargets is GET /v0/run-targets: the resolved project's machines
// (?person=: that person's), for who may operate on it.
func (s *Service) handleRunTargets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.d.WriteTarget(ctx, "", authz.Operate)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if !core.SubjectOf(ctx).Scope.WholeProject(t.Project.ID) {
		// As askOwner: an area member learns nothing of who has an account,
		// or a machine, here.
		api.WriteError(w, api.Errorf(api.CodeForbidden, "the project's machines and whose they are are for members of the whole project").
			WithHint("name the machine you were given with --run-on, or ask a project owner"))
		return
	}
	l, err := s.runTargets(ctx, t.Project.ID, r.URL.Query().Get("person"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	l.Project = t.Project.Slug
	if len(orchapi.ReadyTargets(l.Targets)) == 0 {
		l.Hint = noTargetHint(l.Person, t.Project.Slug)
	}
	api.WriteJSON(w, http.StatusOK, l)
}

// runTargets is project's machines, or person's ("" = everyone's; any
// name, alias or GitHub login of theirs, with or without human:): the
// machines any of that person's identities joined.
func (s *Service) runTargets(ctx context.Context, project, person string) (orchapi.RunTargetList, error) {
	rt, _ := orchcore.Team(s.d).(orchcore.AskRunTargets)
	if rt == nil {
		return orchapi.RunTargetList{}, api.Errorf(api.CodeUnavailable, "this daemon runs no actions on machines")
	}
	l := orchapi.RunTargetList{Targets: []orchapi.RunTarget{}}
	var names []string
	if person = strings.TrimSpace(person); person != "" {
		p, err := s.personIdentity(ctx, project, person)
		if err != nil {
			return l, err
		}
		l.Person, names = p.Name, p.Names
	}
	ts, err := rt.AskRunTargets(ctx, project)
	if err != nil {
		return l, err
	}
	for _, t := range ts {
		if l.Person == "" || slices.ContainsFunc(names, func(n string) bool { return hasName(t.People, n) }) {
			l.Targets = append(l.Targets, t)
		}
	}
	return l, nil
}

// personIdentity is the person (one with an identity that reads the whole
// project) a name, alias or login resolves to; anyone else, here or not, or
// a name two people share, is the same not_found.
func (s *Service) personIdentity(ctx context.Context, project, named string) (person, error) {
	people, err := s.people(ctx, project)
	if err != nil {
		return person{}, err
	}
	var readers []person
	for _, p := range people {
		if p.Reads {
			readers = append(readers, p)
		}
	}
	p, _, err := resolvePerson(readers, named)
	if err != nil {
		return p, api.NotFound("person", named)
	}
	return p, nil
}

func hasName(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// cardPerson is the person a card ask is for, as handleAsk will name it
// (askOwner: --to human:<x> or --owner, else the work item's owner:<name>;
// the asker's own machine's human, who takes its own machine's asks), or ""
// when none is named. Only for a caller of the whole project: an area member
// learns nothing of who has a machine here.
func (s *Service) cardPerson(ctx context.Context, sub *authz.Subject, a *orchapi.Ask, named string) (string, error) {
	if !sub.Scope.WholeProject(a.Project) {
		return "", nil
	}
	p, _, err := s.askOwner(ctx, sub, a.Project, a.Area, a.Subject.Work, named)
	if err != nil {
		own, _ := s.askerHuman(sub)
		if n := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(named), "human:")); n != "" && "human:"+n == strings.ToLower(own) {
			return n, nil
		}
		return "", err
	}
	return p, nil
}

// defaultRunOn is the machine a run or command action that names none runs
// on: the approver's only ready machine that runs act (one whose zm runs
// scripts, for a script; commands, for a command). None or several is
// refused with the list, never guessed: the approver approves a place.
func (s *Service) defaultRunOn(ctx context.Context, sub *authz.Subject, a *orchapi.Ask, named string, act orchapi.AskAction) (string, error) {
	person, err := s.cardPerson(ctx, sub, a, named)
	if err != nil {
		return "", err
	}
	slug := s.d.Projects.Slug(a.Project)
	if person == "" {
		return "", api.Errorf(api.CodeBadRequest, "a run action names its machine (--run-on), or the person it is for (--to human:<name>) so zm picks their machine").
			WithHint("zm ask machines -P " + slug + " lists the project's machines and whose they are")
	}
	l, err := s.runTargets(ctx, a.Project, person)
	if err != nil {
		return "", err
	}
	lacks := func(t orchapi.RunTarget) string { return targetLacks(t, act) }
	var ready []string
	for _, t := range orchapi.ReadyTargets(l.Targets) {
		if lacks(t) == "" {
			ready = append(ready, t.Machine)
		}
	}
	switch len(ready) {
	case 1:
		return ready[0], nil
	case 0:
		why := ""
		for _, t := range l.Targets {
			if !t.Ready {
				why += fmt.Sprintf("; %s %s", t.Machine, t.Why)
			} else if what := lacks(t); what != "" {
				why += fmt.Sprintf("; %s's zm runs no %s (update zm there)", t.Machine, what)
			}
		}
		return "", api.Errorf(api.CodeBadRequest, "%s has no machine in %s that takes approved actions%s", person, slug, why).
			WithHint(noTargetHint(person, slug))
	}
	return "", api.Errorf(api.CodeBadRequest, "%s has %d machines that take approved actions (%s): name one with --run-on", person, len(ready), strings.Join(ready, ", ")).
		WithHint("zm ask machines human:" + person + " -P " + slug)
}

// targetLacks is what keeps ready machine t from running act ("" =
// nothing), as cleanRunning checks it: a script needs a zm that runs
// scripts; a command, one that runs scripts and commands.
func targetLacks(t orchapi.RunTarget, act orchapi.AskAction) string {
	switch {
	case act.Kind == orchapi.ActionCommand && !t.ScriptRuns && !t.CommandRuns:
		return "scripts or commands"
	case act.Kind == orchapi.ActionCommand && !t.CommandRuns:
		return "commands"
	case (act.Kind == orchapi.ActionCommand || act.Kind == orchapi.ActionRun && act.Script != "") && !t.ScriptRuns:
		return "scripts"
	}
	return ""
}

// runForms is the action flags ready machine t takes, for a hint.
func runForms(t orchapi.RunTarget) string {
	switch {
	case targetLacks(t, orchapi.AskAction{Kind: orchapi.ActionCommand}) == "":
		return " (an agent does it there, as them), or --command '<one line>' or --script <path>, with --ref <ref> --commit <sha>,"
	case targetLacks(t, orchapi.AskAction{Kind: orchapi.ActionRun, Script: "x"}) == "":
		return " (an agent does it there, as them), or --script <path> --ref <ref> --commit <sha>,"
	}
	return " (an agent does it there, as them; its zm runs no scripts or commands),"
}

// noTargetHint is what to do when person ("" = nobody named) has no ready
// machine in project slug.
func noTargetHint(person, slug string) string {
	who := "the person"
	if person != "" {
		who = person
	}
	return fmt.Sprintf("%s's machine joins the project (an owner: zm invite -P %s, then zm join <code> there) and its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon; until then ask for a judgement and do the step yourself, or name another trusted machine with --run-on", who, slug)
}

// actionHint is the action form a card refused by the judge can use.
const actionHint = "carry a step as an action (zm ask new --run-on, --authorize-key or --ack-mail, with --ack-on for its choice), or for an approver with no machine that takes tasks a complete numbered brief (--hand --agent claude --brief <file>) plus --verify; or ask for a judgement and do the work yourself"

// actionHintFor is a judge refusal's hint: the action form, with the
// approver's ready machine named when there is one.
func (s *Service) actionHintFor(ctx context.Context, sub *authz.Subject, a *orchapi.Ask, named string) string {
	person, err := s.cardPerson(ctx, sub, a, named)
	if err != nil || person == "" {
		return actionHint + "; zm ask machines lists the machines an action may run on"
	}
	l, err := s.runTargets(ctx, a.Project, person)
	if err != nil {
		return actionHint
	}
	var ready []string
	ts := orchapi.ReadyTargets(l.Targets)
	for _, t := range ts {
		ready = append(ready, t.Machine)
	}
	switch len(ready) {
	case 0:
		return actionHint + "; " + person + " has no machine that takes approved actions yet (zm ask machines human:" + person + "), so a step of theirs goes in a --hand brief"
	case 1:
		return fmt.Sprintf("carry the step as an action on %s's machine %s: zm ask new --to human:%s --run-on %s --agent claude --brief <file>%s and --ack-on for its choice; or ask for a judgement and do the work yourself",
			person, ready[0], person, ready[0], runForms(ts[0]))
	}
	return fmt.Sprintf("carry the step as an action on one of %s's machines (%s): zm ask new --to human:%s --run-on <machine> --agent claude --brief <file>, or --command '<one line>' or --script <path>, with --ref <ref> --commit <sha> where its zm runs them (zm ask machines), and --ack-on for its choice; or ask for a judgement and do the work yourself",
		person, strings.Join(ready, ", "), person)
}
