// Package orchapi holds zm's orchestration wire types: work, mail,
// sessions, tasks, issues, approvals, trackers, webhooks, the door and the
// replica sync answer. potluck's kit/api keeps only the
// base (KIT) types; zm imports this package, never the reverse. The JSON
// is the one zm's daemons have always sent, so old and new builds still talk.
//
// Some KIT types stay in kit/api though orchestration reads them:
// api.WorkSkipped (a partial list's skipped authorities),
// api.DaemonSessionFailure (the daemon stop answer, filled through
// core.SessionHooks), api.HeaderPersonalMail and api.PersonalMailProtocol
// (the base's private-audience filter), and api.Machine and its stats.
// Health is the base's api.Health with the parts zm's modules add.
package orchapi
