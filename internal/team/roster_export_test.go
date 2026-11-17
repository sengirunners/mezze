package team

import (
	"time"

	"github.com/sengirunners/potluck/kit/teamlink"
)

// RosterState says whether project slug's copy holds a roster, and whether it
// would be served now. rp is the copy at the call: later states read it
// through the returned func (a left project has no x).
func (s *Service) RosterState(slug string) func() (cached, valid bool) {
	x := s.ByName(slug)
	if x == nil {
		panic("RosterState: no project " + slug)
	}
	rp := rep(x)
	return func() (bool, bool) {
		if rp == nil {
			return false, false
		}
		return rp.roster.get() != nil, s.validRoster(x, rp) != nil
	}
}

// SetRosterWait sets how long an owner read waits for the authority (the
// returned func undoes it).
func SetRosterWait(d time.Duration) func() {
	old := RosterWait
	RosterWait = d
	return func() { RosterWait = old }
}

// Ended says whether project slug's remote is revoked or purged.
func (s *Service) Ended(slug string) bool {
	x := s.ByName(slug)
	return x == nil || teamlink.Ended(x.Snapshot()) != nil
}
