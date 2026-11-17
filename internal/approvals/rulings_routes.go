package approvals

// The rulings routes: GET, POST /v0/rulings and DELETE
// /v0/rulings/{name} read and change the resolved project's owner rulings
// (rulings.go) at its authority. A change rewrites project.toml and takes
// effect at once: the card judge reads the loaded file on every card, so no
// daemon restart. Changing them is an owner decision (SPEC 16 "Card
// judge"): an add names the owner's signed, approved ask whose card proposed
// the ruling (its name and text, quoted), which an agent session may carry
// out, and the ruling records it; a removal is only a person's who owns or
// administers the project.

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

func (s *Service) rulingRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/rulings", core.Authed, "read on the whole resolved project (an area guest is forbidden)", s.handleRulings)
	r.HandleFunc("POST /v0/rulings", core.Authed, "a person who owns or administers the whole resolved project (core.Deps.ProjectAdmin), or anyone who operates on all of it, whose file this daemon holds; the named ask is the project's, approved with a signature by a present owner or admin, carrying the ruling's name and text quoted (for anyone but the person, in the approved action's brief or plan); once per ask and name", s.handleRulingAdd)
	r.HandleFunc("DELETE /v0/rulings/{name}", core.Authed, "a person (never a session) who owns or administers the whole resolved project (core.Deps.ProjectAdmin), whose file this daemon holds", s.handleRulingRemove)
}

// rulingsProject is the resolved project, for a reader of all of it (else
// as WriteTarget answers, and an area member is forbidden).
func (s *Service) rulingsProject(ctx context.Context) (api.ProjectRef, error) {
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return api.ProjectRef{}, err
	}
	if !core.SubjectOf(ctx).Scope.WholeProject(t.Project.ID) {
		return api.ProjectRef{}, api.Errorf(api.CodeForbidden, "a project's rulings are for readers of the whole project")
	}
	return t.Project, nil
}

// mayChangeRulings checks the caller may change project's rulings here.
func (s *Service) mayChangeRulings(ctx context.Context, p api.ProjectRef) error {
	sub := core.SubjectOf(ctx)
	if !aPerson(sub) {
		return api.Errorf(api.CodeForbidden, "a person who owns or administers the project removes a ruling; an agent session cannot").
			WithHint("a removal is a person's; an add is carried out by anyone who operates on the project, with the owner's approved ask (zm project ruling add --ask)")
	}
	ok, err := s.d.ProjectAdmin(ctx, sub, p.ID)
	if err != nil {
		return err
	}
	if !ok {
		return api.Errorf(api.CodeForbidden, "only an owner or admin of %s changes its rulings", p.Slug)
	}
	if _, held := s.d.Projects.File(p.ID); !held {
		return api.NotFound("project", p.Slug).WithHint("a project joined from another machine has its rulings changed at its authority")
	}
	return nil
}

// mayAddRuling checks the caller may add a ruling to project here, and
// says whether it is a person who owns or administers it. The signed ask an
// add always names (rulingAsk) is the authority: such a person adds what it
// carries anywhere on its card; anyone else who operates on the whole
// project (a member, an agent session: owners never type commands) only
// carries out a ruling written into the approved choice's own action, which
// would run with the owner's power anyway.
func (s *Service) mayAddRuling(ctx context.Context, p api.ProjectRef) (bool, error) {
	sub := core.SubjectOf(ctx)
	owner := false
	if aPerson(sub) {
		ok, err := s.d.ProjectAdmin(ctx, sub, p.ID)
		if err != nil {
			return false, err
		}
		owner = ok
	}
	if !owner && !sub.Can(authz.Operate, authz.Object{Project: p.ID}) {
		return false, api.Errorf(api.CodeForbidden, "only a member of all of %s carries out an owner's ruling", p.Slug)
	}
	if _, held := s.d.Projects.File(p.ID); !held {
		return false, api.NotFound("project", p.Slug).WithHint("a project joined from another machine has its rulings changed at its authority")
	}
	return owner, nil
}

func (s *Service) rulingList(p api.ProjectRef) orchapi.RulingList {
	l := orchapi.RulingList{Project: p.Slug, Rulings: []orchapi.Ruling{}}
	l.Rulings = append(l.Rulings, s.rulings(p.ID)...)
	return l
}

func (s *Service) handleRulings(w http.ResponseWriter, r *http.Request) {
	p, err := s.rulingsProject(r.Context())
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.rulingList(p))
}

func (s *Service) handleRulingAdd(w http.ResponseWriter, r *http.Request) {
	var req orchapi.RulingAddRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	p, err := s.rulingsProject(ctx)
	owner := false
	if err == nil {
		owner, err = s.mayAddRuling(ctx, p)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	add := orchapi.Ruling{Name: strings.TrimSpace(req.Name), Text: strings.TrimSpace(req.Text), Ask: strings.ToUpper(strings.TrimSpace(req.Ask))}
	if add.Text == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "a ruling needs its text"))
		return
	}
	if err := s.rulingAsk(ctx, p, add, owner); err != nil {
		api.WriteError(w, err)
		return
	}
	// One yes adds each ruling it carries once, ever: a ruling an owner
	// removes never comes back on the old answer.
	reg := s.d.Store.Registry()
	if _, err := reg.ExecContext(ctx, `INSERT INTO ruling_asks (project, ask, name, used) VALUES (?, ?, ?, ?)`, p.ID, add.Ask, add.Name, ms(s.now())); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY") {
			err = api.Errorf(api.CodeConflict, "ask %s already added a ruling named %s once", add.Ask, add.Name).
				WithHint("adding it again takes a new owner's yes: file a new card carrying it")
		}
		api.WriteError(w, err)
		return
	}
	_, err = s.d.Projects.SetSection(ctx, p.ID, rulingsSectionName, func(old any) (any, error) {
		rs, _ := old.([]Ruling)
		if slices.ContainsFunc(rs, func(r Ruling) bool { return r.Name == add.Name }) {
			return nil, api.Errorf(api.CodeConflict, "%s already has a ruling named %s", p.Slug, add.Name).
				WithHint("zm project ruling rm " + add.Name + " first to replace it")
		}
		return append(slices.Clone(rs), add), nil
	})
	if err != nil {
		_, _ = reg.ExecContext(context.WithoutCancel(ctx), `DELETE FROM ruling_asks WHERE project = ? AND ask = ? AND name = ?`, p.ID, add.Ask, add.Name)
		api.WriteError(w, err)
		return
	}
	s.d.Log.Info("approvals: owner ruling added", "project", p.ID, "ruling", add.Name, "ask", add.Ask, "by", core.SubjectOf(ctx).Address)
	api.WriteJSON(w, http.StatusOK, s.rulingList(p))
}

func (s *Service) handleRulingRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	p, err := s.rulingsProject(ctx)
	if err == nil {
		err = s.mayChangeRulings(ctx, p)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	_, err = s.d.Projects.SetSection(ctx, p.ID, rulingsSectionName, func(old any) (any, error) {
		rs, _ := old.([]Ruling)
		i := slices.IndexFunc(rs, func(r Ruling) bool { return r.Name == name })
		if i < 0 {
			return nil, api.NotFound("ruling", name)
		}
		rs = slices.Delete(slices.Clone(rs), i, i+1)
		if len(rs) == 0 {
			return nil, nil // the section goes
		}
		return rs, nil
	})
	if err != nil {
		api.WriteError(w, err)
		return
	}
	s.d.Log.Info("approvals: owner ruling removed", "project", p.ID, "ruling", name, "by", core.SubjectOf(ctx).Address)
	api.WriteJSON(w, http.StatusOK, s.rulingList(p))
}

// rulingAsk checks that add.Ask is an ask of project p whose frozen card
// proposes add (cardCarries), answered on a paired device or with a key
// (a signed receipt), by an owner speaking for themselves, with the choice
// the card proposes (its action's, else its recommended one), by someone who
// still owns or administers the project. Anything else is refused: an ask
// the caller cannot see is not_found.
func (s *Service) rulingAsk(ctx context.Context, p api.ProjectRef, add orchapi.Ruling, owner bool) error {
	if add.Ask == "" {
		return api.Errorf(api.CodeBadRequest, "name the approved ask that carries the ruling (--ask <id>)").
			WithHint("changing rulings is an owner decision: file a card (zm ask new) with the ruling's exact text, and add it once an owner says yes")
	}
	sub := core.SubjectOf(ctx)
	a, err := s.ask(ctx, add.Ask)
	if err == nil && (a.Project != p.ID || !mayRead(sub.Scope, sub.Address, a)) {
		err = api.NotFound("ask", add.Ask)
	}
	if err != nil {
		return err
	}
	refuse := func(format string, args ...any) error {
		return api.Errorf(api.CodeConflict, format, args...).
			WithHint("file a card (zm ask new) carrying the ruling's exact text, and have an owner answer yes on a paired phone, in the Mezze app or with a key")
	}
	switch {
	case a.Card == nil || !cardCarries(*a.Card, add.Name, add.Text, owner):
		if !owner {
			return refuse("ask %s's approved action does not carry this ruling: its brief or plan must quote the name and the text whole, each in double quotes (or an owner adds it)", a.ID)
		}
		return refuse("ask %s's card does not propose this ruling: one field of it must carry the name and the text word for word, each in double quotes", a.ID)
	case a.Withdrawn != nil:
		return refuse("ask %s was withdrawn", a.ID)
	case a.Decision == nil:
		return refuse("ask %s has no answer yet", a.ID)
	case a.Decision.Decision != proposedChoice(*a.Card) || a.Decision.Text != "" ||
		a.Decision.AnsweredAs != "" && a.Decision.AnsweredAs != orchapi.AnsweredAsOwner:
		return refuse("ask %s was not answered with the choice its card proposes, plainly, by an owner speaking for themselves", a.ID)
	case a.Decision.Method != orchapi.MethodApprover:
		return refuse("ask %s was answered without a signature: a ruling takes an answer on a paired device or with a key", a.ID)
	}
	why, err := s.approverAdmin(ctx, a.Decision.Approver, a.Decision.Receipt.Node, ms(a.Decision.Time), p.ID)
	if err != nil {
		return err
	}
	if why != "" {
		return refuse("%s, who answered, %s", a.Decision.Approver, why)
	}
	return nil
}

// proposedChoice is the choice card proposes: its action's, else its
// recommended one.
func proposedChoice(c orchapi.AskCard) string {
	if c.Action != nil && c.Action.On != "" {
		return c.Action.On
	}
	return c.Recommend
}

// cardCarries reports whether the card was written to propose this
// ruling: a field of it (what card_sha256 and so the receipt cover) carries
// the ruling's name and its text, each whole and in double quotes, as a
// [[rulings]] table quotes them. For anyone but an owner (anywhere) only
// the action's own brief or plan counts: what the approver's choice runs.
func cardCarries(c orchapi.AskCard, name, text string, anywhere bool) bool {
	var fields []string
	if anywhere {
		fields = append(fields, c.Title, c.Context, c.Background, c.Question, c.Why)
		for _, ch := range c.Choices {
			fields = append(fields, ch.Label, ch.Pros, ch.Cons)
		}
	}
	if c.Action != nil {
		fields = append(fields, c.Action.Brief, c.Action.Plan)
	}
	qn, qt := `"`+name+`"`, `"`+text+`"`
	return slices.ContainsFunc(fields, func(f string) bool { return strings.Contains(f, qn) && strings.Contains(f, qt) })
}

var rulingMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0015_ruling_asks", Script: `
CREATE TABLE ruling_asks (
	project TEXT NOT NULL,
	ask TEXT NOT NULL,
	name TEXT NOT NULL,
	used INTEGER NOT NULL,
	PRIMARY KEY (project, ask, name)
);`},
}
