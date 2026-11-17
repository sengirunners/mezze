package team

import (
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// A named machine at its session limit is refused at the authority, as
// auto placement passes it over (review finding).
func TestPickNamedMachineAtSessionLimit(t *testing.T) {
	now := time.Now()
	full := api.Machine{Machine: "full", Seen: &now}
	full.Stats.RemoteTasks, full.Stats.MaxSessions, full.Stats.Sessions = true, 2, 2
	room := api.Machine{Machine: "room", Seen: &now}
	room.Stats.RemoteTasks, room.Stats.MaxSessions, room.Stats.Sessions, room.Stats.DiskFreeMB = true, 2, 1, 100000
	ms := []api.Machine{full, room}
	if _, err := pickMachine(ms, "full", "mac", "mini", false); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("named, at its limit: %v", err)
	}
	if m, err := pickMachine(ms, "room", "mac", "mini", false); err != nil || m != "room" {
		t.Fatalf("named, with room: %q %v", m, err)
	}
	if m, err := pickMachine(ms, "auto", "mac", "mini", false); err != nil || m != "room" {
		t.Fatalf("auto: %q %v", m, err)
	}
}

// The per-parent start locks keep no entry once no start holds or waits.
func TestParentLocksPrune(t *testing.T) {
	var p parentLocks
	release := p.lock("mac/aaaaaaaa")
	done := make(chan struct{})
	go func() {
		p.lock("mac/aaaaaaaa")()
		close(done)
	}()
	for p.held() != 1 {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("a second start got in while the first held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-done
	if n := p.held(); n != 0 {
		t.Fatalf("%d parents still held", n)
	}
}
