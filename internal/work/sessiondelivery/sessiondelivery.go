// Package sessiondelivery writes work responses that embed cached session
// projections (inflight rows, an item's holder) through the session owner's
// delivery barrier (orchcore.DeliverSessionMetadata), so a response read before
// an issue redaction cannot deliver its stale snapshot after it.
package sessiondelivery

import (
	"net/http"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// WriteJSON writes value as a 200 JSON response. Values carrying session
// projections are re-projected at the actual write; others are written as
// before. A delivery failure is written as the error response and returned.
func WriteJSON(w http.ResponseWriter, r *http.Request, sessions orchcore.SessionService, value any) error {
	err := write(w, r, sessions, value)
	if err != nil {
		api.WriteError(w, err)
	}
	return err
}

func write(w http.ResponseWriter, r *http.Request, sessions orchcore.SessionService, value any) error {
	var snapshots []orchapi.Session
	var replace func([]orchapi.Session)
	switch out := value.(type) {
	case orchapi.InflightList:
		var positions []int
		for i, row := range out.Inflight {
			if row.Session != nil {
				snapshots = append(snapshots, *row.Session)
				positions = append(positions, i)
			}
		}
		replace = func(safe []orchapi.Session) {
			out.Inflight = append([]orchapi.Inflight(nil), out.Inflight...)
			for i, position := range positions {
				ss := safe[i]
				out.Inflight[position].Session = &ss
			}
			value = out
		}
	case orchapi.HolderResponse:
		if out.Session != nil {
			snapshots = append(snapshots, *out.Session)
		}
		replace = func(safe []orchapi.Session) {
			// Sanitization may reload content from a full session row. Keep the
			// holder endpoint's existing restricted field projection afterward.
			out.Session = HolderView(safe[0])
			value = out
		}
	}
	if len(snapshots) == 0 {
		api.WriteJSON(w, http.StatusOK, value)
		return nil
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	return orchcore.DeliverSessionMetadata(r.Context(), sessions, snapshots, func(safe []orchapi.Session) error {
		if len(safe) != len(snapshots) {
			return api.Errorf(api.CodeUnavailable, "session metadata delivery returned an invalid projection")
		}
		replace(safe)
		api.WriteJSON(w, http.StatusOK, value)
		return nil
	})
}

// HolderView is the holder endpoint's restricted projection of a session.
func HolderView(ss orchapi.Session) *orchapi.Session {
	return &orchapi.Session{Address: ss.Address, ID: ss.ID, Machine: ss.Machine, Project: ss.Project, Area: ss.Area,
		Agent: ss.Agent, Title: ss.Title, State: ss.State, Note: ss.Note, Managed: ss.Managed, Started: ss.Started,
		Updated: ss.Updated, Seen: ss.Seen}
}
