package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// joinedTeam fakes the team module on a replica: sessions started here
// belong to a joined project whose authority is elsewhere.
type joinedTeam struct {
	proj api.ProjectRef
}

func (j *joinedTeam) TeamNotice(string) []orchapi.NoticePart { return nil }

func (j *joinedTeam) TeamTarget(context.Context, string, string) (api.ProjectRef, string, bool, error) {
	return j.proj, "", true, nil
}

// joinedLeases also takes leases at that authority (orchcore.TeamLeases).
type joinedLeases struct {
	joinedTeam
	mu       sync.Mutex
	err      error
	calls    [][3]string // project, id, session
	released [][3]string
}

func (j *joinedLeases) ReleaseAt(_ context.Context, project, id, session string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.released = append(j.released, [3]string{project, id, session})
	return nil
}

func (j *joinedLeases) BindAt(_ context.Context, _ *authz.Subject, project, id, session string) (orchapi.Lease, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, [3]string{project, id, session})
	if j.err != nil {
		return orchapi.Lease{}, j.err
	}
	return orchapi.Lease{Project: project, Key: id, Holder: session, Bound: true}, nil
}

// spawn --work of a joined project's item takes the bound lease at
// its authority through the team module; refused there, no session is
// left looking assigned.
func TestSpawnWorkInJoinedProjectBindsAtAuthority(t *testing.T) {
	joined := api.ProjectRef{ID: "01JZTEAMPROJECT00000000000", Slug: "teamproj"}
	work := func(r *orchapi.SpawnRequest) {
		r.Command = []string{"/bin/sh", "-c", "exec sleep 30"}
		r.Cwd, r.Work = "", "bd:tp-1"
	}
	spawn := func(h *harness) (orchapi.Session, error) {
		req := orchapi.SpawnRequest{Agent: "shell"}
		work(&req)
		req.Cwd = h.home
		return orchclient.Spawn(context.Background(), h.c, req)
	}
	live := func(h *harness) int {
		var n int
		if err := h.d.Store.Registry().QueryRow(`SELECT count(*) FROM sessions WHERE state != 'ended' AND work = 'bd:tp-1'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("bound at the authority", func(t *testing.T) {
		h := newHarness(t, nil)
		tl := &joinedLeases{joinedTeam: joinedTeam{proj: joined}}
		orchcore.Wire(h.d).Team = tl
		ss, err := spawn(h)
		if err != nil {
			t.Fatal(err)
		}
		if len(tl.calls) != 1 || tl.calls[0] != [3]string{joined.ID, "bd:tp-1", ss.Address} {
			t.Fatalf("BindAt calls %v, session %s", tl.calls, ss.Address)
		}
		if ss.Project != joined.ID || ss.Work != "bd:tp-1" {
			t.Fatalf("session %+v", ss)
		}
	})

	t.Run("refused at the authority", func(t *testing.T) {
		h := newHarness(t, nil)
		tl := &joinedLeases{joinedTeam: joinedTeam{proj: joined}, err: api.Errorf(api.CodeConflict, "bd:tp-1 is held by mini/0a1b2c3d")}
		orchcore.Wire(h.d).Team = tl
		if _, err := spawn(h); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("spawn: %v", err)
		}
		if len(tl.calls) != 1 || live(h) != 0 {
			t.Fatalf("calls %v, live sessions with the item %d", tl.calls, live(h))
		}
	})

	t.Run("start fails after the claim", func(t *testing.T) {
		h := newHarness(t, nil)
		tl := &joinedLeases{joinedTeam: joinedTeam{proj: joined}}
		orchcore.Wire(h.d).Team = tl
		bad := filepath.Join(t.TempDir(), "bad-agent")
		if err := os.WriteFile(bad, []byte("#!/nonexistent/interpreter\n"), 0700); err != nil {
			t.Fatal(err)
		}
		req := orchapi.SpawnRequest{Agent: "shell", Command: []string{bad}, Cwd: h.home, Work: "bd:tp-1"}
		if _, err := orchclient.Spawn(context.Background(), h.c, req); err == nil {
			t.Fatal("spawn of an unrunnable program succeeded")
		}
		tl.mu.Lock()
		defer tl.mu.Unlock()
		if len(tl.calls) != 1 || len(tl.released) != 1 || tl.released[0] != tl.calls[0] {
			t.Fatalf("bound %v, released %v", tl.calls, tl.released)
		}
	})

	t.Run("no team leases here", func(t *testing.T) {
		h := newHarness(t, nil)
		orchcore.Wire(h.d).Team = &joinedTeam{proj: joined}
		_, err := spawn(h)
		if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "authority") {
			t.Fatalf("spawn: %v", err)
		}
		if live(h) != 0 {
			t.Fatal("a session was left")
		}
	})
}
