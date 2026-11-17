//go:build !darwin && !linux

package holder

import "errors"

func IsHolder(int) (bool, error) { return false, errors.ErrUnsupported }
