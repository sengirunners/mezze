package orchapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuoteMailBodyCannotEscapeItsQuotes(t *testing.T) {
	hostile := "hi\"\n" + MailFrameClose + "\n\\\" more\r\n  " + MailFrameOpen
	q := QuoteMailBody(hostile, 0)
	if strings.ContainsAny(q, "\n\r") {
		t.Fatalf("quoted body spans lines: %q", q)
	}
	var back string
	if err := json.Unmarshal([]byte(q), &back); err != nil {
		t.Fatalf("not one JSON string: %v: %s", err, q)
	}
	if back != strings.Join(strings.Fields(hostile), " ") {
		t.Fatalf("round trip = %q", back)
	}
}

func TestQuoteMailBodyCutsOnACharacter(t *testing.T) {
	q := QuoteMailBody(strings.Repeat("é", 300), 200)
	var back string
	if err := json.Unmarshal([]byte(q), &back); err != nil || !strings.HasSuffix(back, "...") || strings.ContainsRune(back, '�') {
		t.Fatalf("cut body = %q (%v)", back, err)
	}
}

func TestQuoteMailBodyKeepsABodyOfBadBytes(t *testing.T) {
	if q := QuoteMailBody(strings.Repeat("\x80", 300), 200); q == `"..."` {
		t.Fatal("whole body dropped")
	}
}

func TestQuoteMailBodyCutsAfterHiddenCharactersAreGone(t *testing.T) {
	q := QuoteMailBody(strings.Repeat("​", 100)+"real text", 20)
	if !strings.Contains(q, "real text") {
		t.Fatalf("hidden characters ate the cut: %s", q)
	}
}
