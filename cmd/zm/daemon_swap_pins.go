package main

// An upgrade's driver pins (SPEC 20, "Drivers are pinned"; potluck
// DRIVERS.md). The daemon runs a driver only while its file has the sha256
// pinned for it, so the pins must follow the files whichever way a swap
// ends:
//
//   - before it places anything, the swap writes swapPins: the sha256 of
//     each driver it will place (read from the staged files) and of each
//     file it replaces, and, when the daemon's driver folder moves (a --dir
//     that turns a symlinked zm into a real file elsewhere), the pins in the
//     old folder;
//   - right after placing, it re-pins the drivers pinned there to the files
//     it placed (forward), so the new daemon runs them during its verify;
//   - after a restore, it re-pins them to the files that came back
//     (backward), so the previous daemon runs them;
//   - at its end (and the next update tick, for a swapper killed midway) it
//     settles: each driver is pinned to whichever of the two files is there.
//
// Every re-pin is runtarget.RepinPlaced (or PinDriver for a moved folder,
// checked against the same sums): a driver is pinned only to a file whose
// sha256 is one this swap read itself, from the build it verified or the
// file that was there before it. Anything else stays refused.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
)

// fileSwapPins is a swap's driver pins plan in $ZM_HOME, from before it
// places anything until it settled.
const fileSwapPins = "swap-driver-pins.json"

type swapPins struct {
	home string
	// Dir is where the swap places files; OldDir the daemon's driver folder
	// before it, when that differs (both with symlinks resolved).
	Dir    string `json:"dir"`
	OldDir string `json:"old_dir,omitempty"`
	// Placed and Prev are the sha256 of each driver program placed and of
	// the file it replaced, by file name.
	Placed map[string]string `json:"placed"`
	Prev   map[string]string `json:"prev,omitempty"`
	// Moved are the pins in OldDir of drivers this swap places in Dir.
	Moved map[string]runtarget.DriverPin `json:"moved,omitempty"`
}

func sumFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readPins reads the pins file (never writes it: Potluck does, under its
// lock). A missing file holds none.
func readPins(home string) (map[string]runtarget.DriverPin, bool, error) {
	b, err := os.ReadFile(filepath.Join(home, runtarget.PinFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var f struct {
		Drivers map[string]runtarget.DriverPin `json:"drivers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, true, err
	}
	return f.Drivers, true, nil
}

func resolvedDir(d string) string {
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	if a, err := filepath.Abs(d); err == nil {
		return a
	}
	return d
}

// planPins is the plan for placing names from src into dir; oldDir is the
// daemon's driver folder now ("" or the same: none moves). nil when the
// swap places no driver.
func planPins(home, src, dir, oldDir string, names []string) (*swapPins, error) {
	p := &swapPins{home: home, Dir: resolvedDir(dir), Placed: map[string]string{}, Prev: map[string]string{}}
	pins, _, err := readPins(home)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if protocol.DriverProgram(n) == "" {
			continue
		}
		sum, err := sumFile(filepath.Join(src, n))
		if err != nil {
			return nil, err
		}
		p.Placed[n] = sum
		// The file it replaces is trusted back on a rollback only when it is
		// the file pinned now: a driver changed by hand (refused) or never
		// pinned is never pinned by the swap.
		prev := filepath.Join(p.Dir, n)
		if pin, ok := pins[protocol.DriverProgram(n)]; ok && pin.Path == prev {
			if sum, err := sumFile(prev); err == nil && sum == pin.SHA256 {
				p.Prev[n] = sum
			}
		}
	}
	if len(p.Placed) == 0 {
		return nil, nil
	}
	if oldDir != "" {
		if o := resolvedDir(oldDir); o != p.Dir {
			for d, pin := range pins {
				if filepath.Dir(pin.Path) != o {
					continue
				}
				if _, ok := p.Placed[filepath.Base(pin.Path)]; ok || p.hasDriver(d) {
					if p.Moved == nil {
						p.Moved = map[string]runtarget.DriverPin{}
					}
					p.Moved[d], p.OldDir = pin, o
				}
			}
		}
	}
	return p, nil
}

// hasDriver: the swap places a program of driver d (either name).
func (p *swapPins) hasDriver(d string) bool {
	for n := range p.Placed {
		if protocol.DriverProgram(n) == d {
			return true
		}
	}
	return false
}

// save writes the plan to home (0600, synced, then renamed into place).
func (p *swapPins) save() error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(p.home, "."+fileSwapPins+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(p.home, fileSwapPins))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(p.home); err == nil { // the rename itself
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// loadSwapPins is the plan a swap left in home, if any. A plan that does
// not parse is moved aside (<file>.corrupt) and reported: nothing can be
// settled from it, and it must not stop every later swap.
func loadSwapPins(home string) (*swapPins, error) {
	path := filepath.Join(home, fileSwapPins)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := &swapPins{home: home}
	if err := json.Unmarshal(b, p); err != nil {
		aside := path + ".corrupt"
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("%s does not parse (%v) and could not be moved aside: %w", fileSwapPins, err, rerr)
		}
		return nil, fmt.Errorf("%s did not parse (%v): moved to %s; a driver the daemon refuses is pinned again with zm driver pin <driver>", fileSwapPins, err, aside)
	}
	return p, nil
}

func (p *swapPins) clear() { _ = os.Remove(filepath.Join(p.home, fileSwapPins)) }

// pinTo pins driver d to the file the daemon finds for it in dir, only
// when that file has the sum sums gives its name, for a driver whose
// folder moved (RepinPlaced keeps a pin in its own folder). PinDriver pins
// what is there when it reads it: when that is not the checked file (it
// changed in between), the previous pin is put back, from its own folder
// and only if that file still has its sum; else the driver is left pinned
// to what PinDriver found (Potluck has no way to remove a pin) and the
// error says so.
func pinTo(home, dir, d string, sums map[string]string) error {
	found, err := runtarget.FindDriver([]string{dir}, d)
	if err != nil {
		return err
	}
	want, ok := sums[filepath.Base(found)]
	if !ok {
		return fmt.Errorf("driver %s: %s is not a file this swap knows", d, found)
	}
	if got, err := sumFile(found); err != nil || got != want {
		return fmt.Errorf("driver %s: %s is not the file this swap placed or kept (sha256 %.12s, want %.12s)", d, found, got, want)
	}
	pins, _, err := readPins(home)
	if err != nil {
		return err
	}
	before, had := pins[d]
	pin, err := pinDriver(home, []string{dir}, d)
	if err != nil {
		return err
	}
	if pin.SHA256 == want {
		return nil
	}
	if had {
		// Pinned back only while the old file is still the one it pinned:
		// PinDriver trusts whatever it reads.
		if got, err := sumFile(before.Path); err != nil || got != before.SHA256 {
			return fmt.Errorf("driver %s: %s changed while it was pinned, and its previous file %s changed too: it stays pinned to sha256 %.12s, which nothing checked; run zm driver pin %s once a file is the one you trust",
				d, found, before.Path, pin.SHA256, d)
		}
		if back, err := pinDriver(home, []string{filepath.Dir(before.Path)}, d); err == nil && back.Path == before.Path && back.SHA256 == before.SHA256 {
			return fmt.Errorf("driver %s: %s changed while it was pinned; its previous pin (%s) is back", d, found, before.Path)
		}
	}
	return fmt.Errorf("driver %s: %s changed while it was pinned (now pinned to sha256 %.12s; run zm driver pin %s once it is the file you trust)", d, found, pin.SHA256, d)
}

// pinDriver is runtarget.PinDriver (tests: a file changed between the
// check and the pin).
var pinDriver = runtarget.PinDriver

// apply re-pins the drivers to the placed files (forward) or the files
// they replaced (backward). A home without a pins file has none to move:
// nothing is written there. It answers the drivers re-pinned.
func (p *swapPins) apply(forward bool) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	if _, have, _ := readPins(p.home); !have {
		return nil, nil
	}
	sums := p.Prev
	if forward {
		sums = p.Placed
	}
	var errs []error
	done, err := runtarget.RepinPlaced(p.home, p.Dir, sums)
	errs = append(errs, err)
	for d, pin := range p.Moved {
		if forward {
			err = pinTo(p.home, p.Dir, d, p.Placed)
		} else {
			err = pinTo(p.home, p.OldDir, d, map[string]string{filepath.Base(pin.Path): pin.SHA256})
		}
		if err == nil {
			done = append(done, d)
		}
		errs = append(errs, err)
	}
	sort.Strings(done)
	return done, errors.Join(errs...)
}

// settle pins each driver to whichever file is there now, the placed one
// or the one it replaced, for a swap whose end is not known (a failed
// restore, a swapper killed midway). runningDir is the folder the daemon
// runs from now ("" when none answers), which decides a moved driver's
// folder. It answers the drivers still not runnable, with why.
func (p *swapPins) settle(runningDir string) []string {
	if p == nil {
		return nil
	}
	if _, have, _ := readPins(p.home); !have {
		return nil
	}
	_, _ = runtarget.RepinPlaced(p.home, p.Dir, p.Placed)
	_, _ = runtarget.RepinPlaced(p.home, p.Dir, p.Prev)
	newDir := runningDir == p.Dir
	if runningDir == "" { // no daemon answers: the files say which build is in place
		if s, ok := p.Placed["zm"]; ok {
			got, _ := sumFile(filepath.Join(p.Dir, "zm"))
			newDir = got == s
		} else {
			newDir = true
		}
	}
	for d, pin := range p.Moved {
		if newDir {
			_ = pinTo(p.home, p.Dir, d, p.Placed)
		} else {
			_ = pinTo(p.home, p.OldDir, d, map[string]string{filepath.Base(pin.Path): pin.SHA256})
		}
	}
	return p.unsettled()
}

// unsettled are the drivers pinned in the plan's folders whose file is not
// the one pinned (the daemon refuses them), with how to fix each.
func (p *swapPins) unsettled() []string {
	pins, _, err := readPins(p.home)
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for d, pin := range pins {
		if dir := filepath.Dir(pin.Path); dir != p.Dir && dir != p.OldDir {
			continue
		}
		if got, err := sumFile(pin.Path); err != nil || got != pin.SHA256 {
			out = append(out, fmt.Sprintf("driver %s: %s is not the pinned file (zm driver pin %s, once it is the file you trust)", d, pin.Path, d))
		}
	}
	sort.Strings(out)
	return out
}

// daemonDir is the folder the daemon answering c runs from (its manifest,
// symlinks resolved), "" when none answers.
func daemonDir(ctx context.Context, c *client.Client) string {
	cctx, cancel := context.WithTimeout(ctx, swapCallWait)
	defer cancel()
	m, err := c.Manifest(cctx)
	if err != nil || m.Exe == "" {
		return ""
	}
	return filepath.Dir(resolvedDir(m.Exe))
}

// settleLeftPins settles the plan a swap left in home (its swapper died
// before its end), then removes it. Call it holding swap.lock.
func settleLeftPins(ctx context.Context, home string, c *client.Client, w io.Writer) {
	p, err := loadSwapPins(home)
	if err != nil {
		fmt.Fprintln(w, "zm: an earlier swap's driver pins:", err)
		return
	}
	if p == nil {
		return
	}
	if left := p.settle(daemonDir(ctx, c)); len(left) > 0 {
		fmt.Fprintln(w, "zm: after an unfinished swap the daemon refuses: "+strings.Join(left, "; "))
	}
	p.clear()
}

// driverFolder is the upgrade's driver check (SPEC 20, "Drivers are
// pinned"): when the build places drivers where the daemon will run them
// from (its install directory, when the daemon runs from there or none
// runs), that folder must pass Potluck's rule, else every driver would be
// refused after the swap. What this user owns up to its home is repaired;
// anything else refuses the upgrade before a file moves ("" when fine);
// refused: Potluck refused the folder (the caller holds updates,
// daemon_update_autohold.go), not a passing failure.
func (s *swapper) driverFolder(target string) (problem, detail string, refused bool) {
	names, err := swap.Installable(filepath.Dir(target))
	if err != nil || !placedDrivers(names) {
		return "", "", false
	}
	dir := s.installDir()
	if s.health != nil && !runsFrom(s.exe, dir) {
		return "", "", false // the daemon is not restarted from there: it keeps its drivers
	}
	home, _ := os.UserHomeDir()
	return driverFolderProblem(dir, home, os.Stderr)
}

// checkDriverFolder adds driverFolder's problem to the preflight. Only
// Potluck's refusal of the folder holds updates (every tick would fetch and
// be refused again until someone fixes a mode); a passing failure refuses
// this attempt alone, and the next tick tries again.
func (s *swapper) checkDriverFolder(target string) {
	why, detail, refused := s.driverFolder(target)
	if why == "" {
		return
	}
	s.pf.Problems, s.pf.OK = append(s.pf.Problems, why), false
	if refused && !s.o.force {
		holdForDrivers(s.cfg.Home, s.pf.Build, detail)
	}
}

// planPins writes the swap's driver pins plan before a file moves
// (daemon_swap_pins.go). restarts: the daemon will run from dir after it.
func (s *swapper) planPins(src, dir string, names []string, restarts bool) error {
	oldDir := ""
	if restarts {
		oldDir = filepath.Dir(resolvedDir(s.exe))
	}
	p, err := planPins(s.cfg.Home, src, dir, oldDir, names)
	if err == nil && p != nil {
		err = p.save()
	}
	if err != nil {
		return fmt.Errorf("cannot record the drivers' pins before placing them (%w); nothing was placed", err)
	}
	s.pins = p
	return nil
}

// pinsTo re-pins the drivers to the placed files (forward) or the ones
// that came back (backward), saying which.
func (s *swapper) pinsTo(forward bool) {
	done, err := s.pins.apply(forward)
	if len(done) > 0 {
		which := "the files this upgrade placed"
		if !forward {
			which = "the files put back"
		}
		fmt.Printf("re-pinned drivers %s to %s\n", strings.Join(done, ", "), which)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm: re-pinning drivers:", err)
	}
}

// settlePins ends the plan: each driver pinned to the file that is there
// (placed or put back), then the plan file goes. The daemon's refusals
// that remain are said, with the fix.
func (s *swapper) settlePins() {
	if s.pins == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), swapSettleWait)
	defer cancel()
	if left := s.pins.settle(daemonDir(ctx, s.c)); len(left) > 0 {
		fmt.Fprintln(os.Stderr, "zm: the daemon refuses: "+strings.Join(left, "; "))
	}
	s.pins.clear()
}
