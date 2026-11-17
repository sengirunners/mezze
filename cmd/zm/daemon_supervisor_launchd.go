package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

// A loaded ad-hoc executable can carry a launchd cdhash requirement. Refresh
// only the verified owning registration after placement, retaining its plist.
type ownedLaunchAgent struct {
	path, target, program string
	body                  []byte
	job                   launchdJob
	registration          launchRegistration
	userHome              string
	unloadAttempted       bool  // only our verified exact bootout attempt can own a missing job
	ownershipErr          error // loss or uncertainty survives the guarded rollback retry
}

func launchField(s, key string) string {
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+" = "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func agentBytes(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	uid, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || !ok || int(uid.Uid) != os.Getuid() {
		return nil, fmt.Errorf("LaunchAgent must be an unchanged user-owned regular file without group/other write")
	}
	return os.ReadFile(path)
}

var prepareOwnedLaunchAgent = func(ctx context.Context, v *supervisor, exe, home string) error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), v.Unit)
	out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "print", target)
	if err != nil {
		return fmt.Errorf("inspect owning LaunchAgent: %w", err)
	}
	path := launchField(string(out), "path")
	uh, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if filepath.Dir(path) != filepath.Join(uh, "Library", "LaunchAgents") || filepath.Ext(path) != ".plist" {
		return fmt.Errorf("owning LaunchAgent has no existing user plist")
	}
	body, err := agentBytes(path)
	if err != nil {
		return err
	}
	raw, err := supervisorOutput(ctx, supervisorCallWait, false, "plutil", "-convert", "json", "-o", "-", path)
	if err != nil {
		return fmt.Errorf("read owning LaunchAgent: %w", err)
	}
	var j launchdJob
	if err = json.Unmarshal(raw, &j); err != nil {
		return err
	}
	if j.Label != v.Unit || !j.runs(exe, home, uh) || !samePath(launchdProgram(string(out)), exe) {
		return fmt.Errorf("LaunchAgent is not this daemon's owning job")
	}
	registration, err := loadedRegistration(string(out), j, uh)
	if err != nil {
		return err
	}
	if !samePath(registration.home, home) {
		return fmt.Errorf("loaded LaunchAgent serves another home")
	}
	after, err := agentBytes(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, after) {
		return fmt.Errorf("LaunchAgent changed during inspection")
	}
	v.launch = &ownedLaunchAgent{path: path, target: target, program: exe, body: body, job: j, registration: registration, userHome: uh}
	return nil
}

func restartOwnedLaunchAgent(ctx context.Context, v *supervisor) error {
	a := v.launch
	if a.ownershipErr != nil {
		return a.ownershipErr
	}
	body, err := agentBytes(a.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, a.body) {
		return fmt.Errorf("owning LaunchAgent changed; refusing re-registration")
	}
	out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "print", a.target)
	if err == nil {
		if err := a.validateLoaded(string(out)); err != nil {
			return err
		}
		// bootout can return while launchd is still terminating the job. Share
		// its existing stop budget with the removal wait, before bootstrap.
		stop, cancel := context.WithTimeout(ctx, swapSupervisorStopWait)
		defer cancel()
		a.unloadAttempted = true
		if _, err = supervisorOutput(stop, swapSupervisorStopWait, true, "launchctl", "bootout", a.target); err != nil {
			return fmt.Errorf("unregister owning LaunchAgent: %w", err)
		}
		if err := a.waitRemoved(stop, v.Unit, launchField(string(out), "pid")); err != nil {
			return err
		}
	} else {
		if !a.unloadAttempted {
			return fmt.Errorf("owning LaunchAgent disappeared before re-registration: %w", err)
		}
		// print may fail for reasons other than absence. Reconcile the exact label
		// through list before restoring a job our attempted bootout could remove.
		list, e := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "list")
		if e != nil {
			return fmt.Errorf("cannot reconcile attempted LaunchAgent unload: %w", e)
		}
		if _, loaded := jobPID(string(list), v.Unit); loaded {
			return fmt.Errorf("owning LaunchAgent remains loaded but cannot be inspected")
		}
	}
	// Recheck after unload as well. A failure goes through the existing guarded
	// swap's file restoration and registration retry, never a direct daemon start.
	body, err = agentBytes(a.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, a.body) {
		return fmt.Errorf("owning LaunchAgent changed while unloading")
	}
	if _, err = supervisorOutput(ctx, swapSupervisorStopWait+swapStartWait, true, "launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), a.path); err != nil {
		return fmt.Errorf("register owning LaunchAgent: %w", err)
	}
	// Retain ownership of the gap if launchd removes a consistently failing job.
	return nil
}

func (a *ownedLaunchAgent) validateLoaded(out string) (err error) {
	defer func() {
		if err != nil {
			a.ownershipErr = err
		}
	}()
	if launchField(out, "path") != a.path || !samePath(launchdProgram(out), a.program) {
		return fmt.Errorf("loaded LaunchAgent identity changed")
	}
	registration, err := loadedRegistration(out, a.job, a.userHome)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(registration, a.registration) {
		return fmt.Errorf("loaded LaunchAgent configuration changed")
	}
	return nil
}

// Only an absent exact label, reconciled through list, completes an owned
// unload. A replacement or an unreadable loaded job is never ours to restart.
func (a *ownedLaunchAgent) waitRemoved(ctx context.Context, unit, pid string) (err error) {
	// Once this unload observes a replacement or cannot establish ownership,
	// restoring artifacts must not give rollback permission to touch that job.
	defer func() {
		if err != nil {
			a.ownershipErr = err
		}
	}()
	for {
		body, err := agentBytes(a.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(body, a.body) {
			return fmt.Errorf("owning LaunchAgent changed while unloading")
		}
		out, err := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "print", a.target)
		if err == nil {
			if err := a.validateLoaded(string(out)); err != nil {
				return err
			}
			if current := launchField(string(out), "pid"); current != "" && current != "0" && current != pid {
				return fmt.Errorf("owning LaunchAgent process changed while unloading")
			}
		} else {
			list, e := supervisorOutput(ctx, supervisorCallWait, false, "launchctl", "list")
			if e != nil {
				return fmt.Errorf("cannot reconcile owning LaunchAgent unload: %w", e)
			}
			if _, loaded := jobPID(string(list), unit); loaded {
				return fmt.Errorf("owning LaunchAgent remains loaded but cannot be inspected")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for owning LaunchAgent removal: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
