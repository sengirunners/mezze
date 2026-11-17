package orchcore

import (
	"context"
	"errors"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// hookSessions is a session module with names, caps and holders.
type hookSessions struct {
	SessionService
	blockers []orchapi.Session
}

func (hookSessions) ResolveName(_ context.Context, ref string) (orchapi.Session, bool) {
	return orchapi.Session{Address: "mini/abcd1234"}, ref == "mini/worker"
}

func (hookSessions) SessionCap(address string) (string, authz.Relation, bool) {
	return "p1", authz.Guest, address == "mini/abcd1234"
}

func (hookSessions) HolderReport() *orchapi.HolderReport { return nil }

func (h hookSessions) RestartBlockers(context.Context) []orchapi.Session { return h.blockers }

type hookHomes struct{ HomeService }

func (hookHomes) HomeBackup() *orchapi.HomeBackup    { return &orchapi.HomeBackup{Kind: "file"} }
func (hookHomes) Heartbeat() *orchapi.HeartbeatBrief { return nil }

// Wire registers the hooks once; they answer from the Services fields at
// call time, as the base used to from the core.Deps fields.
func TestWireRegistersHooks(t *testing.T) {
	ctx := context.Background()
	d := &core.Deps{Config: &config.Config{Machine: "mini"}}
	o := Wire(d)
	if d.SessionHooks() != nil {
		t.Fatal("session hooks with no session module")
	}
	if p := d.HealthParts(ctx); p != nil {
		t.Fatalf("health parts with no modules: %v", p)
	}
	if err := d.Notify(ctx, core.Notice{To: "owners", Body: "x"}); !errors.Is(err, core.ErrNoNotifier) {
		t.Fatalf("notify with no mail module: %v", err)
	}

	o.Sessions, o.Homes = hookSessions{}, hookHomes{}
	h := d.SessionHooks()
	if h == nil || h.Name == nil || h.Cap == nil || h.Seen != nil || h.End != nil || h.Keep != nil {
		t.Fatalf("hooks %+v", h)
	}
	if a, ok := h.Name(ctx, "mini/worker"); !ok || a != "mini/abcd1234" {
		t.Fatalf("name: %q %v", a, ok)
	}
	if p, r, ok := h.Cap("mini/abcd1234"); !ok || p != "p1" || r != authz.Guest {
		t.Fatalf("cap: %q %q %v", p, r, ok)
	}
	if err := h.StopBlocked(ctx); err != nil {
		t.Fatalf("no blockers refused: %v", err)
	}
	o.Sessions = hookSessions{blockers: []orchapi.Session{{Address: "mini/abcd1234", Agent: "claude"}}}
	if err := d.SessionHooks().StopBlocked(ctx); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("blockers: %v", err)
	}
	parts := d.HealthParts(ctx)
	if len(parts) != 1 || string(parts["backup"]) != `{"kind":"file","every_s":0,"last_ok":null}` {
		t.Fatalf("health parts: %v", parts)
	}
	// A pointer module's hooks are built once.
	ptr := &hookSessions{}
	o.Sessions = ptr
	first := d.SessionHooks()
	if d.SessionHooks() != first {
		t.Fatal("hooks rebuilt for the same module")
	}
	o.Sessions = &hookSessions{} // a test swapping the module is heard
	if d.SessionHooks() == first {
		t.Fatal("the hooks of a swapped-out module answered")
	}
	if Wire(d) != o {
		t.Fatal("Wire provided twice")
	}
}

// Every getter is nil on a Deps nothing wired.
func TestGettersUnwired(t *testing.T) {
	d := &core.Deps{Config: &config.Config{Machine: "mini"}}
	if Mail(d) != nil || Sessions(d) != nil || Homes(d) != nil || Work(d) != nil || Trackers(d) != nil ||
		Issues(d) != nil || DevicePerson(d) != nil || DevicePushOf(d) != nil || People(d) != nil {
		t.Fatal("a getter answered on an unwired Deps")
	}
	if d.SessionHooks() != nil || d.HealthParts(context.Background()) != nil {
		t.Fatal("hooks on an unwired Deps")
	}
}

// Wire is the only way to the Services, and the first Wire registers the
// hooks: a wiring path cannot set the session module without them.
func TestWireIsOnceAndRegistersHooks(t *testing.T) {
	d := &core.Deps{Config: &config.Config{Machine: "mini"}}
	o := Wire(d)
	if Wire(d) != o || of(d) != o {
		t.Fatal("Wire twice gave two Services")
	}
	o.Sessions = hookSessions{}
	if Sessions(d) == nil || d.SessionHooks() == nil || d.SessionHooks().Cap == nil {
		t.Fatal("a session module set through Wire has no hooks")
	}
	for _, key := range []string{"backup", "heartbeat", "holders"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("health part %q was not registered by Wire", key)
				}
			}()
			d.AddHealthPart(key, func(context.Context) any { return nil })
		}()
	}
	if err := d.Notify(context.Background(), core.Notice{To: "owners"}); !errors.Is(err, core.ErrNoNotifier) {
		t.Fatalf("Wire's notifier with no mail module: %v", err)
	}
	o.Mail = sentMail{}
	if err := d.Notify(context.Background(), core.Notice{To: "owners"}); err != nil {
		t.Fatalf("Wire's notifier with a mail module: %v", err)
	}
}

type sentMail struct{ MailService }

func (sentMail) Send(context.Context, *authz.Subject, api.ProjectRef, string, orchapi.SendMailRequest) (orchapi.Mail, error) {
	return orchapi.Mail{}, nil
}
