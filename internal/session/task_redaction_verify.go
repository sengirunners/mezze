package session

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// normalizedByte retains the exact source range represented by a decoded byte.
type normalizedByte struct {
	b          byte
	start, end int
}

// normalizeTaskBytes decodes escape spellings independently of JSON syntax,
// quotes and lines. Repeated passes handle strings embedded in strings.
func normalizeTaskBytes(input string, terminal bool) ([]normalizedByte, error) {
	x := make([]normalizedByte, len(input))
	for i := 0; i < len(input); i++ {
		x[i] = normalizedByte{input[i], i, i + 1}
	}
	for pass := 0; pass < 16; pass++ {
		out := make([]normalizedByte, 0, len(x))
		changed := false
		for i := 0; i < len(x); {
			if x[i].b == '\r' && i+1 < len(x) && x[i+1].b == '\n' {
				out = append(out, normalizedByte{'\n', x[i].start, x[i+1].end})
				i += 2
				changed = true
				continue
			}
			if x[i].b != '\\' || i+1 >= len(x) || terminal && i > 0 && x[i-1].b == '\x1b' {
				out = append(out, x[i])
				i++
				continue
			}
			c := x[i+1].b
			n := 2
			var r rune
			valid := true
			switch c {
			case 'n':
				r = '\n'
			case 'r':
				r = '\r'
			case 't':
				r = '\t'
			case 'b':
				// In terminal output a literal \b is far more often source text
				// (a regexp word boundary) than an encoded backspace.
				valid = !terminal
				r = '\b'
			case 'f':
				r = '\f'
			case '\\', '"', '/':
				r = rune(c)
			case 'u', 'x':
				digits := 4
				if c == 'x' {
					digits = 2
				}
				n = 2 + digits
				if i+n > len(x) {
					valid = false
					break
				}
				var hex strings.Builder
				for j := i + 2; j < i+n; j++ {
					hex.WriteByte(x[j].b)
				}
				v, err := strconv.ParseUint(hex.String(), 16, 16)
				if err != nil {
					valid = false
					break
				}
				r = rune(v)
				if c == 'u' && utf16.IsSurrogate(r) {
					if i+n+6 > len(x) || x[i+n].b != '\\' || x[i+n+1].b != 'u' {
						valid = false
						break
					}
					hex.Reset()
					for j := i + n + 2; j < i+n+6; j++ {
						hex.WriteByte(x[j].b)
					}
					v, err = strconv.ParseUint(hex.String(), 16, 16)
					if err != nil {
						valid = false
						break
					}
					r = utf16.DecodeRune(r, rune(v))
					if r == utf8.RuneError {
						valid = false
						break
					}
					n += 6
				}
			default:
				valid = false
			}
			if !valid {
				out = append(out, x[i])
				i++
				continue
			}
			for _, b := range []byte(string(r)) {
				out = append(out, normalizedByte{b, x[i].start, x[i+n-1].end})
			}
			i += n
			changed = true
		}
		x = out
		if !changed {
			return x, nil
		}
	}
	return nil, fmt.Errorf("too many nested escapes")
}

// taskRedactionSafe reports whether input holds none of texts. An error means
// unverifiable: input edits the screen ambiguously (checkTaskTerminal) and
// holds long fragments covering half a text, or (scoped) the text in order
// but interleaved, so a rearranged copy may render. Other edits are everyday
// terminal output and verify normally.
func taskRedactionSafe(input string, texts []string, minLen int) (bool, error) {
	eligible := false
	for _, t := range texts {
		if t != "" && len(t) >= minLen {
			eligible = true
			if strings.Contains(input, t) {
				return false, nil
			}
		}
	}
	if !eligible {
		return true, nil
	}
	decoded, err := normalizeTaskCheck(input, false)
	if err != nil {
		return false, err
	}
	// ESC followed by a backslash can be a terminal string terminator or
	// precede an encoded byte, so the terminal view decodes separately.
	terminal, err := normalizeTaskCheck(input, true)
	if err != nil {
		return false, err
	}
	view, edits := checkTaskTerminal(terminal)
	var unverifiable error
	for _, t := range texts {
		if t == "" || len(t) < minLen {
			continue
		}
		needle, err := normalizeTaskCheck(t, false)
		if err != nil {
			return false, err
		}
		if strings.Contains(decoded, needle) {
			return false, nil
		}
		if needle, err = normalizeTaskCheck(t, true); err != nil {
			return false, err
		}
		if strings.Contains(terminal, needle) {
			return false, nil
		}
		// A text with no rendered characters is covered by the byte checks.
		if needle, _ = checkTaskTerminal(needle); needle == "" {
			continue
		}
		if strings.Contains(view, needle) {
			return false, nil
		}
		// Interleaved in-order copies are checked only for a session on the
		// redacted issue: unscoped, a short text spread over 4x its length
		// would hold unrelated sessions by chance.
		if edits && (taskTerminalFragment(view, needle, minLen) ||
			minLen == 0 && len(needle) >= 16 && taskTerminalInterleaved(view, needle)) {
			unverifiable = fmt.Errorf("unverifiable terminal edits around redacted text")
		}
	}
	if unverifiable != nil {
		return false, unverifiable
	}
	return true, nil
}

// The verifier independently decodes all escapes; it does not use the
// scrubber's source-span mapping or make assumptions about JSON structure.
func normalizeTaskCheck(input string, terminal bool) (string, error) {
	for pass := 0; pass < 16; pass++ {
		var out strings.Builder
		changed := false
		for i := 0; i < len(input); {
			if input[i] == '\r' && i+1 < len(input) && input[i+1] == '\n' {
				out.WriteByte('\n')
				i += 2
				changed = true
				continue
			}
			if input[i] != '\\' || i+1 >= len(input) || terminal && i > 0 && input[i-1] == '\x1b' {
				out.WriteByte(input[i])
				i++
				continue
			}
			c := input[i+1]
			if c == 'u' || c == 'x' {
				n := 4
				if c == 'x' {
					n = 2
				}
				if i+2+n > len(input) {
					out.WriteByte(input[i])
					i++
					continue
				}
				v, err := strconv.ParseUint(input[i+2:i+2+n], 16, 16)
				if err != nil {
					out.WriteByte(input[i])
					i++
					continue
				}
				r := rune(v)
				end := i + 2 + n
				if c == 'u' && utf16.IsSurrogate(r) {
					if end+6 > len(input) || input[end:end+2] != `\u` {
						out.WriteByte(input[i])
						i++
						continue
					}
					v, err = strconv.ParseUint(input[end+2:end+6], 16, 16)
					if err != nil {
						out.WriteByte(input[i])
						i++
						continue
					}
					r = utf16.DecodeRune(r, rune(v))
					if r == utf8.RuneError {
						out.WriteByte(input[i])
						i++
						continue
					}
					end += 6
				}
				i = end
				out.WriteRune(r)
				changed = true
				continue
			}
			var v byte
			switch c {
			case 'n':
				v = '\n'
			case 'r':
				v = '\r'
			case 't':
				v = '\t'
			case 'b':
				if terminal { // as normalizeTaskBytes
					out.WriteByte(input[i])
					i++
					continue
				}
				v = '\b'
			case 'f':
				v = '\f'
			case '\\', '"', '/':
				v = c
			default:
				out.WriteByte(input[i])
				i++
				continue
			}
			out.WriteByte(v)
			i += 2
			changed = true
		}
		input = out.String()
		if !changed {
			return input, nil
		}
	}
	return "", fmt.Errorf("too many nested escapes")
}

// scrubTaskBytes maps decoded matches back to their raw source spans.
func scrubTaskBytes(input string, texts []string, minLen int) (string, error) {
	for _, t := range texts {
		if t == "" || len(t) < minLen {
			continue
		}
		input = strings.ReplaceAll(input, t, "[redacted]")
		// Keep the decoded view too: control-string payloads can themselves
		// contain the secret, even though they do not render as terminal text.
		for _, terminal := range []bool{false, true} {
			x, err := normalizeTaskBytes(input, terminal)
			if err != nil {
				return "", err
			}
			n, err := normalizeTaskBytes(t, terminal)
			if err != nil {
				return "", err
			}
			if terminal {
				x, n = normalizeTaskTerminal(x), normalizeTaskTerminal(n)
			}
			if len(n) > 0 {
				input = replaceTaskSpans(input, x, n)
			}
		}
	}
	return input, nil
}

// replaceTaskSpans redacts the source span of every match of n in x, in one
// pass. Spans that share a source byte (one escape decoding to several bytes)
// merge into one redaction.
func replaceTaskSpans(input string, x, n []normalizedByte) string {
	b := make([]byte, len(x))
	for i := range x {
		b[i] = x[i].b
	}
	needle := make([]byte, len(n))
	for i := range n {
		needle[i] = n[i].b
	}
	var out strings.Builder
	last := 0
	for from := 0; from < len(b); {
		j := bytes.Index(b[from:], needle)
		if j < 0 {
			break
		}
		j += from
		from = j + len(needle)
		start, end := x[j].start, x[from-1].end
		if start >= last {
			if out.Len() == 0 {
				out.Grow(len(input))
			}
			out.WriteString(input[last:start])
			out.WriteString("[redacted]")
		}
		last = max(last, end)
	}
	if last == 0 {
		return input
	}
	out.WriteString(input[last:])
	return out.String()
}
