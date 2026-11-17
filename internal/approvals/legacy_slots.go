package approvals

import (
	"context"
	"database/sql"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// migrateLegacySlots explicitly upgrades pre-namespace slots. The old schema
// did not retain a subject or token cap. Reconstruct only from registry APIs,
// never by treating an arbitrary reply address as an authenticated local caller.
// Ambiguous identities (including identities with guest-capped tokens) keep a
// party slot. This grants no rights and never changes an ask or signed receipt.
func (s *Service) migrateLegacySlots(ctx context.Context) error {
	s.legacyMu.Lock()
	defer s.legacyMu.Unlock()
	if s.legacyDone {
		return nil
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id,project,area,asked_by,dedupe FROM asks WHERE dedupe != '' AND instr(dedupe,char(31))=0 ORDER BY created,id`)
	if err != nil {
		return err
	}
	type slot struct{ id, project, area, by, key string }
	var slots []slot
	for rows.Next() {
		var a slot
		if err = rows.Scan(&a.id, &a.project, &a.area, &a.by, &a.key); err != nil {
			break
		}
		slots = append(slots, a)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range slots {
		operator, err := s.legacyOperator(ctx, a.by, a.project, a.area)
		if err != nil {
			return err
		}
		authority := "party:" + a.by
		if operator {
			authority = "operators"
		}
		key := a.area + "\x1f" + authority + "\x1f" + a.key
		err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
			// A daemon that already ran 0003 may have accepted duplicates. The oldest
			// ask retains the primary key. Duplicates keep reservations too,
			// so withdrawing one open ask cannot free another ask's slot.
			var winner string
			err := tx.QueryRowContext(ctx, `SELECT id FROM asks WHERE project=? AND area=? AND (id=? OR dedupe=?) ORDER BY created,id LIMIT 1`, a.project, a.area, a.id, key).Scan(&winner)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE asks SET dedupe=?||char(31)||'duplicate:'||id WHERE project=? AND area=? AND (id=? OR dedupe=?) AND id!=?`, key, a.project, a.area, a.id, key, winner); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE asks SET dedupe=? WHERE id=?`, key, winner)
			return err
		})
		if err != nil {
			return err
		}
	}
	s.legacyDone = true
	return nil
}

func (s *Service) legacyOperator(ctx context.Context, address, project, area string) (bool, error) {
	a, err := api.ParseAddress(address)
	if err != nil {
		return false, nil
	}
	name := ""
	switch a.Kind {
	case api.KindHuman:
		name = a.Human
	case api.KindSession:
		if orchcore.Sessions(s.d) == nil {
			return false, nil
		}
		ss, ok, e := orchcore.LookupSession(ctx, orchcore.Sessions(s.d), address)
		if e != nil {
			return false, e
		}
		if !ok {
			return false, nil
		}
		name = ss.Launcher
	}
	if name == "" {
		return false, nil
	}
	id, err := s.d.Projects.Identity(ctx, name)
	if api.IsCode(err, api.CodeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tokens, err := s.d.Projects.Tokens(ctx)
	if err != nil {
		return false, err
	}
	// Even an expired/revoked cap could have authored the legacy ask. The old
	// row cannot disambiguate it from an uncapped operator of the same identity.
	for _, t := range tokens {
		if t.Identity == id.Name && t.Role == api.RoleGuest && (t.Project == "" || t.Project == s.d.Projects.Slug(project)) && (t.Area == "" || t.Area == area) {
			return false, nil
		}
	}
	if id.ID == s.d.Projects.LocalIdentity().ID {
		return true, nil
	}
	grants, err := s.d.Projects.Grants(ctx, s.d.System(), project)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		if g.Identity == id.Name && (g.Area == "" || g.Area == area) && (g.Role == api.RoleMember || g.Role == api.RoleOwner) {
			return true, nil
		}
	}
	return false, nil
}
