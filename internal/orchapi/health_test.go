package orchapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// zm's Health encodes byte for byte as the daemon's answer with the same
// sections as parts, and decodes that answer back into its fields.
func TestHealthReadsTheDaemonsParts(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	backup := &HomeBackup{Kind: "file", EverySeconds: 60, LastOK: &at, Error: "<a & b>"}
	beat := &HeartbeatBrief{LastSent: &at, LastStatus: 401, Failing: true}
	holders := &HolderReport{Adopted: 2, Unreachable: []HolderProblem{{Session: "abcd1234", Why: "gone"}}}
	base := api.Health{OK: true, Protocol: api.WireProtocol, Version: "v", Commit: "c", Machine: "mini", PID: 7, Started: at,
		Hedges: &api.HedgeStats{Reads: 3}, LastSwap: &api.SwapRecord{}}

	daemon := base
	daemon.Parts = map[string]json.RawMessage{}
	for k, v := range map[string]any{"holders": holders, "backup": backup, "heartbeat": beat, "drivers": map[string]int{"refused": 1}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		daemon.Parts[k] = b
	}
	wire, err := json.Marshal(daemon)
	if err != nil {
		t.Fatal(err)
	}
	var h Health
	if err := json.Unmarshal(wire, &h); err != nil {
		t.Fatal(err)
	}
	if h.Backup == nil || h.Backup.Error != "<a & b>" || h.Heartbeat == nil || h.Heartbeat.LastStatus != 401 ||
		h.Holders == nil || h.Holders.Adopted != 2 || h.Machine != "mini" || h.Hedges == nil || h.Parts != nil {
		t.Fatalf("decoded %+v", h)
	}
	delete(daemon.Parts, "drivers") // zm's Health keeps only its own parts
	want, _ := json.Marshal(daemon)
	if got, err := json.Marshal(h); err != nil || string(got) != string(want) {
		t.Fatalf("encoded %s %v\nwant %s", got, err, want)
	}
	// No parts: the base's plain answer.
	plain, _ := json.Marshal(base)
	if got, _ := json.Marshal(Health{Health: base}); string(got) != string(plain) {
		t.Fatalf("no parts: %s, want %s", got, plain)
	}
}
