//go:build !darwin && !linux

package holder

func cleanupProcessInfo(int) (cleanupProcess, bool) { return cleanupProcess{}, false }
