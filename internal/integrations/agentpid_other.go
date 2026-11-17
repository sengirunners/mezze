//go:build !darwin && !linux

package integrations

func parentOf(int) (int, string, bool) { return 0, "", false }

func procArgs(int) (string, []string) { return "", nil }
