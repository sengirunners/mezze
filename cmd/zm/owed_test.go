package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func TestOwedTextListsItemsAndEveryUnknown(t *testing.T) {
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	out := owedText(orchapi.OwedList{
		For: orchapi.OwedForSession, Subject: "box/aaaa1111", Sessions: []string{"box/aaaa1111"},
		Items: []orchapi.OwedItem{
			{Kind: orchapi.OwedPR, Ref: "https://example.test/o/r/pull/3", Title: "make it so", State: "open, linked to local:x"},
			{Kind: orchapi.OwedAskOpen, Ref: "01ABC", Title: "ship it?", State: "waiting for an answer", Since: &since},
		},
		Unknown: []orchapi.OwedGap{{Kind: orchapi.OwedGapUnseen, Name: "cloud_tasks", Why: "zm does not record whether one still runs"}},
	})
	for _, want := range []string{"owed by session box/aaaa1111 (1 sessions)", "pr            https://example.test/o/r/pull/3  make it so [open, linked to local:x]",
		"ask_open      01ABC  ship it? [waiting for an answer] since ", "unknown       cloud_tasks: zm does not record whether one still runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOwedTextSaysNothingIsOwedOnlyOfWhatItCanSee(t *testing.T) {
	out := owedText(orchapi.OwedList{For: orchapi.OwedForPerson, Subject: "ada"})
	if !strings.Contains(out, "nothing owed that zm can see") || strings.Contains(out, "all clear") {
		t.Fatalf("empty list:\n%s", out)
	}
}
