package session

// Machine numbers (SPEC 8): this machine's memory, swap, disk and live
// sessions against the admission limit, for presence and GET /v0/machines
// (teamlink serves it), and an authority's record of each replica's
// (presence_machines).

import (
	"context"
	"encoding/json"
	"os/user"
	"sync"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var machineMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0004_machines", Script: `
CREATE TABLE presence_machines (
	machine TEXT NOT NULL,
	project TEXT NOT NULL,
	stats   TEXT NOT NULL,   -- JSON api.MachineStats
	seen    INTEGER NOT NULL,
	PRIMARY KEY (machine, project)
);`},
}

// machineEvery is how often this machine's numbers are sampled at most
// (tests shorten it).
var machineEvery = 30 * time.Second

var _ core.MachinePresence = (*Service)(nil)

// machineCache holds the last sample.
type machineCache struct {
	mu sync.Mutex
	st api.MachineStats
}

// MachineStats implements core.MachinePresence: this machine's numbers,
// sampled at most every machineEvery.
func (s *Service) MachineStats(ctx context.Context) api.MachineStats {
	st := s.machineNumbers(ctx)
	if s.d.Zm != nil { // fresh each time: a hold or an update check shows at once
		st.Zm = s.d.Zm()
	}
	return st
}

// machineNumbers are the sampled numbers, at most every machineEvery.
func (s *Service) machineNumbers(ctx context.Context) api.MachineStats {
	c := &s.machine
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.st.At.IsZero() && time.Since(c.st.At) < machineEvery {
		return c.st
	}
	home := s.d.Config.Home
	p := s.Probe(home)
	memTotal, swapTotal := memTotals()
	st := api.MachineStats{MemoryUsedMB: -1, MemoryTotalMB: memTotal, SwapUsedMB: p.SwapUsedMB, SwapTotalMB: swapTotal,
		DiskFreeMB: p.DiskFreeMB, DiskTotalMB: -1, Sessions: s.managedCounted(ctx),
		MaxSessions: s.d.Config.File.Admission.WithDefaults().MaxManagedSessions, At: storedTime(time.Now()),
		RemoteTasks: s.d.Config.File.RemoteTasks, ScriptRuns: s.d.Config.File.RemoteTasks, CommandRuns: s.d.Config.File.RemoteTasks, T3Enabled: s.d.Config.File.T3.Enabled()}
	if st.RemoteTasks {
		st.TaskSlots = max(1, s.d.Config.File.RemoteTaskSlots) // the waiting line places no more here (SPEC 8)
		// A task driver runs sandbox tasks, on slots of their own (SPEC 8
		// "Waiting line"); unset, the line places none here.
		if cmd, slots := s.d.Config.File.SandboxTasks(); cmd != "" {
			st.SandboxTasks, st.SandboxSlots = true, slots
		}
		// The one account an authorize-key action may name here.
		if u, err := user.Current(); err == nil {
			st.OSUser = u.Username
		}
	}
	if mt, ok := orchcore.Team(s.d).(orchcore.MachineT3); ok {
		if st.T3Enabled = mt.T3Enabled(); st.T3Enabled {
			st.T3 = mt.T3Stats(ctx)
		}
	}
	if memTotal > 0 && p.MemoryFreePercent >= 0 {
		st.MemoryUsedMB = memTotal * int64(100-p.MemoryFreePercent) / 100
	}
	// Free and total from one statfs of $ZM_HOME's volume: a free figure
	// from elsewhere (the probe, which tests replace) could exceed the total.
	var fs syscall.Statfs_t
	if syscall.Statfs(home, &fs) == nil {
		st.DiskTotalMB = int64(uint64(fs.Blocks) * uint64(fs.Bsize) >> 20)
		st.DiskFreeMB = int64(uint64(fs.Bavail) * uint64(fs.Bsize) >> 20)
	}
	c.st = st
	return st
}

// RecordMachine implements core.MachinePresence: an authority keeps a
// replica's latest numbers per project it heartbeats for.
func (s *Service) RecordMachine(ctx context.Context, project, machine string, st api.MachineStats) error {
	js, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO presence_machines (machine, project, stats, seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (machine, project) DO UPDATE SET stats = excluded.stats, seen = excluded.seen`,
		machine, project, string(js), store.Millis(time.Now()))
	return err
}

// ForgetMachine implements core.MachinePresence.
func (s *Service) ForgetMachine(ctx context.Context, project, machine string) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM presence_machines WHERE machine = ? AND project = ?`, machine, project)
	return err
}

// MachineRows implements core.MachinePresence.
func (s *Service) MachineRows(ctx context.Context) ([]core.MachineSeen, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT machine, project, stats, seen FROM presence_machines ORDER BY machine, seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.MachineSeen
	for rows.Next() {
		var r core.MachineSeen
		var js string
		var seen int64
		if err := rows.Scan(&r.Machine, &r.Project, &js, &seen); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(js), &r.Stats) != nil {
			continue
		}
		r.Seen = store.FromMillis(seen)
		out = append(out, r)
	}
	return out, rows.Err()
}

// remoteMachines are the joined machines' last numbers sub may see.
func (s *Service) remoteMachines(ctx context.Context, sub *authz.Subject) ([]api.Machine, error) {
	rows, err := s.MachineRows(ctx)
	if err != nil {
		return nil, err
	}
	return s.d.VisibleMachines(ctx, s.d.Config.Machine, sub, rows), nil
}
