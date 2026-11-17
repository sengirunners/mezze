package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/core"
)

func TestSessionMetadataDeliveryReprojectsCachedCompletedScrub(t *testing.T) {
	const secret = "cached cross-brick metadata secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	ctx := context.Background()
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET title=?,note=?,activity=? WHERE id=?`, secret, secret, secret, r.ID); err != nil {
		t.Fatal(err)
	}
	cached, ok := h.s.load(ctx, r.ID)
	if !ok {
		t.Fatal("fixture session missing")
	}
	cached.State = orchapi.StateNeedsHuman // preserve the caller's lifecycle snapshot
	cached.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeFailed, Note: secret, Evidence: secret}
	cached.Needs = &orchapi.SessionNeeds{Kind: orchapi.NeedsInput, Subagent: "child-id"}
	if err := h.s.scrubSessionOutputs(ctx, core.Scrub{Project: r.Project, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	snapshots := []orchapi.Session{cached.Session, {ID: "bbbbbbbb", Address: "elsewhere/bbbbbbbb", Machine: "elsewhere", Project: "other", State: orchapi.StateIdle, Title: "unrelated title"}}
	var sent bytes.Buffer
	sentinel := errors.New("consumer write result")
	err := orchcore.DeliverSessionMetadata(ctx, h.s, snapshots, func(projected []orchapi.Session) error {
		if len(projected) != 2 || projected[0].Address != cached.Address || projected[0].Project != cached.Project || projected[0].State != cached.State || projected[0].Needs == nil || projected[0].Needs.Subagent != "child-id" || projected[1].Address != snapshots[1].Address || projected[1].Title != "unrelated title" {
			t.Fatalf("projection changed ordering, identity or lifecycle: %+v", projected)
		}
		if err := json.NewEncoder(&sent).Encode(projected); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback result lost: %v", err)
	}
	if strings.Contains(sent.String(), secret) {
		t.Fatalf("completed scrub lost at actual delivery: %s", sent.String())
	}
	if cached.Title != secret || cached.Outcome.Evidence != secret {
		t.Fatal("delivery mutated cached input")
	}
}

func TestSessionMetadataDeliveryHoldsBarrierThroughActualWrite(t *testing.T) {
	const secret = "delivery linearization private secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	ctx := context.Background()
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET title=? WHERE id=?`, secret, r.ID); err != nil {
		t.Fatal(err)
	}
	cached, ok := h.s.load(ctx, r.ID)
	if !ok {
		t.Fatal("fixture session missing")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	deliveryDone := make(chan error, 1)
	deliveryFinished := make(chan struct{})
	var scrubFinished chan struct{}
	var sent bytes.Buffer
	go func() {
		defer close(deliveryFinished)
		deliveryDone <- h.s.DeliverSessionMetadata(ctx, []orchapi.Session{cached.Session}, func(projected []orchapi.Session) error {
			close(entered)
			<-release
			return json.NewEncoder(&sent).Encode(projected)
		})
	}()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-deliveryFinished:
		case <-time.After(3 * time.Second):
			t.Error("metadata delivery goroutine did not finish")
		}
		if scrubFinished != nil {
			select {
			case <-scrubFinished:
			case <-time.After(3 * time.Second):
				t.Error("metadata scrub goroutine did not finish")
			}
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata callback did not start")
	}
	scrubStarted := make(chan struct{})
	scrubDone := make(chan error, 1)
	scrubFinished = make(chan struct{})
	go func() {
		defer close(scrubFinished)
		close(scrubStarted)
		scrubDone <- h.s.scrubSessionOutputs(ctx, core.Scrub{Project: r.Project, Texts: []string{secret}})
	}()
	<-scrubStarted
	select {
	case err := <-scrubDone:
		t.Fatalf("redaction passed an unfinished metadata write: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-deliveryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metadata delivery did not finish")
	}
	select {
	case err := <-scrubDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("redaction did not resume")
	}
	// The send linearized before the scrub. A later cached send must instead
	// receive the owner's scrubbed text, even though its original value survives.
	if !strings.Contains(sent.String(), secret) {
		t.Fatal("fixture did not deliver the pre-redaction snapshot")
	}
	sent.Reset()
	if err := h.s.DeliverSessionMetadata(ctx, []orchapi.Session{cached.Session}, func(projected []orchapi.Session) error { return json.NewEncoder(&sent).Encode(projected) }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent.String(), secret) {
		t.Fatalf("later metadata send reused cached secret: %s", sent.String())
	}
}
