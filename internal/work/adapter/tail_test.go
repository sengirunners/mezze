package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

const tailSecret = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"

// leaks reports a piece of tailSecret (8 bytes or more) in msg.
func leaks(msg string) string {
	for i := 0; i+8 <= len(tailSecret); i++ {
		if w := tailSecret[i : i+8]; strings.Contains(msg, w) {
			return w
		}
	}
	return ""
}

// straddling is stderr whose last TailMax bytes start inside tailSecret.
func straddling() string {
	end := " retrying\nordinary error text: tracker said no"
	pad := TailMax - (len(tailSecret) - 20) - len(end)
	return "auth: token " + tailSecret + strings.Repeat(".", pad) + end
}

// An adapter error never quotes a piece of a secret cut by the tail, known
// to zm (WithSecrets) or not; the rest of the error still shows.
func TestErrTailCutSecret(t *testing.T) {
	old := RestartBackoff
	RestartBackoff = time.Hour
	defer func() { RestartBackoff = old }()
	cases := map[string]struct {
		stderr string
		want   string
	}{
		"tail cut":     {straddling(), "ordinary error text: tracker said no"},
		"buffer cut":   {"boom: tracker said no " + strings.Repeat("y", 64<<10-len(tailSecret)/2-23) + " " + tailSecret, "boom"},
		"short stderr": {"tracker said no: token " + tailSecret + " rejected", "rejected"},
	}
	for name, c := range cases {
		f := filepath.Join(t.TempDir(), "stderr")
		if err := os.WriteFile(f, []byte(c.stderr), 0o600); err != nil {
			t.Fatal(err)
		}
		oneShot := script(t, "zm-work-tail", `cat >/dev/null; cat '`+f+`' >&2; exit 3`)
		serve := script(t, "zm-work-tailserve", `cat '`+f+`' >&2; exit 3`)
		for _, known := range []bool{true, false} {
			if name != "tail cut" && name != "buffer cut" && !known {
				continue // a whole unknown token is not a cut piece
			}
			ctx := context.Background()
			if known {
				ctx = WithSecrets(ctx, []string{tailSecret})
			}
			pl := NewPool()
			for how, err := range map[string]error{
				"one-shot": CallEnv(ctx, oneShot, nil, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil),
				"serve":    pl.CallEnv(ctx, serve, nil, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil),
			} {
				if err == nil {
					t.Fatalf("%s %s: no error", name, how)
				}
				msg := err.Error()
				if w := leaks(msg); w != "" {
					t.Errorf("%s %s known=%v: %q of the secret shows: %s", name, how, known, w, msg)
				}
				if name != "buffer cut" && !strings.Contains(msg, c.want) {
					t.Errorf("%s %s known=%v: lost the error text: %s", name, how, known, msg)
				}
			}
			pl.Close()
		}
	}
}

func TestErrTailBounds(t *testing.T) {
	long := strings.Repeat("word ", 200)
	if got := errTail(long, TailMax, false, nil); len(got) > TailMax || !strings.HasPrefix(got, "word") {
		t.Fatalf("bound: %d %q", len(got), got)
	}
	if got := errTail(strings.Repeat("z", 400), TailMax, false, nil); got != "" {
		t.Fatalf("one cut token shows: %q", got)
	}
	if got := errTail("exit: bad config\n", TailMax, false, nil); got != "exit: bad config" {
		t.Fatalf("short: %q", got)
	}
}
