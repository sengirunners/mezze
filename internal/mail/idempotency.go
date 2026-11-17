package mail

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

func payloadHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func requestActor(sub *authz.Subject) string { return sub.Identity.ID + "/" + sub.Address }

func requestReplay(ctx context.Context, q querier, project, actor, key, hash string) (row, bool, error) {
	var id, prior string
	err := q.QueryRowContext(ctx, `SELECT mail_id,payload_hash FROM mail_requests WHERE actor=? AND request_id=?`, actor, key).Scan(&id, &prior)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, false, nil
	}
	if err != nil {
		return row{}, false, err
	}
	if prior != hash {
		return row{}, false, api.Errorf(api.CodeConflict, "client_request_id already used with a different payload or operation")
	}
	m, err := getRow(ctx, q, project, id)
	m.Replayed = true
	return m, true, err
}
