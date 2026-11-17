// Package orchclient is zm's orchestration calls (mail, work, issues,
// sessions, tasks, approvals, trackers, webhooks, the door, backup) as
// functions over potluck's *client.Client, the same pattern as potluck's
// computeclient: M(ctx, c, ...) is what was c.M(ctx, ...) on the client.
// The client keeps the transport and the base (KIT)
// calls.
package orchclient
