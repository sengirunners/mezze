package orchcore

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// The delivery helper never uses the other SessionService methods.
type legacyMetadataSessions struct{ SessionService }
type guardedMetadataSessions struct {
	SessionService
	deliver func(context.Context, []orchapi.Session, func([]orchapi.Session) error) error
}

func (s guardedMetadataSessions) DeliverSessionMetadata(ctx context.Context, snapshots []orchapi.Session, consume func([]orchapi.Session) error) error {
	return s.deliver(ctx, snapshots, consume)
}

func TestDeliverSessionMetadataLegacyFallback(t *testing.T) {
	snapshots := []orchapi.Session{{ID: "aaaaaaaa", Project: "project", Area: "area", Title: "legacy title", State: orchapi.StateRunning}}
	sentinel := errors.New("write failed")
	for _, service := range []SessionService{nil, legacyMetadataSessions{}} {
		called := 0
		err := DeliverSessionMetadata(context.Background(), service, snapshots, func(got []orchapi.Session) error {
			called++
			if !reflect.DeepEqual(got, snapshots) {
				t.Fatalf("fallback changed snapshots: %+v", got)
			}
			return sentinel
		})
		if called != 1 || !errors.Is(err, sentinel) {
			t.Fatalf("fallback callback=%d err=%v", called, err)
		}
	}
}

func TestDeliverSessionMetadataUsesOwnerBarrier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := []orchapi.Session{{ID: "aaaaaaaa", Title: "cached secret"}, {ID: "bbbbbbbb", Title: "unaffected"}}
	sentinel := errors.New("delivery failed")
	called := 0
	service := guardedMetadataSessions{deliver: func(gotCtx context.Context, got []orchapi.Session, consume func([]orchapi.Session) error) error {
		if gotCtx != ctx || !reflect.DeepEqual(got, original) {
			t.Fatal("owner did not receive context and snapshots")
		}
		projected := append([]orchapi.Session(nil), got...)
		projected[0].Title = "[redacted]"
		return consume(projected)
	}}
	err := DeliverSessionMetadata(ctx, service, original, func(got []orchapi.Session) error {
		called++
		if got[0].Title != "[redacted]" || got[1].Title != "unaffected" {
			t.Fatalf("owner projection bypassed: %+v", got)
		}
		return sentinel
	})
	if called != 1 || !errors.Is(err, sentinel) {
		t.Fatalf("owner callback=%d err=%v", called, err)
	}
	if original[0].Title != "cached secret" {
		t.Fatal("helper changed cached input")
	}
}

func TestDeliverSessionMetadataOwnerFailureDoesNotFallback(t *testing.T) {
	sentinel := errors.New("barrier refused")
	service := guardedMetadataSessions{deliver: func(context.Context, []orchapi.Session, func([]orchapi.Session) error) error { return sentinel }}
	called := false
	err := DeliverSessionMetadata(context.Background(), service, nil, func([]orchapi.Session) error { called = true; return nil })
	if called || !errors.Is(err, sentinel) {
		t.Fatalf("owner failure bypassed: callback=%v err=%v", called, err)
	}
}
