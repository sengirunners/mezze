package main

// zm daemon update enable/disable: the daily signed-release
// update as this user's own scheduled job, so a machine lent with zm pool host
// or zm host share keeps itself current with nothing to remember. It runs the
// farm's update (a systemd service of the machine fleet's own tooling): --release github, so only
// a release that verifies with the pinned key installs, never an older one,
// through the guarded swap that leaves the running zm as it was when anything
// fails. A systemd --user timer on Linux, a LaunchAgent on macOS; never root.
// An updater zm did not write (the farm's, or one written by hand from the
// upgrade runbook) is left alone: zm never adds a second.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
)

// defaultUpdateMinAge is the farm's ring for every box but the canary and
// the authority (the fleet tooling's default ring): a lent machine takes a
// release once it is this old.
const defaultUpdateMinAge = 6 * time.Hour

// Unit names: the farm's and the runbook's Linux names, so a machine has one
// updater whoever wrote it; the LaunchAgent takes zm's label prefix.
const (
	updateService = "zm-update.service"
	updateTimer   = "zm-update.timer"
	updateLabel   = "dev.zm.update"
	// scheduleMark starts every file zm writes: what is not marked is
	// another updater's.
	scheduleMark = "Written by zm (zm daemon update enable)"
)

// executable is os.Executable (tests replace it).
var executable = os.Executable

// runningInstall is the running zm binary, its symlinks resolved, and the
// directory it lives in: where an update places the new build.
func runningInstall() (exe, dir string, err error) {
	p, err := executable()
	if err != nil {
		return "", "", fmt.Errorf("cannot tell where this zm is installed (%v): pass --dir", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if p, err = filepath.Abs(p); err != nil {
		return "", "", err
	}
	return p, filepath.Dir(p), nil
}

// writableDir refuses, in one sentence, a directory this user cannot write:
// an update there could only fail at the swap.
func writableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".zm-update-check-*")
	if err != nil {
		why := "this user cannot write " + dir
		if !errors.Is(err, fs.ErrPermission) {
			why = fmt.Sprintf("zm cannot write in %s (%v)", dir, err)
		}
		return fmt.Errorf("zm cannot update itself where it is installed: %s; reinstall zm in a directory you own (such as ~/.local/bin), or pass --dir", why)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// scheduleSpec is what the daily job runs.
type scheduleSpec struct {
	Exe, Dir string // the zm to run, and where it places a new build
	Home     string // ZM_HOME
	Path     string // PATH: gh and the agents' directories
	MinAge   time.Duration
	Minute   int // past 03:00, spreading machines over the hour
}

func (s scheduleSpec) args() []string {
	return []string{s.Exe, "daemon", "update", "--release", "github", "--dir", s.Dir, "--min-age", s.MinAge.String(), "--no-canary"}
}

// updateSchedule writes, loads and removes this user's update job. run is
// systemctl or launchctl (tests record it).
type updateSchedule struct {
	goos     string
	userHome string
	uid      int
	run      func(name string, args ...string) error
	// linger reports whether this user's systemd services outlive a logout
	// (Linux; nil: unknown).
	linger func() (bool, error)
}

// newUpdateSchedule is this machine's (TestMain replaces it: no test
// writes or loads a real job).
var newUpdateSchedule = func() (*updateSchedule, error) {
	uh, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &updateSchedule{goos: runtime.GOOS, userHome: uh, uid: os.Getuid(), linger: linger, run: func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}}, nil
}

func (s *updateSchedule) unitDir() string {
	if s.goos == "darwin" {
		return filepath.Join(s.userHome, "Library", "LaunchAgents")
	}
	return filepath.Join(s.userHome, ".config", "systemd", "user")
}

func (s *updateSchedule) ours() []string {
	if s.goos == "darwin" {
		return []string{filepath.Join(s.unitDir(), updateLabel+".plist")}
	}
	return []string{filepath.Join(s.unitDir(), updateService), filepath.Join(s.unitDir(), updateTimer)}
}

// mine reports whether path is a regular file zm wrote.
func mine(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && bytes.Contains(b[:min(len(b), 512)], []byte(scheduleMark))
}

var plistUpdate = regexp.MustCompile(`<string>daemon</string>\s*<string>update</string>`)

// foreign is the path of an updater zm did not write ("" for none): a unit
// at one of zm's names that is not zm's (the farm links its own there), or
// any other unit or agent that runs zm daemon update.
func (s *updateSchedule) foreign() string {
	for _, p := range s.ours() {
		if _, err := os.Lstat(p); err == nil && !mine(p) {
			return p
		}
	}
	pat := "*.service"
	if s.goos == "darwin" {
		pat = "*.plist"
	}
	names, _ := filepath.Glob(filepath.Join(s.unitDir(), pat))
	for _, p := range names {
		if slices.Contains(s.ours(), p) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if s.goos == "darwin" && plistUpdate.Match(b) || s.goos != "darwin" && bytes.Contains(b, []byte(" daemon update")) {
			return p
		}
	}
	return ""
}

type scheduleResult struct {
	foreign  string // another updater's file: nothing was written
	replaced bool   // zm's own job was there and is rewritten
}

func (s *updateSchedule) supported() error {
	if s.goos != "linux" && s.goos != "darwin" {
		return fmt.Errorf("zm cannot schedule its own updates on %s (it uses systemd on Linux and launchd on macOS)", s.goos)
	}
	return nil
}

// install writes zm's job and loads it, replacing its own earlier one.
func (s *updateSchedule) install(sp scheduleSpec) (scheduleResult, error) {
	if err := s.supported(); err != nil {
		return scheduleResult{}, err
	}
	if f := s.foreign(); f != "" {
		return scheduleResult{foreign: f}, nil
	}
	r := scheduleResult{replaced: mine(s.ours()[0])}
	if err := os.MkdirAll(s.unitDir(), 0o755); err != nil {
		return r, err
	}
	if s.goos == "darwin" {
		if err := writeFileAtomic(s.ours()[0], launchdUpdatePlist(sp)); err != nil {
			return r, err
		}
		target := "gui/" + strconv.Itoa(s.uid)
		_ = s.run("launchctl", "bootout", target+"/"+updateLabel) // the old one, when loaded
		var err error
		for i := range 3 { // a bootout may still be unloading the old job
			if err = s.run("launchctl", "bootstrap", target, s.ours()[0]); err == nil {
				break
			}
			time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
		}
		return r, err
	}
	if err := writeFileAtomic(s.ours()[0], systemdUpdateService(sp)); err != nil {
		return r, err
	}
	if err := writeFileAtomic(s.ours()[1], systemdUpdateTimer(sp)); err != nil {
		return r, err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", updateTimer}, {"restart", updateTimer}} {
		if err := s.run("systemctl", append([]string{"--user"}, args...)...); err != nil {
			return r, err
		}
	}
	return r, nil
}

// remove unloads and deletes zm's job. foreign names another updater, which
// it leaves alone.
func (s *updateSchedule) remove() (removed bool, foreign string, err error) {
	if err := s.supported(); err != nil {
		return false, "", err
	}
	foreign = s.foreign() // reported, never touched
	if !slices.ContainsFunc(s.ours(), mine) {
		return false, foreign, nil
	}
	var errs []error
	if s.goos == "darwin" {
		_ = s.run("launchctl", "bootout", "gui/"+strconv.Itoa(s.uid)+"/"+updateLabel)
	} else if e := s.run("systemctl", "--user", "disable", "--now", updateTimer); e != nil {
		errs = append(errs, e)
	}
	for _, p := range s.ours() {
		if e := os.Remove(p); e != nil && !errors.Is(e, fs.ErrNotExist) {
			errs = append(errs, e)
		}
	}
	if s.goos != "darwin" {
		if e := s.run("systemctl", "--user", "daemon-reload"); e != nil {
			errs = append(errs, e)
		}
	}
	return true, foreign, errors.Join(errs...)
}

func writeFileAtomic(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// unitQuote quotes one systemd word: % is a specifier, $ expands in ExecStart.
func unitQuote(s string, exec bool) string {
	r := []string{`\`, `\\`, `"`, `\"`, `%`, `%%`}
	if exec {
		r = append(r, `$`, `$$`)
	}
	q := strings.NewReplacer(r...).Replace(s)
	if q == s && s != "" && !strings.ContainsAny(s, " \t'") {
		return s
	}
	return `"` + q + `"`
}

func systemdUpdateService(sp scheduleSpec) string {
	words := []string{}
	for _, a := range sp.args() {
		words = append(words, unitQuote(a, true))
	}
	return "# " + scheduleMark + "; zm daemon update disable removes it.\n" +
		"[Unit]\nDescription=Install the newest signed zm release\nAfter=network-online.target\n\n" +
		"[Service]\nType=oneshot\n" +
		"# The upgrade starts a new daemon; nothing it starts may die with this unit.\nKillMode=process\n" +
		"Environment=" + unitQuote("PATH="+sp.Path, false) + "\n" +
		"Environment=" + unitQuote(orchapi.EnvHome+"="+sp.Home, false) + "\n" +
		"ExecStart=" + strings.Join(words, " ") + "\n" +
		"TimeoutStartSec=35min\n"
}

func systemdUpdateTimer(sp scheduleSpec) string {
	return "# " + scheduleMark + "; zm daemon update disable removes it.\n" +
		"[Unit]\nDescription=Check for a signed zm release once a day\n\n" +
		fmt.Sprintf("[Timer]\nOnCalendar=*-*-* 03:%02d:00\n", sp.Minute) +
		"# A machine that was off or asleep checks when it comes back.\nPersistent=true\n\n" +
		"[Install]\nWantedBy=timers.target\n"
}

func launchdUpdatePlist(sp scheduleSpec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ` + scheduleMark + `; zm daemon update disable removes it. -->
<plist version="1.0">
<dict>
  <key>Label</key><string>` + updateLabel + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range sp.args() {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	log := xmlEscape(filepath.Join(sp.Home, "update.log"))
	fmt.Fprintf(&b, `  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
    <key>%s</key><string>%s</string>
  </dict>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>%d</integer></dict>
  <key>AbandonProcessGroup</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, xmlEscape(sp.Path), orchapi.EnvHome, xmlEscape(sp.Home), sp.Minute, log, log)
	return b.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ageWords is d as an owner writes it: 6h, 90m, 30s.
func ageWords(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// specFor is the job for cfg's home with this zm.
func specFor(cfg *config.Config, minAge time.Duration) (scheduleSpec, error) {
	exe, dir, err := runningInstall()
	if err != nil {
		return scheduleSpec{}, err
	}
	if err := writableDir(dir); err != nil {
		return scheduleSpec{}, err
	}
	path := []string{dir}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(p) && !strings.ContainsAny(p, "\"\\$%\n") && !slices.Contains(path, p) {
			path = append(path, p)
		}
	}
	for _, p := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		if !slices.Contains(path, p) {
			path = append(path, p)
		}
	}
	h := fnv.New32a()
	h.Write([]byte(cfg.Machine))
	return scheduleSpec{Exe: exe, Dir: dir, Home: cfg.Home, Path: strings.Join(path, ":"), MinAge: minAge, Minute: int(h.Sum32() % 60)}, nil
}

// enableAutoUpdate pins the channel's key and installs the daily job, in one
// line to w. With another updater on this machine it changes nothing.
func enableAutoUpdate(cfg *config.Config, s *updateSchedule, sp scheduleSpec, w io.Writer) error {
	if err := s.supported(); err != nil {
		return err
	}
	if f := s.foreign(); f != "" {
		fmt.Fprintf(w, "updates: this machine already has its own zm updater (%s); zm left it as it is\n", f)
		return nil
	}
	if _, err := cfg.AddUpdateKey(release.ChannelKey); err != nil {
		return err
	}
	r, err := s.install(sp)
	if err != nil {
		return err
	}
	if r.foreign != "" { // appeared meanwhile
		fmt.Fprintf(w, "updates: this machine already has its own zm updater (%s); zm left it as it is\n", r.foreign)
		return nil
	}
	note := ""
	if s.goos == "linux" && s.linger != nil {
		if on, err := s.linger(); err == nil && !on {
			note = "; it runs only while you are logged in unless you run sudo loginctl enable-linger " + os.Getenv("USER")
		}
	}
	fmt.Fprintf(w, "updates: once a day zm installs its newest signed release here, once it is %s old (turn off: zm daemon update disable)%s\n", ageWords(sp.MinAge), note)
	return nil
}

// autoUpdateAfterLend is what zm pool host and zm host share run once a
// machine is lent: a failure is said in one line and never undoes the lend.
func autoUpdateAfterLend(w io.Writer) {
	err := func() error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		s, err := newUpdateSchedule()
		if err != nil {
			return err
		}
		sp, err := specFor(cfg, defaultUpdateMinAge)
		if err != nil {
			return err
		}
		return enableAutoUpdate(cfg, s, sp, w)
	}()
	if err != nil {
		fmt.Fprintf(w, "updates: zm could not set up its daily update (%v); zm daemon update enable tries again\n", err)
	}
}

func updateEnableCmd() *cobra.Command {
	minAge := defaultUpdateMinAge
	cmd := &cobra.Command{Use: "enable", Short: "Install this machine's daily update: the newest signed release, once it is --min-age old (a systemd user timer or a LaunchAgent; no root)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if minAge < 0 {
				return errors.New("--min-age: want a duration of 0 or more")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			s, err := newUpdateSchedule()
			if err != nil {
				return err
			}
			sp, err := specFor(cfg, minAge)
			if err != nil {
				return err
			}
			return enableAutoUpdate(cfg, s, sp, cmd.OutOrStdout())
		}}
	cmd.Flags().DurationVar(&minAge, "min-age", minAge, "take a release only once it is this old")
	return cmd
}

func updateDisableCmd() *cobra.Command {
	return &cobra.Command{Use: "disable", Short: "Remove the daily update zm daemon update enable (or zm pool host) installed", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newUpdateSchedule()
			if err != nil {
				return err
			}
			removed, foreign, err := s.remove()
			w := cmd.OutOrStdout()
			switch {
			case removed:
				fmt.Fprintln(w, "daily update removed: zm updates here only when you run zm daemon update")
			case foreign == "":
				fmt.Fprintln(w, "zm had no daily update installed here")
			}
			if foreign != "" {
				fmt.Fprintf(w, "this machine's zm updater %s is not one zm installed; zm left it as it is\n", foreign)
			}
			return err
		}}
}
