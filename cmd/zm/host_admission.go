package main

// zm host share's and zm pool host's admission keys: what an
// earlier share bound is read back so a re-run keeps it, and doctor names it.

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// bindingsIn is this machine's bindings in one scope: machine-level (host),
// else the resolved project's (the owner sees their config). start lets it
// start the daemon; else a daemon that is not running has nothing to say.
// Tests replace it.
var bindingsIn = func(ctx context.Context, start, host bool) ([]computeapi.RunTarget, error) {
	var c *client.Client
	if start {
		var err error
		if c, _, err = newClient(); err != nil {
			return nil, err
		}
	} else {
		cfg, err := config.LoadClient()
		if err != nil {
			return nil, err
		}
		c = client.New(cfg)
		if flagProject != "" {
			c.Project, c.ProjectFrom = flagProject, "flag"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var l computeapi.RunTargetList
	var err error
	if host {
		l, err = computeclient.HostRunTargets(ctx, c)
	} else {
		l, err = computeclient.RunTargets(ctx, c, false)
	}
	return targetsOf(l), err
}

// targetsOf is a list's bindings on this machine (not another host's).
func targetsOf(l computeapi.RunTargetList) []computeapi.RunTarget {
	var out []computeapi.RunTarget
	for _, v := range l.Targets {
		if v.RunTarget.Host == "" {
			out = append(out, v.RunTarget)
		}
	}
	return out
}

func (s *shareFlags) addAdmission(cmd *cobra.Command) {
	fl, o := cmd.Flags(), &s.o
	fl.StringVar(&o.AdmissionURL, "admission-url", "", "a host that runs other work too: every box first takes a lease from this admission controller (sengirunners/potluck cmd/potluck-driver-tart/README.md; default: what an earlier share bound; none removes it)")
	fl.StringVar(&o.AdmissionTokenFile, "admission-token-file", "", "a 0600 file whose token goes with every admission call (default: an earlier share's)")
	fl.IntVar(&o.AdmissionTTLS, "admission-ttl-s", 0, "how long a lease lasts unrenewed, 120..3600 s (default: an earlier share's, else the driver's 120)")
}

// earlier reads into o the bindings a share replaces, in the one scope it
// binds in (machine-level with --pool, else the project's), always: for the
// admission keys to keep without --admission-url, and for a keyless binding
// to unbind when a run adding them fails. A failed read fails closed in
// Check (o.EarlierUnread); a dry run never starts the daemon, so there it is
// a note. zm pool host reads its scope itself once the pool is known (a dry
// run here: machine-level, where a pool joined binds).
func (s *shareFlags) earlier(ctx context.Context, o *hostprobe.ShareOptions) {
	if s.poolHost && !o.DryRun {
		return
	}
	ts, err := bindingsIn(ctx, !o.DryRun, s.poolHost || o.Pool != "")
	o.Earlier = ts
	switch {
	case err == nil:
	case !o.DryRun:
		o.EarlierUnread = err.Error()
	case !flagJSON:
		fmt.Fprintln(os.Stderr, "note: this machine's bindings could not be read without starting zm's daemon, so the plan does not show admission keys an earlier share bound (a share keeps them)")
	}
}

// printAdmission is share's admission line.
func printAdmission(w io.Writer, res hostprobe.ShareResult) {
	a := res.Admission
	if a == nil {
		return
	}
	fmt.Fprintf(w, "admission: every box, conformance's too, first takes a lease from %s%s", a["admission_url"], admissionDetail(a))
	if res.AdmissionFrom != "" {
		fmt.Fprintf(w, "; kept from %s (--admission-url none removes it)", res.AdmissionFrom)
	}
	fmt.Fprintln(w)
}

func admissionDetail(a map[string]string) string {
	var d []string
	if t := a["admission_ttl_s"]; t != "" {
		d = append(d, "lease "+t+" s")
	}
	if f := a["admission_token_file"]; f != "" {
		d = append(d, "token from "+f)
	}
	if len(d) == 0 {
		return ""
	}
	return " (" + strings.Join(d, ", ") + ")"
}

// doctorAdmission is a box binding's admission controller, for doctor.
type doctorAdmission struct {
	Binding   string `json:"binding"`
	URL       string `json:"admission_url"`
	TTLS      string `json:"admission_ttl_s,omitempty"`
	TokenFile string `json:"admission_token_file,omitempty"`
}

// bindingAdmission is each of this machine's box bindings that asks an
// admission controller first, by name (read without starting the daemon).
func bindingAdmission(ctx context.Context) []doctorAdmission {
	ts, _ := bindingsIn(ctx, false, true)
	if p, err := bindingsIn(ctx, false, false); err == nil {
		ts = append(ts, p...)
	}
	var out []doctorAdmission
	for _, t := range ts {
		if t.Kind == "box" && t.Host == "" && t.Config["admission_url"] != "" {
			out = append(out, doctorAdmission{Binding: t.Name, URL: t.Config["admission_url"], TTLS: t.Config["admission_ttl_s"], TokenFile: t.Config["admission_token_file"]})
		}
	}
	slices.SortFunc(out, func(a, b doctorAdmission) int { return strings.Compare(a.Binding, b.Binding) })
	return slices.CompactFunc(out, func(a, b doctorAdmission) bool { return a.Binding == b.Binding })
}

// printDoctorAdmission is doctor's admission line, one per controller.
func printDoctorAdmission(w io.Writer, as []doctorAdmission) {
	var order []string
	by := map[string][]string{}
	for _, a := range as {
		k := a.URL + admissionDetail(map[string]string{"admission_ttl_s": a.TTLS, "admission_token_file": a.TokenFile})
		if by[k] == nil {
			order = append(order, k)
		}
		by[k] = append(by[k], a.Binding)
	}
	for _, k := range order {
		fmt.Fprintf(w, "admission: %s take a lease from %s before each box\n", strings.Join(by[k], ", "), k)
	}
}
