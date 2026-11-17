package mail

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

const MaxImportBytes = 1 << 20
const MaxImportBodyBytes = 256 << 10

var _ orchcore.LegacyMailImporter = (*Service)(nil)

func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) {
	if raw := r.URL.Query().Get("project"); raw != "" {
		r = r.WithContext(core.WithResolveInput(r.Context(), project.ResolveInput{Explicit: raw, ExplicitFrom: project.RuleFlag}))
	}
	if err := s.authorizeImporter(r.Context(), core.SubjectOf(r.Context())); err != nil {
		api.WriteError(w, err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxImportBytes+1))
	if err != nil || len(raw) > MaxImportBytes {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "import body exceeds finite read budget"))
		return
	}
	var req orchapi.LegacyMailImportRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "invalid import artifact"))
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "import requires one JSON object"))
		return
	}
	target, err := s.d.WriteTarget(r.Context(), "", authz.Write)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.ImportLegacy(r.Context(), core.SubjectOf(r.Context()), target.Project, target.Area, req)
	respond(w, v, err)
}

// ImportLegacy uses a registry reservation then a replayable project sink.
// The registry tuple is immutable even if the daemon dies before the sink
// commits. Retry the identical artifact to complete a reserved row. No cursor
// or checkpoint is advanced here, and no source mutation or send path is used.
func (s *Service) ImportLegacy(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, req orchapi.LegacyMailImportRequest) (orchapi.LegacyMailImportResult, error) {
	out := orchapi.LegacyMailImportResult{DryRun: req.DryRun, Records: []orchapi.LegacyMailImportRecordResult{}}
	if err := s.authorizeImporter(ctx, sub); err != nil {
		return out, err
	}
	if sub.Node != "" && (sub.Cap.Project != p.ID || !s.d.Projects.IsTeam(p.ID)) {
		return out, api.Errorf(api.CodeForbidden, "import requires the authenticated project authority")
	}
	if err := sub.Check(authz.Write, authz.Object{Project: p.ID, Area: area}, "project", p.ID); err != nil {
		return out, err
	}
	db, err := s.d.Store.Project(sub.Scope, authz.Write, p.ID)
	if err != nil {
		return out, err
	}
	raw, marshalErr := json.Marshal(req)
	if marshalErr != nil {
		return out, api.Errorf(api.CodeBadRequest, "invalid import timestamp or JSON payload")
	}
	if len(raw) > MaxImportBytes || len(req.Records) == 0 || len(req.Records) > 100 || len(req.SourceIDs) == 0 || len(req.SourceIDs) > 100 || len(req.Mappings) > 100 || (strings.TrimSpace(req.SourceInstance) == "" || strings.TrimSpace(req.SourceInstance) != req.SourceInstance || strings.EqualFold(req.SourceInstance, "hub")) || len(req.SourceInstance) > 200 || req.MappingVersion == "" || len(req.MappingVersion) > 200 {
		return out, api.Errorf(api.CodeBadRequest, "import requires stable source_instance, mapping_version, 1..100 explicit source_ids and records; maximum 1 MiB")
	}
	allowed := map[string]bool{}
	for _, id := range req.SourceIDs {
		if id == "" || len(id) > 200 || allowed[id] {
			return out, api.Errorf(api.CodeBadRequest, "invalid or duplicate source_ids")
		}
		allowed[id] = true
	}
	seen := map[string]bool{}
	// Every mapping must target exactly this authenticated current person.
	mappings := map[string]orchapi.LegacyMailMapping{}
	for _, m := range req.Mappings {
		key := m.SourceProject + "\x00" + m.SourceRecipient
		if m.SourceProject == "" || m.SourceRecipient == "" || m.Recipient != sub.Address {
			return out, api.Errorf(api.CodeBadRequest, "explicit mappings must target the authenticated current person")
		}
		if _, ok := mappings[key]; ok {
			return out, api.Errorf(api.CodeBadRequest, "ambiguous mapping")
		}
		mappings[key] = m
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.prepareLegacyImports(ctx, req.DryRun); err != nil {
		return out, err
	}
	for _, r := range req.Records {
		result := orchapi.LegacyMailImportRecordResult{SourceID: r.ID}
		status := func(v, reason string) { result.Status, result.Reason = v, reason; out.Partial = true }
		mapping, ok := mappings[r.Project+"\x00"+r.Recipient]
		switch {
		case !allowed[r.ID] || seen[r.ID]:
			status("invalid", "record outside explicit IDs or duplicated")
		case !ok:
			status("unmapped", "no explicit source project and recipient mapping")
		case r.Kind != "mail" || !r.BodyComplete || r.Sender == "" || len(r.Sender) > 200 || r.SentAt.IsZero() || r.SentAt.Year() < 1970 || r.SentAt.Year() > 9999 || !utf8.ValidString(r.Body) || textclean.Blank(r.Body) || len(r.Body) > MaxImportBodyBytes || r.Delivery.DeliveredCount < 0:
			status("invalid", "requires full body, mail kind, sender, original timestamp and bounded valid delivery fields")
		default:
			// Hash the original artifact and immutable destination, before text cleaning.
			hash := payloadHash(struct {
				Record                 orchapi.LegacyMailRecord
				Mapping                orchapi.LegacyMailMapping
				Project, Area, Version string
			}{r, mapping, p.ID, area, req.MappingVersion})
			id := newID()
			check := func(q querier) error {
				var prior, project string
				err := q.QueryRowContext(ctx, `SELECT payload_hash,project,mail_id FROM mail_imports WHERE importer_identity=? AND source_instance=? AND record_kind=? AND source_id=?`, sub.Identity.ID, req.SourceInstance, r.Kind, r.ID).Scan(&prior, &project, &id)
				if errors.Is(err, sql.ErrNoRows) {
					if !req.DryRun {
						return nil
					}
					// Preview an attributable old row without mutating its journal.
					var old legacyReservation
					old.instance, old.kind, old.id = req.SourceInstance, r.Kind, r.ID
					e := q.QueryRowContext(ctx, `SELECT payload_hash,project,mail_id FROM mail_import_legacy WHERE source_instance=? AND record_kind=? AND source_id=?`, old.instance, old.kind, old.id).Scan(&old.hash, &old.project, &old.mail)
					if errors.Is(e, sql.ErrNoRows) {
						return nil
					}
					if e != nil {
						return e
					}
					owner, e := s.legacyOwner(ctx, old)
					if e != nil {
						return e
					}
					if owner != sub.Identity.ID {
						return nil
					}
					prior, project, id = old.hash, old.project, old.mail
					err = nil
				}
				if err != nil {
					return err
				}
				if prior != hash || project != p.ID {
					return api.Errorf(api.CodeConflict, "source identity has different immutable payload or mapping")
				}
				return nil
			}
			if req.DryRun {
				err = check(s.d.Store.Registry())
			} else {
				err = store.DurableTx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
					if err := check(tx); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mail_imports(importer_identity,source_instance,record_kind,source_id,payload_hash,project,mail_id) VALUES (?,?,?,?,?,?,?)`, sub.Identity.ID, req.SourceInstance, r.Kind, r.ID, hash, p.ID, id)
					return err
				})
			}
			if api.IsCode(err, api.CodeConflict) {
				status("conflict", "source identity has different immutable payload or mapping")
				break
			}
			if err != nil {
				return out, err
			}
			var sinkHash string
			err = db.QueryRowContext(ctx, `SELECT payload_hash FROM mail_import_sink WHERE mail_id=?`, id).Scan(&sinkHash)
			if err == nil {
				if sinkHash != hash {
					status("conflict", "sink hash differs")
				} else {
					result.Status, result.NativeID = "already_present", id
				}
				break
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
			if req.DryRun {
				result.Status = "would_import"
				break
			}
			delivery := r.Delivery
			if delivery.AckBy == "digest" && delivery.AckAt != nil && delivery.AckAt.Equal(r.SentAt) && delivery.DigestAt == nil && delivery.DeliveredCount == 0 {
				delivery.State = "digest_held"
			}
			m := row{Mail: orchapi.Mail{ID: id, Project: p.ID, Area: area, From: sub.Address, To: mapping.Recipient, Recipient: mapping.Recipient, Via: "legacy import", Kind: orchapi.MailKindNote, Needs: orchapi.NeedsHuman, Body: textclean.Clean(r.Body), Created: r.SentAt,
				Audience: orchapi.MailAudienceParticipants, Participants: []string{mapping.Recipient}, ReplyUnavailable: "legacy_return_route_unverified",
				Origin: &orchapi.MailOrigin{System: "hub", Instance: req.SourceInstance, ID: r.ID, ThreadID: r.ThreadID, ParentID: r.ParentID, SourceKind: r.SourceKind, SourceRef: r.SourceRef, Sender: r.Sender, Recipient: r.Recipient, SentAt: r.SentAt, ImportedAt: s.now(), ImportedBy: sub.Address, Trust: "source_reported"}, LegacyDelivery: &delivery, Thread: id}}
			// Parent/thread references remain source-only until a verified return mapping exists.
			err = s.commit(ctx, db, p.ID, func(tx *sql.Tx) ([]pub, error) {
				var prior string
				e := tx.QueryRowContext(ctx, `SELECT payload_hash FROM mail_import_sink WHERE mail_id=?`, id).Scan(&prior)
				if e == nil {
					if prior != hash {
						return nil, api.Errorf(api.CodeConflict, "sink hash differs")
					}
					result.Status = "already_present"
					return nil, nil
				}
				if !errors.Is(e, sql.ErrNoRows) {
					return nil, e
				}
				if e := insertRow(ctx, tx, m); e != nil {
					return nil, e
				}
				if _, e := tx.ExecContext(ctx, `INSERT INTO mail_import_sink(mail_id,payload_hash) VALUES (?,?)`, id, hash); e != nil {
					return nil, e
				}
				result.Status = "imported"
				return []pub{{orchapi.EventMailImported, m}}, nil
			})
			if err != nil {
				return out, err
			}
			result.NativeID = id
		}
		seen[r.ID] = true
		out.Records = append(out.Records, result)
	}
	for _, id := range req.SourceIDs {
		if !seen[id] {
			out.Partial = true
			out.Records = append(out.Records, orchapi.LegacyMailImportRecordResult{SourceID: id, Status: "missing", Reason: "explicit source ID absent from full-body artifact"})
		}
	}
	return out, nil
}
