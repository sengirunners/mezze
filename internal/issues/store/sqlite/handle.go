package sqlite

import (
	"crypto/rand"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// Native issue handles (the part after "<prefix>-"): design note
// identifiers-2026-11-01, stage 1.
//
//   - Symbols: a-z and 2-9 without the look-alikes 0 1 i l o (31).
//   - At least one letter: a handle never looks like a number (acme-742
//     could be read as a count, and a client may upper-case it).
//   - Random (crypto/rand), not cut from a content hash.
//   - Length: the shortest in 3..8 for which one new mint hits an existing
//     handle with probability at most 1 in 1,000; a hit is drawn again.
//   - Drawn again when it contains a blocked word.
//
// Ids minted before (base36 content hashes) stay valid and are never
// renumbered: only new handles follow these rules.
const handleAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"

// handleSpace is how many handles of length l there are: every string over
// the alphabet but the all-digit ones.
func handleSpace(l int) float64 {
	all, digits := 1.0, 1.0
	for range l {
		all *= float64(len(handleAlphabet))
		digits *= 8
	}
	return all - digits
}

// handleLen is the shortest length in 3..8 at which a new handle hits one of
// n existing ones with probability at most 0.001.
func handleLen(n int) int {
	for l := 3; l < 8; l++ {
		if float64(n)/handleSpace(l) <= 0.001 {
			return l
		}
	}
	return 8
}

// api.BlockedWords are substrings a minted handle must not contain (vowels
// stay in the alphabet, so random strings can spell words; these are
// redrawn). Chosen names check whole words (api.HasBlockedWord).

// newHandle draws a handle of length l (crypto/rand, uniform over the
// alphabet, at least one letter, no blocked word).
func newHandle(l int) string {
	buf := make([]byte, l)
	var r [1]byte
	for {
		for i := range buf {
			for {
				if _, err := rand.Read(r[:]); err != nil {
					panic(err) // crypto/rand does not fail on supported platforms
				}
				// 248 = 8 * 31: reject the rest, so every symbol is equally likely.
				if r[0] < 248 {
					buf[i] = handleAlphabet[int(r[0])%len(handleAlphabet)]
					break
				}
			}
		}
		h := string(buf)
		if strings.Trim(h, "23456789") != "" && !isBlocked(h) {
			return h
		}
	}
}

func isBlocked(h string) bool {
	for _, w := range api.BlockedWords {
		if strings.Contains(h, w) {
			return true
		}
	}
	return false
}
