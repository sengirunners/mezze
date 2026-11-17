package session

import (
	"os"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// Keys that leave nothing behind do not hold the wake back: Enter, an answer
// to a hook-set permission prompt. Uncorrelated hooks never clear drafts.
func TestDraftClears(t *testing.T) {
	for _, tc := range []struct {
		keys  string
		draft bool
	}{
		{"hello", true},
		{"hello\r", false},
		{"\r", false},
		{"hello\\\r", true},   // backslash-Enter: a new line in the same prompt
		{"hello\x1b\r", true}, // alt-Enter: the same
		{"hello\n", true},     // ctrl-J: the same
		{"\x1b[13;2u", true},  // shift-Enter
		{"\x1b[A", true},      // history: a line may have come back into the prompt
		{"\x03", true},        // no way to tell what ctrl-C left
	} {
		p := &proc{conns: nil}
		p.noteKeys([]byte(tc.keys))
		if p.draft != tc.draft {
			t.Errorf("keys %q: draft %v, want %v", tc.keys, p.draft, tc.draft)
		}
	}

	// A chunk boundary between the backslash and the Enter changes nothing.
	p := &proc{}
	p.noteKeys([]byte(`hello\`))
	p.noteKeys([]byte("\r"))
	if !p.draft {
		t.Error("backslash, then Enter in the next chunk: not a draft")
	}
	p.noteKeys([]byte("\r"))
	if p.draft {
		t.Error("Enter on the continued line: still a draft")
	}

	// Answering a hook's permission prompt leaves nothing in the prompt.
	p = &proc{hookHuman: true}
	p.noteKeys([]byte("1"))
	if p.draft {
		t.Error("an answer to a permission prompt counted as a draft")
	}
}

// Draft parser context, not just the draft bit, must survive adoption.
func TestDraftContinuationSurvivesAdoption(t *testing.T) {
	for _, prefix := range []string{"\\", "\x1b", "plain"} {
		t.Run(prefix, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(holder.Dir(home), 0o700); err != nil {
				t.Fatal(err)
			}
			s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}}
			p := &proc{id8: "aaaaaaaa"}
			s.loadDraft(p)
			// Change the last key while draft already is true too.
			p.noteKeys([]byte("draft"))
			p.noteKeys([]byte(prefix))
			adopted := &proc{id8: p.id8}
			s.loadDraft(adopted)
			if !adopted.draft || adopted.lastKey != prefix[len(prefix)-1] {
				t.Fatalf("adopted draft/context: %v %q", adopted.draft, adopted.lastKey)
			}
			adopted.noteKeys([]byte{'\r'})
			want := prefix != "plain"
			if adopted.draft != want {
				t.Fatalf("Enter after restart: draft=%v want=%v", adopted.draft, want)
			}
		})
	}
}

func TestLegacyDraftRemainsConservativeAfterAdoption(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(holder.Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}}
	p := &proc{id8: "aaaaaaaa"}
	s.loadDraft(p)
	if err := os.WriteFile(p.draftFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.loadDraft(p)
	p.noteKeys([]byte{'\r'})
	if !p.draft {
		t.Fatal("legacy draft may end in a backslash; first Enter cannot prove submission")
	}
	p.noteKeys([]byte{'\r'})
	if p.draft {
		t.Fatal("second plain Enter should submit the continued line")
	}
}

// An existing marker that cannot be read still means a draft. A directory
// gives a portable read error, including when tests run as root.
func TestUnreadableDraftMarkerRemainsConservative(t *testing.T) {
	home := t.TempDir()
	s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}}
	p := &proc{id8: "aaaaaaaa"}
	if err := os.MkdirAll(holder.DraftPath(home, p.id8), 0o700); err != nil {
		t.Fatal(err)
	}
	s.loadDraft(p)
	if !p.draft {
		t.Fatal("existing unreadable marker lost draft protection")
	}
	p.noteKeys([]byte{'\r'})
	if !p.draft {
		t.Fatal("unknown parser context cannot prove Enter submitted")
	}
}

// Absent, legacy-empty, malformed and unknown records cannot prove empty
// input, including when another daemon repeats adoption before any new keys.
func TestAdoptedDraftRequiresVersionedEmptyProof(t *testing.T) {
	for _, data := range [][]byte{nil, {}, []byte("corrupt"), {'z', 'd', 2, 'e', 0}, {'z', 'd', 1, '?', 0}} {
		home := t.TempDir()
		if err := os.MkdirAll(holder.Dir(home), 0o700); err != nil {
			t.Fatal(err)
		}
		s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}}
		if data != nil {
			if err := os.WriteFile(holder.DraftPath(home, "aaaaaaaa"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			p := &proc{id8: "aaaaaaaa", state: orchapi.StateIdle}
			s.loadDraft(p)
			if !p.draft || p.draftKnown || p.mayNudge() {
				t.Fatalf("record %q adoption%d established empty input", data, i)
			}
		}
	}
}

func TestKnownEmptyDraftProofSurvivesAdoption(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(holder.Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}}
	p := &proc{id8: "aaaaaaaa", state: orchapi.StateIdle}
	if err := s.initDraft(p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		adopted := &proc{id8: p.id8, state: orchapi.StateIdle}
		s.loadDraft(adopted)
		if !adopted.mayNudge() {
			t.Fatal("known empty input did not survive adoption")
		}
	}
	if err := p.noteKeys([]byte("draft")); err != nil {
		t.Fatal(err)
	}
	if err := p.noteKeys([]byte{'\r'}); err != nil {
		t.Fatal(err)
	}
	adopted := &proc{id8: p.id8, state: orchapi.StateIdle}
	s.loadDraft(adopted)
	if !adopted.mayNudge() {
		t.Fatal("ordered plain submission did not persist empty proof")
	}
}

func TestCorruptEmptyDraftProofRemainsUnknown(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(holder.Dir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Config: &config.Config{Home: home}}}
	p := &proc{id8: "aaaaaaaa"}
	if err := s.initDraft(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.draftFile)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err := os.WriteFile(p.draftFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	adopted := &proc{id8: p.id8, state: orchapi.StateIdle}
	s.loadDraft(adopted)
	if adopted.mayNudge() || !adopted.draft || adopted.draftKnown {
		t.Fatal("corrupt empty proof became wakeable")
	}
}
