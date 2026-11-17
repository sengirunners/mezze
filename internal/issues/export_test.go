package issues

import (
	"context"
	"time"
)

// Test hooks for the external tests (package issues_test).

// SetMoveHook runs f before each issue of a move; the answer undoes it.
func SetMoveHook(f func(id string)) func() {
	moveHooks.beforeOne = f
	return func() { moveHooks.beforeOne = nil }
}

// SetWriteHook runs f between a write's held check and its commit.
func SetWriteHook(f func(project string)) func() {
	writeHooks.beforeApply = f
	return func() { writeHooks.beforeApply = nil }
}

// SweepAttachments runs the attachment expiry sweep now.
func (s *Service) SweepAttachments(ctx context.Context) { s.sweepAttachments(ctx) }

// ReleaseUnleased runs the unleased-claim sweep now, with grace.
func (s *Service) ReleaseUnleased(ctx context.Context, grace time.Duration) {
	s.releaseUnleased(ctx, grace)
}
