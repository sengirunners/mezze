//go:build !darwin

package session

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// memoryProbe reads /proc/meminfo: MemAvailable as a percent of MemTotal,
// and swap in use. Unknown values are -1.
func memoryProbe() (freePercent int, swapUsedMB int64) {
	freePercent, swapUsedMB = -1, -1
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	kb := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) == 0 {
			continue
		}
		if n, err := strconv.ParseInt(fs[0], 10, 64); err == nil {
			kb[k] = n
		}
	}
	if t, a := kb["MemTotal"], kb["MemAvailable"]; t > 0 {
		freePercent = int(a * 100 / t)
	}
	if t, ok := kb["SwapTotal"]; ok {
		swapUsedMB = (t - kb["SwapFree"]) >> 10
	}
	return
}

// memTotals reads MemTotal and SwapTotal from /proc/meminfo (MB); -1
// unknown.
func memTotals() (memMB, swapMB int64) {
	memMB, swapMB = -1, -1
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		fs := strings.Fields(v)
		if !ok || len(fs) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fs[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			memMB = n >> 10
		case "SwapTotal":
			swapMB = n >> 10
		}
	}
	return
}
