package main

// zm ask content-count: over a window of past cards, how many
// content cards one daily card with a yes per item would have merged. A
// count only: it changes no card and sends nothing. An agent judge says
// which cards are content (internal/contentcards); a judge that cannot answer
// holds the card and the report says it is incomplete.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/contentcards"
	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// asksListCap is the most asks the daemon's list returns (GET /v0/asks).
const asksListCap = 200

// contentCountOut is --json: the full report and the one row a weekly
// report prints.
type contentCountOut struct {
	Row    contentcards.Row    `json:"row"`
	Report contentcards.Report `json:"report"`
}

// parseWhen reads a window end: RFC 3339, or a date at midnight in loc.
func parseWhen(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation(time.DateOnly, s, loc)
	if err != nil {
		return t, fmt.Errorf("%q is neither a date (2006-01-02) nor an RFC 3339 time", s)
	}
	return t, nil
}

// readAsks reads ask records from r: a list, or {"asks": [...]} (zm ask ls --json).
func readAsks(r io.Reader) ([]orchapi.Ask, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, err
	}
	var list []orchapi.Ask
	if json.Unmarshal(b, &list) == nil {
		return list, nil
	}
	var l orchapi.AskList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("not a list of asks: %w", err)
	}
	return l.Asks, nil
}

func askContentCountCmd() *cobra.Command {
	var from, since, until, tz string
	var days int
	cmd := &cobra.Command{
		Use:   "content-count [--days 7] [--from asks.json]",
		Short: "Count how many content cards one daily card would have merged (a count only; changes no card)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loc, err := time.LoadLocation(tz)
			if err != nil {
				return fmt.Errorf("--tz: %w", err)
			}
			if days < 1 {
				return fmt.Errorf("--days must be at least 1")
			}
			// The default window is the last N whole days in --tz, today
			// left out: its end (--until) is exclusive, so --since 2030-01-01
			// --until 2030-01-08 is those seven days.
			o := contentcards.Options{Location: loc}
			if until == "" {
				y, m, d := time.Now().In(loc).Date()
				o.Until = time.Date(y, m, d, 0, 0, 0, 0, loc)
			} else if o.Until, err = parseWhen(until, loc); err != nil {
				return fmt.Errorf("--until: %w", err)
			}
			o.Since = o.Until.In(loc).AddDate(0, 0, -days)
			if since != "" {
				if o.Since, err = parseWhen(since, loc); err != nil {
					return fmt.Errorf("--since: %w", err)
				}
			}
			if !o.Since.Before(o.Until) {
				return fmt.Errorf("the window ends before it starts (%s to %s)", o.Since.Format(time.DateOnly), o.Until.Format(time.DateOnly))
			}
			var asks []orchapi.Ask
			var agents map[string][]string
			if from != "" {
				f := os.Stdin
				if from != "-" {
					if f, err = os.Open(from); err != nil {
						return err
					}
					defer f.Close()
				}
				if asks, err = readAsks(f); err != nil {
					return err
				}
			} else {
				c, cfg, err := newClient()
				if err != nil {
					return err
				}
				agents = cfg.File.Agents
				if asks, err = orchclient.Asks(cmd.Context(), c, flagProject, false); err != nil {
					return err
				}
				if len(asks) >= asksListCap {
					o.Capped = true
					fmt.Fprintf(cmd.ErrOrStderr(), "zm: the daemon lists at most %d asks, so an older window may be cut short: pass the full set with --from\n", asksListCap)
				}
			}
			o.Caller = judge.Default(agents)
			r := contentcards.Count(context.WithoutCancel(cmd.Context()), asks, o)
			if flagJSON {
				return printJSON(contentCountOut{Row: r.Row(), Report: r})
			}
			fmt.Fprint(cmd.OutOrStdout(), r.Text())
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "read ask records from this file (a list, or zm ask ls --json); - is stdin; default: the daemon's list")
	cmd.Flags().IntVar(&days, "days", 7, "the window's length in whole days, ending at --until")
	cmd.Flags().StringVar(&since, "since", "", "window start: a date or RFC 3339 time (overrides --days)")
	cmd.Flags().StringVar(&until, "until", "", "window end, exclusive: a date or RFC 3339 time (default: the start of today in --tz)")
	cmd.Flags().StringVar(&tz, "tz", "UTC", "the time zone that says where one day ends and the next begins (an IANA name)")
	return cmd
}
