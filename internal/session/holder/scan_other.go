//go:build !darwin && !linux

package holder

func scan() ([]Running, error) { return nil, nil }
