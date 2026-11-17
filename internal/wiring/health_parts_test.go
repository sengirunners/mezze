package wiring

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

type partHomes struct{ orchcore.HomeService }

func (partHomes) HomeBackup() *orchapi.HomeBackup {
	return &orchapi.HomeBackup{Kind: "file", EverySeconds: 60}
}
func (partHomes) Heartbeat() *orchapi.HeartbeatBrief { return nil }

type partSessions struct{ orchcore.SessionService }

func (partSessions) HolderReport() *orchapi.HolderReport { return &orchapi.HolderReport{Adopted: 1} }

// Health's sections as zm registers them: backup, heartbeat and holders
// from orchcore.Wire, drivers from here only while a driver is refused,
// each once and in key order.
func TestHealthPartsJSON(t *testing.T) {
	ctx := context.Background()
	d := &core.Deps{Config: &config.Config{Machine: "mini"}}
	o := orchcore.Wire(d)
	o.Homes, o.Sessions = partHomes{}, partSessions{}
	w := &driverWatch{}
	healthParts(d, w)
	enc := func() string {
		b, err := json.Marshal(api.Health{OK: true, Protocol: "p", Version: "v", Commit: "c", Parts: d.HealthParts(ctx)})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	base := `{"ok":true,"protocol":"p","version":"v","commit":"c","backup":{"kind":"file","every_s":60,"last_ok":null}`
	if got, want := enc(), base+`,"holders":{"adopted":1}}`; got != want {
		t.Fatalf("no driver refused:\n%s\nwant\n%s", got, want)
	}
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	w.notice = &DriverNotice{Checked: at, Refused: 1, Problems: []string{"driver machine/x: refused"}}
	want := base + `,"drivers":{"checked":"2026-11-10T12:00:00Z","refused":1,"problems":["driver machine/x: refused"]},"holders":{"adopted":1}}`
	if got := enc(); got != want {
		t.Fatalf("a driver refused:\n%s\nwant\n%s", got, want)
	}
}
