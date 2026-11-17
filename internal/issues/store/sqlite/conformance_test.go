package sqlite

import (
	"os"
	"testing"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/issues/store/storetest"
)

// TestConformance runs the shared driver suite against SQLite.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) istore.Store {
		home, err := os.MkdirTemp("", "zmis")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(home) })
		return Open(home)
	})
}
