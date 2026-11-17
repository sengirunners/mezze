package summaries

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
)

const sourceLimit = 1000

func configVersion(c orchapi.SummaryConfig) string {
	c.Version = ""
	c.LastAttempt, c.LastGenerated, c.LastError = nil, nil, ""
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validateConfig(c *orchapi.SummaryConfig) error {
	if c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 || len(c.Timezone) > 100 {
		return api.Errorf(api.CodeBadRequest, "hour must be 0–23 and minute 0–59")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return api.Errorf(api.CodeBadRequest, "timezone must name an installed IANA timezone")
	}
	c.Version = configVersion(*c)
	return nil
}

func (s *Service) generate(ctx context.Context, sub *authz.Subject, c orchapi.SummaryConfig, day string) (orchapi.ProjectSummary, error) {
	if !s.mu.TryLock() {
		return orchapi.ProjectSummary{}, api.Errorf(api.CodeUnavailable, "another summary operation is running; retry later")
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return orchapi.ProjectSummary{}, err
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return orchapi.ProjectSummary{}, err
	}
	start, err := time.ParseInLocation(time.DateOnly, day, loc)
	today := s.now().In(loc)
	localToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	if err != nil || start.After(today) || start.Before(localToday.AddDate(-1, 0, 0)) {
		return orchapi.ProjectSummary{}, api.Errorf(api.CodeBadRequest, "day must be an ISO date in the last year")
	}
	if out, e := s.Get(ctx, c.Project, day, c.Version); e == nil {
		return out, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return orchapi.ProjectSummary{}, e
	}
	reader, ok := orchcore.Work(s.d).(orchcore.WorkReader)
	if !ok {
		return orchapi.ProjectSummary{}, api.Errorf(api.CodeUnavailable, "work summaries are unavailable on this daemon")
	}
	out := orchapi.ProjectSummary{ID: ids.New(), Project: c.Project, Day: day, Timezone: c.Timezone, ConfigVersion: c.Version, Generated: s.now().UTC(), Counts: map[orchapi.WorkStatus]int{}, Notes: []string{}}
	var body strings.Builder
	var bindings []titleBinding
	fmt.Fprintf(&body, "Project work summary · %s (%s)\n\n", day, c.Timezone)
	fmt.Fprintln(&body, "Current work snapshot; done entries below were updated during this local day. Counts are sampled if coverage is partial. No mail, comments or transcripts are included; machine notices appear as counts only.")
	notes := map[string]bool{}
	for _, status := range []orchapi.WorkStatus{orchapi.WorkDone, orchapi.WorkInReview, orchapi.WorkClaimed, orchapi.WorkOpen} {
		list, e := reader.ReadWork(ctx, sub, orchapi.WorkQuery{Project: c.Project, Status: status, Limit: sourceLimit})
		if e != nil {
			return orchapi.ProjectSummary{}, api.Errorf(api.CodeUnavailable, "project work could not be read; no summary saved")
		}
		if len(list.Items) >= sourceLimit {
			out.Partial = true
			notes["A work status reached the 1,000-item read bound; counts and titles are incomplete."] = true
		}
		for range list.Skipped {
			out.Partial = true
			notes["A work source was unavailable; its work is missing."] = true
		}
		for range list.Behind {
			out.Partial = true
			notes["A work source was loading or stale; its work is last known data."] = true
		}
		var items []orchapi.WorkItem
		for _, item := range list.Items {
			if item.Project != c.Project {
				return orchapi.ProjectSummary{}, api.Errorf(api.CodeUnavailable, "work returned a different project")
			}
			if status == orchapi.WorkDone && (item.Updated.Before(start) || !item.Updated.Before(start.AddDate(0, 0, 1))) {
				continue
			}
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool {
			if !items[i].Updated.Equal(items[j].Updated) {
				return items[i].Updated.After(items[j].Updated)
			}
			return items[i].ID < items[j].ID
		})
		out.Counts[status] = len(items)
		fmt.Fprintf(&body, "\n%s: %d\n", status, len(items))
		for i, item := range items {
			if i >= 40 {
				fmt.Fprintf(&body, "… %d more titles omitted.\n", len(items)-i)
				break
			}
			if utf8.RuneCountInString(item.Title) > 200 {
				out.Partial = true
				notes["Some work titles were shortened to 200 characters."] = true
			}
			line := fmt.Sprintf("• %s — %s", plain(item.ID, 100), plain(item.Title, 200))
			bindings = append(bindings, bindTitle(strings.Count(body.String(), "\n"), item.ID, line))
			fmt.Fprintln(&body, line)
		}
	}
	// Machine notices folded in instead of anyone's inbox: counts
	// by label from the modules that hold them, never a body or an address.
	counted, cerr := orchcore.NoticeCounts(s.d, ctx, c.Project, start, start.AddDate(0, 0, 1))
	if cerr != nil {
		out.Partial = true
		notes["A machine notice count was unavailable; its notices are missing."] = true
	}
	if len(counted) > 0 {
		out.Notices = counted
		labels := make([]string, 0, len(counted))
		total := 0
		for label, n := range counted {
			labels = append(labels, label)
			total += n
		}
		sort.Strings(labels)
		fmt.Fprintf(&body, "\nmachine notices (in no one's inbox): %d\n", total)
		for _, label := range labels {
			fmt.Fprintf(&body, "• %s: %d\n", plain(label, 100), counted[label])
		}
	}
	for note := range notes {
		out.Notes = append(out.Notes, note)
	}
	sort.Strings(out.Notes)
	if out.Partial {
		body.WriteString("\nCoverage is partial:\n")
		for _, note := range out.Notes {
			fmt.Fprintf(&body, "• %s\n", note)
		}
	}
	fmt.Fprintf(&body, "\nRead at %s. This is a work digest, not evidence of deployment, human approval or message delivery.\n", out.Generated.Format(time.RFC3339))
	out.Body = body.String()
	if len(out.Body) > 64<<10 {
		const note = "Further titles were omitted to keep the digest within 64 KiB."
		cut := strings.LastIndex(out.Body[:(64<<10)-len(note)-2], "\n")
		if cut < 0 {
			cut = 0
		}
		out.Body = out.Body[:cut] + "\n" + note + "\n"
		out.Partial = true
		out.Notes = append(out.Notes, note)
	}
	saved, err := s.putArchive(ctx, out, bindings)
	if err == nil && saved.ID == out.ID && s.d.Events != nil {
		// The archive is authoritative; this content-free hint lets readers
		// reload it. It is not a human delivery/read receipt.
		if _, e := s.d.Events.Publish(ctx, events.Input{Type: orchapi.EventSummaryCreated, Project: out.Project, Data: orchapi.SummaryCreated{ID: out.ID, Day: out.Day}}); e != nil && s.d.Log != nil {
			s.d.Log.Warn("summary refresh hint unavailable", "project", out.Project)
		}
	}
	return saved, err
}

func plain(s string, max int) string {
	r := make([]rune, 0, max)
	for _, c := range s {
		if len(r) == max {
			return string(append(r, '…'))
		}
		if unicode.IsControl(c) || unicode.In(c, unicode.Cf) {
			c = ' '
		}
		r = append(r, c)
	}
	return string(r)
}
