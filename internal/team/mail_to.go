package team

import (
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// itemMailTo is where the authority sends mail to it (work.mailTarget): its
// lease holder, else a tracker's owner, else its creator; else fallback.
func itemMailTo(it orchapi.WorkItem, h orchapi.HolderResponse, fallback string) string {
	if h.Lease != nil {
		return h.MailTo
	}
	if a, ok := routable(it.Owner); it.Source != "local" && ok {
		return a
	}
	if h.MailTo != "" {
		return h.MailTo
	}
	return fallback
}

// routable is work/address.Routable (a brick the replica does not import):
// an explicit session, human: or machine: address, canonical. A bare login
// parses as a machine, but the authority never routes to it.
func routable(s string) (string, bool) {
	a, err := api.ParseAddress(s)
	if err != nil {
		return "", false
	}
	low := strings.ToLower(strings.TrimSpace(s))
	if a.Kind == api.KindSession || (a.Kind == api.KindHuman && strings.HasPrefix(low, "human:")) || (a.Kind == api.KindMachine && strings.HasPrefix(low, "machine:")) {
		return a.String(), true
	}
	return "", false
}

// replyParty is the other side of a mail the sender is answering.
func replyParty(from, to, me string) string {
	if from == me {
		return to
	}
	return from
}

// mailTo parses POST /v0/mail's to as the authority will: one address
// (addr is its canonical form), or mail to several people (members set,
// addr the recipients joined by commas).
func mailTo(me, raw string) (to api.Address, addr string, members []string, err error) {
	if orchapi.IsMailGroup(raw) {
		recipients, members, err := orchapi.MailGroup(me, raw)
		return api.Address{}, strings.Join(recipients, ","), members, err
	}
	to, err = api.ParseAddress(raw)
	if _, _, named := api.SessionNameRef(raw); err != nil && named {
		err = api.NotFound("session", raw) // a name mailNames did not resolve (names.go)
	}
	return to, to.String(), nil, err
}
