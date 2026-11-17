package main

import (
	"fmt"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// cardFlagsMissing names every card flag zm ask new still needs, with both
// forms of the verb, before the daemon refuses the first one alone. The
// daemon still checks the card in full (word counts, choice keys, the judge).
func cardFlagsMissing(c orchapi.AskCard) error {
	var miss []string
	if strings.TrimSpace(c.Question) == "" {
		miss = append(miss, "the question")
	}
	if strings.TrimSpace(c.Context) == "" {
		miss = append(miss, "--context")
	}
	if n := len(c.Choices); n < 2 || n > 4 {
		miss = append(miss, fmt.Sprintf("--choice (2 to 4, got %d)", n))
	}
	if strings.TrimSpace(c.Recommend) == "" {
		miss = append(miss, "--recommend")
	}
	if strings.TrimSpace(c.Why) == "" {
		miss = append(miss, "--why")
	}
	if strings.TrimSpace(c.Searched) == "" {
		miss = append(miss, "--searched")
	}
	if len(miss) == 0 {
		return nil
	}
	return exitWith{code: 2, err: fmt.Errorf(`a card needs %s
  a plain question, or one about a work item (add --work <id>):
    zm ask new "<question>" --context "<explain it like the reader is 15>" \
      --choice "<label> | <pros> | <cons>" --choice "<label> | <pros> | <cons>" \
      --recommend a --why "<what that choice does, good and bad>" --searched "<what you checked first>"
  a pull request (no card):
    zm ask new "<question>" --repo <owner/name> --pr <number> --head <full commit sha>`, strings.Join(miss, ", "))}
}
