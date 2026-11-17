package orchapi

// Where approvers are paired. Not generated: zm's own shape.
// Approvers live per daemon, and an approver decides only the asks its
// daemon decides: a joined project's asks are decided at its authority.

// ApproversAt is GET /v0/approvers and GET /v0/approvers/self: approvers and
// the daemon (machine name) they are paired on.
type ApproversAt struct {
	Daemon    string     `json:"daemon"`
	Approvers []Approver `json:"approvers"`
}

// PairedApprover is an Approver with the daemon it is paired on: one row of
// zm approver ls --json.
type PairedApprover struct {
	Approver
	Daemon string `json:"daemon"`
}

// ApproverPlace is zm approver ls -P <project> --json: the daemon that
// decides the project's asks and the approvers paired there (at a team
// authority only the caller's own: its full list is local to it).
type ApproverPlace struct {
	Project   string           `json:"project"`
	DecidedAt string           `json:"decided_at"`
	Joined    bool             `json:"joined"`
	Only      string           `json:"only,omitempty"`
	Approvers []PairedApprover `json:"approvers"`
}

// Paired stamps each approver of l with its daemon.
func (l ApproversAt) Paired() []PairedApprover {
	out := make([]PairedApprover, 0, len(l.Approvers))
	for _, a := range l.Approvers {
		out = append(out, PairedApprover{Approver: a, Daemon: l.Daemon})
	}
	return out
}
