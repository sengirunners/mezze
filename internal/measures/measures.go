// Package measures counts how often zm's cards and mail needed a person, per
// day and per owner, from zm's own ask and mail records.
//
// It is a pure computation: the caller (zm measures) reads the records through
// the REST door and hands them in, so the package reads no table of another
// brick. Every number is a Row (rows.go): a name and one Scan function that
// walks the records and emits observations. Another lane adds a measure by
// appending a Row to Rows; nothing else changes. No person, account, machine,
// project, time zone or limit is named here: owners come from the records,
// the zone and the thresholds from Config.
//
// Meaning is never judged here. A card's kind and "repeats" come from labels
// a judge stored on the card (orchapi.Ask.Labels) or from zm's own join
// record; a card with no label reads "unlabelled".
package measures

import (
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Config holds every threshold and the owner-facing choices. DefaultConfig is
// the one place the defaults live; the CLI's flags override them.
type Config struct {
	// WithdrawnWithin: a card taken back this soon after it was filed counts
	// as withdrawn fast (noise the asker retracted).
	WithdrawnWithin time.Duration
	// NotDoneAfter: a yes whose work is still not done this long after the
	// answer counts as stuck.
	NotDoneAfter time.Duration
	// GroupUntakenAfter: group mail nobody took within this long.
	GroupUntakenAfter time.Duration
	// ExcludeAskedBy drops every card asked by these addresses (a bulk import
	// is not a day's work).
	ExcludeAskedBy []string
}

// DefaultConfig is the thresholds' defaults.
func DefaultConfig() Config {
	return Config{WithdrawnWithin: 10 * time.Minute, NotDoneAfter: 24 * time.Hour, GroupUntakenAfter: 24 * time.Hour}
}

// Input is everything one computation reads.
type Input struct {
	Asks []orchapi.Ask
	Mail []orchapi.Mail
	// From and To bound the window [From, To); Now is when the records were
	// read (a card "still not done after 24 hours" is judged against it).
	From, To, Now time.Time
	Loc           *time.Location // days are cut in this zone
	Cfg           Config
	// Canon maps an owner's name or alias to the one name its person is shown
	// under, so one person is one column. Nil leaves names as recorded.
	Canon func(string) string
}

// Obs is one thing a Row saw: when, for whom, and (for rates and medians)
// what.
type Obs struct {
	At    time.Time
	Owner string
	// Group splits a row into lines ("kind=run"); "" is one line.
	Group string
	// Value feeds a Median (seconds); Hit feeds a Rate's numerator.
	Value float64
	Hit   bool
}

// Agg says how a row's observations become a number.
type Agg int

const (
	Count  Agg = iota // how many observations
	Rate              // Hit of N
	Median            // the median of Value
)

// Row is one measure. Scan walks the input and emits observations; it never
// filters by the window (Compute does) and never reads anything but Input.
type Row struct {
	ID    string
	Title string
	Agg   Agg
	Scan  func(in *Input, emit func(Obs))
}

// Cell is one number: N observations, of which Hits (a Rate), with Value the
// median (a Median). Zero value = nothing seen.
type Cell struct {
	N     int     `json:"n"`
	Hits  int     `json:"hits,omitempty"`
	Value float64 `json:"value,omitempty"`
}

// Keys of the result that are not days or owners.
const (
	AllOwners = "all"    // every owner added up
	Window    = "window" // the whole window, as one "day"
)

// Series is one line of the table: a row (and group) across days and owners.
type Series struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Group string `json:"group,omitempty"`
	Agg   Agg    `json:"agg"`
	// Cells maps day (YYYY-MM-DD, or Window) to owner (or AllOwners) to Cell.
	Cells map[string]map[string]Cell `json:"cells"`
}

// Report is the result of Compute.
type Report struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Zone   string   `json:"zone"`
	Days   []string `json:"days"`
	Owners []string `json:"owners"`
	Series []Series `json:"series"`
	// Notes say what the numbers do not cover (a read that could not see
	// every card); Render prints them first.
	Notes []string `json:"notes,omitempty"`
}

// Compute runs rows over the input and buckets what they saw by day, owner
// and group, keeping only observations inside [From, To).
func Compute(in Input, rows []Row) Report {
	if in.Loc == nil {
		in.Loc = time.UTC
	}
	in.Asks = withoutAskers(in.Asks, in.Cfg.ExcludeAskedBy)
	rep := Report{From: dayOf(in.From, in.Loc), To: dayOf(in.To.Add(-time.Nanosecond), in.Loc), Zone: in.Loc.String(), Days: days(in)}
	owners := map[string]bool{}
	for _, row := range rows {
		type key struct{ group, day, owner string }
		obs := map[key][]Obs{}
		groups := map[string]bool{}
		row.Scan(&in, func(o Obs) {
			if o.At.Before(in.From) || !o.At.Before(in.To) || o.Owner == "" {
				return
			}
			d := dayOf(o.At, in.Loc)
			if in.Canon != nil {
				o.Owner = in.Canon(o.Owner)
			}
			owners[o.Owner] = true
			groups[o.Group] = true
			for _, day := range []string{d, Window} {
				for _, ow := range []string{o.Owner, AllOwners} {
					k := key{o.Group, day, ow}
					obs[k] = append(obs[k], o)
				}
			}
		})
		if len(groups) == 0 {
			groups[""] = true // a row with nothing to show still shows zeros
		}
		names := make([]string, 0, len(groups))
		for g := range groups {
			names = append(names, g)
		}
		sort.Strings(names)
		for _, g := range names {
			s := Series{ID: row.ID, Title: row.Title, Group: g, Agg: row.Agg, Cells: map[string]map[string]Cell{}}
			for k, os := range obs {
				if k.group != g {
					continue
				}
				if s.Cells[k.day] == nil {
					s.Cells[k.day] = map[string]Cell{}
				}
				s.Cells[k.day][k.owner] = cellOf(row.Agg, os)
			}
			rep.Series = append(rep.Series, s)
		}
	}
	for o := range owners {
		rep.Owners = append(rep.Owners, o)
	}
	sort.Strings(rep.Owners)
	return rep
}

func cellOf(a Agg, os []Obs) Cell {
	c := Cell{N: len(os)}
	switch a {
	case Rate:
		for _, o := range os {
			if o.Hit {
				c.Hits++
			}
		}
	case Median:
		v := make([]float64, len(os))
		for i, o := range os {
			v[i] = o.Value
		}
		sort.Float64s(v)
		if n := len(v); n%2 == 1 {
			c.Value = v[n/2]
		} else {
			c.Value = (v[n/2-1] + v[n/2]) / 2
		}
	}
	return c
}

func dayOf(t time.Time, loc *time.Location) string { return t.In(loc).Format(time.DateOnly) }

// days lists every calendar day of the window in the input's zone, so a
// quiet day shows as zeros instead of being missing.
func days(in Input) []string {
	var out []string
	d := time.Date(in.From.In(in.Loc).Year(), in.From.In(in.Loc).Month(), in.From.In(in.Loc).Day(), 0, 0, 0, 0, in.Loc)
	for d.Before(in.To) {
		out = append(out, d.Format(time.DateOnly))
		d = time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, in.Loc)
	}
	return out
}

func withoutAskers(asks []orchapi.Ask, skip []string) []orchapi.Ask {
	if len(skip) == 0 {
		return asks
	}
	out := make([]orchapi.Ask, 0, len(asks))
	for _, a := range asks {
		drop := false
		for _, s := range skip {
			if strings.EqualFold(a.AskedBy, s) {
				drop = true
			}
		}
		if !drop {
			out = append(out, a)
		}
	}
	return out
}
