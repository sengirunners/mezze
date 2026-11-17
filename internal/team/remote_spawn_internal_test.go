package team

import (
	"maps"
	"slices"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// TestRemoteSpawnCarriesToolGrant: a tool grant and safe outputs cross to the
// other machine, and the authority refuses malformed ones before anything is
// queued.
func TestRemoteSpawnCarriesToolGrant(t *testing.T) {
	req := orchapi.SpawnRequest{Agent: "claude", Mode: orchapi.SessionModePipe, Approvals: "never",
		AllowedTools: []string{"Bash(gh pr view:*)", "Read"}, PermissionMode: "acceptEdits",
		SafeOutputs: map[string]int{"bead-write": 2, "zm-ask": 1}}
	got := remoteSpawn(req)
	if !slices.Equal(got.AllowedTools, req.AllowedTools) || got.PermissionMode != "acceptEdits" || !maps.Equal(got.SafeOutputs, req.SafeOutputs) {
		t.Fatalf("remoteSpawn dropped the grant: %+v", got)
	}
	if err := checkRemoteSpawn(got); err != nil {
		t.Fatalf("a good grant: %v", err)
	}
	for name, bad := range map[string]orchapi.SpawnRequest{
		"mode":  {Agent: "claude", Mode: orchapi.SessionModePipe, PermissionMode: "bypassPermissions"},
		"comma": {Agent: "claude", Mode: orchapi.SessionModePipe, AllowedTools: []string{"Read,Bash"}},
		"kind":  {Agent: "claude", Mode: orchapi.SessionModePipe, SafeOutputs: map[string]int{"pr-merge": 1}},
	} {
		if err := checkRemoteSpawn(remoteSpawn(bad)); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: want bad_request, got %v", name, err)
		}
	}
}
