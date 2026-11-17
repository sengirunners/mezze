package team

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// mailQueued refreshes the notice cache soon after a mail write (round
// B2's relay did this for proxied mail).
func (s *Service) mailQueued(as string, needs orchapi.Needs) { s.kickPresence() }

// waitingOn: needs=human mail a session queued makes it wait for a human
// here (round B2's relay did this for proxied mail).
func (s *Service) waitingOn(as string, m orchapi.Mail) {
	if m.Needs != orchapi.NeedsHuman || m.HeldFor != "" || m.Kind == orchapi.MailKindReport || as == "" {
		return
	}
	if a, err := api.ParseAddress(as); err == nil && a.Kind == api.KindSession {
		id := m.ID
		if len(id) > 8 {
			id = id[len(id)-8:]
		}
		note := "waiting on mail"
		if m.Audience == "" {
			note += " " + id
		}
		s.setLocalState(as, orchapi.StateNeedsHuman, note)
	}
}

// fileReport is the authority's report rule (SPEC 9 "Reports") applied to a
// queued write's answer: an agent's note to a person is filed as a report.
// (An ask's mail never comes this way: asks are written at the authority.)
func fileReport(m *orchapi.Mail) {
	switch {
	case m.Kind == orchapi.MailKindReport:
		m.Needs = orchapi.NeedsHuman
	case m.Kind == orchapi.MailKindNote && orchapi.AgentSender(m.From) && orchapi.ToPerson(m.To, m.Needs):
		m.Kind, m.Needs, m.Note = orchapi.MailKindReport, orchapi.NeedsHuman, orchapi.ReportNote
	}
}
