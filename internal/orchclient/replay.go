package orchclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// EmbedStream is an explicitly selected fresh-surface output protocol. Framed
// false means the server selected the offered legacy protocol; it is never
// inferred from an error or guessed from output bytes.
type EmbedStream struct {
	io.ReadWriteCloser
	Framed bool
}

// AttachEmbed offers framed replay and legacy raw replay on the same authorized
// request (SPEC 8). Older servers explicitly select zm-pty. Auth/transport/task
// failures are returned unchanged; there is no error-triggered fallback request.
func AttachEmbed(ctx context.Context, c *client.Client, id string, readOnly bool) (*EmbedStream, error) {
	input := ""
	if readOnly {
		input = "0"
	}
	path := client.Query(sessionPath(id, "/attach"), "replay", "all", "input", input)
	resp, err := c.StreamHeader(ctx, http.MethodGet, path, nil, http.Header{"Connection": {"Upgrade"}, "Upgrade": {orchapi.AttachReplayProtocol + ", " + orchapi.AttachProtocol}, orchapi.HeaderReplaySurface: {orchapi.FreshReplaySurface}})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, fmt.Errorf("embedded attach: got %s", resp.Status)
	}
	selected := strings.TrimSpace(resp.Header.Get("Upgrade"))
	if selected != orchapi.AttachProtocol && selected != orchapi.AttachReplayProtocol {
		resp.Body.Close()
		return nil, orchapi.ErrReplayFrame
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("embedded attach body is not writable")
	}
	return &EmbedStream{ReadWriteCloser: rwc, Framed: selected == orchapi.AttachReplayProtocol}, nil
}
