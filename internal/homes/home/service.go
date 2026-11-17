package home

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Unit is a long-running server zm keeps alive through the machine's
// service manager (launchd on macOS, a systemd user unit on Linux).
type Unit struct {
	Label string   // e.g. dev.zm.cloudflared.<hash>: unique per ZM_HOME
	Args  []string // argv, absolute executable first
	Dir   string   // working directory
	Log   string   // stdout and stderr
	Env   map[string]string
}

// Manager installs, starts, restarts and removes Units.
type Manager interface {
	Name() string
	// Install writes the unit and starts it (idempotent).
	Install(ctx context.Context, u Unit) error
	Restart(ctx context.Context, u Unit) error
	// Remove stops it and deletes the unit file (the data stays).
	Remove(ctx context.Context, u Unit) error
	// Path is where the unit file lives ("" for none).
	Path(u Unit) string
}

// DefaultManager is launchd on macOS and systemd --user on Linux.
func DefaultManager(userHome string) (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return Launchd{Dir: filepath.Join(userHome, "Library", "LaunchAgents")}, nil
	case "linux":
		return Systemd{Dir: filepath.Join(userHome, ".config", "systemd", "user")}, nil
	}
	return nil, fmt.Errorf("zm cannot keep a server running on %s (no launchd or systemd)", runtime.GOOS)
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// LaunchdPlist is u as a launchd agent: started at login, restarted if it
// exits.
func LaunchdPlist(u Unit) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlEscape(u.Label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range u.Args {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	if len(u.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range slices.Sorted(maps.Keys(u.Env)) {
			b.WriteString("    <key>" + xmlEscape(k) + "</key><string>" + xmlEscape(u.Env[k]) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString(`  <key>WorkingDirectory</key><string>` + xmlEscape(u.Dir) + `</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>` + xmlEscape(u.Log) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(u.Log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// systemdQuote quotes one ExecStart word.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// SystemdUnit is u as a systemd service (a --user unit).
func SystemdUnit(u Unit) string {
	var b strings.Builder
	b.WriteString("# Written by zm; zm rewrites it.\n[Unit]\nDescription=zm service (" + u.Label + ")\nAfter=network-online.target\n\n[Service]\nType=simple\n")
	words := make([]string, len(u.Args))
	for i, a := range u.Args {
		words[i] = systemdQuote(a)
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	b.WriteString("WorkingDirectory=" + systemdQuote(u.Dir) + "\n")
	for _, k := range slices.Sorted(maps.Keys(u.Env)) {
		b.WriteString("Environment=" + systemdQuote(k+"="+u.Env[k]) + "\n")
	}
	if u.Log != "" {
		b.WriteString("StandardOutput=append:" + u.Log + "\nStandardError=append:" + u.Log + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func runCmd(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeUnit(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Launchd keeps units as launchd agents in Dir (~/Library/LaunchAgents).
type Launchd struct{ Dir string }

func (Launchd) Name() string           { return "launchd" }
func (l Launchd) Path(u Unit) string   { return filepath.Join(l.Dir, u.Label+".plist") }
func (Launchd) domain() string         { return "gui/" + strconv.Itoa(os.Getuid()) }
func (l Launchd) target(u Unit) string { return l.domain() + "/" + u.Label }
func (l Launchd) loaded(ctx context.Context, u Unit) bool {
	return exec.CommandContext(ctx, "launchctl", "print", l.target(u)).Run() == nil
}

func (l Launchd) Install(ctx context.Context, u Unit) error {
	if err := writeUnit(l.Path(u), LaunchdPlist(u)); err != nil {
		return err
	}
	if l.loaded(ctx, u) {
		return runCmd(ctx, "launchctl", "kickstart", l.target(u))
	}
	return runCmd(ctx, "launchctl", "bootstrap", l.domain(), l.Path(u))
}

func (l Launchd) Restart(ctx context.Context, u Unit) error {
	if !l.loaded(ctx, u) {
		return l.Install(ctx, u)
	}
	return runCmd(ctx, "launchctl", "kickstart", "-k", l.target(u))
}

func (l Launchd) Remove(ctx context.Context, u Unit) error {
	if l.loaded(ctx, u) {
		if err := runCmd(ctx, "launchctl", "bootout", l.target(u)); err != nil {
			return err
		}
	}
	err := os.Remove(l.Path(u))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Systemd keeps units as systemd --user services in Dir
// (~/.config/systemd/user). A server that must run with nobody logged in
// needs `loginctl enable-linger <user>` (zm says so; it never runs sudo).
type Systemd struct{ Dir string }

func (Systemd) Name() string         { return "systemd" }
func (s Systemd) Path(u Unit) string { return filepath.Join(s.Dir, u.Label+".service") }

func (s Systemd) Install(ctx context.Context, u Unit) error {
	if err := writeUnit(s.Path(u), SystemdUnit(u)); err != nil {
		return err
	}
	if err := runCmd(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return runCmd(ctx, "systemctl", "--user", "enable", "--now", u.Label+".service")
}

func (s Systemd) Restart(ctx context.Context, u Unit) error {
	return runCmd(ctx, "systemctl", "--user", "restart", u.Label+".service")
}

func (s Systemd) Remove(ctx context.Context, u Unit) error {
	_ = runCmd(ctx, "systemctl", "--user", "disable", "--now", u.Label+".service")
	err := os.Remove(s.Path(u))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Process runs units as plain detached processes, recording each pid in
// Dir/<label>.pid. Only tests use it (in home, work and issues), so they
// never install a real launchd agent or systemd unit; DefaultManager never
// picks it. Nothing restarts a unit that exits.
type Process struct{ Dir string }

func (Process) Name() string         { return "process" }
func (p Process) Path(u Unit) string { return filepath.Join(p.Dir, u.Label+".pid") }

func (p Process) pid(u Unit) int {
	b, err := os.ReadFile(p.Path(u))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func (p Process) Install(ctx context.Context, u Unit) error {
	if alive(p.pid(u)) {
		return nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(u.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(u.Args[0], u.Args[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = u.Dir, logf, logf
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for _, k := range slices.Sorted(maps.Keys(u.Env)) {
		cmd.Env = append(cmd.Env, k+"="+u.Env[k])
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it if it exits while we run
	return os.WriteFile(p.Path(u), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}

func (p Process) stop(u Unit) {
	pid := p.pid(u)
	if !alive(pid) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 100 && alive(pid); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(100 * time.Millisecond)
	}
}

func (p Process) Restart(ctx context.Context, u Unit) error {
	p.stop(u)
	return p.Install(ctx, u)
}

func (p Process) Remove(_ context.Context, u Unit) error {
	p.stop(u)
	err := os.Remove(p.Path(u))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
