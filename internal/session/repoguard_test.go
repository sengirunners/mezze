package session

import (
	"os"
	"testing"

	"github.com/sengirunners/mezze/internal/repoguard"
)

// TestMain fails the run if a test rewrote the git config of the repository around the package (zm-jmep).
func TestMain(m *testing.M) { os.Exit(repoguard.Main(m)) }
