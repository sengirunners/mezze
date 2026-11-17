package approvals

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/sengirunners/potluck/kit/api"
)

// Identity hygiene (SPEC section 16): identity is never inferred from a name.
// A name is used only when it is typed exactly in its canonical form; case,
// whitespace, compatibility forms (NFKC), lookalike characters and prefixes
// or suffixes of another name are refused, never matched. Two approver names
// may not share a confusables skeleton, so no approver can look like
// another one or like an identity.

// reservedNames can never be approvers: they are mail keywords or words the
// daemon uses for itself.
var reservedNames = map[string]bool{"none": true, "all": true, "owners": true, "someone": true, "zm": true,
	"system": true, "bootstrap": true}

// checkName returns the name when raw is exactly canonical, else bad_request.
func checkName(raw string) (string, error) {
	bad := func(why string) (string, error) {
		e := api.Errorf(api.CodeBadRequest, "approver name %q refused: %s", raw, why).
			WithHint("type the name exactly: lowercase a-z, 0-9, . _ - (names are never matched loosely)")
		return "", e
	}
	switch {
	case raw == "":
		return bad("empty")
	case strings.TrimSpace(raw) != raw || strings.ContainsFunc(raw, isSpace):
		return bad("whitespace")
	case norm.NFKC.String(raw) != raw:
		return bad("not in Unicode NFKC form (lookalike or compatibility characters)")
	case strings.ToLower(raw) != raw:
		return bad("upper case")
	case !isASCII(raw):
		return bad("non-ASCII characters (a lookalike of " + skeleton(raw) + "?)")
	case !api.ValidName(raw) || reservedNames[raw]:
		return bad("not a valid name")
	}
	return raw, nil
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x85, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0x200B, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// skeleton is a confusables skeleton in the spirit of Unicode TR39: NFKC,
// then lookalike characters mapped to one representative, lower case,
// separators dropped, lookalike digits (0/o, 1/l, 2/z, 5/s, 6/b, 8/b, 9/g)
// and digraphs (rn/m, nn/m, vv/w, cl/d) folded. Two
// names with the same skeleton look alike to a person.
func skeleton(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	for _, r := range s {
		if m, ok := confusable[r]; ok {
			r = m
		}
		switch r {
		case '.', '_', '-', ' ', '·', '‐', '‑', '‒', '–', '—', '−':
			continue // separators look alike (or like nothing)
		}
		b.WriteString(strings.ToLower(string(r)))
	}
	out := b.String()
	// Single lookalikes first (after lowering, I became i), then digraphs.
	for _, p := range [][2]string{{"1", "l"}, {"i", "l"}, {"0", "o"}, {"|", "l"}, {"5", "s"}, {"2", "z"}, {"8", "b"}, {"6", "b"},
		{"9", "g"}, {"rn", "m"}, {"nn", "m"}, {"vv", "w"}, {"cl", "d"}} {
		out = strings.ReplaceAll(out, p[0], p[1])
	}
	return out
}

// confusable maps characters that look like a Latin letter or digit to it
// (a subset of Unicode confusables.txt: Cyrillic, Greek, Latin and symbol
// lookalikes of [a-z0-9]). Anything outside ASCII is refused as a name
// anyway; the skeleton makes the refusal say what it looks like.
var confusable = map[rune]rune{
	// Latin and digits
	'I': 'l', '1': 'l', '|': 'l', 'O': 'o', '0': 'o', 'ı': 'i', 'ł': 'l', 'ɑ': 'a', 'ɡ': 'g', 'ƅ': 'b',
	// Cyrillic
	'а': 'a', 'А': 'a', 'в': 'b', 'В': 'b', 'с': 'c', 'С': 'c', 'ԁ': 'd', 'е': 'e', 'Е': 'e', 'ё': 'e', 'һ': 'h', 'Н': 'h',
	'і': 'i', 'І': 'l', 'ӏ': 'l', 'Ӏ': 'l', 'ј': 'j', 'Ј': 'j', 'к': 'k', 'К': 'k', 'М': 'm', 'о': 'o', 'О': 'o', 'р': 'p',
	'Р': 'p', 'ԛ': 'q', 'ѕ': 's', 'Ѕ': 's', 'Т': 't', 'ц': 'u', 'ѵ': 'v', 'ԝ': 'w', 'Ԝ': 'w', 'х': 'x', 'Х': 'x', 'у': 'y',
	'У': 'y', 'ү': 'y',
	// Greek
	'α': 'a', 'Α': 'a', 'Β': 'b', 'ϲ': 'c', 'Ϲ': 'c', 'ε': 'e', 'Ε': 'e', 'Η': 'h', 'ι': 'i', 'Ι': 'l', 'Κ': 'k', 'κ': 'k',
	'Μ': 'm', 'Ν': 'n', 'ν': 'v', 'ο': 'o', 'Ο': 'o', 'σ': 'o', 'ρ': 'p', 'Ρ': 'p', 'Τ': 't', 'τ': 't', 'υ': 'u', 'Χ': 'x',
	'χ': 'x', 'Υ': 'y', 'γ': 'y', 'Ζ': 'z',
}
