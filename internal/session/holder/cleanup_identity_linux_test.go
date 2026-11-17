package holder

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux field 22 is the kernel start tick. Parenthesized comm may itself
// contain spaces or parentheses, so fields begin after its final closing ).
func cleanupProcessInfo(pid int) (cleanupProcess, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return cleanupProcess{}, false
	}
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return cleanupProcess{}, false
	}
	f := strings.Fields(s[end+1:])
	if len(f) < 20 {
		return cleanupProcess{}, false
	}
	start, err := strconv.ParseInt(f[19], 10, 64)
	return cleanupProcess{start: start, zombie: f[0] == "Z" || f[0] == "X"}, err == nil && start > 0
}
