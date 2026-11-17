package main

// `zm driver pin` (SPEC 20, "Drivers are pinned"): trust the driver file
// there now. The daemon runs a driver only from its own program's
// directory or $ZM_HOME/drivers, and only while its sha256 is the one
// pinned in $ZM_HOME/driver_pins.json (potluck runtarget/pin.go,
// DRIVERS.md); a refusal names this command. Also the driver folder check
// an upgrade runs before it places anything. Owned by run targets.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
)

func init() {
	d := &cobra.Command{Use: "driver", Short: "The run-target driver programs this machine runs, each pinned by sha256: pin"}
	d.AddCommand(driverPinCmd())
	rootCmd.AddCommand(d)
}

func driverPinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pin <driver>...",
		Short: "Trust the driver file there now (after you upgraded it on purpose): record its path and sha256",
		Long: `Drivers run as you and see renters' secrets, so the daemon runs one only from
its own program's directory or $ZM_HOME/drivers (never PATH), and only while
its sha256 is the one pinned in $ZM_HOME/driver_pins.json. A driver is
pinned the first time it runs, and zm daemon update and make install re-pin
the drivers they place; after you replace one any other way, pin it again
with this. With a daemon running, the directory is the daemon's (its
program, symlinks resolved), whichever zm runs this.`,
		Example: "  zm driver pin tart",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadClient()
			if err != nil {
				return err
			}
			dirs, note := daemonDriverDirs(cmd.Context(), cfg.Home, cfg.SocketPath())
			if note != "" {
				fmt.Fprintln(os.Stderr, "zm:", note)
			}
			pins, err := pinDrivers(cfg.Home, dirs, args, os.Stdout)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(pins)
			}
			return nil
		},
	}
}

// pinDrivers pins each driver to the file found in dirs now, saying what
// it recorded (but under --json).
func pinDrivers(home string, dirs, drivers []string, w io.Writer) ([]runtarget.DriverPin, error) {
	var out []runtarget.DriverPin
	for _, d := range drivers {
		p, err := runtarget.PinDriver(home, dirs, d)
		if err != nil {
			return out, err
		}
		out = append(out, p)
		if !flagJSON {
			fmt.Fprintf(w, "pinned %s: %s sha256 %s\n", d, p.Path, p.SHA256)
		}
	}
	return out, nil
}

// daemonDriverDirs are the directories the daemon on this home runs
// drivers from: its own program's directory (from its manifest, symlinks
// resolved), then home's drivers directory. runtarget.DriverDirs takes the
// directory of the program calling it, which is this CLI: a zm elsewhere on
// PATH would pin, or test, a file the daemon never runs. With no daemon
// answering, the next one is this CLI's (autostart runs it), so its own
// directories stand. note says when the daemon's differs from this CLI's.
func daemonDriverDirs(ctx context.Context, home, socket string) (dirs []string, note string) {
	own := runtarget.DriverDirs(home)
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	m, err := client.NewUnix(socket).Manifest(cctx)
	if err != nil || m.Exe == "" {
		return own, ""
	}
	exe, err := filepath.EvalSymlinks(m.Exe)
	if err != nil {
		return own, ""
	}
	d := filepath.Dir(exe)
	dirs = []string{d, filepath.Join(home, runtarget.DriversDir)}
	if len(own) > 1 && own[0] != d {
		note = fmt.Sprintf("the daemon runs %s: using its directory %s for drivers, not this zm's %s", m.Exe, d, own[0])
	}
	return dirs, note
}

// checkDriverDir is Potluck's own rule (runtarget.PinDriver's check:
// DRIVERS.md) applied to a driver in dir: it puts a stand-in driver there,
// asks PinDriver to pin it in a throwaway home, and removes both. nil: a
// driver placed in dir would run. Potluck exports no check of a directory
// alone; the stand-in lets this use the daemon's rule as it is.
func checkDriverDir(dir string) error {
	// A stand-in an earlier check left (killed between write and remove).
	if old, _ := filepath.Glob(filepath.Join(dir, protocol.DriverPrefix+dirCheckPrefix+"*")); len(old) > 0 {
		for _, o := range old {
			_ = os.Remove(o)
		}
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := dirCheckPrefix + hex.EncodeToString(b[:])
	p := filepath.Join(dir, protocol.DriverPrefix+name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, err = f.WriteString("#!/bin/sh\nexit 1\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	defer os.Remove(p)
	if err != nil {
		return err
	}
	if err := os.Chmod(p, 0o700); err != nil { // whatever the umask
		return err
	}
	tmp, err := os.MkdirTemp("", "zm-dircheck")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	_, err = runtarget.PinDriver(tmp, []string{dir}, name)
	return err
}

// dirCheckPrefix starts the stand-in driver's name.
const dirCheckPrefix = "zm-dircheck-"

// repairDriverDir takes group and other write off each directory from dir
// up to home (included), never above it, that this user owns and that
// can be written by others (a sticky one above dir is left: Potluck allows
// it). It answers the directories it changed.
func repairDriverDir(dir, home string) []string {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	if home, err = filepath.EvalSymlinks(home); err != nil || home == "" || home == "/" {
		return nil
	}
	if dir != home && !strings.HasPrefix(dir, home+string(filepath.Separator)) {
		return nil
	}
	var fixed []string
	for d, first := dir, true; ; d, first = filepath.Dir(d), false {
		fi, err := os.Stat(d)
		if err != nil {
			return fixed
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		mode := fi.Mode()
		if ok && int(st.Uid) == os.Geteuid() && mode.Perm()&0o022 != 0 && (first || mode&os.ModeSticky == 0) {
			if os.Chmod(d, mode.Perm()&^0o022|mode&(os.ModeSticky|os.ModeSetgid|os.ModeSetuid)) == nil {
				fixed = append(fixed, d)
			}
		}
		if d == home {
			return fixed
		}
	}
}

// driverFolderProblem is why the daemon would refuse the drivers an
// upgrade places in dir, after repairing what this user may (said on w),
// or "" when they would run; detail is the short form (for the hold).
// refused: Potluck refused the folder itself (isFolderRefusal), which
// stays so until someone changes a mode or owner. Any other failure (a
// full disk, an unwritable /tmp) still refuses this attempt, but is not a
// refusal: the next try may pass.
func driverFolderProblem(dir, home string, w io.Writer) (problem, detail string, refused bool) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("could not make the driver folder %s (%v): not upgrading this time", dir, err), err.Error(), false
	}
	err := checkDriverDir(dir)
	if err == nil {
		return "", "", false
	}
	if isFolderRefusal(err) {
		if fixed := repairDriverDir(dir, home); len(fixed) > 0 {
			fmt.Fprintf(w, "zm: took group and other write off %s (chmod go-w): the daemon runs drivers only from folders no one else can change\n", strings.Join(fixed, ", "))
			if err = checkDriverDir(dir); err == nil {
				return "", "", false
			}
		}
	}
	why := err.Error()
	if _, after, ok := strings.Cut(why, " refused: "); ok { // the stand-in's name says nothing
		why = after
	}
	if !isFolderRefusal(err) {
		return fmt.Sprintf("could not check the driver folder %s (%s): not upgrading this time", dir, why), why, false
	}
	return fmt.Sprintf("the daemon would refuse every driver this upgrade places in %s (%s), so every box and job on this machine would stop: "+
			"this machine keeps its build. Fix it (the chmod above needs the folder's owner or root), or install zm in a folder only you can write, then update again", dir, why),
		fmt.Sprintf("drivers in %s would be refused (%s)", dir, why), true
}

// isFolderRefusal reports whether err is Potluck's refusal of a driver for
// its file's or folders' mode or owner (runtarget/pin.go's safeMode and
// safeOwner), the one error a folder keeps until someone fixes it. Potluck
// exports no typed error for it: the text is matched here alone, and a
// test pins it.
func isFolderRefusal(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, " is writable by group or others (fix: chmod go-w ") ||
		strings.Contains(m, ", not the daemon's user (uid ")
}

// placedDrivers reports whether names hold a driver program.
func placedDrivers(names []string) bool {
	for _, n := range names {
		if protocol.DriverProgram(n) != "" {
			return true
		}
	}
	return false
}
