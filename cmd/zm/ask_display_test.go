package main

import (
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// A joined ask prints as one card, under the ask it joined.
func TestAskInboxJoinedIsOneCard(t *testing.T) {
	head := orchapi.Ask{ID: "A1", Status: orchapi.AskOpen, Card: &orchapi.AskCard{Title: "Keep the notes?"}, Subject: orchapi.AskSubject{Work: "bd:zm-1"}}
	join := orchapi.Ask{ID: "B2", Status: orchapi.AskOpen, SameAs: "A1", Card: &orchapi.AskCard{Title: "Keep the notes?"}, Subject: orchapi.AskSubject{Work: "bd:zm-2"}}
	orphan := orchapi.Ask{ID: "C3", Status: orchapi.AskOpen, SameAs: "GONE", Body: "Another?"}
	in := orchapi.Inbox{Decisions: []orchapi.InboxItem{
		{Kind: "ask", Priority: 2, ProjectSlug: "zm", Ask: &join},
		{Kind: "ask", Priority: 2, ProjectSlug: "zm", Ask: &head},
		{Kind: "ask", Priority: 2, ProjectSlug: "zm", Ask: &orphan},
	}}
	got := askInboxText(in)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "A1") || !strings.Contains(lines[1], "joined") ||
		!strings.Contains(lines[1], "B2") || !strings.Contains(lines[1], "bd:zm-2") || !strings.Contains(lines[2], "C3") {
		t.Fatalf("inbox:\n%s", got)
	}
	if strings.Count(got, "Keep the notes?") != 1 {
		t.Fatalf("the card printed twice:\n%s", got)
	}
}

// Zm ask ls shows a withdrawn ask as withdrawn, not open.
func TestAskStateWithdrawn(t *testing.T) {
	if got := askState(orchapi.Ask{Status: orchapi.AskWithdrawn}); got != "withdrawn" {
		t.Fatalf("withdrawn ask: %q", got)
	}
	if got := askState(orchapi.Ask{}); got != "open" {
		t.Fatalf("no status: %q", got)
	}
	if got := askState(orchapi.Ask{Status: orchapi.AskRecorded, Decision: &orchapi.Decision{Decision: "a"}}); got != "a" {
		t.Fatalf("decided: %q", got)
	}
}

// A card the judge could not check is marked held in the
// inbox, and a released one names who released it.
func TestAskInboxMarksUncheckedCards(t *testing.T) {
	held := orchapi.Ask{ID: "H1", Status: orchapi.AskOpen, Card: &orchapi.AskCard{Title: "Ship it?"}, NotChecked: true, NotCheckedReason: "model over its limit"}
	released := orchapi.Ask{ID: "R2", Status: orchapi.AskOpen, Card: &orchapi.AskCard{Title: "Keep it?"}, ReleasedUnchecked: "ana-phone"}
	plain := orchapi.Ask{ID: "P3", Status: orchapi.AskOpen, Card: &orchapi.AskCard{Title: "Move it?"}}
	got := askInboxText(orchapi.Inbox{Decisions: []orchapi.InboxItem{
		{Kind: "ask", Priority: 1, ProjectSlug: "proj", Ask: &held},
		{Kind: "ask", Priority: 2, ProjectSlug: "proj", Ask: &released},
		{Kind: "ask", Priority: 2, ProjectSlug: "proj", Ask: &plain},
	}})
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 5 || !strings.Contains(lines[1], "NOT CHECKED") || !strings.Contains(lines[1], "model over its limit") ||
		!strings.Contains(lines[3], "released by ana-phone") || !strings.Contains(lines[4], "P3") {
		t.Fatalf("inbox:\n%s", got)
	}
}
