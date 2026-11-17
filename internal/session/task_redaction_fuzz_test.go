package session

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestTaskRedactionVerifierRejectsMissedEscape(t *testing.T) {
	input := `stray " quote` + "\n" + `12 private s\u0065cret marker`
	safe, err := taskRedactionSafe(input, []string{"private secret marker"}, 0)
	if err != nil || safe {
		t.Fatalf("missed escaped secret: safe=%v err=%v", safe, err)
	}
}

func TestTaskRedactionUnicodeAndSurrogates(t *testing.T) {
	secret := "piñata 🔥"
	for _, input := range []string{"before " + secret + " after", `before pi\u00f1ata \ud83d\udd25 after`} {
		safe, err := taskRedactionSafe(input, []string{secret}, 0)
		if err != nil || safe {
			t.Fatalf("verifier missed %q: %v", input, err)
		}
		cleaned, err := scrubTaskBytes(input, []string{secret}, 0)
		if err != nil {
			t.Fatal(err)
		}
		safe, err = taskRedactionSafe(cleaned, []string{secret}, 0)
		if err != nil || !safe {
			t.Fatalf("scrub leaked %q: %v", cleaned, err)
		}
	}
}

func TestTaskRedactionTerminalRendering(t *testing.T) {
	const secret = "private secret marker"
	controls := []string{
		"\x1b[1m", "\x1b[38:2::1:2:3m", "\x1b[12;1H", "\x1b[2K\r",
		"\n", "\r\n", "\r\r\n", "\x1bE", "\x1b(B",
		"\x1b]0;title\a", "\x1b]8;;https://example.test\x1b\\",
		"\x1bPignored\x1b\\", "\x1b_ignored\x1b\\",
		`\u001b[1m`, `\\u001b[1m`, `\r\r\n`,
	}
	for _, control := range controls {
		for split := 1; split < len(secret); split++ {
			input := "before:" + secret[:split] + control + secret[split:] + ":after"
			safe, err := taskRedactionSafe(input, []string{secret}, 0)
			if err != nil || safe {
				t.Fatalf("verifier missed split %d by %q: safe=%v err=%v", split, control, safe, err)
			}
			out, err := cleanSessionText(input, []string{secret}, 0)
			if err != nil || out != "before:[redacted]:after" {
				t.Fatalf("split %d by %q: out=%q err=%v", split, control, out, err)
			}
		}
	}
	// Checking only the rendered view would hide an escaped secret in a title.
	input := `\x1b]0;private s\u0065cret marker` + "\a"
	if safe, err := taskRedactionSafe(input, []string{secret}, 0); err != nil || safe {
		t.Fatalf("hidden control payload missed: safe=%v err=%v", safe, err)
	}
	if out, err := cleanSessionText(input, []string{secret}, 0); err != nil || out != `\x1b]0;[redacted]`+"\a" {
		t.Fatalf("hidden payload: %q %v", out, err)
	}
	input = "private \x1b[1msecret\r\n marker / " + secret
	if out, err := cleanSessionText(input, []string{secret}, 0); err != nil || out != "[redacted] / [redacted]" {
		t.Fatalf("multiple renderings: %q %v", out, err)
	}
	// Unrelated styling stays intact and the unscoped minimum uses original bytes.
	input = "public \x1b[1mtext\x1b[0m\r\n"
	if out, err := cleanSessionText(input, []string{secret}, 0); err != nil || out != input {
		t.Fatalf("unrelated output changed: %q %v", out, err)
	}
	if out, err := cleanSessionText("c\x1b[1mat\b", []string{"cat"}, 8); err != nil || out != "c\x1b[1mat\b" {
		t.Fatalf("short unscoped text changed: %q %v", out, err)
	}
}

func TestTaskRedactionAmbiguousTerminalFailsClosed(t *testing.T) {
	const secret = "private secret marker"
	for _, input := range []string{
		"private X\bsecret marker", "private \x1b[12;\nsecret marker",
		"private \x1b]unterminated secret marker", "private \x1bPunterminated secret marker",
	} {
		if safe, err := taskRedactionSafe(input, []string{secret}, 0); err == nil && safe {
			t.Fatalf("ambiguous rendering verified safe: %q", input)
		}
		// Either the whole span is redacted or the output is discarded.
		if out, err := cleanSessionText(input, []string{secret}, 0); err == nil && (strings.Contains(out, "private") || strings.Contains(out, "secret")) {
			t.Fatalf("ambiguous output retained: %q", out)
		}
	}
	// A needle with no rendered text is checked by its bytes alone.
	if out, err := cleanSessionText("unrelated", []string{"\r\n"}, 0); err != nil || out != "unrelated" {
		t.Fatalf("control-only needle: %q %v", out, err)
	}
}

const plantedSecret = "sk-live-PLANTED2secret0123456789abcdef"

// Screen edits that reorder or overwrite text: the stream shows the secret
// although no contiguous rendering exists, so output must not verify safe.
func TestTaskRedactionCursorMovesNeverVerifySafe(t *testing.T) {
	for _, input := range []string{
		"XXXXXXXXXXXXXXXXsecret0123456789abcdef\rsk-live-PLANTED2",
		"\x1b[1;17Hsecret0123456789abcdef\x1b[1;1Hsk-live-PLANTED2",
		"\x1b[1;17fsecret0123456789abcdef\x1b[1;1fsk-live-PLANTED2",
		"secret0123456789abcdef\x1b[38Dsk-live-PLANTED2",
		"secret0123456789abcdef\x1b[1Gsk-live-PLANTED2",
		"\x1b[2dsecret0123456789abcdef\x1b[1A\x1b[1Gsk-live-PLANTED2",
		"\u009b1;17Hsecret0123456789abcdef\u009b1;1Hsk-live-PLANTED2",
		"sk-live-PLANTED2X\bsecret0123456789abcdef",
	} {
		for _, minLen := range []int{0, 8} {
			if safe, err := taskRedactionSafe(input, []string{plantedSecret}, minLen); err == nil && safe {
				t.Fatalf("reordered rendering verified safe (minLen %d): %q", minLen, input)
			}
			if out, err := cleanSessionText(input, []string{plantedSecret}, minLen); err == nil {
				t.Fatalf("reordered rendering retained (minLen %d): %q", minLen, out)
			}
		}
	}
}

// Renderings that a TUI produces for one contiguous copy are scrubbed.
func TestTaskRedactionTUIRenderings(t *testing.T) {
	for _, c := range []struct{ secret, input, want string }{
		{plantedSecret, "sk-live-PLANTED2secret01234\r\n  56789abcdef", "[redacted]"},
		{plantedSecret, "\u2502 sk-live-PLANTED2secret01234 \u2502\r\n\u2502 56789abcdef \u2502", "\u2502 [redacted] \u2502"},
		{plantedSecret, "sk-live-PLANTED2\u009b1msecret0123456789abcdef", "[redacted]"},
		{plantedSecret, "sk-live-PLANTED2\x9b1msecret0123456789abcdef", "[redacted]"},
		{plantedSecret, "sk-live-PLANTED2\u009d0;t\u009csecret0123456789abcdef", "[redacted]"},
		{plantedSecret, "sk-live-PLANTED2secret01234\u008556789abcdef", "[redacted]"},
		{"correct horse battery staple", "correct\x1b[1Chorse battery staple", "[redacted]"},
		{"correct horse battery staple", "correct horse\r\nbattery staple", "[redacted]"},
	} {
		for _, minLen := range []int{0, 8} {
			if safe, err := taskRedactionSafe(c.input, []string{c.secret}, minLen); err != nil || safe {
				t.Fatalf("verifier missed %q: safe=%v err=%v", c.input, safe, err)
			}
			if out, err := cleanSessionText(c.input, []string{c.secret}, minLen); err != nil || out != c.want {
				t.Fatalf("%q: out=%q err=%v; want %q", c.input, out, err, c.want)
			}
		}
	}
}

// Terminal noise without any fragment of the text is verifiable: unrelated
// sessions are never held and their output is never discarded.
func TestTaskRedactionUnrelatedTerminalNoiseIsSafe(t *testing.T) {
	for _, input := range []string{
		`grep -P '\bfoo\b' main.go`, `fmt.Print("\x1b[1m")`, `const esc = "\x1b"`,
		"\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\", "ring tail \x1b[", "ring tail \x1b", "ring tail \u009b",
		"unrelated text\x1b[12;", "unrelated text\x1b]title\x1b", "unrelated \x1b]unterminated",
		"progress 45%\rprogress 46%", "\x1b[5;1Hdone\bX\x1b[2;3H\x1b7\x1b8", "abc\x9b\xff\xc2",
		// Pieces shorter than half the text do not count.
		"sk-live-\x1b[Hsecret01\b",
	} {
		safe, err := taskRedactionSafe(input, []string{plantedSecret}, 8)
		if err != nil || !safe {
			t.Fatalf("unrelated output not safe: %q safe=%v err=%v", input, safe, err)
		}
		if out, err := cleanSessionText(input, []string{plantedSecret}, 8); err != nil || out != input {
			t.Fatalf("unrelated output changed: %q -> %q %v", input, out, err)
		}
	}
}

// Secrets of one kind share a public prefix: every HS256 JWT starts with the
// same 36 bytes, every Slack webhook with a 32-byte URL, every OpenSSH key
// with a 35-byte header and a magic first line. Siblings share more: webhooks
// of one workspace (43 of 79 bytes), a JWT reissued to the same user (all but
// iat and signature), keys with an account prefix (21 of 40). Each case
// redacts one secret while a session shows another of the same kind beside a
// progress-bar CR or line erase: only the shared part shows, so it is not a
// rearranged copy.
var sharedPrefixCases = []struct{ name, secret, output string }{
	{"jwt",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		"Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJib3QtNDIiLCJyb2xlIjoiY2kifQ.Xk3m9QpL2vR8sT1uW6yZ0aB4cD7eF5gH3iJ2kL1mN0o\r\n\x1b[2K\rTests: 3 passed\r\n"},
	{"slack",
		"https://hooks.slack.com/services/T024BE7LD/B01J2K3L4M5/xoxbAbCdEfGhIjKlMnOpQrSt",
		"curl -d @msg.json https://hooks.slack.com/services/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\r\n" +
			"  0     0    0     0    0     0      0      0 --:--:-- --:--:-- --:--:--     0\r100    27  100     2  100    25     12    150 --:--:-- --:--:-- --:--:--   162\r\nok\r\n"},
	{"pem",
		"-----BEGIN OPENSSH PRIVATE KEY-----\n" + sshKeyMagic + "\n" +
			"QyNTUxOQAAACBq7Xh3kLmN2pQrS4tUvW5xYz6AbC7dEfG8hIjK9lMn0oPAAAAJgV2xYzAb\n" +
			"cDeFgHiJkLmNoPqRsTuVwXyZ0123456789AbCdEfGhIjKlMnOpQrStUvWxYz+/AbCdEfGh\n" +
			"IjKlMnOpQrStUvWxYz0123456789aBcDeFgHiJkLmNoPqRsTuVwXyZ+/0123456789AbCd\n" +
			"EfGhIjKlMnOpQrStUvWxYzAAAAEGRldkBleGFtcGxlLmNvbQECAwQFBgc=\n" +
			"-----END OPENSSH PRIVATE KEY-----\n",
		"$ head -2 ~/.ssh/other_ed25519\r\n-----BEGIN OPENSSH PRIVATE KEY-----\r\n" + sshKeyMagic + "\r\n\x1b[1A\x1b[2K$ "},
	{"slack-same-workspace",
		"https://hooks.slack.com/services/T01ABCDEF/B04NQ7XK2PL/Qm8vT3zR6yW1nE5sJ9kH2cFd",
		"curl -d @msg.json https://hooks.slack.com/services/T01ABCDEF/B07RT2MW9ZC/a4LpX8eG1uYb6NqD3vKs0HjT\r\n" +
			"  0     0    0     0\r100    27  100     2\r\nok\r\n"},
	{"jwt-reissued",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwicm9sZSI6ImFkbWluIiwiaWF0IjoxNTE2MjM5MDIyfQ.8vQe3Lr0TzKp6YmWn1JxB4sHc7GdUa2FiE5oN9tRqSk",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwicm9sZSI6ImFkbWluIiwiaWF0IjoxNzMxNDU2MDAwfQ.Zp2Wk7Nc4Xr9Bq1Lm6Ty3Hs8Ve0Dg5Ja2Fu7Ri4Oe1\r\n\x1b[2K\rTests: 3 passed\r\n"},
	{"key-shared-prefix",
		"sk_live_51HxQ2bCdEfGhK8mP3vR7tY1wZ4nQ6sL",
		"STRIPE_KEY=sk_live_51HxQ2bCdEfGhA2jN9cX5bV0uM7eW3rT\r\ndeploying 40%\rdeploying 100%\r\n"},
	// A 930-byte Supabase-style access token and its refresh: only iat, exp
	// and the signature differ, under 7% of the token.
	{"jwt-access-refresh",
		authJWT(1731456000, "RIgP_58waM-Dx3A5idNoDCDBwb2Dc4_dsdc6lC1MXlP"),
		"access ok\r\n" + authJWT(1731459000, "HLKuVngbEU3yv4iEDu7ow2VWedDWpWRuX51utu5Uz7f") + "\r\n\x1b[2K\rrefreshed\r\n"},
}

func authJWT(iat int, sig string) string {
	claims := fmt.Sprintf(`{"iss":"https://abcdefghijklmnop.supabase.co/auth/v1","sub":"5f0c3a1e-8b2d-4e6f-9a7c-1d2e3f4a5b6c",`+
		`"aud":"authenticated","exp":%d,"iat":%d,"email":"dev.person@example.com","phone":"",`+
		`"app_metadata":{"provider":"email","providers":["email","github"]},"user_metadata":{"avatar_url":`+
		`"https://avatars.githubusercontent.com/u/1234567?v=4","email":"dev.person@example.com","email_verified":true,`+
		`"full_name":"Dev Person","user_name":"devperson"},"role":"authenticated","aal":"aal1","amr":[{"method":"password",`+
		`"timestamp":1731400000}],"session_id":"9e8d7c6b-5a4f-3e2d-1c0b-a9f8e7d6c5b4","is_anonymous":false}`, iat+3600, iat)
	return "eyJhbGciOiJIUzI1NiIsImtpZCI6IlptOXZZbUZ5WW1GNiIsInR5cCI6IkpXVCJ9." +
		base64.RawURLEncoding.EncodeToString([]byte(claims)) + "." + sig
}

const sshKeyMagic = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW"

func TestTaskRedactionSharedPublicPrefixIsSafe(t *testing.T) {
	for _, c := range sharedPrefixCases {
		for _, minLen := range []int{0, 8} {
			// The redacted secret itself, swapped into two pieces by cursor
			// moves at any cut, is a rearranged copy and stays unverifiable.
			for cut := 1; cut < len(c.secret); cut++ {
				swapped := fmt.Sprintf("\x1b[1;%dH%s\x1b[1;1H%s\r\n", cut+1, c.secret[cut:], c.secret[:cut])
				if safe, err := taskRedactionSafe(swapped, []string{c.secret}, minLen); err == nil && safe {
					t.Fatalf("%s (minLen %d): swapped at %d verified safe", c.name, minLen, cut)
				}
			}
			if safe, err := taskRedactionSafe(c.output, []string{c.secret}, minLen); err != nil || !safe {
				t.Fatalf("%s (minLen %d): unrelated output not safe: safe=%v err=%v", c.name, minLen, safe, err)
			}
			if out, err := cleanSessionText(c.output, []string{c.secret}, minLen); err != nil || out != c.output {
				t.Fatalf("%s (minLen %d): unrelated output changed: %q %v", c.name, minLen, out, err)
			}
		}
	}
}

// Where a rearranged copy ends and a sibling begins: every 8-byte piece but
// at most 7 bytes must show, at any text length, and one piece must hold half
// the text (at most 32 bytes) once the text is longer than the floor.
func TestTaskRedactionRearrangedPiecesBoundary(t *testing.T) {
	const key = "sk_live_51HxQ2bCdEfGhK8mP3vR7tY1wZ4nQ6sL" // 40 bytes
	swap := func(s string, cut int) string {
		return fmt.Sprintf("\x1b[1;%dH%s\x1b[1;1H%s\r\n", cut+1, s[cut:], s[:cut])
	}
	held := map[string]string{
		"last 7 differ":  "KEY=" + key[:33] + "#######\r\nprogress\r",
		"20 split 14/6":  swap("hunter2-correct-pass", 14),
		"24 split 12/12": swap("abcdefghijkl0123456789xy", 12),
	}
	long := plantedSecret + "Zq9w8E7r6T5y4U3i2O1p" // 58 bytes
	held["3 pieces 32+14+14"] = fmt.Sprintf("\x1b[1;47H%s\x1b[1;1H%s\x1b[1;33H%s\r\n", long[46:], long[:32], long[32:46])
	texts := map[string]string{"last 7 differ": key, "20 split 14/6": "hunter2-correct-pass",
		"24 split 12/12": "abcdefghijkl0123456789xy", "3 pieces 32+14+14": long}
	for name, input := range held {
		for _, minLen := range []int{0, 8} {
			if safe, err := taskRedactionSafe(input, []string{texts[name]}, minLen); err == nil && safe {
				t.Fatalf("%s (minLen %d): verified safe: %q", name, minLen, input)
			}
		}
	}
	for _, minLen := range []int{0, 8} {
		for cut := 1; cut < 24; cut++ {
			if safe, err := taskRedactionSafe(swap(texts["24 split 12/12"], cut), []string{texts["24 split 12/12"]}, minLen); err == nil && safe {
				t.Fatalf("24-byte swap at %d (minLen %d) verified safe", cut, minLen)
			}
		}
		input := "KEY=" + key[:32] + "########\r\nprogress\r" // last 8 differ: a sibling
		if safe, err := taskRedactionSafe(input, []string{key}, minLen); err != nil || !safe {
			t.Fatalf("last 8 differ (minLen %d): not safe: %v %v", minLen, safe, err)
		}
	}
}

// A cell-diff renderer (ratatui, as the Codex TUI) streams text a few cells a
// frame, repainting a status line between frames: the copy is in order but
// interleaved. A session on the redacted issue fails closed.
func TestTaskRedactionStreamedCellsFailClosedScoped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < len(plantedSecret); i += 4 {
		fmt.Fprintf(&b, "\x1b[3;%dH%s\x1b[20;1HWorking (%ds)", i+1, plantedSecret[i:min(i+4, len(plantedSecret))], i/4)
	}
	for _, input := range []string{b.String(), b.String() + "\x1b[4;1H" + plantedSecret + "\r\n"} {
		if safe, err := taskRedactionSafe(input, []string{plantedSecret}, 0); err == nil && safe {
			t.Fatalf("streamed copy verified safe: %q", input)
		}
		if out, err := cleanSessionText(input, []string{plantedSecret}, 0); err == nil {
			t.Fatalf("streamed copy kept: %q", out)
		}
	}
	// Short pieces in order but spread wider than 4x the text are not a copy.
	filler := strings.Repeat("unrelated words ", 5)
	for _, input := range []string{
		"00000010  sk-live-PLANTED2|  0123456789abcdef  |\r\n\x1b[2K\r",
		"sk-live-PLANT" + filler + "ED2secret0123" + filler + "456789abcdef\x1b[H",
	} {
		if safe, err := taskRedactionSafe(input, []string{plantedSecret}, 0); err != nil || !safe {
			t.Fatalf("spread pieces not safe: %q safe=%v err=%v", input, safe, err)
		}
	}
}

func TestTaskRedactionScrubLinear(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("x\x1b[1mprivate secret marker\r\n")
	}
	out, err := cleanSessionText(b.String(), []string{"private secret marker"}, 0)
	if err != nil || strings.Count(out, "[redacted]") != 20000 {
		t.Fatalf("bulk scrub: %d %v", strings.Count(out, "[redacted]"), err)
	}
}

func FuzzTaskRedactionVerification(f *testing.F) {
	f.Add("stray \" quote\n12 ", []byte{0, 1, 2, 3}, " trailing")
	f.Add("[1] ", []byte{3, 2, 1, 0}, "\n")
	f.Add("", []byte{2, 2, 2}, "")
	f.Add("\x1b", []byte{1}, "0")
	f.Add("\x1b]title\x1b", []byte{1}, "0")
	f.Fuzz(func(t *testing.T, prefix string, modes []byte, suffix string) {
		if len(prefix) > 256 || len(suffix) > 256 || len(modes) == 0 {
			t.Skip()
		}
		secret := "private secret marker"
		var b strings.Builder
		for i, c := range []byte(secret) {
			switch modes[i%len(modes)] % 4 {
			case 0:
				b.WriteByte(c)
			case 1:
				fmt.Fprintf(&b, "\\u%04x", c)
			case 2:
				fmt.Fprintf(&b, "\\x%02x", c)
			case 3:
				fmt.Fprintf(&b, "\\\\u%04x", c)
			}
		}
		input := prefix + b.String() + suffix
		safe, err := taskRedactionSafe(input, []string{secret}, 0)
		if err == nil && safe {
			t.Fatalf("verifier missed encoded secret: %q", input)
		}
		cleaned, err := scrubTaskBytes(input, []string{secret}, 0)
		if err == nil {
			safe, err = taskRedactionSafe(cleaned, []string{secret}, 0)
			if err == nil && !safe {
				t.Fatalf("scrubber retained secret: %q", cleaned)
			}
		}
	})
}

func FuzzTaskTerminalRedactionVerification(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4})
	f.Add([]byte{3, 4, 2})
	f.Fuzz(func(t *testing.T, modes []byte) {
		if len(modes) == 0 || len(modes) > 256 {
			t.Skip()
		}
		const secret = "private secret marker"
		var b strings.Builder
		// Complete controls surround the protected fragment. An arbitrary
		// terminal prefix could consume its first letters as a CSI final or
		// control-string payload, so would not guarantee this rendering.
		b.WriteString("before:")
		for i, c := range []byte(secret) {
			// Only decorate between letters: leading controls are outside the
			// matched raw span and may correctly survive a redaction.
			if i == 0 {
				b.WriteByte(c)
				continue
			}
			switch modes[i%len(modes)] % 9 {
			case 0:
				b.WriteByte(c)
			case 1:
				fmt.Fprintf(&b, "\\u%04x", c)
			case 2:
				b.WriteString("\x1b[1m")
				b.WriteByte(c)
			case 3:
				b.WriteString("\r\r\n")
				b.WriteByte(c)
			case 4:
				b.WriteString(`\u001b[12;1H`)
				b.WriteByte(c)
			case 5:
				b.WriteString("\u009b1m") // C1 CSI
				b.WriteByte(c)
			case 6:
				b.WriteString("\r\n    ") // indented continuation
				b.WriteByte(c)
			case 7:
				b.WriteString(" \u2502\r\n\u2502 ") // TUI box border
				b.WriteByte(c)
			case 8:
				if c == ' ' {
					b.WriteString("\x1b[1C") // cursor forward renders the space
				} else {
					b.WriteByte(c)
				}
			}
		}
		b.WriteString(":after")
		if safe, err := taskRedactionSafe(b.String(), []string{secret}, 0); err != nil || safe {
			t.Fatalf("verifier missed terminal rendering: %q %v", b.String(), err)
		}
		out, err := cleanSessionText(b.String(), []string{secret}, 0)
		if err != nil || out != "before:[redacted]:after" {
			t.Fatalf("terminal rendering retained: %q %v", out, err)
		}
	})
}

func FuzzTaskRedactionAfterBackslash(f *testing.F) {
	for i := uint8(0); i < 7; i++ {
		f.Add(i, "C:", " trailing")
	}
	f.Fuzz(func(t *testing.T, kind uint8, prefix, suffix string) {
		if len(prefix) > 256 || len(suffix) > 256 {
			t.Skip()
		}
		secret := string("ntruxbf"[int(kind)%7]) + " marker secret"
		input := prefix + `\` + secret + suffix
		safe, err := taskRedactionSafe(input, []string{secret}, 0)
		if safe || err != nil {
			t.Fatalf("raw secret missed: %q safe=%v err=%v", input, safe, err)
		}
		cleaned, err := scrubTaskBytes(input, []string{secret}, 0)
		if err == nil && strings.Contains(cleaned, secret) {
			t.Fatalf("raw secret retained: %q", cleaned)
		}
	})
}

// Two to six pieces of the secret, each drawn at its own column of row 1 in a
// shuffled order (absolute moves, CR plus cursor forward, or backspaces), with
// unrelated text painted on row 20 between pieces. The construction is the
// oracle: the screen shows the secret. The output must not verify safe, and
// must be discarded or have every piece redacted, when the stated rule
// applies: a piece of at least half the secret (19 of its 38 bytes) with at
// most 7 bytes in pieces under 8 bytes, an in-order draw with no paint
// between pieces, or (scoped) an in-order draw within 4x the secret's length.
// Residual, a stated limit like base64: every piece shorter than half
// (per-character or 3+ even pieces), more than 7 bytes in short pieces, and
// in-order interleaved draws in unscoped sessions, verify safe.
func FuzzTaskTerminalReorderedSecret(f *testing.F) {
	f.Add([]byte{16}, []byte{1}, []byte{}, []byte{0})
	f.Add([]byte{12, 25}, []byte{2, 0}, []byte("wxyz"), []byte{0, 1, 2, 3})
	f.Add([]byte{5, 10, 20, 30, 33}, []byte{5, 4, 3, 2, 1}, []byte("Working"), []byte{2})
	f.Add([]byte{19}, []byte{0}, []byte("Working"), []byte{1})              // two halves, reversed
	f.Add([]byte{4, 8, 12, 16, 20}, []byte{}, []byte("spinner"), []byte{0}) // in order, interleaved
	f.Add([]byte{4}, []byte{0}, []byte{}, []byte{3})                        // 5-byte piece left uncovered
	f.Add([]byte{18, 27}, []byte{1, 2}, []byte("tick"), []byte{0})          // 19+9+10, shuffled
	f.Fuzz(func(t *testing.T, cuts, order, noise, modes []byte) {
		if len(cuts) == 0 || len(cuts) > 5 || len(order) > 6 || len(noise) > 32 || len(modes) == 0 || len(modes) > 6 {
			t.Skip()
		}
		bounds := []int{0}
		for i := 1; i < len(plantedSecret); i++ {
			for _, c := range cuts {
				if 1+int(c)%(len(plantedSecret)-1) == i {
					bounds = append(bounds, i)
					break
				}
			}
		}
		bounds = append(bounds, len(plantedSecret))
		k := len(bounds) - 1
		perm := make([]int, k)
		for i := range perm {
			perm[i] = i
		}
		for i := k - 1; i > 0 && len(order) > 0; i-- {
			j := int(order[i%len(order)]) % (i + 1)
			perm[i], perm[j] = perm[j], perm[i]
		}
		// Paint: printable, no control, no backslash, no secret letter.
		const quiet = "ghjmnopquwxyzBCFGHIJKMOQRSUVWXYZ!#%&*+=?@^_~.,:;()<>"
		var paint strings.Builder
		for _, c := range noise {
			paint.WriteByte(quiet[int(c)%len(quiet)])
		}
		var b strings.Builder
		b.WriteString("$ \r\n")
		col, inOrder, long, short := 1, true, false, 0 // col 0: the cursor is on row 20
		for n, p := range perm {
			lo, hi := bounds[p], bounds[p+1]
			inOrder = inOrder && p == n
			long = long || 2*(hi-lo) >= len(plantedSecret)
			if hi-lo < 8 {
				short += hi - lo
			}
			switch m := modes[n%len(modes)] % 4; {
			case m == 0 || col == 0:
				fmt.Fprintf(&b, "\x1b[1;%dH", lo+1)
			case m == 1:
				fmt.Fprintf(&b, "\u009b1;%dH", lo+1)
			case lo+1 < col && m == 2:
				b.WriteByte('\r')
				if lo > 0 {
					fmt.Fprintf(&b, "\x1b[%dC", lo)
				}
			case lo+1 < col:
				b.WriteString(strings.Repeat("\b", col-lo-1))
			case lo+1 > col:
				fmt.Fprintf(&b, "\x1b[%dC", lo+1-col)
			}
			b.WriteString(plantedSecret[lo:hi])
			col = hi + 1
			if n < k-1 && paint.Len() > 0 {
				b.WriteString("\x1b[20;1H" + paint.String())
				col = 0
			}
		}
		input := b.String() + "\r\n"
		spread := len(plantedSecret) + (k-1)*paint.Len()
		for _, minLen := range []int{0, 8} {
			caught := long && short <= 7 || inOrder && (paint.Len() == 0 || minLen == 0 && spread <= 4*len(plantedSecret))
			safe, err := taskRedactionSafe(input, []string{plantedSecret}, minLen)
			out, cleanErr := cleanSessionText(input, []string{plantedSecret}, minLen)
			if !caught {
				continue
			}
			if err == nil && safe {
				t.Fatalf("drawn secret verified safe (minLen %d): %q", minLen, input)
			}
			if cleanErr == nil {
				for i := 0; i < k; i++ {
					if piece := plantedSecret[bounds[i]:bounds[i+1]]; len(piece) >= 6 && strings.Contains(out, piece) {
						t.Fatalf("drawn secret kept (minLen %d): %q -> %q", minLen, input, out)
					}
				}
			}
		}
	})
}

// Terminal noise built from bytes that cannot spell the secret (the oracle)
// verifies safe and is kept byte for byte: unrelated sessions are not held.
func FuzzTaskTerminalNoiseNeverHeld(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{0, 9, 9, 30, 0, 12, 31})
	tokens := []string{"\x1b", "[", "]", ";", "?", "1", "2", "H", "f", "D", "A", "G", "\b", "\r", "\n",
		"\a", "\\", "b", "n", "\x9b", "\u009b", "\u009c", "P", "_", "o", "q", "z", "\u2502", " ", "/", "0", "("}
	f.Fuzz(func(t *testing.T, modes []byte) {
		if len(modes) > 512 {
			t.Skip()
		}
		var b strings.Builder
		for _, m := range modes {
			b.WriteString(tokens[int(m)%len(tokens)])
		}
		input := b.String()
		for _, minLen := range []int{0, 8} {
			safe, err := taskRedactionSafe(input, []string{"private secret marker", plantedSecret}, minLen)
			if err != nil || !safe {
				t.Fatalf("noise not safe: %q safe=%v err=%v", input, safe, err)
			}
			if out, err := cleanSessionText(input, []string{"private secret marker", plantedSecret}, minLen); err != nil || out != input {
				t.Fatalf("noise changed: %q -> %q %v", input, out, err)
			}
		}
	})
}
