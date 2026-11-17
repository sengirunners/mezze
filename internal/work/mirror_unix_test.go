//go:build unix

package work

import (
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
)

// The serve-mode adapter is one process; killed, it is restarted and the
// mirror keeps syncing. Close leaves no adapter behind.
func TestMirrorServeRestart(t *testing.T) {
	quiet(t)
	old := adapter.RestartBackoff
	adapter.RestartBackoff = 0
	t.Cleanup(func() { adapter.RestartBackoff = old })
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "one", Status: "open", UpdatedAt: fake.Now()}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	path, err := adapter.Find("fake", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1 := h.s.pool.Proc(path)
	if p1 == nil {
		t.Fatal("no serve-mode process")
	}
	pid1 := p1.Pid()
	p1.Kill("test")
	edit(t, state, func(st *fake.State) {
		st.Issues["solver-2"] = &fake.Issue{ID: "solver-2", Title: "two", Status: "open", UpdatedAt: fake.Now()}
	})
	h.sync()
	owner := h.as(h.owner, "")
	if _, err := orchclient.GetWork(h.ctx, owner, "bd:solver-2"); err != nil {
		t.Fatalf("sync after a crash: %v", err)
	}
	p2 := h.s.pool.Proc(path)
	if p2 == nil || p2.Pid() == pid1 {
		t.Fatal("not restarted")
	}
	pid2 := p2.Pid()
	h.s.Close()
	if !processGone(pid2) {
		t.Fatalf("adapter %d left running after Close", pid2)
	}
}

func processGone(pid int) bool {
	for range 60 {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
