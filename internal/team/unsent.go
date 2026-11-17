package team

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// unsentSuffix names the file a dropped copy leaves its still-pending
// outbox writes in (SPEC 17.4): JSON Lines next to the copy. Nothing reads
// it back or re-sends it; it is for the person to re-send after re-joining.
const unsentSuffix = ".unsent.jsonl"

func unsentPath(replicaPath string) string { return replicaPath + unsentSuffix }

type unsentRow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Body    any    `json:"body"`
	As      string `json:"as,omitempty"`
	Item    string `json:"item,omitempty"`
	Created string `json:"created"`
}

// exportUnsent appends every pending outbox row of db to dbPath's unsent
// file (0600, fsynced) and returns how many it wrote. Settled rows (acked,
// conflict, failed) are not exported. No pending rows: no file.
func exportUnsent(ctx context.Context, db *sql.DB, dbPath string) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, kind, method, path, body, as_addr, item, created FROM outbox WHERE state = ? ORDER BY seq`, api.OutboxPending)
	if err != nil {
		return 0, err
	}
	var lines [][]byte
	for rows.Next() {
		var r unsentRow
		var body string
		var created int64
		if err := rows.Scan(&r.ID, &r.Kind, &r.Method, &r.Path, &body, &r.As, &r.Item, &created); err != nil {
			rows.Close()
			return 0, err
		}
		if json.Valid([]byte(body)) {
			r.Body = json.RawMessage(body)
		} else {
			r.Body = body
		}
		r.Created = store.FromMillis(created).UTC().Format(time.RFC3339)
		b, err := json.Marshal(r)
		if err != nil {
			rows.Close()
			return 0, err
		}
		lines = append(lines, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(lines) == 0 {
		return 0, nil
	}
	f, err := os.OpenFile(unsentPath(dbPath), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	for _, l := range lines {
		w.Write(l)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	return len(lines), f.Close()
}

// exportUnsentFile is exportUnsent for a copy nobody has open (its file is
// still there although no replica is running), opened through st.
func exportUnsentFile(ctx context.Context, st *store.Store, dbPath string) (int, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return 0, nil
	}
	db, err := st.OpenReplica(ctx, dbPath, 1)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return exportUnsent(ctx, db, dbPath)
}

// unsentSaved is what Remote.Unsent shows: the export file's path and the
// count of lines in it, when the file exists.
func unsentSaved(replicaPath string) (string, int) {
	p := unsentPath(replicaPath)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return p, n
}

func (s *Service) logUnsent(project string, n int, err error) {
	if s.d.Log == nil {
		return
	}
	if err != nil {
		s.d.Log.Error("team: pending writes of a dropped copy could not be saved", "project", project, "err", err)
	} else if n > 0 {
		s.d.Log.Warn("team: pending writes of a dropped copy saved, not re-sent", "project", project, "count", n, "file", unsentPath(s.replicaPath(project)))
	}
}
