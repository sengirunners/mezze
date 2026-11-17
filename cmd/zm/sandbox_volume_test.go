package main

import (
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// --volume name:path[:mode]: rw by default; a bad shape or mode is refused here.
func TestVolumeFlags(t *testing.T) {
	got, err := volumeFlags([]string{"nm:checkout/node_modules:cow", "go-sdk:.local/go:ro", "cache:cache"})
	want := []computeapi.BoxVolume{{Name: "nm", Path: "checkout/node_modules", Mode: "cow"}, {Name: "go-sdk", Path: ".local/go", Mode: "ro"}, {Name: "cache", Path: "cache", Mode: "rw"}}
	if err != nil || len(got) != len(want) {
		t.Fatalf("%+v %v", got, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("volume %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"nm", "nm:", ":p", "nm:p:rwx", "a:b:c:d"} {
		if _, err := volumeFlags([]string{bad}); err == nil {
			t.Errorf("--volume %q was accepted", bad)
		}
	}
}
