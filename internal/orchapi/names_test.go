package orchapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
)

// Importing orchapi is what names zm: with no home variable set, kit puts
// zm's home at ~/.zm and its socket at zm.sock in it; ZM_HOME overrides the
// home. If this broke, every zm binary would look for another daemon.
func TestZMNamesResolveToZMHomeAndSocket(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv(EnvHome, "") // unset: kit treats empty as not set

	home, err := config.HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(user, ".zm"); home != want {
		t.Fatalf("home = %q, want %q", home, want)
	}
	if got, want := (&config.Config{Home: home}).SocketPath(), filepath.Join(user, ".zm", "zm.sock"); got != want {
		t.Fatalf("socket = %q, want %q", got, want)
	}
	if got, want := (&config.Config{Home: home}).ListenSocketPath(), filepath.Join(user, ".zm", "zm.sock"); got != want {
		t.Fatalf("listen socket = %q, want %q", got, want)
	}

	over := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv(EnvHome, over)
	home, err = config.HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if home != over {
		t.Fatalf("with %s set, home = %q, want %q", EnvHome, home, over)
	}
	if _, err := os.Stat(filepath.Join(user, ".zm")); err == nil {
		t.Fatal("resolving the home must not create it")
	}
}
