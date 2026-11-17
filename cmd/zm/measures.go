package main

// zm measures: how often cards and mail needed a person, per
// day and per owner, from zm's own records. The numbers are internal/measures;
// this file reads the records through the REST door (past the 200-card page)
// and prints or mails the result.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/measures"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func init() { rootCmd.AddCommand(measuresCmd()) }

// measuresNow is time.Now (tests replace it).
var measuresNow = time.Now

const (
	askPageSize  = 1000
	mailPageSize = 500
)

type measuresFlags struct {
	days     int
	from, to string // YYYY-MM-DD; to is the last day, inclusive
	tz       string
	lookback time.Duration
	// allowShort reads what an old daemon will give (its newest 200 cards, its newest
	// mail) instead of refusing; the report then says what it may be missing.
	allowShort bool
	md         bool
	cfg        measures.Config
}

func (f *measuresFlags) register(cmd *cobra.Command, lastDay string) {
	d := measures.DefaultConfig()
	cmd.Flags().IntVar(&f.days, "days", 7, "how many days, ending with the last day (default today; yesterday for a report)")
	cmd.Flags().StringVar(&f.from, "from", "", "first day, YYYY-MM-DD (beats --days)")
	cmd.Flags().StringVar(&f.to, lastDay, "", "last day, inclusive, YYYY-MM-DD (default today)")
	cmd.Flags().StringVar(&f.tz, "tz", "", "time zone name that cuts the days (default: this machine's)")
	cmd.Flags().DurationVar(&f.lookback, "lookback", 30*24*time.Hour, "how far before the window to read cards, so one filed earlier but answered inside it still counts")
	cmd.Flags().BoolVar(&f.allowShort, "allow-short", false, "with a daemon that cannot page or window, read its newest 200 cards and 500 mails instead of refusing (the report opens with a NOTE saying how far back it saw)")
	cmd.Flags().BoolVar(&f.md, "md", false, "Markdown tables instead of aligned columns")
	cmd.Flags().DurationVar(&f.cfg.WithdrawnWithin, "withdrawn-within", d.WithdrawnWithin, "a card taken back this soon counts as withdrawn fast")
	cmd.Flags().DurationVar(&f.cfg.NotDoneAfter, "not-done-after", d.NotDoneAfter, "a yes still not done this long after the answer counts as stuck")
	cmd.Flags().DurationVar(&f.cfg.GroupUntakenAfter, "group-untaken-after", d.GroupUntakenAfter, "group mail nobody took within this long")
	cmd.Flags().StringArrayVar(&f.cfg.ExcludeAskedBy, "exclude-asker", nil, "leave out every card asked by this address (a bulk import); repeatable")
}

// window resolves the flags to [from, to) in the chosen zone.
func (f *measuresFlags) window(now time.Time) (from, to time.Time, loc *time.Location, err error) {
	loc = now.Location()
	if f.tz != "" {
		if loc, err = time.LoadLocation(f.tz); err != nil {
			return from, to, nil, fmt.Errorf("--tz: %v", err)
		}
	}
	day := func(s string) (time.Time, error) { return time.ParseInLocation(time.DateOnly, s, loc) }
	last := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	if f.to != "" {
		if last, err = day(f.to); err != nil {
			return from, to, nil, fmt.Errorf("the last day is a day like 2026-11-14")
		}
	}
	to = time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, loc)
	if f.from != "" {
		if from, err = day(f.from); err != nil {
			return from, to, nil, fmt.Errorf("--from is a day like 2026-11-14")
		}
	} else {
		if f.days < 1 {
			return from, to, nil, errors.New("--days is at least 1")
		}
		from = time.Date(last.Year(), last.Month(), last.Day()-f.days+1, 0, 0, 0, 0, loc)
	}
	if !from.Before(to) {
		return from, to, nil, errors.New("--from is after the last day")
	}
	return from, to, loc, nil
}

// collectAsks pages through every readable card, newest first, until it is
// older than before (the window's start less the lookback).
func collectAsks(ctx context.Context, c *client.Client, project string, before time.Time, allowShort bool) ([]orchapi.Ask, string, error) {
	// An old daemon ignores limit and answers its newest 200 whatever is asked: ask
	// for one card, and refuse it if it sends more (reading it would be silently short).
	if probe, _, err := orchclient.AskPage(ctx, c, project, "", 1); err != nil {
		return nil, "", err
	} else if len(probe) > 1 {
		if !allowShort {
			return nil, "", errors.New("this daemon lists only the newest 200 cards and cannot page: update it (zm daemon update), or accept a short read with --allow-short")
		}
		// The daemon ignored limit=1: the probe is its whole newest page.
		return probe, "this daemon cannot page: only its newest " + strconv.Itoa(len(probe)) + " cards were read, back to " +
			oldestAsk(probe).UTC().Format(time.DateTime) + " UTC: nothing earlier is counted, and a card filed earlier but answered later is missing", nil
	}
	var all []orchapi.Ask
	cursor, seen := "", map[string]bool{}
	for {
		page, next, err := orchclient.AskPage(ctx, c, project, cursor, askPageSize)
		if err != nil {
			return nil, "", err
		}
		for _, a := range page {
			if !seen[a.ID] {
				seen[a.ID] = true
				all = append(all, a)
			}
		}
		if next == "" || next == cursor || (len(page) > 0 && page[len(page)-1].Created.Before(before)) {
			return all, "", nil
		}
		cursor = next
	}
}

// collectMail pages back through the project's mail from since to until by
// moving until to the oldest row of each full page.
func collectMail(ctx context.Context, c *client.Client, project string, since, until time.Time, allowShort bool) ([]orchapi.Mail, string, error) {
	var all []orchapi.Mail
	seen := map[string]bool{}
	for {
		page, err := orchclient.Mail(ctx, c, orchapi.MailQuery{Project: project, Since: since, Until: until, Limit: mailPageSize})
		if err != nil {
			return nil, "", err
		}
		fresh := 0
		for _, m := range page {
			if !seen[m.ID] {
				seen[m.ID] = true
				all = append(all, m)
				fresh++
			}
		}
		if len(page) < mailPageSize {
			return all, "", nil
		}
		if fresh == 0 && allowShort {
			return all, "this daemon does not take since and until, or more mails share one millisecond: only the newest " + strconv.Itoa(len(all)) +
				" mails were read, back to " + oldestMail(all).UTC().Format(time.DateTime) + " UTC: nothing earlier is counted", nil
		}
		if fresh == 0 {
			return nil, "", fmt.Errorf("cannot page past %d mails: more share one millisecond, or this daemon does not take since and until (update it, or accept a short read with --allow-short)", mailPageSize)
		}
		until = page[len(page)-1].Created // newest first: the last row is the oldest
	}
}

func oldestAsk(as []orchapi.Ask) time.Time {
	t := as[0].Created
	for _, a := range as {
		if a.Created.Before(t) {
			t = a.Created
		}
	}
	return t
}

func oldestMail(ms []orchapi.Mail) time.Time {
	t := ms[0].Created
	for _, m := range ms {
		if m.Created.Before(t) {
			t = m.Created
		}
	}
	return t
}

// computeMeasures reads the records and runs every row.
func computeMeasures(ctx context.Context, c *client.Client, project string, f *measuresFlags) (measures.Report, error) {
	// One project: the flag, else the one this client resolved (the repo you are in). Without
	// one the daemon would add up every project you can read.
	project = strings.SplitN(first(project, c.Project), "/", 2)[0]
	if project == "" {
		if r, err := c.Resolve(ctx); err == nil && r.Project != nil { // the repo you are in
			project = r.Project.Slug
		}
	}
	if project == "" {
		return measures.Report{}, errors.New("name the project: zm measures -P <project>")
	}
	now := measuresNow()
	from, to, loc, err := f.window(now)
	if err != nil {
		return measures.Report{}, err
	}
	asks, note, err := collectAsks(ctx, c, project, from.Add(-f.lookback), f.allowShort)
	if err != nil {
		return measures.Report{}, err
	}
	mail, mailNote, err := collectMail(ctx, c, project, from, to, f.allowShort)
	if err != nil {
		return measures.Report{}, err
	}
	rep := measures.Compute(measures.Input{Asks: asks, Mail: mail, From: from, To: to, Now: now, Loc: loc, Cfg: f.cfg, Canon: ownerNames(ctx, c, project)}, measures.Rows)
	for _, n := range []string{note, mailNote} {
		if n != "" {
			rep.Notes = append(rep.Notes, n)
		}
	}
	return rep, nil
}

// ownerNames maps every name or alias of a person (zm people) to the one
// name they are shown under, so a person is one column. Unreadable people
// leave names as recorded.
func ownerNames(ctx context.Context, c *client.Client, project string) func(string) string {
	l, err := orchclient.People(ctx, c, project)
	if err != nil {
		return nil
	}
	canon := map[string]string{}
	for _, p := range l.People {
		for _, n := range append([]string{p.Name}, p.Names...) {
			canon[n] = p.Name
		}
	}
	return func(n string) string { return first(canon[n], n) }
}

func (f *measuresFlags) format() measures.Format {
	if f.md {
		return measures.Markdown
	}
	return measures.Text
}

func measuresCmd() *cobra.Command {
	var f measuresFlags
	cmd := &cobra.Command{
		Use:   "measures",
		Short: "How often cards and mail needed a person, per day and per owner",
		Long: `Counts, from zm's own ask and mail records and per day and per owner: cards filed and
cards answered (kept apart), the share answered with the recommendation per kind, cards
withdrawn within minutes, repeats of an open card, the median time to answer, yeses still
not done after a day, mail sent to ended sessions, group mail nobody took, and harm (an
owner's later objection or undo). Read-only: it changes nothing.

A card's kind and "repeat" come from labels a judge stored on the card; a card with none
reads "unlabelled". Nothing is guessed from the card's words. The window defaults to the
last 7 days; days are cut in --tz (default this machine's zone).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (measures.Report, error) {
				return computeMeasures(ctx, c, flagProject, &f)
			}, func(r measures.Report) string { return measures.Render(r, f.format()) })
		},
	}
	f.register(cmd, "to")
	var rf measuresFlags
	var to string
	report := &cobra.Command{
		Use:   "report",
		Short: "Mail the measures to the project's owners as a report",
		Long: `Computes the same table and files it as a report (zm's report mail kind) addressed to the
project's owners, whoever they are. One report per window: running it again for the same
window says it was already sent. Schedule it weekly with whatever scheduler you run
(a cron line, a launchd job, a scheduled agent task): zm measures report -P <project>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rf.md = true
			if rf.to == "" {
				// A weekly report covers whole days: end yesterday, so windows never overlap.
				_, _, loc, err := rf.window(measuresNow())
				if err != nil {
					return err
				}
				rf.to = measuresNow().In(loc).AddDate(0, 0, -1).Format(time.DateOnly)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Mail, error) {
				rep, err := computeMeasures(ctx, c, flagProject, &rf)
				if err != nil {
					return orchapi.Mail{}, err
				}
				return sendMeasuresReport(ctx, c, rep, to)
			}, func(m orchapi.Mail) string { return fmt.Sprintf("sent %s to %s", short(m.ID), sentTo(m)) })
		},
	}
	rf.register(report, "through") // --to is the recipient here
	report.Flags().StringVar(&to, "to", "owners", "who gets it: owners, or human:<name>")
	cmd.AddCommand(report)
	return cmd
}

// sendMeasuresReport files the report as mail of kind report to the owners
// (or whoever to names). The retry key names the window, so a second run for
// the same window finds the first one's mail and says so.
func sendMeasuresReport(ctx context.Context, c *client.Client, rep measures.Report, to string) (orchapi.Mail, error) {
	key := "measures-" + rep.From + "-" + rep.To
	if len(rep.Notes) > 0 {
		key += "-short" // a short read must not use up the window's one complete report
	}
	m, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{
		To: to, Kind: orchapi.MailKindReport, Needs: orchapi.NeedsHuman,
		ClientRequestID: key,
		Body:            reportBody(rep, measures.Markdown),
	})
	if api.IsCode(err, api.CodeConflict) {
		return orchapi.Mail{}, fmt.Errorf("the measures for %s to %s were already sent: nothing to do", rep.From, rep.To)
	}
	return m, err
}

func reportBody(rep measures.Report, f measures.Format) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Measures for %s to %s: how often cards and mail needed a person.\n\n", rep.From, rep.To)
	b.WriteString(measures.Render(rep, f))
	return b.String()
}
