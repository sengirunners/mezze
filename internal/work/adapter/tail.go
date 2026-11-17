package adapter

import (
	"context"
	"strings"
	"unicode"
)

// TailMax bounds the stderr tail an adapter error carries.
const TailMax = 300

type secretsKey struct{}

// WithSecrets returns ctx carrying the adapter's secret values (Env's), so
// an error built from its stderr masks them before the tail is cut.
func WithSecrets(ctx context.Context, secrets []string) context.Context {
	if len(secrets) == 0 {
		return ctx
	}
	return context.WithValue(ctx, secretsKey{}, secrets)
}

func secretsOf(ctx context.Context) []string {
	s, _ := ctx.Value(secretsKey{}).([]string)
	return s
}

// Mask replaces every secret value in s with "***" (the one masker for
// adapter errors: internal/work/clean uses it too).
func Mask(s string, secrets []string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// errTail is what an error may quote of an adapter's stderr: masked whole,
// then its last n bytes, minus a partial token at either cut (the start of
// the tail, or the end when the buffer dropped output: truncated). A cut
// secret's piece matches no known value, so it must not show at all.
func errTail(stderr string, n int, truncated bool, secrets []string) string {
	s := Mask(stderr, secrets)
	if truncated {
		if i := strings.LastIndexFunc(s, unicode.IsSpace); i >= 0 {
			s = s[:i]
		} else {
			s = ""
		}
	}
	if len(s) > n {
		cutInToken := !unicode.IsSpace(rune(s[len(s)-n-1]))
		s = s[len(s)-n:]
		if cutInToken {
			if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
				s = s[i:]
			} else {
				s = ""
			}
		}
	}
	return strings.TrimSpace(s)
}
