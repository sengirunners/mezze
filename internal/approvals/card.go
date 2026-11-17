package approvals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/text/unicode/norm"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
)

// A note card's limits (SPEC 16 "Note cards"). A person answers about
// DailyTarget of these a day, so each must read in one sitting.
const (
	// MaxCardWords bounds what a person reads: context, background, question,
	// choices (labels, pros, cons) and why. Only against abuse: whether a
	// card says enough, and no more, is the card judge's call (cardjudge.go),
	// and long detail goes in background, which the card folds under More.
	MaxCardWords = 600
	// DailyTarget is how many asks a person should see in 24 hours. Going past
	// it adds a note to the answer; it is never a refusal.
	DailyTarget = 40
	minSearched = 3 // words: what the asker checked first
	maxTitle    = 80
	maxAnswer   = 2000
)

var (
	topicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,79}$`)
	// Text a person reads carries no ids, hashes or paths (the rule an agent mail service applies to person mail).
	pathRe    = regexp.MustCompile(`(?:^|[\s(])(?:~|\.{1,2})?/[\w.-]+/[\w./-]*`)
	fileRe    = regexp.MustCompile(`(?i)\b[\w-]+\.(?:ts|tsx|js|mjs|py|sql|md|json|sh|swift|yml|yaml|go)\b`)
	hexRe     = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)
	numberRe  = regexp.MustCompile(`#\d+\b`)
	envRe     = regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`)
	hexDigit  = regexp.MustCompile(`[0-9]`)
	hexLetter = regexp.MustCompile(`(?i)[a-f]`)
)

func words(s string) int {
	return len(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }))
}

// Reserved presentation is never accepted in agent-controlled one-line fields.
func unsafePresentation(s string) bool {
	if strings.ContainsAny(s, "\n\r\u2028\u2029") {
		return true
	}
	l := strings.ToLower(norm.NFKC.String(textclean.Clean(s)))
	for _, marker := range []string{"owner decision", "program decision", "recommended:", "(recommended)", "p256:", "verified fingerprint"} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}
func quoted(s string) string { b, _ := json.Marshal(s); return string(b) }

// machineToken names the first id, hash or path in text, or "". prefixes are
// the id prefixes in play (the project's slug, the subject's source): only
// those with a digit count, so a word like zm-native is fine.
func machineToken(text string, prefixes []string) string {
	for _, c := range []struct {
		re   *regexp.Regexp
		what string
	}{{pathRe, "a path"}, {fileRe, "a file name"}, {numberRe, "a message or PR number"}, {envRe, "an env var name"}} {
		if hit := c.re.FindString(text); hit != "" {
			return fmt.Sprintf("%s (%q)", c.what, strings.TrimSpace(hit))
		}
	}
	if regexp.MustCompile(`(?i)(https?://|(?:[a-z0-9_.-]+/){2,}[a-z0-9_.-]+)`).MatchString(text) {
		return "a link or path"
	}
	for _, hit := range hexRe.FindAllString(text, -1) {
		if len(hit) >= 8 || (hexDigit.MatchString(hit) && hexLetter.MatchString(hit)) {
			return fmt.Sprintf("a commit hash (%q)", hit)
		}
	}
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(p) + `-[a-z0-9]*[0-9][a-z0-9.]*\b`)
		if err == nil {
			if hit := re.FindString(text); hit != "" {
				return fmt.Sprintf("a work id (%q)", hit)
			}
		}
	}
	return ""
}

// cleanCard validates a card and returns its normal form and option keys.
func cleanCard(c orchapi.AskCard, prefixes []string) (orchapi.AskCard, []string, error) {
	bad := func(format string, a ...any) (orchapi.AskCard, []string, error) {
		return c, nil, api.Errorf(api.CodeBadRequest, format, a...)
	}
	bare := c
	bare.Action, bare.Verify, bare.OwnerStep = nil, nil, nil // bounded on their own (cleanAction, cleanVerify, cleanStep)
	raw := marshal(bare)
	if len(raw) > 12000 || !utf8.ValidString(raw) {
		return bad("card exceeds byte limit")
	}
	fields := []string{c.Title, c.Context, c.Background, c.Question, c.Why, c.Searched, c.AfterPress}
	for _, ch := range c.Choices {
		fields = append(fields, ch.Label, ch.Pros, ch.Cons, ch.Effect)
	}
	for _, f := range fields {
		if unsafePresentation(f) || len(f) > 2000 || !utf8.ValidString(f) {
			return bad("card fields must be bounded single lines without reserved presentation")
		}
	}
	if len([]rune(c.Title)) > maxTitle || words(c.Searched) > MaxCardWords {
		return bad("title or searched exceeds limit")
	}
	field := func(s string) string { return strings.TrimSpace(textclean.Clean(s)) }
	c.Title, c.Context, c.Background, c.Question = field(c.Title), field(c.Context), field(c.Background), field(c.Question)
	c.Recommend, c.Why, c.Searched = strings.ToLower(field(c.Recommend)), field(c.Why), field(c.Searched)
	c.AfterPress = field(c.AfterPress)
	switch {
	case c.Context == "":
		return bad("a card needs context: explain it like the reader is 15")
	case c.Question == "":
		return bad("a card needs the one question")
	case c.Why == "":
		return bad("a card needs why: what the recommendation does, good and bad")
	case words(c.Searched) < minSearched:
		return bad("a card needs searched: what you checked before asking (past decisions, comments, code) and what you found")
	case len(c.Choices) < 2 || len(c.Choices) > 4:
		return bad("a card offers 2 to 4 choices, got %d", len(c.Choices))
	}
	if c.Title == "" {
		c.Title = c.Question
	}
	if r := []rune(c.Title); len(r) > maxTitle {
		c.Title = strings.TrimSpace(string(r[:maxTitle-1])) + "…"
	}
	read := []string{c.Title, c.Context, c.Background, c.Question, c.Why, c.AfterPress}
	seen := map[string]bool{}
	keys := make([]string, 0, len(c.Choices))
	choices := make([]orchapi.AskChoice, 0, len(c.Choices))
	for i, ch := range c.Choices {
		ch.Key, ch.Label, ch.Pros, ch.Cons = strings.ToLower(field(ch.Key)), field(ch.Label), field(ch.Pros), field(ch.Cons)
		ch.Effect = field(ch.Effect)
		if ch.Key == "" {
			ch.Key = string(rune('a' + i))
		}
		switch {
		case ch.Label == "":
			return bad("choice %s has no label", ch.Key)
		case !api.ValidName(ch.Key) || len(ch.Key) > 32:
			return bad("choice key %q must be a short lowercase word", ch.Key)
		case ch.Key == orchapi.DecisionOther:
			return bad("choice key %q is taken: it records a free-text answer", ch.Key)
		case ch.Key == orchapi.DecisionNeedsContext:
			return bad("choice key %q is taken: it sends the ask back for more context", ch.Key)
		case ch.Key == orchapi.DecisionReleaseUnchecked:
			return bad("choice key %q is taken: it releases a card the judge could not check", ch.Key)
		case seen[ch.Key]:
			return bad("two choices use the key %q", ch.Key)
		}
		seen[ch.Key] = true
		keys = append(keys, ch.Key)
		choices = append(choices, ch)
		read = append(read, ch.Label, ch.Pros, ch.Cons, ch.Effect)
	}
	c.Choices = choices
	if !seen[c.Recommend] {
		return bad("recommend %q: one of %s", c.Recommend, strings.Join(keys, ", "))
	}
	text := strings.Join(read, "\n")
	if n := words(text); n > MaxCardWords {
		return bad("too long for a note card: %d words (at most %d); keep what a person needs to decide, and put the rest in background", n, MaxCardWords)
	}
	if hit := machineToken(strings.Join(read, "\n"), prefixes); hit != "" {
		return bad("a person reads this card: no ids, hashes or paths; it has %s. Say it in plain words", hit)
	}
	return c, keys, nil
}

// choiceAnswer is a decision as a person reads it: "B) Keep last year's swaps
// [only these three]", or the free text alone.
func choiceAnswer(card *orchapi.AskCard, decision, text string) string {
	if decision == orchapi.DecisionOther {
		return text
	}
	if decision == orchapi.DecisionNeedsContext {
		return "Needs more context: " + text
	}
	answer := decision
	if card != nil {
		for _, ch := range card.Choices {
			if ch.Key == decision {
				answer = strings.ToUpper(ch.Key) + ") " + ch.Label
			}
		}
	}
	if text != "" {
		answer += " [" + text + "]"
	}
	return answer
}

// renderCard is the card as text: the ask's mail body and body field.
func renderCard(c orchapi.AskCard, f *orchapi.AskFor) string {
	var b strings.Builder
	if f != nil {
		fmt.Fprintf(&b, "For %s at %s\n\n", f.Contact, f.Group)
	}
	fmt.Fprintf(&b, "%s\n\n%s\n", c.Title, c.Context)
	if c.Background != "" {
		fmt.Fprintf(&b, "\nBackground: %s\n", c.Background)
	}
	fmt.Fprintf(&b, "\n%s\n\n", c.Question)
	for _, ch := range c.Choices {
		fmt.Fprintf(&b, "%s) %s", strings.ToUpper(ch.Key), ch.Label)
		if ch.Effect != "" {
			fmt.Fprintf(&b, ": %s", ch.Effect)
		}
		if ch.Pros != "" {
			fmt.Fprintf(&b, ". Pros: %s", ch.Pros)
		}
		if ch.Cons != "" {
			fmt.Fprintf(&b, ". Cons: %s", ch.Cons)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nRecommended: %s. %s", strings.ToUpper(c.Recommend), c.Why)
	if c.AfterPress != "" {
		fmt.Fprintf(&b, "\nAfter you press: %s", c.AfterPress)
	}
	if c.Action != nil {
		fmt.Fprintf(&b, "\n\n%s", actionText(c.Action))
	}
	if c.Verify != nil {
		fmt.Fprintf(&b, "\n\n%s", verifyText(orchapi.Ask{Card: &c}))
	}
	if c.OwnerStep != nil {
		fmt.Fprintf(&b, "\n\n%s", stepText(c.OwnerStep, c.Verify))
	}
	return b.String()
}

// cardSHA is the sha256 of the card's JSON as stored.
func cardSHA(stored string) string {
	h := sha256.Sum256([]byte(stored))
	return hex.EncodeToString(h[:])
}

func textSHA(text string) string {
	if text == "" {
		return ""
	}
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// cleanFor validates an ask held for a person outside the team.
func cleanFor(f orchapi.AskFor, topic string, by bool) (orchapi.AskFor, error) {
	field := func(s string) string { return strings.TrimSpace(textclean.Clean(s)) }
	if unsafePresentation(f.Group) || unsafePresentation(f.Contact) || machineToken(f.Group+" "+f.Contact, nil) != "" {
		return f, api.Errorf(api.CodeBadRequest, "party and contact must be plain single-line names")
	}
	f.Group, f.Contact, f.Ref, f.ContactRef = field(f.Group), field(f.Contact), field(f.Ref), field(f.ContactRef)
	switch {
	case f.Group == "" || len(f.Group) > 80:
		return f, api.Errorf(api.CodeBadRequest, "for.group: the outside party's name, at most 80 characters")
	case f.Contact == "" || len(f.Contact) > 60:
		return f, api.Errorf(api.CodeBadRequest, "for.contact: who decides, a name or role, at most 60 characters")
	case len(f.Ref) > 80 || len(f.ContactRef) > 80:
		return f, api.Errorf(api.CodeBadRequest, "for.ref and for.contact_ref: at most 80 characters")
	case topic == "":
		return f, api.Errorf(api.CodeBadRequest, "an ask held for an outside party needs a topic: one ask per party and topic")
	case by:
		return f, api.Errorf(api.CodeBadRequest, "an ask held for an outside party has no deadline: it waits until they are asked")
	}
	return f, nil
}
