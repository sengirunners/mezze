package orchcore

import (
	"context"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// DoorRelay is implemented by the team module (owner: team; asked by the
// door, SPEC 18.4): it hands one Mezze app door delivery to machine in team
// project (the authority's ULID) and waits up to wait for the machine's
// answer: the Mezze app's HTTP status. To this machine it posts at once; to another
// it queues a door_delivery machine op. A machine not heard from lately,
// or no answer in time, is a CodeUnavailable error (the door retries);
// an op carried but unanswered may still run once there. Reach it as
// orchcore.Team(d).(DoorRelay).
type DoorRelay interface {
	RelayDoor(ctx context.Context, project, machine string, op orchapi.DoorDeliveryOp, wait time.Duration) (orchapi.DoorDeliveryResult, error)
}
