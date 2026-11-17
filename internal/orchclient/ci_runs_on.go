package orchclient

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

// CIWorkflowLine reads the pool's machines for both CI status and runners ls.
func CIWorkflowLine(ctx context.Context, c *client.Client, st runners.Setup) (string, error) {
	poolClient := *c
	poolClient.Project, poolClient.ProjectFrom = st.Pool, "flag"
	targets, err := computeclient.RunTargets(ctx, &poolClient, true)
	if err != nil {
		return "", err
	}
	// The broker owns eligibility and labels. Prefer a Mac among its serving
	// targets; this is a workflow request, not a placement reservation.
	var lines []string
	for _, target := range targets.Targets {
		labels, serves, _ := runners.RunsOn(target, st)
		if !serves {
			continue
		}
		// RunsOn returns the pool request followed by its OS label.
		encoded, _ := json.Marshal([]string{"self-hosted", labels[1], labels[0]})
		lines = append(lines, "runs-on: "+string(encoded))
	}
	slices.SortFunc(lines, func(a, b string) int {
		am, bm := strings.Contains(a, `"macos"`), strings.Contains(b, `"macos"`)
		if am != bm {
			if am {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(lines) == 0 {
		return "", nil
	}
	return lines[0], nil
}
