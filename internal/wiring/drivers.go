package wiring

// Run-target drivers this machine serves (SPEC 20, "Drivers are pinned"):
// DriverItems asks the daemon to describe each binding, so a driver it
// cannot find or refuses (in a folder others can write, moved, not the
// file pinned) shows. A swap's verify compares the old daemon's answers
// with the new one's (DriverRegressions: a binding that ran before and
// does not now rolls the swap back). Day to day the driver watch keeps the
// last answer as health's `drivers` section, a warning that never fails a
// check: a binding broken for months is the owner's to fix, not a reason
// for zm status to fail.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/swap"
)

// ItemDriver is a binding this machine serves, OK when its driver answers
// describe through the daemon; its name is <scope>/<binding>, the scope
// "machine" or a project's slug.
const ItemDriver = "driver"

// DriverCheck is one ask: the items answered, and the scopes whose
// listing failed (unknown: neither served nor missing). AllProjects is set
// when the projects themselves could not be listed: every project scope
// is unknown.
type DriverCheck struct {
	Items       []api.ServeItem
	Unknown     map[string]string // scope -> why
	AllProjects string
}

// DriverItems asks the daemon c reaches (its owner, on the socket) to
// describe every binding it serves itself: the machine-level ones
// (machine/<name>) and those of each project not joined from another
// machine (<project>/<name>; a joined project's bindings are its
// authority's). A scope whose listing has a failing item is asked once
// more (a busy driver).
func DriverItems(ctx context.Context, c *client.Client) DriverCheck {
	out := DriverCheck{Unknown: map[string]string{}}
	call := func(fn func(context.Context) error) error {
		cctx, cancel := context.WithTimeout(ctx, swap.ProbeTimeout)
		defer cancel()
		return fn(cctx)
	}
	type scope struct {
		name string
		list func(context.Context) (computeapi.RunTargetList, error)
	}
	scopes := []scope{{"machine", func(ctx context.Context) (computeapi.RunTargetList, error) {
		return computeclient.HostRunTargets(ctx, c)
	}}}
	joined := map[string]bool{}
	var slugs []string
	err := call(func(ctx context.Context) error {
		rs, err := c.Remotes(ctx)
		if err != nil {
			return err
		}
		for _, r := range rs {
			joined[r.Project.Slug] = true
		}
		ps, err := c.Projects(ctx)
		for _, p := range ps {
			slugs = append(slugs, p.Slug)
		}
		return err
	})
	if err != nil {
		out.AllProjects = swap.OneLine(err.Error())
	} else {
		for _, slug := range slugs {
			if joined[slug] {
				continue
			}
			cc := *c
			cc.Project, cc.ProjectFrom = slug, "flag"
			scopes = append(scopes, scope{slug, func(ctx context.Context) (computeapi.RunTargetList, error) {
				return computeclient.RunTargets(ctx, &cc, true)
			}})
		}
	}
	for _, sc := range scopes {
		items, err := driverItems(call, sc.name, sc.list)
		for _, it := range items {
			if !it.OK {
				items, err = driverItems(call, sc.name, sc.list)
				break
			}
		}
		if err != nil {
			out.Unknown[sc.name] = swap.OneLine(err.Error())
			continue
		}
		out.Items = append(out.Items, items...)
	}
	return out
}

func driverItems(call func(func(context.Context) error) error, scope string, list func(context.Context) (computeapi.RunTargetList, error)) ([]api.ServeItem, error) {
	var out []api.ServeItem
	err := call(func(ctx context.Context) error {
		l, err := list(ctx)
		if err != nil {
			return err
		}
		for _, v := range l.Targets {
			if v.RunTarget.Host != "" {
				continue
			}
			it := api.ServeItem{Kind: ItemDriver, Name: scope + "/" + v.Name, OK: v.Error == ""}
			if !it.OK {
				it.Code, it.Error = api.CodeUnavailable, fitDriverErr(strings.TrimSpace(strings.SplitN(v.Error, "\n", 2)[0]))
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// unknown says why item's scope was not answered in c ("" when it was).
func (c DriverCheck) unknown(name string) string {
	scope, _, _ := strings.Cut(name, "/")
	if why := c.Unknown[scope]; why != "" {
		return why
	}
	if scope != "machine" && c.AllProjects != "" {
		return c.AllProjects
	}
	return ""
}

// DriverRegressions are the bindings that answered in before and do not in
// after (failing, or gone from a scope that was listed). One whose scope
// after could not list is unknown, never a regression: it is answered in
// unknown, for the log.
func DriverRegressions(before []api.ServeItem, after DriverCheck) (regressions, unknown []string) {
	now := map[string]api.ServeItem{}
	for _, it := range after.Items {
		now[it.Name] = it
	}
	for _, it := range before {
		if it.Kind != ItemDriver || !it.OK {
			continue
		}
		if why := after.unknown(it.Name); why != "" {
			unknown = append(unknown, fmt.Sprintf("%s %s: not known now (%s)", it.Kind, it.Name, why))
			continue
		}
		switch got, ok := now[it.Name]; {
		case !ok:
			regressions = append(regressions, fmt.Sprintf("%s %s: served before, missing now", it.Kind, it.Name))
		case !got.OK:
			regressions = append(regressions, fmt.Sprintf("%s %s: served before, now %s", it.Kind, it.Name, got.Error))
		}
	}
	return regressions, unknown
}

// fitDriverErr fits a refusal into 200 bytes keeping its end, the fix
// (re-pin command or chmod), which paths would push out.
func fitDriverErr(s string) string {
	if len(s) <= 200 {
		return s
	}
	for _, k := range []string{"re-pin with: ", "pin it with: ", "now with: ", "(fix: "} {
		if i := strings.LastIndex(s, k); i >= 0 {
			if head := 200 - (len(s) - i) - 5; head >= 40 {
				return s[:head] + " ... " + s[i:]
			}
		}
	}
	return swap.OneLine(s)
}

// DriverNotice is health's `drivers` section: the bindings whose driver
// the daemon cannot run, as of the last check. Absent when there are none.
// The type is orchapi's, so the client schema can name it.
type DriverNotice = orchapi.DriverNotice

// Driver watch timing (tests shorten them).
var (
	DriverWatchFirst = 2 * time.Minute  // after start: the tailnet and the modules settle
	DriverWatchEvery = 30 * time.Minute // a describe per binding each time
)

// driverWatch is the module that keeps DriverNotice: it asks the daemon
// through its own socket, as its owner (DriverItems).
type driverWatch struct {
	socket string
	log    *slog.Logger
	mu     sync.Mutex
	notice *DriverNotice
}

func newDriverWatch(d *core.Deps) *driverWatch {
	return &driverWatch{socket: d.Config.SocketPath(), log: d.Log}
}

func (w *driverWatch) Routes(*core.Router) {}

// Start checks DriverWatchFirst after start, then every DriverWatchEvery.
func (w *driverWatch) Start(ctx context.Context) error {
	go func() {
		wait := DriverWatchFirst
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			w.check(ctx)
			wait = DriverWatchEvery
		}
	}()
	return nil
}

func (w *driverWatch) check(ctx context.Context) {
	dc := DriverItems(ctx, client.NewUnix(w.socket))
	if ctx.Err() != nil {
		return
	}
	var problems []string
	for _, it := range dc.Items {
		if !it.OK {
			problems = append(problems, fmt.Sprintf("%s %s: %s", it.Kind, it.Name, it.Error))
		}
	}
	sort.Strings(problems)
	var n *DriverNotice
	if len(problems) > 0 {
		n = &DriverNotice{Checked: time.Now().UTC(), Refused: len(problems), Problems: problems}
	}
	w.mu.Lock()
	prev := w.notice
	w.notice = n
	w.mu.Unlock()
	if w.log == nil || (prev == nil) == (n == nil) && (n == nil || strings.Join(prev.Problems, "\n") == strings.Join(n.Problems, "\n")) {
		return // said already
	}
	if n == nil {
		w.log.Info("run targets: every bound driver runs again")
		return
	}
	for _, p := range problems {
		w.log.Warn("run targets: a bound driver does not run", "problem", p)
	}
}

// part is health's drivers section (nil: none refused).
func (w *driverWatch) part(context.Context) any {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.notice == nil {
		return nil
	}
	n := *w.notice
	return &n
}

// healthParts registers health's drivers section (core.Deps.AddHealthPart).
// The backup, heartbeat and holders sections are orchcore.Wire's.
func healthParts(d *core.Deps, w *driverWatch) {
	d.AddHealthPart("drivers", w.part)
}
