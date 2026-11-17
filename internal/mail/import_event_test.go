package mail

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
)

const importBarrierType = "dev.zm.test.import-barrier"

func importEventBarrier(t *testing.T, f *fixture, subject string) api.Event {
	t.Helper()
	// Publish and subscription forwarding preserve order. This public, live-only
	// marker contains no private identifier and cannot pollute durable replay.
	e, err := f.d.Events.Publish(ctx, events.Input{Type: importBarrierType, Project: f.harbor.ID, Subject: subject, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func importEventsThrough(t *testing.T, ch <-chan api.Event, barrier api.Event) []api.Event {
	t.Helper()
	// A liveness failure guard, not a performance threshold or a settling sleep.
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var got []api.Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("import subscription closed before barrier")
			}
			if e.ID == barrier.ID {
				if e.Type != barrier.Type || e.Subject != barrier.Subject || e.Project != barrier.Project {
					t.Fatalf("incorrect import barrier: %+v", e)
				}
				return got
			}
			got = append(got, e)
		case <-wait.Done():
			t.Fatal("missing import subscription barrier")
		}
	}
}
