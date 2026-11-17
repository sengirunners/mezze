package measures

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Format is how Render lays out the report.
type Format int

const (
	Text     Format = iota // aligned columns for a terminal
	Markdown               // pipe tables, for a report mail
)

// Render writes the whole window first, then each day, one table apiece:
// a line per measure, a column per owner seen that day, and a total.
func Render(rep Report, f Format) string {
	var b strings.Builder
	for _, n := range rep.Notes {
		fmt.Fprintf(&b, "NOTE: %s\n\n", n)
	}
	sections := append([]string{Window}, rep.Days...)
	for i, day := range sections {
		title := day
		if day == Window {
			title = fmt.Sprintf("%s to %s (%s)", rep.From, rep.To, rep.Zone)
		}
		owners := ownersOn(rep, day)
		if f == Markdown {
			fmt.Fprintf(&b, "### %s\n\n", title)
			fmt.Fprintf(&b, "| measure | %s |\n|---|%s\n", strings.Join(append([]string{AllOwners}, owners...), " | "), strings.Repeat("---|", len(owners)+1))
			for _, s := range rep.Series {
				cells := []string{cellText(s.Agg, s.Cells[day][AllOwners])}
				for _, o := range owners {
					cells = append(cells, cellText(s.Agg, s.Cells[day][o]))
				}
				fmt.Fprintf(&b, "| %s | %s |\n", seriesName(s), strings.Join(cells, " | "))
			}
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, "%s\n", title)
		tw := tabwriter.NewWriter(&b, 2, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "\t%s\n", strings.Join(append([]string{AllOwners}, owners...), "\t"))
		for _, s := range rep.Series {
			cells := []string{cellText(s.Agg, s.Cells[day][AllOwners])}
			for _, o := range owners {
				cells = append(cells, cellText(s.Agg, s.Cells[day][o]))
			}
			fmt.Fprintf(tw, "%s\t%s\n", seriesName(s), strings.Join(cells, "\t"))
		}
		tw.Flush()
		if i < len(sections)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ownersOn lists the owners that have any observation on day (a stable,
// small column set), sorted.
func ownersOn(rep Report, day string) []string {
	var out []string
	for _, o := range rep.Owners {
		for _, s := range rep.Series {
			if s.Cells[day][o].N > 0 {
				out = append(out, o)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

func seriesName(s Series) string {
	if s.Group == "" {
		return s.Title
	}
	return s.Title + " [" + s.Group + "]"
}

func cellText(a Agg, c Cell) string {
	switch a {
	case Rate:
		if c.N == 0 {
			return "-"
		}
		return fmt.Sprintf("%d/%d (%d%%)", c.Hits, c.N, (c.Hits*100+c.N/2)/c.N)
	case Median:
		if c.N == 0 {
			return "-"
		}
		return (time.Duration(c.Value) * time.Second).Round(time.Second).String()
	}
	return fmt.Sprint(c.N)
}
