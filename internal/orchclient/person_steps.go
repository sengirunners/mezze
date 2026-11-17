package orchclient

import (
	"context"
	"net/http"
	"strconv"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Person steps (SPEC 16 "Person steps"): zm ask bundle's routes.

// AddStep is POST /v0/steps: a new step, a completed draft or a hand ask
// moved into a step, after the step judge passes it.
func AddStep(ctx context.Context, c *client.Client, req orchapi.StepAddRequest) (orchapi.PersonStep, error) {
	var st orchapi.PersonStep
	return st, c.Do(ctx, http.MethodPost, "/v0/steps", req, &st)
}

// Steps is GET /v0/steps: the steps the caller may read, oldest first; to
// (a person) and state ("" = not done or dropped, "all") narrow it.
func Steps(ctx context.Context, c *client.Client, project, to, state string) (orchapi.StepList, error) {
	var l orchapi.StepList
	return l, c.Do(ctx, http.MethodGet, client.Query("/v0/steps", "project", project, "to", to, "state", state), nil, &l)
}

// GetStep is GET /v0/steps/{id}.
func GetStep(ctx context.Context, c *client.Client, id string) (orchapi.PersonStep, error) {
	var st orchapi.PersonStep
	return st, c.Do(ctx, http.MethodGet, "/v0/steps/"+client.Seg(id), nil, &st)
}

// DropStep is POST /v0/steps/{id}/drop.
func DropStep(ctx context.Context, c *client.Client, id string) (orchapi.PersonStep, error) {
	var st orchapi.PersonStep
	return st, c.Do(ctx, http.MethodPost, "/v0/steps/"+client.Seg(id)+"/drop", struct{}{}, &st)
}

// CheckStep is POST /v0/steps/{id}/check: the exit of the step's check line.
func CheckStep(ctx context.Context, c *client.Client, id string, req orchapi.StepCheckRequest) (orchapi.PersonStep, error) {
	var st orchapi.PersonStep
	return st, c.Do(ctx, http.MethodPost, "/v0/steps/"+client.Seg(id)+"/check", req, &st)
}

// GetBundle is GET /v0/bundles/{id}: a steps card's steps (id: its bundle
// or its ask).
func GetBundle(ctx context.Context, c *client.Client, id string) (orchapi.Bundle, error) {
	var b orchapi.Bundle
	return b, c.Do(ctx, http.MethodGet, "/v0/bundles/"+client.Seg(id), nil, &b)
}

// ReportStep is POST /v0/bundles/{id}/steps/{n}/report: step n of the card
// is done or failed.
func ReportStep(ctx context.Context, c *client.Client, id string, n int, req orchapi.StepReportRequest) (orchapi.PersonStep, error) {
	var st orchapi.PersonStep
	return st, c.Do(ctx, http.MethodPost, "/v0/bundles/"+client.Seg(id)+"/steps/"+strconv.Itoa(n)+"/report", req, &st)
}
