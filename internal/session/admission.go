package session

import (
	"fmt"
	"syscall"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/config"
)

// Admit is admission control as a pure function (SPEC section 8): it says
// whether a new managed session may start given what the machine looks
// like now. It never kills anything; a refusal names one typed reason.
// A probe value below zero means "unknown" and never refuses.
func Admit(p orchapi.AdmissionProbe, lim config.Admission) orchapi.Admission {
	lim = lim.WithDefaults()
	a := orchapi.Admission{OK: true, Probe: p}
	refuse := func(reason, format string, args ...any) orchapi.Admission {
		a.OK, a.Reason, a.Detail = false, reason, fmt.Sprintf(format, args...)
		return a
	}
	switch {
	case p.MemoryFreePercent >= 0 && p.MemoryFreePercent < lim.MinMemoryFreePercent:
		return refuse(orchapi.AdmitMemory, "memory free %d%% < %d%%", p.MemoryFreePercent, lim.MinMemoryFreePercent)
	case p.SwapUsedMB >= 0 && p.SwapUsedMB > lim.MaxSwapUsedMB:
		return refuse(orchapi.AdmitMemory, "swap used %d MB > %d MB", p.SwapUsedMB, lim.MaxSwapUsedMB)
	case p.DiskFreeMB >= 0 && p.DiskFreeMB < lim.MinDiskFreeMB:
		return refuse(orchapi.AdmitDisk, "disk free %d MB < %d MB", p.DiskFreeMB, lim.MinDiskFreeMB)
	case lim.MaxManagedSessions > 0 && p.ManagedSessions >= lim.MaxManagedSessions:
		return refuse(orchapi.AdmitSessions, "%d managed sessions alive (max %d)", p.ManagedSessions, lim.MaxManagedSessions)
	}
	return a
}

// probeMachine measures memory, swap and the free disk of every dir's
// volume (the smallest wins). Unknown values are -1.
func probeMachine(dirs ...string) orchapi.AdmissionProbe {
	mem, swap := memoryProbe()
	p := orchapi.AdmissionProbe{MemoryFreePercent: mem, SwapUsedMB: swap, DiskFreeMB: -1}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(d, &st) != nil {
			continue
		}
		mb := int64(uint64(st.Bavail) * uint64(st.Bsize) >> 20)
		if p.DiskFreeMB < 0 || mb < p.DiskFreeMB {
			p.DiskFreeMB = mb
		}
	}
	return p
}

// agentNice is the nice value a session of kind agent starts at (SPEC 8):
// an agent's builds and tests yield the CPU to the owner's interactive UIs;
// a person's shell keeps the daemon's priority (0).
func (s *Service) agentNice(agent string) int {
	if agent == "shell" {
		return 0
	}
	return s.d.Config.AgentNiceness()
}
