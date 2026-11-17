package session

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"testing"

	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// A partial service or a registry released during shutdown holds no durable
// PID/start/home evidence. Neither the direct signal path nor late-exit cleanup
// may panic or treat that absence as permission to signal a process group.
func TestTaskGroupWithoutRegistryHasNoSignalAuthority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service *Service
	}{
		{"nil_service", nil},
		{"nil_deps", &Service{}},
		{"nil_store", &Service{d: &core.Deps{}}},
		{"nil_registry", &Service{d: &core.Deps{Store: &store.Store{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No PID, process, config, home or logger is supplied. Reaching any
			// downstream authority check (or warning) is a regression here.
			if err := tc.service.signalRecordedGroup(context.Background(), "unrecorded-task", syscall.SIGKILL); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("missing registry authorized signalling or lost absence: %v", err)
			}
			tc.service.killRecordedTaskGroup(context.Background(), "unrecorded-task")
		})
	}
}

func TestTaskGroupAfterRegistryShutdownHasNoSignalAuthority(t *testing.T) {
	st, err := storetest.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Store: st}}
	if err := s.signalRecordedGroup(context.Background(), "finished-task", syscall.SIGKILL); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("closed registry authorized signalling or lost absence: %v", err)
	}
	s.killRecordedTaskGroup(context.Background(), "finished-task")
}

// A run finishing during daemon Close reached completeTask after
// Store.Close cleared Registry and panicked on the nil *sql.DB.
func TestCompleteTaskAfterStoreCloseDoesNotPanic(t *testing.T) {
	st, err := storetest.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s.taskIDs.Store("abcd2345", true)
	s.completeTask("abcd2345")
	if _, ok, err := s.loadErr(context.Background(), "abcd2345"); ok || !errors.Is(err, errRegistryClosed) {
		t.Fatalf("load after Store.Close: ok=%v err=%v", ok, err)
	}
}
