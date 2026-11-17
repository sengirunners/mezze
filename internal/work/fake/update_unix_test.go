//go:build unix

package fake

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func TestUpdateHoldsCLIStateLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := (&State{}).Save(path); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, func(s *State) {
		// Probe the same stable sidecar used by tx: atomic rename of the
		// state file must not replace the inode on which writers lock.
		f, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			if err == nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			t.Fatalf("Update callback does not hold the state lock: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSerializesWithLoadedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := (&State{Issues: map[string]*Issue{"solver-1": {ID: "solver-1", Status: "open"}}}).Save(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvState, path)
	t.Setenv("ZM_FAKE_UPDATE_PROBE", "retained")
	loaded, release, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
	readDone, editDone := make(chan error, 1), make(chan error, 1)
	var once sync.Once
	releaseRead := func() { once.Do(func() { close(release) }) }
	defer releaseRead()
	go func() {
		_, err := tx("list", orchapi.AdapterRequest{}, func(s *State) (any, error) {
			close(loaded) // tx has loaded the pre-delete item and holds the file lock
			<-release
			return nil, nil
		})
		readDone <- err
	}()
	select {
	case <-loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("fake read did not reach its loaded-state barrier")
	}
	go func() {
		close(started)
		editDone <- Update(path, func(s *State) { delete(s.Issues, "solver-1") })
	}()
	<-started // deletion is launched while the old read is still holding its snapshot
	releaseRead()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("fake read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake read did not finish")
	}
	select {
	case err := <-editDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("test edit did not finish")
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Issues["solver-1"] != nil {
		t.Fatal("the in-flight fake read resurrected the deleted item")
	}
	if len(s.Calls) != 1 || !slices.Contains(s.Env, "ZM_FAKE_UPDATE_PROBE=retained") || !slices.Contains(s.EnvKeys, "ZM_FAKE_UPDATE_PROBE") {
		t.Fatalf("test edit lost fake read metadata: calls=%d env=%t keys=%t", len(s.Calls), slices.Contains(s.Env, "ZM_FAKE_UPDATE_PROBE=retained"), slices.Contains(s.EnvKeys, "ZM_FAKE_UPDATE_PROBE"))
	}
}
