//go:build !darwin && !linux

package session

import "errors"

func procTable() (map[int]procEntry, error) { return nil, errors.ErrUnsupported }

func listenPorts([]int) (map[int][]int, error) { return nil, nil }

func procStat(int) (pstat, bool) { return pstat{}, false }
