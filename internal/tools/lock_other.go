//go:build !unix

package tools

import (
	"context"
	"time"
)

func lock(context.Context, string, time.Duration, func()) (func(), error) { return func() {}, nil }
