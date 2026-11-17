package issues

// Moving open issues between projects of this authority (SPEC "Native
// issues": moves): POST /v0/issues/move, and the reads that follow a moved
// issue's redirect and resolve its cross-project edges.

import (
	"context"
	"errors"
	"sort"
	"strings"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// maxHops bounds a redirect chain (an issue moved on again).
const maxHops = 4

// locateFor is locate, then, for an id that moved to another project, the
// issue it became, as sub sees it there (hidden there = not_found).
func (s *Service) locateFor(ctx context.Context, sub *authz.Subject, sc authz.Scope, id string) (located, error) {
	l, err := s.locate(ctx, sc, id)
	if err == nil || !api.IsCode(err, api.CodeNotFound) || sub == nil {
		return l, err
	}
	nf := err
	cur := id
	for range maxHops {
		nid, ok, rerr := s.redirect(ctx, cur)
		if rerr != nil {
			return located{}, rerr
		}
		if !ok {
			return located{}, nf
		}
		if l, err = s.locate(ctx, sub.Scope, nid); err == nil {
			l.from = id
			return l, nil
		}
		if !api.IsCode(err, api.CodeNotFound) {
			return located{}, err
		}
		cur = nid
	}
	return located{}, nf
}

// redirect is where id went, when it moved out of the project its prefix
// names.
func (s *Service) redirect(ctx context.Context, id string) (string, bool, error) {
	project, err := s.projectOf(ctx, id)
	if err != nil || project == "" {
		return "", false, err
	}
	ps, err := s.st.Project(ctx, project, false)
	if errors.Is(err, istore.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	mv, ok := ps.(istore.Mover)
	if !ok {
		return "", false, nil
	}
	nid, _, ok, err := mv.MovedTo(ctx, id)
	return nid, ok, err
}

// MovedTo implements core.IssueService.
func (s *Service) MovedOut(ctx context.Context, project, id string) (bool, error) {
	ps, err := s.st.Project(ctx, project, false)
	if err != nil {
		return false, err
	}
	mv, ok := ps.(istore.Mover)
	if !ok {
		return false, nil
	}
	_, _, moved, err := mv.MovedTo(ctx, id)
	return moved, err
}

// MovedTo implements core.IssueService.
func (s *Service) MovedTo(ctx context.Context, id string) (string, string, string, bool, error) {
	cur := id
	for hops := 0; ; hops++ {
		nid, ok, err := s.redirect(ctx, cur)
		if err != nil {
			return "", "", "", false, err
		}
		if !ok {
			break
		}
		if hops == maxHops {
			return "", "", "", false, nil
		}
		cur = nid
	}
	if cur == id {
		return "", "", "", false, nil
	}
	from, err := s.projectOf(ctx, id)
	if err != nil {
		return "", "", "", false, err
	}
	to, err := s.projectOf(ctx, cur)
	if err != nil || to == "" {
		return "", "", "", false, err
	}
	return cur, from, to, true, nil
}

// crossEdges resolves d's edges to issues of other projects here (the
// store keeps them as external edges naming the id): the issue as sub sees
// it, or left out when sub cannot. An id no project here owns stays as is.
func (s *Service) crossEdges(ctx context.Context, sub *authz.Subject, d *orchapi.IssueDetail) error {
	out := d.DependsOn[:0]
	for _, ln := range d.DependsOn {
		if ln.Title != "" || strings.HasPrefix(ln.ID, "external:") {
			out = append(out, ln)
			continue
		}
		p, err := s.projectOf(ctx, ln.ID)
		if err != nil {
			return err
		}
		if p == "" {
			out = append(out, ln)
			continue
		}
		l, err := s.locateFor(ctx, sub, sub.Scope, ln.ID)
		if api.IsCode(err, api.CodeNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		it, err := l.ps.Get(ctx, l.id, l.vis)
		if err != nil {
			return s.storeErr(err, l.id)
		}
		ln.Issue = it
		out = append(out, ln)
	}
	d.DependsOn = out
	return nil
}

// visibleEdge keeps unknown external references, but hides known issues the
// caller cannot read, including issues moved into another project.
func (s *Service) visibleEdge(ctx context.Context, sub *authz.Subject, id string) (bool, error) {
	if strings.HasPrefix(id, "external:") {
		return true, nil
	}
	p, err := s.projectOf(ctx, id)
	if err != nil || p == "" {
		return p == "", err
	}
	_, err = s.locateFor(ctx, sub, sub.Scope, id)
	if api.IsCode(err, api.CodeNotFound) {
		return false, nil
	}
	return err == nil, err
}

// moveEnds is a move's two projects.
type moveEnds struct {
	src, dst       api.ProjectRef
	sps, dps       istore.ProjectStore
	smv, dmv       istore.Mover
	srcPre, dstPre string
}

// moveStep is one planned issue.
type moveStep struct {
	item     orchapi.IssueMoveItem
	holder   string // a live lease's holder
	homeArea string // the copy's home area ("" = the target's project level)
}

// move runs (or with DryRun plans) req from the resolved project. A run
// plans again under both projects' holds, so it moves exactly what that
// plan says (an issue closed or claimed since the dry run is refused).
func (s *Service) move(ctx context.Context, sub *authz.Subject, req orchapi.IssueMoveRequest) (orchapi.IssueMoveResult, error) {
	e, err := s.moveEnds(ctx, sub, req.To)
	if err != nil {
		return orchapi.IssueMoveResult{}, err
	}
	res := orchapi.IssueMoveResult{From: e.src.Slug, To: e.dst.Slug, DryRun: req.DryRun, Items: []orchapi.IssueMoveItem{}}
	ids, err := s.moveSet(ctx, e.sps, req)
	if err != nil {
		return res, err
	}
	if req.DryRun {
		return s.movePlan(ctx, e, req, ids, res)
	}
	res, leases, err := s.moveHeld(ctx, sub, e, req, ids, res)
	if err != nil {
		return res, err
	}
	// Leases go through the work module, whose writes to the issue the holds
	// would refuse: after them.
	notes := map[string]string{}
	for _, st := range leases {
		if n := s.moveLease(ctx, sub, e, st.item, st.holder); n != "" {
			notes[st.item.ID] = n
		}
	}
	for i, it := range res.Items {
		if n := notes[it.ID]; n != "" {
			res.Items[i].Note = strings.TrimPrefix(it.Note+"; "+n, "; ")
		}
	}
	// Last: the old lease's end still needs the old row.
	if mv, ok := orchcore.Work(s.d).(orchcore.WorkMoves); ok {
		sp, dp := s.nativePrefix(e.src.ID), s.nativePrefix(e.dst.ID)
		for _, it := range res.Items {
			if sp == "" || (it.Result != "moved" && it.Result != "already moved") {
				continue
			}
			to, nid := e.dst.ID, dp+":"+it.NewID
			if dp == "" {
				to, nid = "", ""
			}
			mv.ItemMoved(context.WithoutCancel(ctx), e.src.ID, sp+":"+it.ID, to, nid)
		}
	}
	return res, nil
}

// moveHooks are test hooks (nil in the daemon): one runs before each
// issue moves (a panic there must not leave the holds behind).
var moveHooks struct{ beforeOne func(id string) }

// moveHeld plans and moves under both projects' holds, released however it
// ends (a panic included): a hold left behind would stop every write there
// until the daemon restarts. It answers the moved issues that had a lease.
func (s *Service) moveHeld(ctx context.Context, sub *authz.Subject, e moveEnds, req orchapi.IssueMoveRequest, ids []string,
	res orchapi.IssueMoveResult) (orchapi.IssueMoveResult, []moveStep, error) {
	// Both projects take no other writes while issues move: nothing lands
	// between the plan, an issue's copy and its removal.
	why := "issues move from " + e.src.Slug + " to " + e.dst.Slug
	rel1, err := s.HoldWrites(e.src.ID, why)
	if err != nil {
		return res, nil, err
	}
	defer rel1()
	rel2, err := s.HoldWrites(e.dst.ID, why)
	if err != nil {
		return res, nil, err
	}
	defer rel2()
	res, steps, err := s.movePlanSteps(ctx, e, req, ids, res)
	if err != nil {
		return res, nil, err
	}
	// One moved out before a stop, its copy still pending: finish it.
	for i, it := range res.Items {
		if it.Result == "already moved" {
			if err := s.moveDone(ctx, sub, e, it.ID, it.NewID); err != nil {
				res.Items[i].Note = "its copy still refuses writes: " + s.storeErr(err, it.NewID).Error()
			}
		}
	}
	rename := map[string]string{}
	for _, st := range steps {
		rename[st.item.ID] = st.item.NewID
	}
	var leases []moveStep
	for _, st := range steps {
		if moveHooks.beforeOne != nil {
			moveHooks.beforeOne(st.item.ID)
		}
		it := s.moveOne(ctx, sub, e, st, rename)
		if it.Result == "moved" {
			res.Moved++
			if st.holder != "" {
				leases = append(leases, moveStep{item: it, holder: st.holder})
			}
		} else {
			res.Refused++
		}
		res.Items = append(res.Items, it)
	}
	return res, leases, nil
}

// moveEnds resolves the move's source (the resolved project) and target.
func (s *Service) moveEnds(ctx context.Context, sub *authz.Subject, to string) (moveEnds, error) {
	var e moveEnds
	t, err := s.d.WriteTarget(ctx, "", authz.Admin)
	if err != nil {
		return e, err
	}
	e.src = t.Project
	ref, ok := s.d.Projects.Lookup(strings.TrimSpace(to))
	if !ok || !sub.Scope.SeesProject(ref.ID) {
		return e, api.NotFound("project", to)
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: ref.ID}, "project", to); err != nil {
		return e, err
	}
	if ref.ID == e.src.ID {
		return e, api.Errorf(api.CodeBadRequest, "the issues are in %s already", to)
	}
	e.dst = ref
	if e.sps, err = s.st.Project(ctx, e.src.ID, false); errors.Is(err, istore.ErrNotFound) {
		return e, api.Errorf(api.CodeNotFound, "%s has no native issues", e.src.Slug)
	} else if err != nil {
		return e, s.storeErr(err, "")
	}
	if e.dps, err = s.st.Project(ctx, ref.ID, true); err != nil {
		return e, s.storeErr(err, "")
	}
	var sok, dok bool
	e.smv, sok = e.sps.(istore.Mover)
	e.dmv, dok = e.dps.(istore.Mover)
	if !sok || !dok {
		return e, api.Errorf(api.CodeBadRequest, "this issue store cannot move issues")
	}
	if e.srcPre, err = e.sps.Prefix(ctx); err != nil {
		return e, s.storeErr(err, "")
	}
	if e.dstPre, err = e.dps.Prefix(ctx); err != nil {
		return e, s.storeErr(err, "")
	}
	if e.dstPre == "" {
		return e, api.Errorf(api.CodeConflict, "%s has no issue prefix yet", ref.Slug).WithHint("zm issues setup -P " + ref.Slug + " <prefix>")
	}
	return e, nil
}

// movePlan is the dry run's answer.
func (s *Service) movePlan(ctx context.Context, e moveEnds, req orchapi.IssueMoveRequest, ids []string, res orchapi.IssueMoveResult) (orchapi.IssueMoveResult, error) {
	res, steps, err := s.movePlanSteps(ctx, e, req, ids, res)
	for _, st := range steps {
		res.Items = append(res.Items, st.item)
	}
	return res, err
}

// movePlanSteps plans every issue: refusals and issues already moved go
// into res, the issues that move come back as steps. Cross-project edges
// are computed against the issues that move, so the plan says what the
// run does.
func (s *Service) movePlanSteps(ctx context.Context, e moveEnds, req orchapi.IssueMoveRequest, ids []string, res orchapi.IssueMoveResult) (orchapi.IssueMoveResult, []moveStep, error) {
	var steps []moveStep
	var deps [][]orchapi.IssueDep
	areas := map[string]bool{}
	if f, ok := s.d.Projects.File(e.dst.ID); ok {
		for _, a := range f.Areas {
			areas[a.Name] = true
		}
	}
	for _, id := range ids {
		it := orchapi.IssueMoveItem{ID: id}
		refuse := func(note string) {
			it.Result, it.Note, it.NewID = "refused", note, ""
			res.Items, res.Refused = append(res.Items, it), res.Refused+1
		}
		if nid, _, ok, err := e.smv.MovedTo(ctx, id); err != nil {
			return res, nil, s.storeErr(err, id)
		} else if ok {
			it.NewID, it.Result = nid, "already moved"
			res.Items = append(res.Items, it)
			continue
		}
		mv, err := e.smv.Export(ctx, id)
		if errors.Is(err, istore.ErrNotFound) {
			refuse("no such issue")
			continue
		}
		if err != nil {
			return res, nil, s.storeErr(err, id)
		}
		it.Title, it.Status = mv.Issue.Title, mv.Issue.Status
		st := moveStep{holder: s.leaseHolder(ctx, e.src.ID, id)}
		it.Claim = claimOf(mv.Issue, st.holder)
		suffix, ok := strings.CutPrefix(id, e.srcPre+"-")
		switch {
		case !ok:
			refuse("its id does not start with " + e.srcPre + "-")
			continue
		case mv.Issue.Status == orchapi.IssueClosed:
			refuse("closed issues stay")
			continue
		case it.Claim != "" && !req.TransferClaims:
			refuse("claimed by " + it.Claim + "; release it or pass --transfer-claims")
			continue
		}
		it.NewID = e.dstPre + "-" + suffix
		from, _, moved, err := e.dmv.MovedFrom(ctx, it.NewID)
		if err != nil {
			return res, nil, s.storeErr(err, id)
		}
		if _, gerr := e.dps.Get(ctx, it.NewID, istore.Areas{All: true}); gerr == nil && (!moved || from != id) {
			refuse(it.NewID + " is another issue in " + e.dst.Slug)
			continue
		}
		if mv.HomeArea != "" {
			if areas[mv.HomeArea] {
				st.homeArea = mv.HomeArea
			} else {
				it.Note = "filed in area " + mv.HomeArea + ", which " + e.dst.Slug + " has not: its area comes from " + e.dst.Slug + "'s rules"
			}
		}
		it.Result = "move"
		st.item = it
		steps = append(steps, st)
		deps = append(deps, mv.Deps)
	}
	movers := map[string]bool{}
	for _, st := range steps {
		movers[st.item.ID] = true
	}
	for i := range steps {
		id := steps[i].item.ID
		var cross []string
		for _, dp := range deps[i] {
			switch {
			case dp.IssueID == id && !movers[dp.DependsOnID] && !strings.HasPrefix(dp.DependsOnID, "external:"):
				cross = append(cross, dp.Type+":"+dp.DependsOnID)
			case dp.IssueID != id && !movers[dp.IssueID]:
				cross = append(cross, dp.IssueID+":"+dp.Type)
			}
		}
		sort.Strings(cross)
		steps[i].item.Cross = cross
	}
	return res, steps, nil
}

// moveOne moves one planned issue: its copy into the target, its removal
// from the source, then the copy takes writes. A failure after the copy
// leaves it pending (refusing writes): running the move again finishes it.
func (s *Service) moveOne(ctx context.Context, sub *authz.Subject, e moveEnds, st moveStep, rename map[string]string) orchapi.IssueMoveItem {
	it := st.item
	fail := func(result string, err error) orchapi.IssueMoveItem {
		it.Result, it.Note = result, s.storeErr(err, it.ID).Error()
		return it
	}
	mv, err := e.smv.Export(ctx, it.ID)
	if err != nil {
		return fail("refused", err)
	}
	rin, err := e.dps.Apply(ctx, s.moveMutation(sub, e.dst.ID, istore.Mutation{Op: istore.OpMoveIn, ID: it.ID,
		Move: &istore.Move{Project: e.src.ID, NewID: it.NewID, From: &mv, Rename: rename, HomeArea: st.homeArea}}))
	if err != nil {
		return fail("refused", err)
	}
	rout, err := e.sps.Apply(ctx, s.moveMutation(sub, e.src.ID, istore.Mutation{Op: istore.OpMoveOut, ID: it.ID,
		Move: &istore.Move{Project: e.dst.ID, NewID: it.NewID}}))
	if err != nil {
		return fail("half moved", err)
	}
	if err := s.moveDone(ctx, sub, e, it.ID, it.NewID); err != nil {
		return fail("half moved", err)
	}
	it.Result = "moved"
	s.dropAttachments(ctx, e.src.ID, it.ID) // they stay behind, and so go
	for _, ch := range rin.Changes {
		s.publishChange(ctx, e.dst.ID, rin.Issue, ch, true)
	}
	for _, ch := range rout.Changes {
		s.publishChange(ctx, e.src.ID, rout.Issue, ch, false)
	}
	for _, tch := range append(rin.Touched, rout.Touched...) {
		s.publishIssue(ctx, tch.Project, tch)
	}
	return it
}

// moveDone lets the copy take writes (a no-op once it does).
func (s *Service) moveDone(ctx context.Context, sub *authz.Subject, e moveEnds, old, nid string) error {
	_, err := e.dps.Apply(ctx, s.moveMutation(sub, e.dst.ID, istore.Mutation{Op: istore.OpMoveDone, ID: old,
		Move: &istore.Move{Project: e.src.ID, NewID: nid}}))
	return err
}

// moveLease ends the moved issue's old lease and makes it again for its
// holder on the new id, where the target binds native issues. The answer
// is a note for the item ("" = nothing to say).
func (s *Service) moveLease(ctx context.Context, sub *authz.Subject, e moveEnds, it orchapi.IssueMoveItem, holder string) string {
	var notes []string
	if p := s.nativePrefix(e.src.ID); p != "" {
		if err := orchcore.Work(s.d).EndItemLease(context.WithoutCancel(ctx), e.src.ID, p+":"+it.ID, "", "moved to "+it.NewID); err != nil {
			notes = append(notes, "the old lease did not end: "+err.Error())
		}
	}
	p := s.nativePrefix(e.dst.ID)
	if p == "" {
		return strings.Join(append(notes, "the claim moved; "+e.dst.Slug+" does not bind native issues, so there is no lease"), "; ")
	}
	if _, err := orchcore.Work(s.d).ClaimItem(ctx, as(sub, holder), e.dst.ID, p+":"+it.NewID, orchapi.ClaimRequest{}); err != nil {
		notes = append(notes, "the claim moved but its lease was not made again ("+err.Error()+"): the holder claims it again")
	}
	return strings.Join(notes, "; ")
}

// moveMutation fills m's authz for a move in project, as mutate does
// (mutate itself refuses while the move holds the project's writes).
func (s *Service) moveMutation(sub *authz.Subject, project string, m istore.Mutation) istore.Mutation {
	m.Actor, m.At = actorOf(sub), s.now()
	m.AreaOf = s.areaFunc(project)
	id := m.ID
	m.Authorize = func(before, after string) error {
		if err := s.canWrite(sub, project, before, id); err != nil {
			return err
		}
		return s.canWrite(sub, project, after, id)
	}
	m.CanRead = func(area string) bool { return sub.Can(authz.Read, authz.Object{Project: project, Area: area}) }
	return m
}

// leaseHolder is who holds a live lease on id's work item ("" = nobody, or
// the project does not bind its native issues).
func (s *Service) leaseHolder(ctx context.Context, project, id string) string {
	p := s.nativePrefix(project)
	if p == "" {
		return ""
	}
	h, err := orchcore.Work(s.d).OwnerOf(ctx, s.d.System(), project, p+":"+id)
	if err != nil {
		return ""
	}
	return h
}

// claimOf is who holds it: the lease, else a native claim.
func claimOf(it orchapi.Issue, holder string) string {
	switch {
	case holder != "":
		return holder
	case it.Status != orchapi.IssueInProgress && it.Status != orchapi.IssueHooked && it.ClaimedBy == "":
		return ""
	case it.ClaimedBy != "":
		return it.ClaimedBy
	case it.Assignee != "":
		return it.Assignee
	}
	return "someone"
}

// moveSet is req's ids (and with Children every open descendant), parents
// before children.
func (s *Service) moveSet(ctx context.Context, ps istore.ProjectStore, req orchapi.IssueMoveRequest) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) bool {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
		ids = append(ids, id)
		return true
	}
	for _, id := range req.IDs {
		add(id)
	}
	if len(ids) == 0 {
		return nil, api.Errorf(api.CodeBadRequest, "name the issues to move")
	}
	if len(ids) > 5000 {
		return nil, api.Errorf(api.CodeBadRequest, "at most 5000 issues per move")
	}
	for i := 0; req.Children && i < len(ids); i++ {
		l, err := ps.List(ctx, orchapi.IssueQuery{Parent: ids[i], Status: "all", All: true}, istore.Areas{All: true}, s.now())
		if err != nil {
			return nil, s.storeErr(err, ids[i])
		}
		for _, c := range l.Issues {
			if c.Status != orchapi.IssueClosed {
				add(c.ID)
			}
		}
	}
	depth := func(id string) int { return strings.Count(id, ".") }
	sort.SliceStable(ids, func(i, j int) bool { return depth(ids[i]) < depth(ids[j]) })
	return ids, nil
}
