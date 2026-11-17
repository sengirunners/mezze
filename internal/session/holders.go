package session

// Session holders (package holder, SPEC 8 "Daemon restarts"): every managed
// session runs under its own holder process, so it outlives the daemon. This
// file starts holders, adopts them (at spawn and when the daemon starts
// again), streams their output into the daemon's mirror and records their
// exits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
)

// catchUpWait bounds how long an adoption waits for the mirror to hold the
// holder's whole ring, so the first attach after a restart replays it all.
const catchUpWait = 2 * time.Second

// recheckEvery is how often sessions this daemon could not adopt are tried
// again (a downgrade, or the holder's child exited).
const recheckEvery = 15 * time.Second

// KeepSessions implements core.SessionKeeper. Kept, sessions outlive this
// daemon: Close lets go of their holders instead of ending them, and a crash
// leaves them running. Only `zm daemon` keeps them (set before Start); every
// other user of the module (tests) ties its holders to itself, so they end
// with it. KeepSessions(false) later (`zm daemon stop --force`) makes Close
// end them.
func (s *Service) KeepSessions(keep bool) { s.keep.Store(keep) }

// RestartNote implements core.SessionKeeper: why kept sessions would end with
// this daemon anyway (a systemd unit without KillMode=process), "" when they
// survive it. Asked afresh: a unit can be fixed while the daemon runs.
func (s *Service) RestartNote(ctx context.Context) string {
	if !s.keep.Load() {
		return ""
	}
	return holder.UnitKillsSessions(ctx)
}

func (s *Service) holderExe() (string, error) {
	if s.HolderExe != "" {
		return s.HolderExe, nil
	}
	return os.Executable()
}

// startHolder starts a holder for spec and adopts it. A child that exited
// before the adoption (a quick command) leaves its exit file: p is nil and
// pid its pid, and the caller ends the session from the file.
func (s *Service) startHolder(spec holder.Spec, addr, agent string, started time.Time) (p *proc, pid int, err error) {
	exe, err := s.holderExe()
	if err != nil {
		return nil, 0, err
	}
	if s.HolderExe == "" && s.pinnedExe != "" {
		exe = s.pinnedExe // this build, whatever was written over the installed file since
	}
	spec.Tied = !s.keep.Load()
	st, err := holder.Start(exe, spec)
	if err != nil {
		return nil, 0, err
	}
	if st.LaunchGrant != nil {
		defer st.LaunchGrant.Close()
	}
	reaped := make(chan struct{})
	go func() { _ = st.Cmd.Wait(); close(reaped) }() // reap it if it ends while this daemon runs
	holderStart, holderKnown := procStat(st.Cmd.Process.Pid)
	group := st.GroupPID
	if group == 0 {
		group = st.ChildPID
	}
	groupStart, groupKnown := procStat(group)
	giveUp := func(err error) (*proc, int, error) {
		if exitedSoon(spec.Home, spec.Session) {
			if st.Tie != nil {
				_ = st.Tie.Close()
			}
			return nil, st.ChildPID, nil
		}
		// This call started these processes; still verify their starts before
		// signalling, because either pid may have been reused meanwhile.
		if current, ok := procStat(group); groupKnown && ok && current.start == groupStart.start {
			if table, e := procTable(); e == nil && table[group].pgid == group {
				_ = syscall.Kill(-group, syscall.SIGKILL)
			}
		}
		if current, ok := procStat(st.Cmd.Process.Pid); holderKnown && ok && current.start == holderStart.start {
			_ = syscall.Kill(st.Cmd.Process.Pid, syscall.SIGKILL)
		}
		if st.Tie != nil {
			_ = st.Tie.Close()
		}
		s.updates.settle(addr, true) // it never started: forget its hold
		// The holder writes its exit file as its child dies: once it is gone,
		// remove what it left (the spawn failed; nothing reads them). Not
		// waited for here: Spawn holds spawnMu.
		go func() {
			select {
			case <-reaped:
			case <-time.After(3 * time.Second):
			}
			holder.Forget(spec.Home, spec.Session)
		}()
		return nil, 0, err
	}
	if err := s.recordStartedHolder(context.Background(), spec.Session, st.Cmd.Process.Pid); err != nil {
		return giveUp(err)
	}
	if spec.Task {
		group := st.GroupPID
		if group == 0 {
			group = st.ChildPID
		}
		// Record the holder's group before probing it: a short-lived task may
		// already have exited, leaving only the holder's verified exit record.
		if _, err := s.d.Store.Registry().ExecContext(context.Background(), `UPDATE session_tasks SET pgid=? WHERE id=?`, group, spec.Session); err != nil {
			return giveUp(err)
		}
		if stt, ok := procStat(group); ok {
			if _, err := s.d.Store.Registry().ExecContext(context.Background(), `UPDATE session_tasks SET pgid=?,group_start=? WHERE id=?`, group, stt.start, spec.Session); err != nil {
				return giveUp(err)
			}
		} else {
			return giveUp(fmt.Errorf("cannot identify task process group"))
		}
		// Adoption can still fail after exec. Keep the known child identity
		// so redaction can later prove every writer gone after giveUp removes
		// holder artifacts. An unknown kernel start remains conservative.
		if child, ok := procStat(st.ChildPID); st.ChildPID > 1 && ok && child.start > 0 {
			res, err := s.d.Store.Registry().ExecContext(context.Background(), `
 UPDATE sessions SET pid=?,pid_start=? WHERE id=? AND managed=1 AND state!='ended'
 AND (pid=0 OR (pid=? AND pid_start=?))
 AND EXISTS(SELECT 1 FROM session_tasks WHERE id=? AND state='running')`,
				st.ChildPID, child.start, spec.Session, st.ChildPID, child.start, spec.Session)
			if err != nil {
				return giveUp(err)
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				return giveUp(fmt.Errorf("task child identity was not recorded: rows=%d: %v", n, err))
			}
		}
	}
	hc, err := holder.Dial(spec.Home, spec.Session)
	if err == nil && hc.Hello.ChildPID != st.ChildPID {
		_ = hc.Close()
		err = fmt.Errorf("holder answered with child %d, started %d", hc.Hello.ChildPID, st.ChildPID)
	}
	if err != nil {
		var inc *holder.IncompatibleError
		if errors.As(err, &inc) {
			err = fmt.Errorf("%w; the zm binary changed under the running daemon: restart it (zm daemon stop; zm daemon --detach)", err)
		}
		return giveUp(err)
	}
	if hc.Hello.Exited {
		_ = hc.Close()
		return giveUp(errors.New("it exited at once"))
	}
	// The private launcher cannot probe or exec the agent until the durable
	// holder/group identity above exists and this holder answered correctly.
	if st.LaunchGrant != nil {
		if err := s.recordedTaskLaunch(spec.Session, st); err != nil {
			_ = hc.Close()
			return giveUp(err)
		}
		if _, err := st.LaunchGrant.Write([]byte{1}); err != nil {
			_ = hc.Close()
			return giveUp(fmt.Errorf("release recorded task launch: %w", err))
		}
		_ = st.LaunchGrant.Close()
	}
	p = &proc{id8: spec.Session, addr: addr, agent: agent, pid: st.ChildPID, state: orchapi.StateRunning,
		started: started, conns: map[io.Closer]struct{}{}, spawned: true}
	if st.Tie != nil {
		p.tie = st.Tie
	}
	if err := s.initDraft(p); err != nil {
		_ = hc.Close()
		p.tie = nil
		return giveUp(err)
	}
	// Adopted, the proc is live (sampled, titled, hooked) before spawn
	// publishes started: its updated events wait for started.
	s.updates.hold(addr)
	if err := s.adopt(p, hc); err != nil {
		p.tie = nil
		return giveUp(err)
	}
	return p, st.ChildPID, nil
}

// exitedSoon reports whether a holder that hung up during its adoption left
// an exit file (its child ended at once), waiting a moment for it.
func exitedSoon(home, session string) bool {
	for i := 0; i < 20; i++ {
		if _, ok := holder.ReadExit(home, session); ok {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// adopt takes the PTY master from hc and registers p as live; the caller
// runs s.run(p).
func (s *Service) adopt(p *proc, hc *holder.Conn) error {
	f, err := hc.Adopt(hc.Hello.Start)
	if err != nil {
		_ = hc.Close()
		return err
	}
	p.hc, p.pty, p.out = hc, f, holder.NewRing(scrollbackSize)
	if hc.Hello.Pipe {
		p.pipe = newPipeState()
		if p.spawned { // registered below, before Spawn's pipeStart writes the start
			p.pipe.startSent = make(chan struct{})
		}
	}
	p.out.WriteAt(hc.Hello.Start, nil) // nothing before it is held (WriteAt keeps a floor)
	p.replayEnd, p.caught, p.exited = hc.Hello.End, make(chan struct{}), make(chan struct{})
	p.rawExited = make(chan struct{})
	if p.replayEnd <= hc.Hello.Start {
		close(p.caught)
	}
	p.modes.on, p.modes.paste = p.utf8Mouse.Store, p.bracketedPaste.Store
	s.loadDraft(p)
	s.unreachable.Delete(p.id8)
	s.mu.Lock()
	s.procs[p.id8] = p
	s.mu.Unlock()
	return nil
}

// run reads the holder's stream until the session ends or the connection
// drops.
func (s *Service) run(p *proc) {
	var pendingReplay *holder.ReplayCheckpoint
	for {
		typ, b, err := p.hc.Next()
		if err != nil {
			break
		}
		switch typ {
		case holder.FrameReplay:
			pendingReplay = readHolderReplay(p.out, p.hc.Hello.ReplayModes, b)
		case holder.FrameOutput:
			if off, data, err := holder.ParseOutput(b); err == nil {
				prepareHolderReplay(p.out, pendingReplay, off)
				s.feed(p, off, data)
			}
			pendingReplay = nil
		case holder.FrameExit:
			pendingReplay = nil
			var x holder.Exit
			if json.Unmarshal(b, &x) == nil {
				s.finish(p, x)
				return
			}
		default:
			pendingReplay = nil
		}
	}
	if p.detached.Load() {
		return // this daemon let go (Close); the holder keeps the session
	}
	if x, ok := holder.ReadExit(s.d.Config.Home, p.id8); ok {
		s.finish(p, x)
		return
	}
	if gone, why := s.recordedHolderGone(context.Background(), p.id8); !gone {
		// The holder lives (it hung up on a daemon too slow to read, or the
		// socket broke): the agent runs on. Never end it or remove its
		// socket; let go and adopt it again.
		if !s.lose(p) {
			return
		}
		s.unreachable.Store(p.id8, "holder is not proven gone: "+why)
		if r, ok := s.load(s.ctx, p.id8); ok && s.ctx.Err() == nil {
			s.readopt(s.ctx, r)
		}
		return
	}
	if exitedSoon(s.d.Config.Home, p.id8) { // it exited as the stream ended
		x, _ := holder.ReadExit(s.d.Config.Home, p.id8)
		s.finish(p, x)
		return
	}
	s.finish(p, holder.Exit{Code: -1, OutBytes: -1})
}

// lose lets go of a live session whose holder stream dropped: it stays
// running and unreachable until readopt (now, then every recheckEvery)
// adopts it again. Attached clients see their stream end and attach again.
func (s *Service) lose(p *proc) bool {
	s.mu.Lock()
	current := s.procs[p.id8] == p
	if current {
		delete(s.procs, p.id8)
	}
	s.mu.Unlock()
	if !current { // a newer adoption replaced it (the holder hung up on this one): nothing to do
		_ = p.hc.Close()
		_ = p.pty.Close()
		return false
	}
	s.unreachable.Store(p.id8, "the connection to the process holding its terminal dropped; zm is adopting it again")
	_ = p.hc.Close()
	_ = p.pty.Close()
	if p.tie != nil {
		// A tied holder stays tied to this daemon: hand the tie to the next proc.
		s.ties.Store(p.id8, p.tie)
	}
	p.out.Close()
	p.closeConns()
	s.d.Log.Warn("session holder stream dropped; adopting again", "session", p.id8, "holder", p.hc.Hello.HolderPID)
	return true
}

// finish records a session's end from its holder's exit (OutBytes -1: the
// holder went away without one) and lets the holder go.
func (s *Service) finish(p *proc, x holder.Exit) {
	_, end := p.out.Bounds()
	var o orchapi.Outcome
	if x.OutBytes < 0 {
		o = orchapi.Outcome{Kind: orchapi.OutcomeInfra, Confidence: "high", Note: "its holder exited",
			Evidence: "the process holding its terminal went away without reporting the exit"}
	} else {
		xi := exitInfo{agent: p.agent, code: x.Code, signal: x.Signal}
		p.mu.Lock()
		xi.stopped, xi.stopBy, xi.daemonStop = p.stopped || x.StopBy != "", p.stopBy, p.daemonStop
		p.mu.Unlock()
		if xi.stopBy == "" {
			xi.stopBy = x.StopBy
		}
		if !xi.stopped && x.StopSig != "" && x.StopBy == "" {
			xi.daemonStop = true // a tied holder whose daemon went away
		}
		if p.pipe == nil { // a pipe session's output is events: its words are the agent's, never evidence
			xi.tail, _, _, _, _ = p.out.Next(end-tailSize, tailSize)
		}
		o = classify(xi)
		end = max(end, x.OutBytes)
	}
	data, _, _, _, _ := p.out.Next(0, scrollbackSize)
	if len(data) > 0 {
		if err := s.saveSessionOutput(p.id8, data); err != nil {
			s.d.Log.Warn("session scrollback", "session", p.id8, "err", err)
		}
	}
	if x.OutBytes >= 0 {
		s.recordSessionDrain(context.Background(), p.id8)
	}
	s.flushTitleNow(p)
	close(p.rawExited)
	if _, ok := s.taskIDs.Load(p.id8); ok {
		unlock := s.lockTaskStop(p.id8)
		defer unlock()
		s.recordTaskGroupStop(context.Background(), p.id8, x.GroupStopped)
		o = s.taskExitOutcome(context.Background(), p.id8, p.agent, o, end)
	}
	groupCleanup := true
	if _, isTask := s.taskIDs.Load(p.id8); isTask && x.StopSig != "" && x.StopSig != "KILL" {
		groupCleanup = false
	}
	s.endWithGroup(context.Background(), p.id8, o, groupCleanup, func(r *row) { r.outBytes = end })
	s.completeTask(p.id8)
	// Record the end before attached clients see EOF, so they can report it.
	p.out.Close()
	s.mu.Lock()
	delete(s.procs, p.id8)
	s.mu.Unlock()
	p.closeConns()
	s.unreachable.Delete(p.id8)
	s.adopting.Delete(p.id8)
	select {
	case <-p.caught:
	default:
		close(p.caught)
	}
	close(p.exited)
	_ = p.hc.Ack()
	_ = p.hc.Close()
	_ = p.pty.Close()
	if p.tie != nil {
		_ = p.tie.Close()
	}
	holder.Forget(s.d.Config.Home, p.id8)
	s.recoverSessionOutputHolds(context.Background())
}

// endFromFile records the end of a session whose holder is gone: from its
// exit file when it left one, else infra.
func (s *Service) endFromFile(ctx context.Context, r row) {
	if _, isTask := s.taskIDs.Load(r.ID); isTask {
		unlock := s.lockTaskStop(r.ID)
		defer unlock()
	}
	if _, exited := holder.ReadExit(s.d.Config.Home, r.ID); !exited {
		if gone, why := s.recordedHolderGone(ctx, r.ID); !gone {
			s.unreachable.Store(r.ID, "holder is not proven gone: "+why)
			return
		}
	}
	home := s.d.Config.Home
	o := orchapi.Outcome{Kind: orchapi.OutcomeInfra, Confidence: "high", Note: "daemon restarted",
		Evidence: "nothing holds its terminal any more"}
	outBytes := r.outBytes
	if x, ok := holder.ReadExit(home, r.ID); ok {
		s.recordSessionDrain(ctx, r.ID)
		if _, task := s.taskIDs.Load(r.ID); task {
			s.recordTaskGroupStop(ctx, r.ID, x.GroupStopped)
		}
		xi := exitInfo{agent: r.Agent, code: x.Code, signal: x.Signal, stopped: x.StopBy != "", stopBy: x.StopBy,
			noDaemon: !x.At.IsZero() && x.At.Before(s.startedAt)}
		if b, err := os.ReadFile(s.scrollbackPath(r.ID)); err == nil && r.Mode != orchapi.SessionModePipe {
			xi.tail = b[max(0, len(b)-tailSize):]
		}
		if !xi.stopped && x.StopSig != "" {
			xi.daemonStop = true
		}
		o, outBytes = classify(xi), x.OutBytes
	}
	if _, ok := s.taskIDs.Load(r.ID); ok {
		o = s.taskExitOutcome(ctx, r.ID, r.Agent, o, outBytes)
	}
	groupCleanup := true
	if _, isTask := s.taskIDs.Load(r.ID); isTask {
		if x, ok := holder.ReadExit(home, r.ID); ok && x.StopSig != "" && x.StopSig != "KILL" {
			groupCleanup = false
		}
	}
	s.endWithGroup(ctx, r.ID, o, groupCleanup, func(rr *row) { rr.outBytes = outBytes })
	s.completeTask(r.ID)
	holder.Forget(home, r.ID)
	s.recoverSessionOutputHolds(ctx)
	s.unreachable.Delete(r.ID)
	s.adopting.Delete(r.ID)
	if t, ok := s.ties.LoadAndDelete(r.ID); ok {
		_ = t.(io.Closer).Close()
	}
}

// readopt finds the holder of a managed session this daemon does not run
// (at start, and every recheckEvery for those it could not adopt).
func (s *Service) readopt(ctx context.Context, r row) {
	// One adoption per session at a time: a lost stream's readopt and the
	// recheck ticker's must not both adopt it.
	mu, _ := s.adopting.LoadOrStore(r.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	defer func() {
		if current, ok := s.load(ctx, r.ID); ok && current.State == orchapi.StateEnded {
			s.unreachable.Delete(r.ID)
		}
	}()
	if s.proc(r.ID) != nil {
		s.unreachable.Delete(r.ID) // adopted meanwhile
		return
	}
	hc, err := holder.Dial(s.d.Config.Home, r.ID)
	var inc *holder.IncompatibleError
	switch {
	case errors.As(err, &inc):
		s.unreachable.Store(r.ID, "its terminal is held by an older or newer zm: "+inc.Error()+
			"; it keeps running, and zm adopts it again once versions match")
		return
	case errors.Is(err, holder.ErrNoHolder):
		s.endFromFile(ctx, r)
		return
	case err != nil:
		_, exited := holder.ReadExit(s.d.Config.Home, r.ID)
		if gone, _ := s.recordedHolderGone(ctx, r.ID); gone || exited {
			s.endFromFile(ctx, r)
			return
		}
		s.unreachable.Store(r.ID, "zm cannot reach the process holding its terminal: "+err.Error())
		s.d.Log.Warn("session holder", "session", r.ID, "err", err)
		return
	}
	if err := s.recordAdoptedHolder(ctx, r.ID, hc.Hello.HolderPID); err != nil {
		_ = hc.Close()
		s.unreachable.Store(r.ID, "holder identity cannot be verified: "+err.Error())
		return
	}
	if hc.Hello.Exited {
		_ = hc.Close()
		s.endFromFile(ctx, r)
		return
	}
	if why := s.checkChild(hc.Hello, r.PID); why != "" {
		_ = hc.Close()
		s.unreachable.Store(r.ID, why)
		s.d.Log.Warn("session holder", "session", r.ID, "err", why)
		return
	}
	p := &proc{id8: r.ID, addr: r.Address, agent: r.Agent, pid: r.PID, state: r.State, started: time.Now(),
		title: r.TermTitle, conns: map[io.Closer]struct{}{}}
	if t, ok := s.ties.LoadAndDelete(r.ID); ok {
		p.tie = t.(io.Closer)
	}
	if err := s.adopt(p, hc); err != nil {
		if p.tie != nil {
			s.ties.Store(r.ID, p.tie)
		}
		if exitedSoon(s.d.Config.Home, r.ID) { // its child exited during the adoption
			s.endFromFile(ctx, r)
			return
		}
		s.unreachable.Store(r.ID, "zm cannot adopt the process holding its terminal: "+err.Error())
		return
	}
	s.goRun(p)
	select {
	case <-p.caught:
	case <-time.After(catchUpWait):
	}
	s.d.Log.Info("session adopted", "session", r.ID, "holder", hc.Hello.HolderPID, "holder_version", hc.Hello.Version)
}

// checkChild says why a holder's child is not the session's process ("" when it is).
func (s *Service) checkChild(h holder.Hello, pid int) string {
	if h.ChildPID != pid {
		return fmt.Sprintf("its holder runs pid %d, not the session's pid %d", h.ChildPID, pid)
	}
	table, err := procTable()
	if err != nil {
		return "" // cannot tell; the holder's socket and hello already matched
	}
	if e, ok := table[pid]; ok && e.ppid != h.HolderPID {
		return fmt.Sprintf("pid %d is not the child of its holder %d", pid, h.HolderPID)
	}
	return ""
}

// readoptAll adopts every managed session that is not ended.
func (s *Service) readoptAll(ctx context.Context) error {
	rows, err := listRows(ctx, s.d.Store.Registry(), `WHERE managed = 1 AND state != ?`, string(orchapi.StateEnded))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Machine != "" && r.Machine != s.d.Config.Machine {
			continue
		}
		s.readopt(ctx, r)
	}
	return nil
}

// recheck retries the sessions this daemon could not adopt.
func (s *Service) recheck(ctx context.Context) {
	s.unreachable.Range(func(k, _ any) bool {
		if r, ok := s.load(ctx, k.(string)); ok && r.Managed && r.State != orchapi.StateEnded {
			s.readopt(ctx, r)
		} else {
			s.unreachable.Delete(k)
		}
		return true
	})
}

// unreachableWhy is why a live managed session cannot be attached ("" when it can).
func (s *Service) unreachableWhy(id8 string) string {
	if v, ok := s.unreachable.Load(id8); ok {
		return v.(string)
	}
	return ""
}

// HolderReport implements core.HolderReporter: what `zm status` says about
// session holders (local callers only).
func (s *Service) HolderReport() *orchapi.HolderReport {
	rep := &orchapi.HolderReport{Adopted: len(s.live())}
	s.unreachable.Range(func(k, v any) bool {
		rep.Unreachable = append(rep.Unreachable, orchapi.HolderProblem{Session: k.(string), Why: v.(string)})
		return true
	})
	sort.Slice(rep.Unreachable, func(i, j int) bool { return rep.Unreachable[i].Session < rep.Unreachable[j].Session })
	if orphans, err := holder.Orphans(); err == nil {
		for _, o := range orphans {
			rep.Orphans = append(rep.Orphans, orchapi.HolderOrphan{PID: o.PID, Session: o.Session, Home: o.Home})
		}
	}
	if rep.Adopted == 0 && len(rep.Unreachable) == 0 && len(rep.Orphans) == 0 {
		return nil
	}
	return rep
}

// Only the holder's explicit successful group signal is evidence for handoff.
func (s *Service) recordTaskGroupStop(ctx context.Context, id string, stopped *bool) {
	if stopped == nil || !*stopped {
		return
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET group_stopped=1 WHERE id=?`, id); err != nil {
		s.d.Log.Warn("record task group stop", "id", id, "err", err)
	}
}
