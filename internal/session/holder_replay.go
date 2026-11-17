package session

import "github.com/sengirunners/mezze/internal/session/holder"

// readHolderReplay accepts only the explicitly advertised bounded schema. This
// is rendering metadata, not output, input authorization, or a provider event.
func readHolderReplay(r *holder.Ring, version int, payload []byte) *holder.ReplayCheckpoint {
	if version != 1 {
		return nil
	}
	checkpoint, err := holder.ParseReplay(payload)
	if err != nil {
		return nil
	}
	_, end := r.Bounds()
	if checkpoint.From < end {
		return nil
	}
	return &checkpoint
}

// prepareHolderReplay associates metadata with exactly the immediately following
// O. Staging changes no bytes/offsets; the existing feed's WriteAt consumes it
// atomically with its raw data, including a first-O clamp beyond Hello.Start.
func prepareHolderReplay(r *holder.Ring, pending *holder.ReplayCheckpoint, from int64) {
	if pending != nil && pending.From == from {
		r.CheckpointNextWrite(*pending)
	}
}
