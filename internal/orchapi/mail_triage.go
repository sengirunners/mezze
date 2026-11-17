package orchapi

// MailTriage is a project's [mail_triage] table (GET and PUT /v0/mail/triage,
// SPEC 9 "Triage"): who takes mail nobody else will. Agent is the project's
// triage agent (<machine>/<name> or a session address); Machines gives a
// machine its own (a name of a session there, or an address), which wins;
// WaitMinutes is how long group mail waits for a taker first (0 in a file
// or update: the default; answers carry the wait in force).
type MailTriage struct {
	Project     string            `json:"project,omitempty"` // answers only: the project's slug
	Agent       string            `json:"agent,omitempty"`
	Machines    map[string]string `json:"machines,omitempty"`
	WaitMinutes int               `json:"wait_minutes,omitempty"`
}

// MailTriageUpdate is PUT /v0/mail/triage's body: a present field replaces
// it. Agent with Machine sets that machine's triage agent, without it the
// project's; "" takes it back.
type MailTriageUpdate struct {
	Machine     string  `json:"machine,omitempty"`
	Agent       *string `json:"agent,omitempty"`
	WaitMinutes *int    `json:"wait_minutes,omitempty"`
}
