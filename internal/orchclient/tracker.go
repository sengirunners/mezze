package orchclient

// Owned by stream A2. One method per /v0/trackers route in API.md.

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Trackers is GET /v0/trackers.
func Trackers(ctx context.Context, c *client.Client, project string) ([]orchapi.TrackerView, error) {
	var l orchapi.TrackerList
	return l.Trackers, c.Do(ctx, http.MethodGet, client.Query("/v0/trackers", "project", project), nil, &l)
}

// AddTracker is POST /v0/trackers (in the resolved project).
func AddTracker(ctx context.Context, c *client.Client, req orchapi.AddTrackerRequest) (orchapi.TrackerResult, error) {
	var r orchapi.TrackerResult
	return r, c.Do(ctx, http.MethodPost, "/v0/trackers", req, &r)
}

// TestTracker is POST /v0/trackers/{prefix}/test.
func TestTracker(ctx context.Context, c *client.Client, prefix string) (orchapi.TrackerResult, error) {
	var r orchapi.TrackerResult
	return r, c.Do(ctx, http.MethodPost, "/v0/trackers/"+client.Seg(prefix)+"/test", struct{}{}, &r)
}

// RemoveTracker is DELETE /v0/trackers/{prefix}.
func RemoveTracker(ctx context.Context, c *client.Client, prefix string) (orchapi.RemoveTrackerResult, error) {
	var r orchapi.RemoveTrackerResult
	return r, c.Do(ctx, http.MethodDelete, "/v0/trackers/"+client.Seg(prefix), nil, &r)
}

// TrackerHome is GET /v0/trackers/{prefix}/home (SPEC 17.9).
func TrackerHome(ctx context.Context, c *client.Client, prefix string) (orchapi.TrackerWhere, error) {
	var r orchapi.TrackerWhere
	return r, c.Do(ctx, http.MethodGet, "/v0/trackers/"+client.Seg(prefix)+"/home", nil, &r)
}

// TrackerBackups is GET /v0/trackers/{prefix}/backups.
func TrackerBackups(ctx context.Context, c *client.Client, prefix string) ([]orchapi.BackupView, error) {
	var r []orchapi.BackupView
	return r, c.Do(ctx, http.MethodGet, "/v0/trackers/"+client.Seg(prefix)+"/backups", nil, &r)
}

// AddTrackerBackup is POST /v0/trackers/{prefix}/backups.
func AddTrackerBackup(ctx context.Context, c *client.Client, prefix string, req orchapi.AddBackupRequest) (orchapi.BackupResult, error) {
	var r orchapi.BackupResult
	return r, c.Do(ctx, http.MethodPost, "/v0/trackers/"+client.Seg(prefix)+"/backups", req, &r)
}

// RemoveTrackerBackup is DELETE /v0/trackers/{prefix}/backups?id=&kind=.
func RemoveTrackerBackup(ctx context.Context, c *client.Client, prefix, id, kind string) (orchapi.BackupView, error) {
	var r orchapi.BackupView
	return r, c.Do(ctx, http.MethodDelete, client.Query("/v0/trackers/"+client.Seg(prefix)+"/backups", "id", id, "kind", kind), nil, &r)
}

// RunTrackerBackups is POST /v0/trackers/{prefix}/backups/run (id "" runs all).
func RunTrackerBackups(ctx context.Context, c *client.Client, prefix, id, kind string) ([]orchapi.BackupResult, error) {
	var r []orchapi.BackupResult
	return r, c.Do(ctx, http.MethodPost, "/v0/trackers/"+client.Seg(prefix)+"/backups/run", map[string]string{"id": id, "kind": kind}, &r)
}
