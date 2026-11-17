// Package budget holds the one test that bounds zm's agent-facing surface:
// everything an agent reads at session start or on every prompt (the
// AGENTS.md snippet, MCP tool descriptions, the notice line). Owned by the
// integrator; the number changes only by owner decision.
package budget

// Bytes is the whole agent-facing budget.
const Bytes = 8 * 1024

// ContributorBytes bounds the repo's own AGENTS.md, which every coding agent
// working on zm reads at session start.
const ContributorBytes = 3 * 1024

// ScopedBytes and ScopedLines bound each directory's own AGENTS.md (the
// seams: potluck/kit/server, internal/issues/store...). An agent reads one before working
// there, on top of the root file.
const (
	ScopedBytes = 3 * 1024
	ScopedLines = 60
)
