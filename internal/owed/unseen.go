package owed

import "github.com/sengirunners/mezze/internal/orchapi"

// unseen is what a session can still owe that zm keeps no record of. Every
// list shows each of them as unknown: a list that says nothing about them
// would read as "none".
var unseen = []orchapi.OwedGap{
	{Name: "cloud_tasks", Why: "tasks started in the cloud: zm does not record whether one still runs"},
	{Name: "outside_lanes", Why: "lanes a host app or another tool started outside zm (a delegate, a subagent): zm never sees them"},
	{Name: "background_processes", Why: "shells, servers and builds a session left running on a machine"},
	{Name: "worktree_changes", Why: "uncommitted or unpushed changes in a session's checkout or worktree"},
	{Name: "unattributed_pull_requests", Why: "pull requests a session opened that are linked to no work item zm tracks, or whose work item no lease holds now: zm does not say whose they are"},
}

func unseenGaps() []orchapi.OwedGap {
	out := make([]orchapi.OwedGap, len(unseen))
	for i, g := range unseen {
		g.Kind = orchapi.OwedGapUnseen
		out[i] = g
	}
	return out
}
