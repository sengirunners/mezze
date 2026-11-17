package team

// team's side of teamlink.Hooks: the replica copy follows its remote's
// life (join, leave, revocation, recovery, rescope), GET /v0/team/remotes
// shows it, and operator classes reach the machine-op queue.

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// rep is x's copy (nil before Start or after it went).
func rep(x *remote) *replica {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return copyOf(x)
}

// copyOf is x's copy; the caller holds x.Mu.
func copyOf(x *remote) *replica {
	rp, _ := x.Copy.(*replica)
	return rp
}

// joined is Hooks.Joined: the copy (round C), its first snapshot, then
// what the member sees.
func (s *Service) joined(ctx context.Context, x *remote, resp *api.JoinResponse) {
	id := x.Snapshot().Project.ID
	if s.rctx != nil {
		s.startReplica(s.rctx, x)
	}
	if rp := rep(x); rp != nil {
		deadline := time.Now().Add(JoinSnapshotWait)
		for !rp.isReady() && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(20 * time.Millisecond)
		}
		if rp.isReady() {
			if wl, err := rp.listItems(ctx, id, orchapi.WorkQuery{Limit: 5000}); err == nil {
				resp.Items = len(wl.Items)
			}
		} else {
			resp.Notes = append(resp.Notes, "the first snapshot is still loading: reads go to the authority until it lands")
		}
	}
	if resp.Items == 0 {
		var wl orchapi.WorkList
		if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/work?project="+id+"&limit=5000", nil, &wl); err == nil {
			resp.Items = len(wl.Items)
		}
	}
}

// removed is Hooks.Removed (zm leave): the notices, the copy and its file.
func (s *Service) removed(x *remote) []string {
	id := x.Snapshot().Project.ID
	s.forgetNotices(id)
	s.stopReplica(x, true)
	var removed []string
	if _, err := os.Stat(s.replicaPath(id)); err == nil {
		removed = append(removed, s.replicaPath(id))
		for _, sfx := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(s.replicaPath(id) + sfx)
		}
	}
	return removed
}

// dropCopy is Hooks.DropCopy (the caller holds x.ReplicaMu): a revoked or
// replaced remote's copy is closed and deleted.
func (s *Service) dropCopy(x *remote) error {
	s.stopReplicaLocked(x, true)
	x.PublicationMu.Lock()
	defer x.PublicationMu.Unlock()
	id := x.Snapshot().Project.ID
	// close's removal is best effort. Never reopen an old copy or WAL when
	// it could contain writes queued before the grant was revoked.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(s.replicaPath(id) + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	s.forgetRoleCache(id)
	return nil
}

func (s *Service) forgetRoleCache(project string) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	delete(s.cache.held, project)
	delete(s.cache.ended, project)
	delete(s.cache.notices, project)
	for _, projects := range s.cache.leaseIn {
		delete(projects, project)
	}
}

// copyRescope is Hooks.ScopeMoved (x.Mu held): a rescope, a role or area
// change takes the copy again from a snapshot.
func (s *Service) copyRescope(x *remote) {
	rp := copyOf(x)
	if rp == nil {
		return
	}
	rp.mu.Lock()
	rp.resnap++
	if rp.endPoll != nil {
		rp.endPoll()
	}
	rp.mu.Unlock()
	kick(rp.kickSync)
}

// projectMachines is Hooks.Machines: the machines with numbers for
// project (the session module's).
func (s *Service) projectMachines(ctx context.Context, project string) []string {
	rt, err := s.runner()
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range rt.ProjectMachines(ctx, project) {
		out = append(out, m.Machine)
	}
	return out
}

// classChanged is a Hooks.ClassChanged: a machine that became public gets
// no task it has not taken yet.
func (s *Service) classChanged(ctx context.Context, project, machine string, c api.MachineClass) {
	if c.Class != api.MachinePublic {
		return
	}
	s.refuseUnsent(ctx, project, machine)
}

// refuseUnsent refuses the starts for machine in project that no answer
// has carried yet, and those waiting in the line pinned to it: it just
// became public. (One already carried is the machine's now, as with a
// cancel.)
func (s *Service) refuseUnsent(ctx context.Context, project, machine string) {
	why := machine + " became a public machine before it took the task"
	ops, _ := s.Ops(ctx, teamlink.OpFilter{Project: project, Machine: machine, Kind: opTaskStart, Unsent: true})
	for _, o := range ops {
		if ok, _ := s.UpdateOp(ctx, o.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: &why, Unsent: true}); ok {
			s.refuseStart(ctx, o.ID, why)
		}
	}
	s.refusePinned(ctx, project, machine, why)
}
