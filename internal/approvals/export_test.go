package approvals

import (
	"context"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

// Owed runs this module's owed source as sub, with sub's own scope.
func (s *Service) Owed(ctx context.Context, sub *authz.Subject, who orchcore.OwedWho) (orchcore.OwedPart, error) {
	return s.owed(ctx, sub, sub.Scope, who)
}

// SetAskMailSentHook installs testHookAskMailSent for the external tests.
func SetAskMailSentHook(f func(id string)) { testHookAskMailSent = f }

// SetWriteBackHook makes the next write-backs fail with what f returns.
func SetWriteBackHook(f func() error) { testHookWriteBack = f }

// RetryDeliveries runs one tick of the retry loop.
func (s *Service) RetryDeliveries(ctx context.Context) { s.retryDeliveries(ctx) }

// SetInboxLimit changes how many asks one inbox read returns.
func SetInboxLimit(n int) (restore func()) {
	old := inboxLimit
	inboxLimit = n
	return func() { inboxLimit = old }
}

// SetBeforeJoinHook installs testHookBeforeJoin for the external tests.
func SetBeforeJoinHook(f func(first string)) { testHookBeforeJoin = f }

// TextSHA is textSHA (tests rebuild an effect's payload hash).
func TextSHA(s string) string { return textSHA(s) }

// RejudgeHeld runs one tick of the held-card sweep.
func (s *Service) RejudgeHeld(ctx context.Context) { s.rejudgeHeld(ctx) }

// ApplyDefaults runs one tick of the deadline sweep.
func (s *Service) ApplyDefaults(ctx context.Context) { s.applyDefaults(ctx) }

// ApplyDefaultsInBackground is the ticker's deadline sweep, off the ticker.
func (s *Service) ApplyDefaultsInBackground(ctx context.Context) { s.applyDefaultsInBackground(ctx) }

// PermitDryRunFor runs the dry run's record for a, as if a was just asked
// with cardJSON (tests give a card with an action without a team).
func (s *Service) PermitDryRunFor(ctx context.Context, a orchapi.Ask, cardJSON string) {
	s.permitDryRun(ctx, s.d.Projects.Local(""), a, cardJSON)
}

// SetPermitDryRunHook installs testHookPermitDryRun.
func SetPermitDryRunHook(f func(ask string)) { testHookPermitDryRun = f }

// SetPermitMachines makes the permit grant's machine check know exactly
// machines (tests have no team).
func SetPermitMachines(machines ...string) (restore func()) {
	old := permitMachineKnown
	permitMachineKnown = func(_ context.Context, _ *Service, _, m string) (bool, error) {
		for _, x := range machines {
			if x == m {
				return true, nil
			}
		}
		return false, nil
	}
	return func() { permitMachineKnown = old }
}

// SetOwedAskCap changes how many cards one owed read takes.
func SetOwedAskCap(n int) (restore func()) {
	old := maxOwedAsks
	maxOwedAsks = n
	return func() { maxOwedAsks = old }
}

// LaneHost is what starts a fresh lane (relane.go).
type LaneHost = laneHost

// SetLaneHost replaces the team module as the fresh lanes' host.
func (s *Service) SetLaneHost(h LaneHost) { s.lanes = h }

// SetAskSettings writes project's [asks] (relane.go) through the registry;
// egress is a VM lane's lane_egress.
func (s *Service) SetAskSettings(ctx context.Context, project string, remind, relane, escalate time.Duration, machine, agent string, egress ...string) error {
	_, err := s.d.Projects.SetSection(ctx, project, asksSectionName, func(any) (any, error) {
		return askSettings{Remind: remind, Relane: relane, Escalate: escalate, LaneMachine: machine, LaneAgent: agent, LaneEgress: egress}, nil
	})
	return err
}

// SweepKeys runs one tick of the key sweep (keybind.go).
func (s *Service) SweepKeys(ctx context.Context) { s.sweepKeys(ctx) }

// SetKeyLoginMark sets when sign-in binding first ran here (the legacy
// grace period starts then).
func (s *Service) SetKeyLoginMark(ctx context.Context, at time.Time) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE approvals_marks SET at = ? WHERE name = 'key_login'`, at.UnixMilli())
	return err
}

// CardSteps, ReturnSteps and SaveDraftStep are the person-step store API the
// daily card and the mail redirect use (person_steps.go), for the external
// tests.
func (s *Service) CardSteps(ctx context.Context, bundle, ask string, steps []string) error {
	return s.cardSteps(ctx, bundle, ask, steps)
}

func (s *Service) ReturnSteps(ctx context.Context, bundle string) error {
	return s.returnSteps(ctx, bundle)
}

// PendingSteps is the ids of the pending steps of the person name names in project.
func (s *Service) PendingSteps(ctx context.Context, project, name string) ([]string, error) {
	rows, err := s.pendingSteps(ctx, s.personOf(ctx, project, name))
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, err
}

func (s *Service) SaveDraftStep(ctx context.Context, project, area, name, from, work, body string) (string, error) {
	st, err := s.saveDraftStep(ctx, project, area, name, s.personOf(ctx, project, name), from, work, body)
	return st.ID, err
}
