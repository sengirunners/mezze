// Package address holds work's address routing rules.
package address

import (
	"github.com/sengirunners/potluck/kit/api"
	"strings"
)

// Routable accepts explicit session, human or machine addresses, not tracker logins.
func Routable(s string) bool {
	a, err := api.ParseAddress(s)
	if err != nil {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(s))
	return a.Kind == api.KindSession || (a.Kind == api.KindHuman && strings.HasPrefix(low, "human:")) ||
		(a.Kind == api.KindMachine && strings.HasPrefix(low, "machine:"))
}
