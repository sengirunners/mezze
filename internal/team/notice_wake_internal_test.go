package team

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

type noticeWakeSessions struct{ orchcore.SessionService }

func (noticeWakeSessions) Lookup(context.Context, string) (orchapi.Session, bool) {
	return orchapi.Session{Project: "p"}, true
}

// A ready copy supersedes heartbeat notices, so it must count wakeability
// itself, including quiet/group overlap and direct notices that never wake.
func TestTeamNoticeReadyCopyCountsDirectWakeable(t *testing.T) {
	for _, withDirect := range []bool{false, true} {
		name := "quiet only"
		if withDirect {
			name = "quiet group plus direct"
		}
		t.Run(name, func(t *testing.T) {
			rp := testReplica(t)
			rp.ready = true
			const address = "mini/aaaaaaaa"
			ms := []orchapi.Mail{
				{ID: "1", Recipient: address, From: "mini/bbbbbbbb", Kind: orchapi.MailKindNotice, Needs: orchapi.NeedsAgent, Created: time.Now()},
				{ID: "2", Recipient: "machine:mini", From: "mini/bbbbbbbb", Kind: orchapi.MailKindNotice, Needs: orchapi.NeedsAgent, Created: time.Now()},
			}
			if withDirect {
				ms = append(ms, orchapi.Mail{ID: "3", Recipient: address, From: "mini/bbbbbbbb", Kind: orchapi.MailKindNote, Needs: orchapi.NeedsAgent, Created: time.Now()})
			}
			if err := store.Tx(context.Background(), rp.db, func(tx *sql.Tx) error {
				for _, m := range ms {
					if err := putMail(context.Background(), tx, m); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			oldCount := 99
			s := &Service{
				d:       withSessions(&core.Deps{Config: &config.Config{Machine: "mini"}}, noticeWakeSessions{}),
				Service: linked(&remote{R: api.Remote{Project: api.ProjectRef{ID: "p", Slug: "project"}, State: api.RemoteOK}, Copy: rp}),
				cache:   replicaCache{notices: map[string]map[string][]orchapi.NoticePart{"p": {address: {{Project: "p", Unread: 99, DirectWakeable: &oldCount}}}}},
			}
			parts := s.TeamNotice(address)
			if len(parts) != 1 {
				t.Fatalf("parts=%+v", parts)
			}
			n := parts[0]
			want := 0
			if withDirect {
				want = 1
			}
			if n.Unread != len(ms) || n.Quiet != 2 || n.Group != 1 || n.DirectWakeable == nil || *n.DirectWakeable != want {
				t.Fatalf("ready copy lost wake semantics or used heartbeat cache: %+v", n)
			}
			metadata, fallback := s.TeamMailNotices(context.Background(), address)
			if len(metadata) != len(ms) || len(fallback) != 0 {
				t.Fatalf("ready copy hook metadata: %+v fallback=%+v", metadata, fallback)
			}
			byID := map[string]orchcore.MailNotice{}
			for _, m := range metadata {
				byID[m.ID] = m
			}
			if byID["1"].Group || !byID["2"].Group || byID["1"].From != "mini/bbbbbbbb" || byID["1"].Project != "p" || byID["2"].From != "" || byID["2"].Ref != "" {
				t.Fatalf("incorrect direct/group metadata: %+v", metadata)
			}
		})
	}
}

// A ready copy's count leaves out an ask held for an outside party: its mail
// waits on that party, not on the reader, and the copy knows only
// from the held_for its mail.sent event carried.
func TestTeamNoticeReadyCopySkipsHeldAsks(t *testing.T) {
	rp := testReplica(t)
	rp.ready = true
	ms := []orchapi.Mail{
		{ID: "1", Recipient: "mini/aaaaaaaa", From: "mini/aaaaaaaa", Kind: orchapi.MailKindNote, Needs: orchapi.NeedsHuman, HeldFor: "Santa Monica", Created: time.Now()},
		{ID: "2", Recipient: "mini/aaaaaaaa", From: "mini/aaaaaaaa", Kind: orchapi.MailKindNote, Needs: orchapi.NeedsHuman, Created: time.Now()},
	}
	if err := store.Tx(context.Background(), rp.db, func(tx *sql.Tx) error {
		for _, m := range ms {
			if err := putMail(context.Background(), tx, m); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := &Service{
		d:       withSessions(&core.Deps{Config: &config.Config{Machine: "mini"}}, noticeWakeSessions{}),
		Service: linked(&remote{R: api.Remote{Project: api.ProjectRef{ID: "p", Slug: "project"}, State: api.RemoteOK, Identity: "ivo"}, Copy: rp}),
	}
	parts := s.TeamNotice("human:ivo")
	if len(parts) != 1 || parts[0].Unread != 1 {
		t.Fatalf("held ask counted on the copy: %+v", parts)
	}
}
