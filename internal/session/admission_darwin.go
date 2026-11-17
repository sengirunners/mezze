package session

import (
	"encoding/binary"
	"syscall"
)

// memoryProbe reads kern.memorystatus_level (percent of memory available,
// what the OS itself uses for memory pressure) and vm.swapusage.
func memoryProbe() (freePercent int, swapUsedMB int64) {
	freePercent, swapUsedMB = -1, -1
	if s, err := syscall.Sysctl("kern.memorystatus_level"); err == nil {
		// syscall.Sysctl drops one trailing NUL byte; pad back to int32.
		b := append([]byte(s), 0, 0, 0, 0)[:4]
		if v := int(binary.LittleEndian.Uint32(b)); v >= 0 && v <= 100 {
			freePercent = v
		}
	}
	// struct xsw_usage { u64 total; u64 avail; u64 used; u32 pagesize; bool encrypted; }
	if s, err := syscall.Sysctl("vm.swapusage"); err == nil && len(s) >= 17 {
		b := append([]byte(s), make([]byte, 8)...)
		swapUsedMB = int64(binary.LittleEndian.Uint64(b[16:24]) >> 20)
	}
	return freePercent, swapUsedMB
}

// memTotals reads hw.memsize and vm.swapusage's total (MB); -1 unknown.
func memTotals() (memMB, swapMB int64) {
	memMB, swapMB = -1, -1
	if s, err := syscall.Sysctl("hw.memsize"); err == nil {
		b := append([]byte(s), make([]byte, 8)...)[:8] // Sysctl drops a trailing NUL: pad back
		if v := binary.LittleEndian.Uint64(b); v > 0 {
			memMB = int64(v >> 20)
		}
	}
	if s, err := syscall.Sysctl("vm.swapusage"); err == nil && len(s) >= 8 {
		b := append([]byte(s), make([]byte, 8)...)
		swapMB = int64(binary.LittleEndian.Uint64(b[0:8]) >> 20)
	}
	return memMB, swapMB
}
