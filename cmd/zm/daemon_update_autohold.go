package main

// The driver folder's hold (SPEC 20, "Drivers are pinned"). An upgrade
// refused because the daemon would refuse its drivers is refused again on
// every tick until someone changes a folder's mode, and each tick
// downloads or builds a release first. So the staged swapper that refuses
// it turns the hold switch on ($ZM_HOME/update.hold, the file every
// updater, old ones included, reads before it fetches anything) with the
// reason and the fix: machine numbers show it (zm.held) fleet-wide. It
// notes in update-autohold.json that the hold is its own, so an updater of
// this build lifts it after autoHoldRetry, or when the channel has a newer
// release than the one refused, and tries again. An older updater keeps it
// until zm daemon update resume. A hold someone set by hand is never
// touched, and a successful upgrade lifts the swapper's own.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

const fileAutoHold = "update-autohold.json"

// autoHoldRetry is how long the driver folder's hold stands before an
// updater tries again.
var autoHoldRetry = 6 * time.Hour

type autoHold struct {
	Build string    `json:"build"` // the build refused
	Since time.Time `json:"since"` // the hold file's
}

// holdForDrivers turns the hold switch on for a refused build, unless a
// hold is on already. It says what it did on stderr.
func holdForDrivers(home, build, why string) {
	path := release.HoldPath(home)
	if _, held, err := release.ReadHold(path); held || err != nil {
		return
	}
	h := release.Hold{Since: time.Now().UTC().Truncate(time.Second),
		Reason: "zm upgrade refused here: " + why + ". Fix the folder (the chmod named there, by its owner or root), then run: zm daemon update resume. Newer zm builds also lift this hold themselves after 6 hours or for a newer release"}
	if len(h.Reason) > 495 { // the reader keeps 500 bytes
		h.Reason = h.Reason[:495] + "..."
	}
	if err := release.WriteHold(path, h); err != nil {
		fmt.Fprintln(os.Stderr, "zm: could not hold updates:", err)
		return
	}
	b, _ := json.Marshal(autoHold{Build: build, Since: h.Since})
	_ = os.WriteFile(filepath.Join(home, fileAutoHold), b, 0o600)
	fmt.Fprintln(os.Stderr, "zm: updates held on this machine until the driver folder is fixed (machine numbers show why)")
}

// ownAutoHold is the swapper's hold note when the hold switch on now is
// the one it set; nil otherwise (none, or someone's own hold).
func ownAutoHold(home string) *autoHold {
	h, held, err := release.ReadHold(release.HoldPath(home))
	if !held || err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, fileAutoHold))
	if err != nil {
		return nil
	}
	var a autoHold
	if json.Unmarshal(b, &a) != nil || !a.Since.Equal(h.Since.Truncate(time.Second)) {
		return nil
	}
	return &a
}

// liftAutoHold removes the swapper's own hold (and its note); a hold set
// by hand stays.
func liftAutoHold(home string) bool {
	if ownAutoHold(home) == nil {
		_ = os.Remove(filepath.Join(home, fileAutoHold))
		return false
	}
	err := os.Remove(release.HoldPath(home))
	_ = os.Remove(filepath.Join(home, fileAutoHold))
	return err == nil || errors.Is(err, os.ErrNotExist)
}

// expireAutoHold lifts the swapper's own hold once autoHoldRetry has
// passed or the newest release (newest: "" when unknown) is not the build
// it refused. It answers why it lifted it ("" when it did not).
func expireAutoHold(home string, now time.Time, newest func() string) string {
	a := ownAutoHold(home)
	if a == nil {
		return ""
	}
	why := ""
	switch {
	case now.Sub(a.Since) >= autoHoldRetry:
		why = fmt.Sprintf("the driver folder's hold is %s old: trying again", now.Sub(a.Since).Truncate(time.Minute))
	default:
		if v := newest(); v != "" && v != a.Build && !strings.HasPrefix(a.Build, v+" ") {
			why = fmt.Sprintf("release %s is newer than the one refused (%s): trying again", v, a.Build)
		}
	}
	if why != "" && liftAutoHold(home) {
		return why
	}
	return ""
}

// newestRelease is the channel's newest release version for --release
// from ("" for a source build, or when it cannot tell): a listing only,
// nothing is downloaded. repos and set are the tick's order (releaseRepos).
func newestRelease(from string, repos []string, set bool) string {
	if from == "" {
		return ""
	}
	var src releaseSource
	if from == "github" {
		src = &githubSource{run: releaseRunner(updateEnv(os.Environ())), repos: repos, set: set}
	} else {
		abs, err := filepath.Abs(from)
		if err != nil {
			return ""
		}
		src = dirSource{dir: abs}
	}
	rs, err := src.list()
	if err != nil || len(rs) == 0 {
		return ""
	}
	return rs[0].Version
}
