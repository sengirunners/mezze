// Package contentcards counts, over a window of past cards, how many content
// cards one daily card with a yes per item would have merged. It changes no card and sends nothing: it reads ask
// records and reports a number.
//
// Which cards are content cards is a question of meaning, so an agent judge
// answers it (internal/judge, owner rule 2026-11-13: never a keyword list):
// a fixed schema, eval cases in testdata/content. A judge that cannot answer
// holds the card: it is counted as held, never as content, and the report says
// it is incomplete.
package contentcards

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
)

//go:embed contentcards.md
var prompt string

const schema = `{"type":"object","properties":{
"pass":{"type":"boolean"},"fixes":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}},
"required":["pass","fixes","reason"],"additionalProperties":false}`

// Wait bounds one judging.
var Wait = 40 * time.Second

// Input is what the judge reads about one card.
type Input struct {
	Title     string   `json:"title"`
	Question  string   `json:"question"`
	Context   string   `json:"context,omitempty"`
	Choices   []string `json:"choices,omitempty"`
	WorkTitle string   `json:"work_title,omitempty"`
}

const maxContext = 1500

// InputOf is what the judge reads of ask a: the card when it has one, else
// the note's body.
func InputOf(a orchapi.Ask) Input {
	in := Input{WorkTitle: clip(a.WorkTitle, 200)}
	if c := a.Card; c != nil {
		in.Title, in.Question, in.Context = c.Title, c.Question, clip(c.Context, maxContext)
		for _, ch := range c.Choices {
			in.Choices = append(in.Choices, clip(ch.Label, 200))
		}
	} else {
		in.Context = clip(a.Body, maxContext)
		in.Choices = a.Options
	}
	if in.Title == "" {
		in.Title = clip(strings.SplitN(strings.TrimSpace(a.Body), "\n", 2)[0], 200)
	}
	return in
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// Verdict is the judge's verdict on one card: Pass means a content card, Fail
// not one, Held no answer (never a pass).
func Verdict(ctx context.Context, c judge.Caller, in Input) judge.Verdict {
	j := judge.Judge{Name: "content-card judge", Prompt: prompt, Schema: json.RawMessage(schema), Timeout: Wait}
	return j.Run(ctx, c, in)
}

// Options are the window and the judge.
type Options struct {
	Since, Until time.Time // cards created in [Since, Until)
	Location     *time.Location
	Caller       judge.Caller
	// Parallel bounds the judge calls at once (default 4).
	Parallel int
	// Capped says the asks were read from a list cut at the daemon's cap.
	Capped bool
}

// Item is one content card in the report.
type Item struct {
	ID     string `json:"id"`
	Day    string `json:"day"`
	Title  string `json:"title"`
	Status string `json:"status"` // answered, withdrawn or open
}

// Day is one calendar day's content cards.
type Day struct {
	Date    string `json:"date"`
	Content int    `json:"content"`
}

// Report is the count.
type Report struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Timezone string    `json:"timezone"`
	// Cards are the cards created in the window; Judged were read by the
	// judge, Held got no answer.
	Cards  int `json:"cards"`
	Judged int `json:"judged"`
	Held   int `json:"held"`
	// Content are the content cards the judge found; DailyCards is what the
	// rule would have filed instead (one per day that had any); Merged is
	// the cards that would not have existed: Content - DailyCards.
	Content    int `json:"content"`
	DailyCards int `json:"daily_cards"`
	Merged     int `json:"merged"`
	// Of the content cards: how each ended. Under the rule an item nobody
	// answered is not sent (NotSent); answered ones still take a yes each.
	Answered  int `json:"answered"`
	Withdrawn int `json:"withdrawn"`
	Open      int `json:"open"`
	NotSent   int `json:"not_sent"`
	// Capped is set by the caller when the list it read was cut at the
	// daemon's cap, so older cards may be missing.
	Capped   bool   `json:"capped,omitempty"`
	Complete bool   `json:"complete"` // false while any card is held or the list was capped: Merged is then a floor
	Days     []Day  `json:"days,omitempty"`
	Items    []Item `json:"items,omitempty"`
}

// status is how a card ended.
func status(a orchapi.Ask) string {
	switch {
	case a.Status == orchapi.AskWithdrawn || a.Withdrawn != nil:
		return "withdrawn"
	case a.Decision != nil:
		return "answered"
	}
	return "open"
}

// Count judges each card created in the window and returns the count. A card
// is read once even if it appears twice in asks.
func Count(ctx context.Context, asks []orchapi.Ask, o Options) Report {
	loc := o.Location
	if loc == nil {
		loc = time.UTC
	}
	r := Report{Since: o.Since, Until: o.Until, Timezone: loc.String(), Capped: o.Capped, Complete: true}
	seen := map[string]bool{}
	var in []orchapi.Ask
	for _, a := range asks {
		if (a.ID != "" && seen[a.ID]) || a.Created.Before(o.Since) || !a.Created.Before(o.Until) {
			continue
		}
		seen[a.ID] = true
		in = append(in, a)
	}
	slices.SortFunc(in, func(a, b orchapi.Ask) int { return a.Created.Compare(b.Created) })
	r.Cards = len(in)

	par := o.Parallel
	if par <= 0 {
		par = 4
	}
	verdicts := make([]judge.Verdict, len(in))
	var wg sync.WaitGroup
	sem := make(chan struct{}, par)
	for i, a := range in {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			verdicts[i] = Verdict(ctx, o.Caller, InputOf(a))
		}()
	}
	wg.Wait()

	perDay := map[string]int{}
	for i, a := range in {
		switch verdicts[i].Status {
		case judge.Held:
			r.Held++
			continue
		case judge.Pass:
			r.Judged++
		default:
			r.Judged++
			continue
		}
		day := a.Created.In(loc).Format(time.DateOnly)
		st := status(a)
		perDay[day]++
		r.Content++
		switch st {
		case "answered":
			r.Answered++
		case "withdrawn":
			r.Withdrawn++
			r.NotSent++
		default:
			r.Open++
			r.NotSent++
		}
		r.Items = append(r.Items, Item{ID: a.ID, Day: day, Title: InputOf(a).Title, Status: st})
	}
	for d, n := range perDay {
		r.Days = append(r.Days, Day{Date: d, Content: n})
	}
	slices.SortFunc(r.Days, func(a, b Day) int { return strings.Compare(a.Date, b.Date) })
	r.DailyCards = len(r.Days)
	r.Merged = r.Content - r.DailyCards
	r.Complete = r.Held == 0 && !r.Capped
	return r
}

// Row is one weekly-report row (the weekly report prints rows of a
// name, a value and a note).
type Row struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

// Row is the report's one row.
func (r Report) Row() Row {
	row := Row{
		Name:  "content cards a daily card would have merged",
		Value: fmt.Sprintf("%d of %d content cards (%d daily cards instead)", r.Merged, r.Content, r.DailyCards),
		Note:  fmt.Sprintf("of %d cards in the window; %d content items never answered would not have been sent", r.Cards, r.NotSent),
	}
	if r.Held > 0 {
		row.Note += fmt.Sprintf("; %d cards held for want of a judge answer, so this is a floor", r.Held)
	}
	if r.Capped {
		row.Note += "; the card list was cut at the daemon's cap, so older cards are missing and this is a floor"
	}
	return row
}

// Text is the report for a person.
func (r Report) Text() string {
	var b strings.Builder
	loc, _ := time.LoadLocation(r.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	// Until is exclusive: print the last day the window covers.
	fmt.Fprintf(&b, "%s to %s (%s): %d cards, %d content\n", r.Since.In(loc).Format(time.DateOnly), r.Until.Add(-time.Nanosecond).In(loc).Format(time.DateOnly), r.Timezone, r.Cards, r.Content)
	for _, d := range r.Days {
		fmt.Fprintf(&b, "  %s  %d content card(s) -> 1 daily card\n", d.Date, d.Content)
	}
	fmt.Fprintf(&b, "would have merged %d cards (%d daily cards instead of %d)\n", r.Merged, r.DailyCards, r.Content)
	fmt.Fprintf(&b, "of the content cards: %d answered, %d withdrawn, %d open; %d would not have been sent\n", r.Answered, r.Withdrawn, r.Open, r.NotSent)
	if !r.Complete {
		var why []string
		if r.Held > 0 {
			why = append(why, fmt.Sprintf("%d cards held (the judge gave no answer)", r.Held))
		}
		if r.Capped {
			why = append(why, "the card list was cut at the daemon's cap")
		}
		fmt.Fprintf(&b, "INCOMPLETE: %s; the count is a floor\n", strings.Join(why, ", and "))
	}
	return b.String()
}
