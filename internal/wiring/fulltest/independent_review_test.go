package fulltest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/events"
)

func TestIndependentPrivateMailSessionMetadataHidden(t *testing.T) {
	f := newLeakFixture(t)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	addr := f.sessions["harbor/solver"]
	sub, err := f.dm.Deps.Projects.FromToken(ctx, f.guestTok, true)
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := f.dm.Deps.Events.Subscribe(sub, events.Filter{Types: []string{orchapi.EventSessionStateChanged}, Subject: addr}, 16)
	defer cancel()
	m, err := orchclient.Send(ctx, f.ownerAt("harbor/solver", addr), orchapi.SendMailRequest{To: "human:ivo", Body: "invented private state canary", Kind: orchapi.MailKindReport, Audience: orchapi.MailAudienceParticipants})
	if err != nil {
		t.Fatal(err)
	}
	guest := client.WithHandler(f.dm.Handler)
	guest.Token = f.guestTok
	if _, err := orchclient.GetMail(ctx, guest, m.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("mail not hidden: %v", err)
	}
	ss, err := orchclient.Session(ctx, guest, addr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ss.Note, m.ID[len(m.ID)-6:]) {
		t.Fatalf("nonparticipant guest learned private mail suffix from session note: %q", ss.Note)
	}
	// The mail is a report (SPEC 9 "Reports"): its sender waits on nobody, so
	// no session state, public or not, carries anything about it.
	if m.Kind != orchapi.MailKindReport {
		t.Fatalf("an agent's private mail to a person: %+v", m)
	}
	if ss.State == orchapi.StateNeedsHuman || strings.Contains(ss.Note, "waiting on mail") {
		t.Fatalf("a report parked its sender: %+v", ss)
	}
	select {
	case ev := <-ch:
		if strings.Contains(string(ev.Data), m.ID[len(m.ID)-6:]) {
			t.Fatalf("private identifier in public session event: %s", ev.Data)
		}
	default:
	}
}
