package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// What the measures (zm measures) read that no other record
// holds: a judge's labels on a card, and an owner's objection to an answered
// one. Both are plain records beside the ask, so another lane's judge adds
// its labels with SetLabels and the measures need no change.

var measuresMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0013_ask_measures", Script: `
CREATE TABLE ask_labels (
	ask    TEXT PRIMARY KEY,
	labels TEXT NOT NULL
);
CREATE TABLE ask_objections (
	ask  TEXT PRIMARY KEY,
	at   INTEGER NOT NULL,
	by   TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT ''
);`},
}

// Registered here, not on Migrations' own line, so no other lane's migration
// edit collides with it.
func init() { Migrations = append(Migrations, measuresMigrations...) }

const (
	maxLabels     = 8
	maxLabelKey   = 32
	maxLabelValue = 64
)

// SetLabels merges labels into the card's: a judge's verdict on its kind,
// the open card it repeats. An empty value removes the key. Keys are short
// lowercase words, values one short line; the ask must exist.
func (s *Service) SetLabels(ctx context.Context, askID string, labels map[string]string) error {
	a, err := s.ask(ctx, askID)
	if err != nil {
		return err
	}
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	cur := map[string]string{}
	if err := tx.QueryRowContext(ctx, `SELECT labels FROM ask_labels WHERE ask = ?`, a.ID).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &cur)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for k, v := range labels {
		if !api.ValidName(k) || len(k) > maxLabelKey {
			return api.Errorf(api.CodeBadRequest, "label %q must be a short lowercase word", k)
		}
		v = oneLine(v)
		if utf8.RuneCountInString(v) > maxLabelValue {
			return api.Errorf(api.CodeBadRequest, "label %q is longer than %d characters", k, maxLabelValue)
		}
		if v == "" {
			delete(cur, k)
			continue
		}
		cur[k] = v
	}
	if len(cur) > maxLabels {
		return api.Errorf(api.CodeBadRequest, "a card carries at most %d labels", maxLabels)
	}
	b, _ := json.Marshal(cur)
	if _, err = tx.ExecContext(ctx, `INSERT INTO ask_labels (ask, labels) VALUES (?, ?)
		ON CONFLICT(ask) DO UPDATE SET labels = excluded.labels`, a.ID, string(b)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) labelsOf(ctx context.Context, id string) map[string]string {
	var raw string
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT labels FROM ask_labels WHERE ask = ?`, id).Scan(&raw); err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) != nil || len(m) == 0 {
		return nil
	}
	return m
}

// withMeasures adds the ask's labels (its objection is view's).
func (s *Service) withMeasures(ctx context.Context, a orchapi.Ask) orchapi.Ask {
	a.Labels = s.labelsOf(ctx, a.ID)
	return a
}

// objectionOf is an answered ask's objection, or nil.
func (s *Service) objectionOf(ctx context.Context, a orchapi.Ask) *orchapi.AskObjection {
	if a.Decision == nil {
		return nil
	}
	var at int64
	var by, note string
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT at, by, note FROM ask_objections WHERE ask = ?`, a.ID).Scan(&at, &by, &note) != nil {
		return nil
	}
	return &orchapi.AskObjection{At: store.FromMillis(at), By: by, Note: note}
}

// handleObject is POST /v0/asks/{id}/object: a project owner says an answered
// card's outcome should not have happened. It records the press and changes
// nothing else (it undoes nothing itself); the first press stands, so asking
// again answers the same record.
func (s *Service) handleObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.ObjectRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	note := oneLine(textclean.Clean(req.Note))
	if len(note) > orchapi.MaxObjectionNote {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "the note is longer than %d bytes", orchapi.MaxObjectionNote))
		return
	}
	sub := core.SubjectOf(ctx)
	id := r.PathValue("id")
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	// An owner presses, not a session: every process of the owner's OS user reaches the
	// socket, an agent's included, and harm is the number that must be the owner's own.
	if pa, perr := api.ParseAddress(sub.Address); perr != nil || pa.Kind == api.KindSession {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "an owner records an objection, not a session"))
		return
	}
	me, err := s.ownerOf(ctx, sub, a.Project)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if a.Decision == nil {
		api.WriteError(w, api.Errorf(api.CodeConflict, "ask %s is not answered: there is nothing to object to", a.ID).
			WithHint("answer it, or withdraw it"))
		return
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT OR IGNORE INTO ask_objections (ask, at, by, note) VALUES (?, ?, ?, ?)`,
		a.ID, ms(s.now()), me.Name, strings.TrimSpace(note)); err != nil {
		api.WriteError(w, err)
		return
	}
	respond(w, s.withMeasures(ctx, s.view(ctx, sub, a)), nil)
}
