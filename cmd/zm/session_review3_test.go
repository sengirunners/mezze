package main

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"strings"
	"testing"
	"time"
)

func TestReview3SessionTableCleansLegacyAndPresence(t *testing.T) {
	s := orchapi.Session{Title: "a\u200bb\u202e\U000e0061", Note: "n\u200bo", Agent: "claude", State: orchapi.StateRunning}
	now := time.Now()
	remote := s
	remote.Seen = &now
	got := sessionTable([]orchapi.Session{s, remote}, nil)
	if strings.ContainsAny(got, "\u200b\u202e\U000e0061") || strings.Count(got, "ab") != 2 || !strings.Contains(got, "[presence") {
		t.Fatalf("session table %q", got)
	}
}
