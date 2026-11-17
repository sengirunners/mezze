package orchapi

// HolderReport is what the daemon knows of session holders (SPEC 8, "Daemon
// restarts"): how many it runs sessions through, the live sessions it could
// not adopt and why, and holders whose home is gone.
type HolderReport struct {
	Adopted     int             `json:"adopted"`
	Unreachable []HolderProblem `json:"unreachable,omitempty"`
	Orphans     []HolderOrphan  `json:"orphans,omitempty"`
}

// HolderProblem is a live managed session this daemon cannot reach.
type HolderProblem struct {
	Session string `json:"session"`
	Why     string `json:"why"`
}

// HolderOrphan is a holder process whose home is gone: no daemon can reach
// it; zm never stops it.
type HolderOrphan struct {
	PID     int    `json:"pid"`
	Session string `json:"session"`
	Home    string `json:"home"`
}
