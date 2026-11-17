package orchclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Backup is GET /v0/backup.
func Backup(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
	var v orchapi.OffsiteView
	return v, c.Do(ctx, http.MethodGet, "/v0/backup", nil, &v)
}

// SetBackup is PUT /v0/backup.
func SetBackup(ctx context.Context, c *client.Client, req orchapi.OffsiteRequest) (orchapi.OffsiteView, error) {
	var v orchapi.OffsiteView
	return v, c.Do(ctx, http.MethodPut, "/v0/backup", req, &v)
}

// BackupOff is DELETE /v0/backup.
func BackupOff(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
	var v orchapi.OffsiteView
	return v, c.Do(ctx, http.MethodDelete, "/v0/backup", nil, &v)
}

// RunBackup is POST /v0/backup/run.
func RunBackup(ctx context.Context, c *client.Client) (orchapi.OffsiteRunResult, error) {
	var r orchapi.OffsiteRunResult
	return r, c.Do(ctx, http.MethodPost, "/v0/backup/run", struct{}{}, &r)
}

// BackupSets is GET /v0/backup/sets.
func BackupSets(ctx context.Context, c *client.Client) (orchapi.OffsiteSets, error) {
	var r orchapi.OffsiteSets
	return r, c.Do(ctx, http.MethodGet, "/v0/backup/sets", nil, &r)
}

// SetWatchdog is PUT /v0/backup/watchdog.
func SetWatchdog(ctx context.Context, c *client.Client, req orchapi.WatchdogRequest) (orchapi.OffsiteView, error) {
	var v orchapi.OffsiteView
	return v, c.Do(ctx, http.MethodPut, "/v0/backup/watchdog", req, &v)
}

// WatchdogOff is DELETE /v0/backup/watchdog.
func WatchdogOff(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
	var v orchapi.OffsiteView
	return v, c.Do(ctx, http.MethodDelete, "/v0/backup/watchdog", nil, &v)
}
