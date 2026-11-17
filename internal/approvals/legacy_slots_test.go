package approvals_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Restore the actual pre-effects schema AND pre-namespace slot representation.
// Reopening runs the real schema and data upgrade, with mail/receipts intact.
func downgradeDecisionSlots(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	reg := h.dm.Deps.Store.Registry()
	rows, err := reg.QueryContext(ctx, `SELECT id,dedupe FROM asks WHERE dedupe!=''`)
	if err != nil {
		t.Fatal(err)
	}
	slots := map[string]string{}
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			t.Fatal(err)
		}
		slots[id] = key[strings.LastIndex(key, "\x1f")+1:]
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for id, key := range slots {
		if _, err := reg.ExecContext(ctx, `UPDATE asks SET dedupe=? WHERE id=?`, key, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`DROP TABLE decision_effects`,
		`ALTER TABLE decisions DROP COLUMN mail_error`,
		`ALTER TABLE decisions DROP COLUMN mailed`,
		`DROP INDEX asks_work`,
		`DROP INDEX asks_once`,
		`CREATE UNIQUE INDEX asks_once ON asks(project,dedupe) WHERE dedupe!=''`,
		`DELETE FROM schema_migrations WHERE name='approvals/0003_effects'`,
	} {
		if _, err := reg.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyDecisionSlotUpgrade(t *testing.T) {
	for _, kind := range []string{"plain", "work", "program"} {
		for _, answered := range []bool{false, true} {
			for _, guest := range []bool{false, true} {
				name := kind
				if answered {
					name += "/answered"
				} else {
					name += "/open"
				}
				if guest {
					name += "/guest"
				} else {
					name += "/operator"
				}
				t.Run(name, func(t *testing.T) {
					h := newHarness(t)
					ctx := context.Background()
					h.pair("ivo", ipPhone)
					author, _ := h.session("face0101")
					if guest {
						author = h.guest()
					}
					req := orchapi.NewAskRequest{Card: card(), Topic: "legacy-topic"}
					switch kind {
					case "work":
						it := h.workItem("legacy work")
						req.Subject.Work = it.ID
					case "program":
						req.For = &orchapi.AskFor{Group: "Cedars", Contact: "Ann"}
					}
					a := must(orchclient.NewAsk(ctx, author, req))
					var receipt string
					if answered {
						receipt = must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a")).Receipt.Sig
					}
					token := author.Token
					downgradeDecisionSlots(t, h)
					reopenDecisions(t, h)
					// Repeat with another operator address; operators must share the old slot.
					repeat := h.ownerIn("harbor/solver", "")
					if guest {
						repeat = client.WithHandler(h.dm.Handler)
						repeat.Token = token
						repeat.Project = "harbor"
						repeat.ProjectFrom = "env"
					}
					if _, err := orchclient.NewAsk(ctx, repeat, req); !api.IsCode(err, api.CodeConflict) {
						t.Fatalf("legacy slot lost: %v", err)
					}
					got := must(orchclient.GetAsk(ctx, repeat, a.ID))
					if answered && (got.Decision == nil || got.Decision.Receipt.Sig != receipt) {
						t.Fatal("upgrade changed signed decision")
					}
					if guest {
						// A guest keeps its own slot without holding it against operators or a
						// different guest. Both asks must succeed after the real upgrade.
						must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), req))
						tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "secondguest", Project: "harbor", Area: "solver", Role: api.RoleGuest}))
						second := client.WithHandler(h.dm.Handler)
						second.Token = tok.Secret
						second.Project = "harbor"
						second.ProjectFrom = "env"
						must(orchclient.NewAsk(ctx, second, req))
					} else {
						// The upgraded operator slot does not become an oracle for guests.
						must(orchclient.NewAsk(ctx, h.guest(), req))
					}
					// The data rewrite is durable and idempotent on a second actual reopen.
					reopenDecisions(t, h)
					var key string
					if err := h.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT dedupe FROM asks WHERE id=?`, a.ID).Scan(&key); err != nil {
						t.Fatal(err)
					}
					if strings.Count(key, "\x1f") != 2 {
						t.Fatalf("not explicitly namespaced: %q", key)
					}
				})
			}
		}
	}
}

func TestLegacyHiddenDecisionSlotUpgrade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	must(h.owner.AddArea(ctx, "harbor", api.Area{Name: "secret"}))
	a := must(orchclient.NewAsk(ctx, h.ownerIn("harbor/secret", ""), orchapi.NewAskRequest{Card: card(), Topic: "hidden-topic"}))
	downgradeDecisionSlots(t, h)
	reopenDecisions(t, h)
	req := orchapi.NewAskRequest{Card: card(), Topic: "hidden-topic"}
	must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), req))
	guest := h.guest()
	must(orchclient.NewAsk(ctx, guest, req))
	_, hidden := orchclient.GetAsk(ctx, guest, a.ID)
	_, missing := orchclient.GetAsk(ctx, guest, "01JZZZZZZZZZZZZZZZZZZZZZZZ")
	if !api.IsCode(hidden, api.CodeNotFound) || !api.IsCode(missing, api.CodeNotFound) {
		t.Fatalf("hidden=%v missing=%v", hidden, missing)
	}
}

func TestLivePairingCodeRejectsSameCallerUntilExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	if _, err := orchclient.AddApprover(ctx, h.owner, "ivo"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("same caller rotated live code: %v", err)
	}
	h.clock.add(10 * time.Minute)
	next := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	if _, err := orchclient.Pair(ctx, h.device(ipPhone), first.Code); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("old code: %v", err)
	}
	must(orchclient.Pair(ctx, h.device(ipPhone), next.Code))
}

// At 0003 the old slot could coexist with a new ask. Reconcile deterministically
// without dropping either open work or an answered receipt, and preserve withdrawal.
func TestLegacySlotReconcilesAlreadyUpgradedDuplicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	owner := h.ownerIn("harbor/solver", "")
	req := orchapi.NewAskRequest{Card: card(), Topic: "existing-slot"}
	old := must(orchclient.NewAsk(ctx, owner, req))
	receipt := must(orchclient.Decide(ctx, h.device(ipPhone), old.ID, "a")).Receipt.Sig
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE asks SET dedupe='topic:existing-slot' WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Second)
	newer := must(orchclient.NewAsk(ctx, owner, req))
	// A withdrawn legacy slot must remain free even after reopening.
	freeReq := orchapi.NewAskRequest{Card: card(), Topic: "withdrawn-slot"}
	free := must(orchclient.NewAsk(ctx, owner, freeReq))
	must(orchclient.WithdrawAsk(ctx, owner, free.ID))
	reopenDecisions(t, h)
	owner = h.ownerIn("harbor/solver", "")
	if _, err := orchclient.NewAsk(ctx, owner, req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("duplicate slot: %v", err)
	}
	got := must(orchclient.GetAsk(ctx, owner, old.ID))
	if got.Decision.Receipt.Sig != receipt {
		t.Fatal("receipt changed")
	}
	if got := must(orchclient.GetAsk(ctx, owner, newer.ID)); got.Status != orchapi.AskOpen {
		t.Fatalf("lost open ask: %+v", got)
	}
	var winner string
	if err := h.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT id FROM asks WHERE dedupe=?`, "solver\x1foperators\x1ftopic:existing-slot").Scan(&winner); err != nil || winner != old.ID {
		t.Fatalf("winner=%s err=%v", winner, err)
	}
	must(orchclient.NewAsk(ctx, owner, freeReq))
}

// The local identity may also have used a guest-capped token. The old address
// alone is insufficient evidence to reserve an operator's slot in that case.
func TestLegacyGuestCapDoesNotBecomeOperatorSlot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "ivo", Project: "harbor", Area: "solver", Role: api.RoleGuest}))
	guest := client.WithHandler(h.dm.Handler)
	guest.Token = tok.Secret
	guest.Project = "harbor"
	guest.ProjectFrom = "env"
	req := orchapi.NewAskRequest{Card: card(), Topic: "capped-slot"}
	a := must(orchclient.NewAsk(ctx, guest, req))
	downgradeDecisionSlots(t, h)
	reopenDecisions(t, h)
	guest = client.WithHandler(h.dm.Handler)
	guest.Token = tok.Secret
	guest.Project = "harbor"
	guest.ProjectFrom = "env"
	if _, err := orchclient.NewAsk(ctx, guest, req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("guest slot lost: %v", err)
	}
	must(orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), req))
	var key string
	if err := h.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT dedupe FROM asks WHERE id=?`, a.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "solver\x1fparty:human:ivo\x1ftopic:capped-slot" {
		t.Fatalf("guest promoted: %q", key)
	}
}

func TestLegacyDuplicateWithdrawalKeepsOtherReservation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.ownerIn("harbor/solver", "")
	req := orchapi.NewAskRequest{Card: card(), Topic: "duplicate-slot"}
	old := must(orchclient.NewAsk(ctx, owner, req))
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE asks SET dedupe='topic:duplicate-slot' WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Second)
	newer := must(orchclient.NewAsk(ctx, owner, req))
	reopenDecisions(t, h)
	owner = h.ownerIn("harbor/solver", "")
	must(orchclient.WithdrawAsk(ctx, owner, old.ID))
	if _, err := orchclient.NewAsk(ctx, owner, req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("withdrawal freed other open slot: %v", err)
	}
	must(orchclient.WithdrawAsk(ctx, owner, newer.ID))
	must(orchclient.NewAsk(ctx, owner, req))
}

func TestLegacyMemberSlotSharesOperatorNamespace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok := must(h.owner.MintToken(ctx, api.MintTokenRequest{As: "member", Project: "harbor", Area: "solver", Role: api.RoleMember}))
	member := client.WithHandler(h.dm.Handler)
	member.Token = tok.Secret
	member.Project = "harbor"
	member.ProjectFrom = "env"
	req := orchapi.NewAskRequest{Card: card(), Topic: "member-slot"}
	a := must(orchclient.NewAsk(ctx, member, req))
	downgradeDecisionSlots(t, h)
	reopenDecisions(t, h)
	if _, err := orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("member slot did not survive in shared operator namespace: %v", err)
	}
	var key string
	if err := h.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT dedupe FROM asks WHERE id=?`, a.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "solver\x1foperators\x1ftopic:member-slot" {
		t.Fatalf("member slot: %q", key)
	}
}
