package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc := time.FixedZone("test", 3600)
	d, err := parseWhen("2030-01-05", loc)
	if err != nil || !d.Equal(time.Date(2030, 1, 5, 0, 0, 0, 0, loc)) {
		t.Fatalf("date: %v %v", d, err)
	}
	if r, err := parseWhen("2030-01-05T10:00:00Z", loc); err != nil || r.Hour() != 10 {
		t.Fatalf("rfc3339: %v %v", r, err)
	}
	if _, err := parseWhen("last tuesday", loc); err == nil {
		t.Fatal("a phrase is not a date")
	}
}

func TestReadAsksTakesAListOrAnAskList(t *testing.T) {
	for _, in := range []string{`[{"id":"a1"}]`, `{"asks":[{"id":"a1"}]}`} {
		as, err := readAsks(strings.NewReader(in))
		if err != nil || len(as) != 1 || as[0].ID != "a1" {
			t.Fatalf("%s: %v %v", in, as, err)
		}
	}
	if _, err := readAsks(strings.NewReader(`"x"`)); err == nil {
		t.Fatal("a string is not a list of asks")
	}
}

// Tests judge nothing for real: a test binary's judge is a fake that passes
// every card, so each card in the file counts as content.
func TestAskContentCountFromFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "asks.json")
	body := `[{"id":"a1","created":"2030-01-02T09:00:00Z","card":{"title":"One","question":"q"}},
{"id":"a2","created":"2030-01-02T10:00:00Z","card":{"title":"Two","question":"q"}},
{"id":"a3","created":"2030-01-03T10:00:00Z","card":{"title":"Three","question":"q"}}]`
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := askContentCountCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--from", f, "--since", "2030-01-01", "--until", "2030-01-08"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would have merged 1 cards (2 daily cards instead of 3)") {
		t.Fatalf("output:\n%s", out.String())
	}
	cmd = askContentCountCmd()
	cmd.SetArgs([]string{"--from", f, "--tz", "Not/AZone"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--tz") {
		t.Fatalf("a bad zone: %v", err)
	}
}

func TestAskContentCountWindowIsWholeDaysEndingToday(t *testing.T) {
	f := filepath.Join(t.TempDir(), "asks.json")
	// A card today and one eight days ago are outside the default window of
	// the last seven whole days; one three days ago is inside.
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, time.UTC)
	mk := func(id string, at time.Time) string {
		return `{"id":"` + id + `","created":"` + at.Format(time.RFC3339) + `","card":{"title":"t","question":"q"}}`
	}
	body := "[" + mk("today", today) + "," + mk("mid", today.AddDate(0, 0, -3)) + "," + mk("old", today.AddDate(0, 0, -8)) + "]"
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := askContentCountCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--from", f})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ": 1 cards, 1 content") {
		t.Fatalf("only the card three days ago is in the window:\n%s", out.String())
	}
	cmd = askContentCountCmd()
	cmd.SetArgs([]string{"--from", f, "--days", "0"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("--days 0 was accepted")
	}
}
