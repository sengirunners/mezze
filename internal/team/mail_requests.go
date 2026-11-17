package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

var mailRequestsMigrations = []store.Migration{
	// An upgraded replica may have advanced past personal events withheld
	// while it was unaware. Re-snapshot once; retain all queued writes.
	{Kind: store.KindReplica, Name: "team/0006_personal_audience", Script: `DELETE FROM meta WHERE key IN ('ready','cursor','synced');`},
	{Kind: store.KindReplica, Name: "team/0005_mail_requests", Script: `CREATE TABLE client_mail_requests (
 actor TEXT NOT NULL, request_id TEXT NOT NULL, payload_hash TEXT NOT NULL, write_id TEXT NOT NULL,
 PRIMARY KEY(actor,request_id));`},
}

// Reserve before registering a waiter or waking the drain. A crash between
// reservation and enqueue is repaired with the same write ID on client retry.
func (rp *replica) reserveMailRequest(ctx context.Context, kind, method, path string, body []byte, actor string) (string, error) {
	id := ids.New()
	if kind != "mail" && kind != "reply" {
		return id, nil
	}
	var req struct {
		ClientRequestID string `json:"client_request_id"`
	}
	if json.Unmarshal(body, &req) != nil || req.ClientRequestID == "" {
		return id, nil
	}
	if len(req.ClientRequestID) > 128 {
		return "", api.Errorf(api.CodeBadRequest, "client_request_id exceeds 128 bytes")
	}
	hash := reqPrint(method, path, body)
	err := rp.write(ctx, func(tx *sql.Tx) error {
		var prior string
		err := tx.QueryRowContext(ctx, `SELECT payload_hash,write_id FROM client_mail_requests WHERE actor=? AND request_id=?`, actor, req.ClientRequestID).Scan(&prior, &id)
		if err == nil {
			if prior != hash {
				return api.Errorf(api.CodeConflict, "client_request_id already used with a different payload or operation")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO client_mail_requests(actor,request_id,payload_hash,write_id) VALUES (?,?,?,?)`, actor, req.ClientRequestID, hash, id)
		return err
	})
	return id, err
}
