package main

// `zm lend switch` and the switch line of `zm lend status`:
// the owner opens, drains or closes this Mac for new jobs, or hands it back to
// its schedule, without calling the driver binary by path. Both go through
// Potluck's own code: vmhost.SetHost validates and writes
// the host-wide override; status reads a lent size's describe host block (the
// driver's own view, its schedule included) and, with nothing lent,
// vmhost.HostNow (the same file rules, no schedule).

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// lendHostSet sets the switch (vmhost.SetHost: mode, then --for, --until and
// --deadline's values, "" for none). Tests replace it: the real one writes
// the host-wide file.
var lendHostSet = func(mode, forD, until, deadline string) error {
	_, err := vmhost.SetHost(mode, forD, until, deadline)
	return err
}

// lendHostNow reads the switch with no binding to describe (vmhost.HostNow:
// the owner's uid, a drain_deadline_s or below 0 for the default). Tests
// replace it: the real one reads the host-wide file.
var lendHostNow = vmhost.HostNow

// LendSwitch is the switch in zm lend status --json and zm lend switch --json.
type LendSwitch struct {
	Mode  string `json:"mode"`            // open, drain, closed, auto, or unreadable
	State string `json:"state"`           // what it means for new jobs: accepting (auto: the schedule's), draining, closed or holding
	Until string `json:"until,omitempty"` // back to auto then (RFC 3339)
	Cut   string `json:"cut,omitempty"`   // drain: when jobs still running are stopped (RFC 3339)
	Why   string `json:"why,omitempty"`   // unreadable: why
	Words string `json:"words"`           // the line status prints
}

// readLendSwitch reads the switch; vs are the lent sizes, whose describe
// answers it when one does.
func readLendSwitch(vs []computeapi.RunTargetView) LendSwitch {
	sw := lendSwitchOf(vs)
	sw.Words = sw.words()
	return sw
}

// lendSwitchOf is the switch from the first lent size whose describe has a
// host block, else from Potluck's reader with the first lent size's owner and
// drain deadline (none lent: this user's, and the default).
func lendSwitchOf(vs []computeapi.RunTargetView) LendSwitch {
	owner, drainS := os.Getuid(), -1 // host_owner_uid's default: the driver's own uid, which is this user's
	for _, v := range vs {
		if n, err := strconv.Atoi(v.Config["host_owner_uid"]); err == nil {
			owner = n
		}
		if n, err := strconv.Atoi(v.Config["drain_deadline_s"]); err == nil {
			drainS = n
		}
		break // zm lend lends all sizes alike
	}
	here := func() LendSwitch {
		h, err := lendHostNow(owner, drainS)
		sw := switchFrom(h)
		if err != nil {
			sw.Mode, sw.State, sw.Why = "unreadable", "holding", plain(err.Error())
		}
		return sw
	}
	for _, v := range vs {
		d := v.Describe
		if d == nil || d.Host == nil {
			continue
		}
		sw := switchFrom(*d.Host)
		if sw.Mode == "unreadable" {
			// describe's why names the override only when nothing else refuses first.
			const unreadable = "the host override cannot be read, so no new work starts: "
			if why, ok := strings.CutPrefix(d.Why, unreadable); ok {
				why, _, _ = strings.Cut(why, "; ")
				sw.Why = plain(why)
			} else if sw.Why = here().Why; sw.Why == "" {
				sw.Why = "the driver could not read it"
			}
		}
		return sw
	}
	return here()
}

// switchFrom is Potluck's host block in zm's words: a drain that returns to
// auto before its deadline cuts nothing.
func switchFrom(h protocol.HostStatus) LendSwitch {
	sw := LendSwitch{Mode: h.Mode, State: h.State, Until: h.Until}
	if h.DrainDeadline != "" {
		cut, cerr := time.Parse(time.RFC3339, h.DrainDeadline)
		until, uerr := time.Parse(time.RFC3339, h.Until)
		if cerr == nil && (h.Until == "" || uerr != nil || until.After(cut)) {
			sw.Cut = h.DrainDeadline
		}
	}
	return sw
}

// clock is t (RFC 3339) as a local time of day, with the date when not today.
func clock(t string, now time.Time) string {
	at, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return t
	}
	at = at.In(now.Location())
	if y, m, d := at.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return at.Format("15:04")
	}
	return at.Format("Mon 2 Jan 15:04")
}

func (s LendSwitch) words() string { return s.wordsAt(time.Now()) }

func (s LendSwitch) wordsAt(now time.Time) string {
	until := ""
	if s.Until != "" {
		until = ", until " + clock(s.Until, now) + " (then back to the schedule)"
	}
	switch s.Mode {
	case "open":
		return "Switch: open: new jobs start whatever the schedule says" + until + "."
	case "drain":
		cut := ""
		if s.Cut != "" {
			cut = " (jobs still running at " + clock(s.Cut, now) + " are stopped)"
		}
		if s.State == "closed" {
			return "Switch: drained: no new jobs start, and jobs still running were stopped at " + clock(s.Cut, now) + until + "."
		}
		return "Switch: draining: no new jobs start; jobs running now finish" + cut + until + "."
	case "closed":
		return "Switch: closed: no new jobs start, and jobs running now are stopped" + until + "."
	case "unreadable":
		fix := " zm lend switch auto sets it again."
		if strings.Contains(s.Why, "is owned by uid") { // Potluck's readOverride: only its owner (or an admin) fixes that
			fix = ""
		}
		more := ""
		switch s.State { // the schedule does more than hold
		case "draining":
			more = "; the schedule is draining this Mac, so jobs running now finish"
			if s.Cut != "" {
				more += " (jobs still running at " + clock(s.Cut, now) + " are stopped)"
			}
		case "closed":
			more = "; the schedule has closed this Mac, so jobs running now are stopped"
		}
		return "Switch: it cannot be read, so no new jobs start (" + s.Why + ")" + more + "." + fix
	}
	switch s.State { // auto: the schedule's own drain or close
	case "draining":
		cut := ""
		if s.Cut != "" {
			cut = " (jobs still running at " + clock(s.Cut, now) + " are stopped)"
		}
		return "Switch: auto: the schedule is draining this Mac: no new jobs start; jobs running now finish" + cut + "."
	case "closed":
		return "Switch: auto: the schedule has closed this Mac for now: no new jobs start, and jobs running now are stopped."
	}
	return "Switch: auto: jobs run as the lend set it up. zm lend switch open, drain or close changes that."
}

func lendSwitchCmd() *cobra.Command {
	var forD, until, deadline string
	cmd := &cobra.Command{
		Use:   "switch <open|drain|close|auto>",
		Short: "Open, drain or close this Mac for new jobs, or hand it back to its schedule (auto)",
		Long: `Switch decides what jobs may do on this Mac now, whatever its schedule says:
open (new jobs start), drain (no new jobs; jobs running now finish, and are
stopped at the deadline), close (no new jobs; jobs running now are stopped)
or auto (the schedule zm lend set up). --for and --until hand it back to auto
then. zm lend status shows it.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"open", "drain", "close", "auto"},
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			switch mode {
			case "close", "closed":
				mode = "closed"
			case "open", "drain", "auto":
			default:
				return plainErr(fmt.Errorf("%q is not open, drain, close or auto", args[0]))
			}
			if deadline != "" && mode != "drain" {
				return errors.New("--deadline is a drain's: how long jobs running now have")
			}
			if err := lendHostSet(mode, forD, until, deadline); err != nil {
				return plainErr(err)
			}
			var vs []computeapi.RunTargetView
			if c, prefix, _, err := lendCtx(cmd); err == nil {
				vs, _ = lentHere(cmd.Context(), c, prefix) // only for a lent size's drain deadline
			}
			sw := readLendSwitch(vs)
			if flagJSON {
				return printJSON(sw)
			}
			fmt.Fprintln(cmd.OutOrStdout(), sw.Words)
			fmt.Fprintln(cmd.OutOrStdout(), "done")
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&forD, "for", "", "hand it back to auto after this long (30m, 2h)")
	fl.StringVar(&until, "until", "", "hand it back to auto at this time (HH:MM, or an RFC 3339 time)")
	fl.StringVar(&deadline, "deadline", "", "drain: how long jobs running now have before they are stopped (default: the lent size's, 30m unless set)")
	return cmd
}
