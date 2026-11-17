package mail

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

type legacyReservation struct{ instance, kind, id, hash, project, mail string }

// prepareLegacyImports never guesses an owner for a reservation-only old row.
// Any ambiguous legacy state holds valid imports uniformly: a per-source hold
// would expose another person's source tuple and could allow owner-name changes
// to duplicate the unowned reservation. Reconciliation is an explicit later step.
func (s *Service) prepareLegacyImports(ctx context.Context, dryRun bool) error {
	s.importLegacyMu.Lock()
	defer s.importLegacyMu.Unlock()
	if dryRun {
		return s.checkLegacyImports(ctx)
	}
	for {
		rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT l.source_instance,l.record_kind,l.source_id,l.payload_hash,l.project,l.mail_id FROM mail_import_legacy l WHERE NOT EXISTS (SELECT 1 FROM mail_import_legacy_state st WHERE st.source_instance=l.source_instance AND st.record_kind=l.record_kind AND st.source_id=l.source_id) LIMIT 100`)
		if err != nil {
			return err
		}
		var pending []legacyReservation
		for rows.Next() {
			var l legacyReservation
			if err = rows.Scan(&l.instance, &l.kind, &l.id, &l.hash, &l.project, &l.mail); err != nil {
				break
			}
			pending = append(pending, l)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			break
		}
		for _, l := range pending {
			identity, err := s.legacyOwner(ctx, l)
			if err != nil {
				return err
			}
			err = store.DurableTx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
				state := "held"
				if identity != "" {
					_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mail_imports(importer_identity,source_instance,record_kind,source_id,payload_hash,project,mail_id) VALUES(?,?,?,?,?,?,?)`, identity, l.instance, l.kind, l.id, l.hash, l.project, l.mail)
					if e != nil {
						return e
					}
					var hash, project, mail string
					if e = tx.QueryRowContext(ctx, `SELECT payload_hash,project,mail_id FROM mail_imports WHERE importer_identity=? AND source_instance=? AND record_kind=? AND source_id=?`, identity, l.instance, l.kind, l.id).Scan(&hash, &project, &mail); e != nil {
						return e
					}
					if hash == l.hash && project == l.project && mail == l.mail {
						state = "migrated"
					}
				}
				_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mail_import_legacy_state(source_instance,record_kind,source_id,state,importer_identity) VALUES(?,?,?,?,?)`, l.instance, l.kind, l.id, state, identity)
				return e
			})
			if err != nil {
				return err
			}
		}
	}
	var held int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mail_import_legacy_state WHERE state='held')`).Scan(&held); err != nil {
		return err
	}
	if held != 0 {
		return legacyHeldError()
	}
	return nil
}

func (s *Service) checkLegacyImports(ctx context.Context) error {
	var held int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mail_import_legacy_state WHERE state='held')`).Scan(&held); err != nil {
		return err
	}
	if held != 0 {
		return legacyHeldError()
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT l.source_instance,l.record_kind,l.source_id,l.payload_hash,l.project,l.mail_id FROM mail_import_legacy l WHERE NOT EXISTS (SELECT 1 FROM mail_import_legacy_state st WHERE st.source_instance=l.source_instance AND st.record_kind=l.record_kind AND st.source_id=l.source_id)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l legacyReservation
		if err := rows.Scan(&l.instance, &l.kind, &l.id, &l.hash, &l.project, &l.mail); err != nil {
			return err
		}
		identity, err := s.legacyOwner(ctx, l)
		if err != nil {
			return err
		}
		if identity == "" {
			return legacyHeldError()
		}
	}
	return rows.Err()
}

func legacyHeldError() error {
	return api.Errorf(api.CodeUnavailable, "legacy import reservations require explicit local reconciliation; no automatic adoption")
}

// legacyOwner uses only server-owned retained sink evidence and unchanged
// local-owner identity continuity. Source sender/recipient claims are never
// identity evidence. Missing/changed owner or sink evidence remains held.
func (s *Service) legacyOwner(ctx context.Context, l legacyReservation) (string, error) {
	if l.kind != "mail" {
		return "", nil
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, l.project)
	if err != nil {
		if api.IsCode(err, api.CodeNotFound) {
			return "", nil
		}
		return "", err
	}
	var hash string
	err = db.QueryRowContext(ctx, `SELECT payload_hash FROM mail_import_sink WHERE mail_id=?`, l.mail).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && hash != l.hash) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	r, err := getRow(ctx, db, l.project, l.mail)
	if err != nil {
		if api.IsCode(err, api.CodeNotFound) {
			return "", nil
		}
		return "", err
	}
	local := s.d.Projects.LocalIdentity()
	address := "human:" + local.Name
	if local.Kind != api.IdentityHuman || local.ID == "" || local.Created.IsZero() || ids.Time(l.mail).IsZero() || local.Created.After(ids.Time(l.mail)) {
		return "", nil
	}
	if r.From != address || r.Recipient != address || r.To != address || r.Via != "legacy import" || r.Audience != orchapi.MailAudienceParticipants || len(r.Participants) != 1 || r.Participants[0] != address || r.Origin == nil {
		return "", nil
	}
	o := r.Origin
	if o.ImportedBy != address || o.Trust != "source_reported" || o.System != "hub" || o.Instance != l.instance || o.ID != l.id || o.ImportedAt.IsZero() {
		return "", nil
	}
	return local.ID, nil
}
