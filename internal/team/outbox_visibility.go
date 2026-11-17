package team

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

var outboxVisibilityMigrations = []store.Migration{
	{Kind: store.KindReplica, Name: "team/0007_outbox_visibility", Script: `ALTER TABLE outbox ADD COLUMN visibility TEXT;`},
}

// A queued operation keeps its server-resolved audience independently of the
// copy: a resnapshot or parent removal must never make a private reply public.
// Nil readers means area visibility; an empty list means nobody.
type outVisibility struct {
	Area    string   `json:"area"`
	Readers []string `json:"readers"`
}

func (rp *replica) writeVisibility(ctx context.Context, kind, path string, body []byte, actor string) outVisibility {
	var v outVisibility
	if kind != "mail" && kind != "reply" && kind != "ack" {
		return v
	}
	v.Readers = []string{actor} // unresolved parents and invalid audiences fail closed
	var req orchapi.SendMailRequest
	if json.Unmarshal(body, &req) != nil {
		return v
	}
	v.Area = req.Area
	if kind == "mail" && orchapi.IsMailGroup(req.To) {
		if _, members, err := orchapi.MailGroup(actor, req.To); err == nil {
			v.Readers = members
		}
		return v
	}
	parentID := req.ReplyTo
	if kind != "mail" {
		parentID, _ = segs(path, "/v0/mail/")
	}
	if parentID != "" {
		if parent, ok := rp.findMail(ctx, parentID, actor); ok {
			v.Area = parent.Area
			if parent.Audience == "" {
				v.Readers = nil
			} else if parent.Audience == orchapi.MailAudienceParticipants {
				v.Readers = append([]string{}, parent.Participants...)
			}
		}
		return v
	}
	if req.Audience == "" {
		v.Readers = nil
		return v
	}
	if req.Audience == orchapi.MailAudienceParticipants {
		if to, err := api.ParseAddress(req.To); err == nil && (to.Kind == api.KindHuman || to.Kind == api.KindSession) {
			v.Readers = []string{actor, to.String()}
		}
	}
	// An empty actor must never authorize an unauthenticated reader.
	if strings.TrimSpace(actor) == "" {
		v.Readers = []string{}
	}
	return v
}
