package work

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// A tracker add flag the adapter does not declare in describe.setup is
// refused before anything runs.
func TestTrackerAddUndeclaredOption(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	dir := t.TempDir()
	owner := h.as(h.owner, "harbor")
	for _, req := range []orchapi.AddTrackerRequest{
		{Adapter: "fake", Prefix: "bd", Server: "h:1"},
		{Adapter: "fake", Prefix: "bd", PasswordFile: filepath.Join(dir, "pw")},
	} {
		if _, err := orchclient.AddTracker(h.ctx, owner, req); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "takes no") {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if f, _ := h.d.Projects.File(h.harbor.ID); len(orchcore.ProjectTrackers(f)) != 0 {
		t.Fatal("bound")
	}
	if st := loadState(t, state); len(st.Calls) != 0 {
		t.Fatalf("calls before the refusal: %v", st.Calls)
	}
}
