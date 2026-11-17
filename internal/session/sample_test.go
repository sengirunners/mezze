package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
)

func fastSampler(t *testing.T) {
	t.Helper()
	old, oldA, oldU, oldS := sampleEvery, askedFor, updatedEvery, portsSoon
	sampleEvery, askedFor, updatedEvery, portsSoon = 100*time.Millisecond, 5*time.Second, 10*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { sampleEvery, askedFor, updatedEvery, portsSoon = old, oldA, oldU, oldS })
}

// TestSampleFgBusy: a shell running sleep in the foreground (job control on,
// as an interactive shell) shows the sleep as fg and busy; when the shell
// itself is in the foreground, not busy.
func TestSampleFgBusy(t *testing.T) {
	fastSampler(t)
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `set -m; sleep 30; exec sleep 31`, nil)
	orchclient.Session(ctx, h.c, ss.ID) // the owner asked: the sampler looks at it
	got := h.wait(ss.ID, "busy with sleep", func(s orchapi.Session) bool {
		return s.Busy && s.Fg != nil && s.Fg.Name == "sleep" && s.Fg.PID != s.PID
	})
	if got.Fg.PID == 0 {
		t.Fatalf("fg %+v", got.Fg)
	}
	// An idle session (its own process in front): not busy.
	idle := h.spawn("shell", `exec sleep 30`, nil)
	orchclient.Session(ctx, h.c, idle.ID)
	got = h.wait(idle.ID, "fg itself", func(s orchapi.Session) bool { return s.Fg != nil && s.Fg.PID == s.PID })
	if got.Busy || got.Fg.Name != "sleep" {
		t.Fatalf("idle: %+v busy=%v", got.Fg, got.Busy)
	}
	// Unmanaged sessions have neither.
	r, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-fg"})
	if err != nil || r.Fg != nil || r.Busy || r.Ports != nil {
		t.Fatalf("unmanaged %+v %v", r, err)
	}
}

// TestSamplePorts: a listener in the session's process tree shows up in
// ports; a token reader and the session's events for one get neither fg nor
// ports; nothing is sampled while nobody looks.
func TestSamplePorts(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	fastSampler(t)
	h := newHarness(t, nil)
	ctx := context.Background()
	p := h.project("acme", h.home)
	script := `import socket,time
s=socket.socket(); s.bind(("127.0.0.1",0)); s.listen()
print("PORT", s.getsockname()[1], flush=True); time.sleep(60)`
	ss := h.spawn("shell", "("+py+" -c '"+script+"'); sleep 30", func(r *orchapi.SpawnRequest) { r.Cwd = h.home })
	port := 0
	deadline := time.Now().Add(15 * time.Second)
	for port == 0 {
		out, _ := orchclient.Output(ctx, h.c, ss.ID, 0)
		sc := bufio.NewScanner(strings.NewReader(out.Data))
		for sc.Scan() {
			if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "PORT" {
				port, _ = strconv.Atoi(f[1])
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no port printed: %q", out.Data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Nobody has asked: nothing sampled.
	time.Sleep(300 * time.Millisecond)
	if pr := h.s.proc(ss.ID); pr != nil {
		pr.mu.Lock()
		sm := pr.sample
		pr.mu.Unlock()
		if sm.fg != nil || sm.ports != nil {
			t.Fatalf("sampled with nobody looking: %+v", sm)
		}
	}
	evs := sessionEvents(t, h)
	orchclient.Session(ctx, h.c, ss.ID)
	got := h.wait(ss.ID, "port "+strconv.Itoa(port), func(s orchapi.Session) bool { return slices.Contains(s.Ports, port) })
	if got.Fg == nil {
		t.Fatalf("no fg: %+v", got)
	}
	// The change went out as an updated event, with the ports for the owner.
	var sawPorts bool
	for _, e := range evs(300 * time.Millisecond) {
		var d orchapi.Session
		if e.Type == orchapi.EventSessionUpdated && json.Unmarshal(e.Data, &d) == nil && slices.Contains(d.Ports, port) {
			sawPorts = true
		}
	}
	if !sawPorts {
		t.Fatal("no updated event with the port")
	}
	// A token reader gets neither, in reads or events.
	m, err := h.d.Projects.Mint(ctx, h.owner(), api.MintTokenRequest{As: "vida", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	tc := *h.c
	tc.Token = m.Secret
	one, err := orchclient.Session(ctx, &tc, ss.ID)
	if err != nil || one.Fg != nil || one.Ports != nil || one.Busy {
		t.Fatalf("token get: %+v %v", one, err)
	}
	l, _ := orchclient.Sessions(ctx, &tc, orchapi.SessionQuery{})
	if r := find(l, ss.Address); r == nil || r.Fg != nil || r.Ports != nil {
		t.Fatalf("token list %+v", r)
	}
	member := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(p.ID)}}, nil)}
	mevs, err := h.d.Events.Since(ctx, member, "", events.Filter{Subject: ss.Address}, 0)
	if err != nil || len(mevs) == 0 {
		t.Fatalf("member events %v %v", mevs, err)
	}
	for _, e := range mevs {
		if s := string(e.Data); strings.Contains(s, `"ports"`) || strings.Contains(s, `"fg"`) || strings.Contains(s, `"busy"`) {
			t.Fatalf("a member's event carries fg/ports: %s", s)
		}
	}
}

// TestSampleCost: a list does not make the sampler look (a cockpit reloads its
// list on events); a get does. Ports are read at most every portsEvery,
// sooner only when a session's process tree changed and never within
// portsSoon of the last read (a churning tree is not an lsof every cycle),
// nor again soon after a read that failed; fg every cycle.
func TestSampleCost(t *testing.T) {
	fastSampler(t)
	oldP := portsEvery
	portsEvery = time.Hour
	t.Cleanup(func() { portsEvery = oldP })
	var mu sync.Mutex
	calls := 0
	var failing bool
	old := readPorts
	readPorts = func(pids []int) (map[int][]int, error) {
		mu.Lock()
		calls++
		f := failing
		mu.Unlock()
		if f {
			return nil, errors.New("lsof: no answer within 1.5s")
		}
		return old(pids)
	}
	t.Cleanup(func() { readPorts = old })
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }

	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `set -m; while :; do sleep 0.05; done`, nil)
	for i := 0; i < 5; i++ {
		if _, err := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	if pr := h.s.proc(ss.ID); pr != nil {
		pr.mu.Lock()
		fg := pr.sample.fg
		pr.mu.Unlock()
		if fg != nil || count() != 0 {
			t.Fatalf("a list made the sampler look: fg %+v, %d port reads", fg, count())
		}
	}
	orchclient.Session(ctx, h.c, ss.ID)
	h.wait(ss.ID, "sampled", func(s orchapi.Session) bool { return s.Fg != nil })
	// The loop's sleep is a new process every 0.05 s: the tree changes, so
	// ports are read again; a quiet tree is not.
	time.Sleep(time.Second)
	// About 10 cycles, each seeing a new tree: a read at most every
	// portsSoon (300 ms), so 2 to 5, never one a cycle.
	if n := count(); n < 2 || n > 5 {
		t.Fatalf("a changing tree: %d port reads in 1 s (portsSoon 300 ms, a cycle every 100 ms)", n)
	}
	// A failing read (an lsof timeout) is not retried on the tree's next
	// changes: the next try waits portsEvery (an hour here).
	mu.Lock()
	failing = true
	mu.Unlock()
	time.Sleep(600 * time.Millisecond) // past portsSoon and a cycle: one more read, which fails
	failed := count()
	time.Sleep(time.Second)
	if n := count() - failed; n > 0 {
		t.Fatalf("a failed ports read was retried %d times within portsEvery", n)
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	quiet := h.spawn("shell", `exec sleep 30`, nil)
	orchclient.Session(ctx, h.c, quiet.ID)
	h.wait(quiet.ID, "sampled", func(s orchapi.Session) bool { return s.Fg != nil })
	if _, err := h.s.Stop(ctx, h.owner(), ss.ID, orchapi.StopRequest{Signal: "KILL"}); err != nil && !api.IsCode(err, api.CodeConflict) {
		t.Fatal(err)
	}
	h.wait(ss.ID, "ended", ended)
	before := count()
	time.Sleep(time.Second) // ~10 cycles, fg each time
	orchclient.Session(ctx, h.c, quiet.ID)
	if n := count() - before; n > 0 {
		t.Fatalf("a quiet tree's ports were read %d times within portsEvery", n)
	}
}

// TestSampleCycleCPU measures one sampler cycle over 10 sessions (set
// ZM_MEASURE=1): CPU of the daemon (user+sys) and of lsof (children), and
// wall time, for a cycle that reads fg only and one that also reads ports.
func TestSampleCycleCPU(t *testing.T) {
	if os.Getenv("ZM_MEASURE") == "" {
		t.Skip("set ZM_MEASURE=1")
	}
	old := sampleEvery
	sampleEvery = time.Hour // only the cycles run here
	t.Cleanup(func() { sampleEvery = old })
	h := newHarness(t, func(c *config.Config) { c.File.Admission.MaxManagedSessions = 20 })
	for i := 0; i < 10; i++ {
		ss := h.spawn("shell", `set -m; sleep 300`, nil)
		h.s.asked(ss.ID)
	}
	time.Sleep(300 * time.Millisecond)
	cpu := func() (self, kids time.Duration) {
		var a, b syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &a)
		_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &b)
		tv := func(x syscall.Timeval) time.Duration { return time.Duration(x.Nano()) }
		return tv(a.Utime) + tv(a.Stime), tv(b.Utime) + tv(b.Stime)
	}
	run := func(name string, ports bool, n int) {
		var self, kids, wall time.Duration
		for i := 0; i < n; i++ {
			if ports {
				for _, p := range h.s.live() {
					p.mu.Lock()
					p.portsAt = time.Time{}
					p.mu.Unlock()
				}
			}
			s0, k0 := cpu()
			w0 := time.Now()
			h.s.sampleOnce()
			wall += time.Since(w0)
			s1, k1 := cpu()
			self, kids = self+s1-s0, kids+k1-k0
		}
		t.Logf("%-22s per cycle: daemon CPU %6.2f ms, lsof CPU %6.2f ms, wall %6.2f ms (n=%d)", name,
			float64(self.Microseconds())/1000/float64(n), float64(kids.Microseconds())/1000/float64(n),
			float64(wall.Microseconds())/1000/float64(n), n)
	}
	run("fg only (10 sessions)", false, 20)
	run("fg + ports (10 sess.)", true, 10)
}

// TestSampleJobs: a shell with a background job holds its terminal (not
// busy) but reports the job; an idle shell reports none; a token reader
// gets no jobs.
func TestSampleJobs(t *testing.T) {
	fastSampler(t)
	h := newHarness(t, nil)
	ctx := context.Background()
	bg := h.spawn("shell", `sleep 300 & exec sleep 301`, nil)
	orchclient.Session(ctx, h.c, bg.ID)
	got := h.wait(bg.ID, "a background job", func(s orchapi.Session) bool { return s.Fg != nil && s.Jobs != nil && *s.Jobs >= 1 })
	if got.Busy {
		t.Fatalf("a background job made the shell busy: %+v", got.Fg)
	}
	idle := h.spawn("shell", `exec sleep 30`, nil)
	orchclient.Session(ctx, h.c, idle.ID)
	got = h.wait(idle.ID, "sampled", func(s orchapi.Session) bool { return s.Fg != nil })
	if got.Jobs == nil || *got.Jobs != 0 || got.Busy {
		t.Fatalf("idle shell: jobs %v busy %v", got.Jobs, got.Busy)
	}
	// 0 is sent, not dropped: a client tells "no jobs" from "an older daemon".
	var raw json.RawMessage
	if err := h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+idle.ID, nil, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"jobs":0`) {
		t.Fatalf("idle shell JSON has no \"jobs\":0: %s", raw)
	}
	// Unmanaged sessions (never sampled) carry no jobs at all.
	if r, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-jobs"}); err != nil || r.Jobs != nil {
		t.Fatalf("unmanaged: jobs %v %v", r.Jobs, err)
	}
	m, err := h.d.Projects.Mint(ctx, h.owner(), api.MintTokenRequest{As: "vida", Role: "member", Project: h.project("acme").Slug})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.MoveSession(ctx, h.c, bg.ID, orchapi.MoveRequest{Project: "acme"}); err != nil {
		t.Fatal(err)
	}
	tc := *h.c
	tc.Token = m.Secret
	if one, err := orchclient.Session(ctx, &tc, bg.ID); err != nil || one.Jobs != nil || one.Fg != nil {
		t.Fatalf("token read: jobs %v fg %+v %v", one.Jobs, one.Fg, err)
	}
	var traw json.RawMessage
	if err := tc.Do(ctx, http.MethodGet, "/v0/sessions/"+bg.ID, nil, &traw); err != nil || strings.Contains(string(traw), `"jobs"`) {
		t.Fatalf("token JSON carries jobs: %s %v", traw, err)
	}
}

// A session nobody looks at any more loses fg, busy and jobs (they would be
// stale); ports stay.
func TestSampleForgottenWhenNotDue(t *testing.T) {
	fastSampler(t)
	oldA, oldR := askedFor, readPorts
	askedFor = 300 * time.Millisecond
	readPorts = func([]int) (map[int][]int, error) { return nil, nil } // no real lsof: ports are not under test here
	t.Cleanup(func() { askedFor, readPorts = oldA, oldR })
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `sleep 300 & exec sleep 301`, nil)
	orchclient.Session(ctx, h.c, ss.ID)
	h.wait(ss.ID, "sampled", func(s orchapi.Session) bool { return s.Jobs != nil && *s.Jobs >= 1 })
	p := h.s.proc(ss.ID)
	if p == nil {
		t.Fatal("no proc")
	}
	p.mu.Lock()
	p.sample.ports = []int{4242} // stands in for a read port
	p.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		sm := p.sample
		p.mu.Unlock()
		if sm.fg == nil && sm.jobs == nil && !sm.busy {
			if !slices.Equal(sm.ports, []int{4242}) {
				t.Fatalf("ports went too: %v", sm.ports)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a stale sample kept: %+v", sm)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// GET /v0/sessions/{id}?sample=1 samples that session before answering: a
// job started just now shows, with no sampler cycle in between.
func TestSampleNowOnGet(t *testing.T) {
	fastSampler(t)
	sampleEvery = time.Hour // no cycles: only ?sample=1 samples
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `read x; sleep 300 & exec sleep 301`, nil)
	get := func() orchapi.Session {
		t.Helper()
		var got orchapi.Session
		if err := h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := get(); got.Jobs == nil || *got.Jobs != 0 || got.Fg == nil {
		t.Fatalf("before: jobs %v fg %+v", got.Jobs, got.Fg)
	}
	if plain, _ := orchclient.Session(ctx, h.c, ss.ID); plain.Jobs == nil || *plain.Jobs != 0 {
		t.Fatalf("a plain get after: jobs %v", plain.Jobs)
	}
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "go", Enter: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := get(); got.Jobs != nil && *got.Jobs >= 1 {
			if got.Busy {
				t.Fatalf("background job: busy")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("?sample=1 never saw the new job")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A token reader's ?sample=1 samples nothing it could see: no fields.
	m, err := h.d.Projects.Mint(ctx, h.owner(), api.MintTokenRequest{As: "vida", Role: "member", Project: h.project("acme").Slug})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.MoveSession(ctx, h.c, ss.ID, orchapi.MoveRequest{Project: "acme"}); err != nil {
		t.Fatal(err)
	}
	tc := *h.c
	tc.Token = m.Secret
	var tok orchapi.Session
	if err := tc.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &tok); err != nil || tok.Jobs != nil || tok.Fg != nil {
		t.Fatalf("token ?sample=1: %+v %v", tok, err)
	}
}

func TestWorthPublishing(t *testing.T) {
	n := func(v int) *int { return &v }
	fg := &orchapi.SessionFg{PID: 1, Name: "zsh"}
	cases := []struct {
		name     string
		old, new sample
		want     bool
	}{
		{"jobs 1 -> 2", sample{fg: fg, jobs: n(1)}, sample{fg: fg, jobs: n(2)}, false},
		{"jobs 3 -> 1", sample{fg: fg, jobs: n(3)}, sample{fg: fg, jobs: n(1)}, false},
		{"jobs 0 -> 1", sample{fg: fg, jobs: n(0)}, sample{fg: fg, jobs: n(1)}, true},
		{"jobs 2 -> 0", sample{fg: fg, jobs: n(2)}, sample{fg: fg, jobs: n(0)}, true},
		{"jobs appear", sample{}, sample{fg: fg, jobs: n(0)}, true},
		{"jobs go", sample{fg: fg, jobs: n(0)}, sample{}, true},
		{"busy", sample{fg: fg, jobs: n(1)}, sample{fg: fg, busy: true, jobs: n(2)}, true},
		{"ports", sample{fg: fg, jobs: n(1)}, sample{fg: fg, jobs: n(1), ports: []int{3000}}, true},
		{"same", sample{fg: fg, jobs: n(1)}, sample{fg: fg, jobs: n(1)}, false},
	}
	for _, c := range cases {
		if got := worthPublishing(c.old, c.new); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A jobs change publishes only when it crosses 0 <-> more; the reads carry
// the exact count either way.
func TestSampleJobsEventsOnlyAcrossZero(t *testing.T) {
	fastSampler(t)
	h := newHarness(t, nil)
	ctx := context.Background()
	evs := sessionEvents(t, h)
	// The shell stays in front (read z): only jobs change, never fg.
	ss := h.spawn("shell", `read x; sleep 300 & read y; sleep 300 & read z; exec sleep 301`, nil)
	orchclient.Session(ctx, h.c, ss.ID)
	h.wait(ss.ID, "0 jobs", func(s orchapi.Session) bool { return s.Jobs != nil && *s.Jobs == 0 })
	jobsEvents := func() []int {
		var out []int
		for _, e := range evs(300 * time.Millisecond) {
			var d orchapi.Session
			if e.Type == orchapi.EventSessionUpdated && json.Unmarshal(e.Data, &d) == nil && d.Address == ss.Address && d.Jobs != nil {
				out = append(out, *d.Jobs)
			}
		}
		return out
	}
	jobsEvents()
	_ = orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "a", Enter: true})
	h.wait(ss.ID, "1 job", func(s orchapi.Session) bool { return s.Jobs != nil && *s.Jobs == 1 })
	if got := jobsEvents(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("0 -> 1: events %v", got)
	}
	_ = orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "b", Enter: true})
	h.wait(ss.ID, "2 jobs", func(s orchapi.Session) bool { return s.Jobs != nil && *s.Jobs == 2 })
	if got := jobsEvents(); len(got) != 0 {
		t.Fatalf("1 -> 2 published: %v", got)
	}
}

// A GET ?sample=1 never waits for a cycle's ports read (lsof may take
// seconds): the read runs outside sampleMu.
func TestSampleNowNotBehindPorts(t *testing.T) {
	fastSampler(t)
	sampleEvery = time.Hour // the test runs the cycle itself
	entered, release := make(chan struct{}), make(chan struct{})
	old := readPorts
	readPorts = func(pids []int) (map[int][]int, error) {
		close(entered)
		<-release
		return nil, nil
	}
	t.Cleanup(func() { readPorts = old })
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `exec sleep 30`, nil)
	h.s.asked(ss.ID) // due for the cycle
	done := make(chan struct{})
	go func() { h.s.sampleOnce(); close(done) }()
	<-entered // the cycle is reading ports now
	var got orchapi.Session
	answered := make(chan error, 1)
	go func() { answered <- h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got) }()
	select {
	case err := <-answered:
		close(release)
		<-done
		if err != nil || got.Fg == nil || got.Jobs == nil {
			t.Fatalf("?sample=1: %+v %v", got, err)
		}
	case <-time.After(time.Second):
		close(release)
		<-done
		<-answered
		t.Fatal("?sample=1 waited behind the cycle's ports read")
	}
}

// A cycle never wipes a sample a GET ?sample=1 has just taken (the session
// is not due, and the answer is still being written): only a sample older
// than sampleEvery is forgotten.
func TestSampleFreshNotForgotten(t *testing.T) {
	fastSampler(t)
	sampleEvery = time.Hour // the test runs the cycles itself
	h := newHarness(t, nil)
	ss := h.spawn("shell", `exec sleep 30`, nil)
	time.Sleep(100 * time.Millisecond)
	h.s.sampleNow(ss.ID) // as a GET ?sample=1 does
	h.s.sampleOnce()     // a cycle right after: the session is not due
	p := h.s.proc(ss.ID)
	p.mu.Lock()
	fresh := p.sample
	p.mu.Unlock()
	if fresh.fg == nil || fresh.jobs == nil {
		t.Fatalf("a fresh sample was forgotten: %+v", fresh)
	}
	p.mu.Lock()
	p.sampledAt = time.Now().Add(-2 * time.Hour) // now it is old
	p.mu.Unlock()
	h.s.sampleOnce()
	p.mu.Lock()
	old := p.sample
	p.mu.Unlock()
	if old.fg != nil || old.jobs != nil {
		t.Fatalf("an old sample was kept: %+v", old)
	}
}

// A sample that fails (no process table) never answers the last one as
// fresh: fg, busy and jobs go, so a UI asks.
func TestSampleFailureClears(t *testing.T) {
	fastSampler(t)
	sampleEvery = time.Hour
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `sleep 300 & exec sleep 301`, nil)
	time.Sleep(100 * time.Millisecond)
	var got orchapi.Session
	if err := h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got); err != nil || got.Jobs == nil {
		t.Fatalf("first: %+v %v", got.Jobs, err)
	}
	old := readProcTable
	readProcTable = func() (map[int]procEntry, error) { return nil, errors.New("sysctl failed") }
	t.Cleanup(func() { readProcTable = old })
	got = orchapi.Session{}
	if err := h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Jobs != nil || got.Fg != nil || got.Busy {
		t.Fatalf("a failed sample answered the old one: jobs %v fg %+v", got.Jobs, got.Fg)
	}
}

// A GET ?sample=1 is a one-off look: it does not make the session due and
// publishes nothing, then or when the cycle later drops the sample.
func TestSampleNowOneOff(t *testing.T) {
	fastSampler(t)
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `sleep 300 & exec sleep 301`, nil)
	evs := sessionEvents(t, h)
	time.Sleep(100 * time.Millisecond)
	evs(50 * time.Millisecond)
	var got orchapi.Session
	if err := h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got); err != nil || got.Jobs == nil {
		t.Fatalf("?sample=1: %v %v", got.Jobs, err)
	}
	if _, asked := h.s.askedAt.Load(ss.ID); asked {
		t.Fatal("?sample=1 made the session due")
	}
	time.Sleep(500 * time.Millisecond) // several cycles: the sample is dropped
	p := h.s.proc(ss.ID)
	p.mu.Lock()
	sm := p.sample
	p.mu.Unlock()
	if sm.fg != nil || sm.jobs != nil {
		t.Fatalf("the one-off sample is still served: %+v", sm)
	}
	for _, e := range evs(100 * time.Millisecond) {
		if e.Subject == ss.Address && e.Type == orchapi.EventSessionUpdated {
			t.Fatalf("a one-off look published: %s", e.Data)
		}
	}
	// A plain owner GET still asks (cycles and events follow).
	orchclient.Session(ctx, h.c, ss.ID)
	if _, asked := h.s.askedAt.Load(ss.ID); !asked {
		t.Fatal("a plain GET did not ask")
	}
}

// A GET ?sample=1 never switches off the event that takes back what a
// cycle published: a cycle publishes busy, a GET's look differs (the job
// ended; or the GET's sample failed), and the forget still publishes, so
// readers do not keep "busy" forever.
func TestSampleForgetAfterGetStillPublishes(t *testing.T) {
	for _, how := range []string{"differs", "fails"} {
		t.Run(how, func(t *testing.T) {
			fastSampler(t)
			sampleEvery = time.Hour // the test runs the cycles itself
			old := readPorts
			readPorts = func([]int) (map[int][]int, error) { return nil, nil }
			t.Cleanup(func() { readPorts = old })
			h := newHarness(t, nil)
			ctx := context.Background()
			ss := h.spawn("shell", `set -m; sleep 300; read x`, nil)
			evs := sessionEvents(t, h)
			var fgPid int
			deadline := time.Now().Add(5 * time.Second)
			for { // a cycle publishes busy
				h.s.asked(ss.ID)
				h.s.sampleOnce()
				if p := h.s.proc(ss.ID); p != nil {
					p.mu.Lock()
					busy, fg := p.told.busy, p.told.fg
					p.mu.Unlock()
					if busy && fg != nil {
						fgPid = fg.PID
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("never busy")
				}
				time.Sleep(50 * time.Millisecond)
			}
			var sawBusy bool
			for _, e := range evs(200 * time.Millisecond) {
				var d orchapi.Session
				if json.Unmarshal(e.Data, &d) == nil && d.Address == ss.Address && d.Busy {
					sawBusy = true
				}
			}
			if !sawBusy {
				t.Fatal("the cycle published no busy")
			}
			// A GET ?sample=1 whose look differs.
			switch how {
			case "differs":
				_ = syscall.Kill(fgPid, syscall.SIGKILL) // the job ends; the shell reads in front
				for i := 0; ; i++ {
					var got orchapi.Session
					_ = h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got)
					if got.Fg != nil && !got.Busy {
						break
					}
					if i > 100 {
						t.Fatalf("the GET never saw the shell in front: %+v", got.Fg)
					}
					time.Sleep(30 * time.Millisecond)
				}
			case "fails":
				oldT := readProcTable
				readProcTable = func() (map[int]procEntry, error) { return nil, errors.New("sysctl failed") }
				var got orchapi.Session
				_ = h.c.Do(ctx, http.MethodGet, "/v0/sessions/"+ss.ID+"?sample=1", nil, &got)
				readProcTable = oldT
				if got.Fg != nil || got.Busy {
					t.Fatalf("a failed GET answered %+v", got)
				}
			}
			evs(100 * time.Millisecond)
			// Nobody looks any more: the forget takes busy back with an event.
			h.s.askedAt.Delete(ss.ID)
			p := h.s.proc(ss.ID)
			p.mu.Lock()
			p.sampledAt = time.Now().Add(-2 * time.Hour)
			p.mu.Unlock()
			h.s.sampleOnce()
			var took bool
			for _, e := range evs(300 * time.Millisecond) {
				var d orchapi.Session
				if e.Type == orchapi.EventSessionUpdated && json.Unmarshal(e.Data, &d) == nil && d.Address == ss.Address && !d.Busy && d.Fg == nil {
					took = true
				}
			}
			if !took {
				t.Fatal("no event took busy back after the GET")
			}
		})
	}
}

// A GET ?sample=1 on a session whose process exited (its end not recorded
// yet) answers no fg, busy or jobs rather than the last look.
func TestSampleNowExited(t *testing.T) {
	h := newHarness(t, nil)
	one := 1
	p := &proc{id8: "0badf00d", exited: make(chan struct{}), sample: sample{fg: &orchapi.SessionFg{PID: 7, Name: "vim"}, busy: true, jobs: &one}}
	close(p.exited)
	h.s.mu.Lock()
	h.s.procs[p.id8] = p
	h.s.mu.Unlock()
	t.Cleanup(func() { h.s.mu.Lock(); delete(h.s.procs, p.id8); h.s.mu.Unlock() })
	h.s.sampleNow(p.id8)
	p.mu.Lock()
	sm := p.sample
	p.mu.Unlock()
	if sm.fg != nil || sm.busy || sm.jobs != nil {
		t.Fatalf("an exited session kept its last look: %+v", sm)
	}
	h.s.sampleNow("0deadbee") // no proc at all: nothing to do, no panic
}

// When the take-back event cannot go out (the session's row cannot be
// read), told goes back, so the next cycle retries instead of assuming
// readers were told.
func TestSampleToldRestoredWhenEventFails(t *testing.T) {
	fastSampler(t)
	sampleEvery = time.Hour // the test runs the cycles itself
	old := readPorts
	readPorts = func([]int) (map[int][]int, error) { return nil, nil }
	t.Cleanup(func() { readPorts = old })
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `set -m; sleep 300; read x`, nil)
	evs := sessionEvents(t, h)
	p := h.s.proc(ss.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.s.asked(ss.ID)
		h.s.sampleOnce()
		p.mu.Lock()
		busy := p.told.busy
		p.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never busy")
		}
		time.Sleep(50 * time.Millisecond)
	}
	forget := func() {
		h.s.askedAt.Delete(ss.ID)
		p.mu.Lock()
		p.sampledAt = time.Now().Add(-2 * time.Hour)
		p.mu.Unlock()
		h.s.sampleOnce()
	}
	reg := h.d.Store.Registry()
	row, err := loadRow(ctx, reg, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteRow(ctx, reg, ss.ID); err != nil { // the event's load fails
		t.Fatal(err)
	}
	evs(100 * time.Millisecond)
	forget()
	p.mu.Lock()
	still := p.told.busy
	p.mu.Unlock()
	if !still {
		t.Fatal("told moved though no event went out")
	}
	if err := insertRow(ctx, reg, row); err != nil {
		t.Fatal(err)
	}
	forget() // the retry: now the event goes out
	var took bool
	for _, e := range evs(300 * time.Millisecond) {
		var d orchapi.Session
		if e.Type == orchapi.EventSessionUpdated && json.Unmarshal(e.Data, &d) == nil && d.Address == ss.Address && !d.Busy && d.Fg == nil {
			took = true
		}
	}
	if !took {
		t.Fatal("the retried forget published nothing")
	}
}
