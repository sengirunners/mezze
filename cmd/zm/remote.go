package main

// Remote terminals (SPEC 8, "Attach over ssh"): `zm attach <machine>/<id>`,
// `zm run --on <machine>` and `zm ssh <machine>`. The system ssh does the
// work, with the user's own config, keys and agent; zm stores no key, opens no
// port and adds no auth. The machine's name is the ssh destination, so a Host
// block in ~/.ssh/config is all the setup a differently named box needs.
//
// ssh gets this terminal directly (stdin and stdout are inherited), so zm is
// not in the typing path and ssh handles raw mode and window size itself. zm
// stays as its parent only to turn how ssh ended into a reason.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// Exit codes of a remote attach with --embed, added to attachCmd's (0 ended,
// 1 anything else, 2 no such session, 3 connection lost: attach again).
const (
	exitUnreachable = 4 // ssh got no answer from the machine
	exitRefused     = 5 // ssh answered and said no: keys or host key
	exitNoZM        = 6 // ssh works, zm is not on the machine's PATH
	exitUnknownHost = 7 // this Mac does not know the name as an ssh host: no ssh ran
)

const (
	sshPersist     = "600" // seconds a shared connection outlives its last terminal
	embedConnect   = "5"   // a surface waits this long for a machine, in seconds
	promptConnect  = "15"
	connectingLine = "Connecting to "
	// farQuietMS widens the far end's reply window (replyfilter.go): a
	// terminal's answers to the replay arrive a round trip late.
	farQuietMS = "1000"
)

// farPath finds zm on a box: ssh runs a command in a non-login shell, whose
// PATH often lacks the places zm is installed. sh, whatever the login shell.
const farPath = `PATH="$PATH:$HOME/go/bin:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin"; export PATH; ` +
	`command -v zm >/dev/null 2>&1 || exit 127; `

// sshTargetRE is a machine name safe to hand to ssh. A name comes from a
// teammate's machine (presence), so it must not be able to read as an option
// or carry ssh's % tokens.
var sshTargetRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// sshHostEntryRE is ssh's own line for a Host block that applied, with the
// pattern that matched. Only an exact name counts: `Host *.local` or `dev-*`
// fits any name a teammate cares to pick, so those are left to known_hosts.
var sshHostEntryRE = regexp.MustCompile(`(?m): Applying options for [^\s*?]*[A-Za-z0-9][^\s*?]*\s*$`)

// sshKnown says whether this Mac already knows name as an ssh host: a Host
// entry naming it exactly in the ssh config (Includes too), or a key for its
// resolved host in known_hosts. ssh's own tools decide, and neither connects. A machine's name
// comes from a teammate (presence), who could otherwise send this Mac's ssh
// anywhere. Anything that can't be read counts as not known.
func sshKnown(name string) bool {
	var out, said bytes.Buffer
	c := exec.Command("ssh", "-v", "-G", "--", name)
	c.Stdout, c.Stderr = &out, &said
	if c.Run() != nil {
		return false
	}
	if sshHostEntryRE.Match(said.Bytes()) {
		return true
	}
	key, files := knownHostsQuery(out.String())
	for _, f := range files {
		if o, err := exec.Command("ssh-keygen", "-F", key, "-f", f).Output(); err == nil && len(o) > 0 {
			return true
		}
	}
	return false
}

// knownHostsQuery reads `ssh -G` output: the name known_hosts files this host
// under, and the files.
func knownHostsQuery(g string) (key string, files []string) {
	v := map[string]string{}
	for _, line := range strings.Split(g, "\n") {
		if k, val, ok := strings.Cut(line, " "); ok {
			v[k] = strings.TrimSpace(val)
		}
	}
	key = v["hostname"]
	if a := v["hostkeyalias"]; a != "" && a != "none" {
		key = a
	} else if p := v["port"]; p != "" && p != "22" {
		key = "[" + key + "]:" + p
	}
	user := v["userknownhostsfile"]
	if user == "" { // ssh -G leaves its defaults out
		user = "~/.ssh/known_hosts ~/.ssh/known_hosts2"
	}
	home, _ := os.UserHomeDir()
	for _, f := range strings.Fields(user + " " + v["globalknownhostsfile"]) {
		if f == "none" {
			continue
		}
		if rest, ok := strings.CutPrefix(f, "~/"); ok {
			f = filepath.Join(home, rest)
		}
		files = append(files, f)
	}
	return key, files
}

// checkKnownHost refuses a name this Mac does not know, before any ssh runs.
// typed: a person named it, so they are told how to go ahead; a name from
// presence never is.
func checkKnownHost(machine string, typed bool) error {
	if sshKnown(machine) {
		return nil
	}
	if typed {
		return exitWith{exitUnknownHost, fmt.Errorf("this Mac has never connected to %[1]s. If you trust it, run `zm ssh %[1]s` once yourself, or add it to your ssh config", machine)}
	}
	return exitWith{exitUnknownHost, errors.New(sshReason(exitUnknownHost, machine))}
}

// onATerminal: a person is at the keyboard.
func onATerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func checkSSHTarget(machine string) error {
	if !sshTargetRE.MatchString(machine) {
		return fmt.Errorf("%q is not a machine name zm will pass to ssh", machine)
	}
	return nil
}

// sshArgs are the options of every ssh zm runs. batch: nobody can answer a
// prompt (a host app's surface, or no terminal), so ssh fails at once instead.
func sshArgs(cfg *config.Config, batch bool) []string {
	connect := promptConnect
	args := []string{"-o", "LogLevel=ERROR", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		// Nothing is forwarded: the machine may be a teammate's, and a config's ForwardAgent must not reach it.
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		// OpenSSH 9.5+ holds keys back to a 20 ms beat: 14 ms on every key, measured
		// (DESIGN-NOTES, "Remote terminals"). IgnoreUnknown: an older ssh has no such option.
		"-o", "IgnoreUnknown=ObscureKeystrokeTiming", "-o", "ObscureKeystrokeTiming=no"}
	if batch {
		connect = embedConnect
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args, "-o", "ConnectTimeout="+connect)
	if dir := controlDir(cfg); dir != "" {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+dir+"/%C", "-o", "ControlPersist="+sshPersist)
	}
	return args
}

// controlDir is where ssh keeps its shared connections, "" when there is
// none: sharing is only a speed-up. %C is 40 characters and ssh adds 17 while
// it creates the socket; a socket path fits in 104 bytes on macOS.
func controlDir(cfg *config.Config) string {
	dir := filepath.Join(cfg.Home, "ssh")
	if len(dir)+1+40+17 >= 104 || os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	return dir
}

// farAttach is the command run on the far side. --embed clears the terminal
// first: it holds only zm's "Connecting" line, and the replay comes next.
func farAttach(id string, readOnly, embed bool) string {
	s := farPath
	flags := ""
	if readOnly {
		flags += " --read-only"
	}
	if embed {
		// ZM_NO_AUTOSTART: as for a local surface, a lost daemon is reported,
		// not restarted from a terminal.
		s += `printf '\033[H\033[2J\033[3J'; ZM_NO_AUTOSTART=1 ` + envReplyQuiet + `=` + farQuietMS + ` `
		s += `exec zm attach --embed` + flags + ` ` + shQuote(id)
	} else {
		s += `exec zm attach` + flags + ` ` + shQuote(id)
	}
	return "sh -c " + shQuote(s)
}

// attachSSHArgs is the whole ssh command line of an attach. -tt: a pty on the
// far side. -e none: ~ is a key like any other.
func attachSSHArgs(cfg *config.Config, machine, id string, readOnly, embed bool) []string {
	args := sshArgs(cfg, embed)
	args = append(args, "-tt", "-e", "none", "--", machine, farAttach(id, readOnly, embed))
	return args
}

// attachOverSSH runs `zm attach` on the session's machine. The daemon has
// already said this caller may see the session (attachCmd); ssh decides
// whether they may log in. typed: a person gave the machine's name (--on), it
// did not come from presence. why, when set, gets the reason for the host app.
func attachOverSSH(ss orchapi.Session, readOnly, embed, typed bool, why string) error {
	said, err := attachSSH(ss, readOnly, embed, typed)
	if why != "" && err != nil {
		if said == "" {
			said = err.Error()
		}
		_ = os.WriteFile(why, []byte(said), 0o600)
	}
	return err
}

// attachSSH returns the line a host app shows for a failure with no reason
// of its own, and how the attach ended.
func attachSSH(ss orchapi.Session, readOnly, embed, typed bool) (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if err := checkSSHTarget(ss.Machine); err != nil {
		return "", exitWith{exitUnknownHost, err}
	}
	if err := checkKnownHost(ss.Machine, typed && !embed); err != nil {
		return "", err
	}
	if embed {
		fmt.Fprint(os.Stderr, connectingLine+ss.Machine+"…") // an embedding client tells this line from the session's screen
	}
	code, said := runSSH(attachSSHArgs(cfg, ss.Machine, ss.ID, readOnly, embed), os.Stdin, os.Stdout)
	return sshLastLine(said, code, ss.Machine), sshResult(code, said, ss.Machine, embed)
}

// runSSH runs ssh and returns its exit code (-1: killed) and what it said on
// stderr, which also goes on to this stderr. WaitDelay: a ControlPersist
// master stays behind and may keep the stderr pipe open.
func runSSH(args []string, in io.Reader, out io.Writer) (int, string) {
	c := exec.Command("ssh", args...)
	c.Stdin, c.Stdout = in, out
	var said tailBuffer
	c.Stderr = io.MultiWriter(os.Stderr, &said)
	c.WaitDelay = 200 * time.Millisecond
	err := c.Run()
	if c.ProcessState != nil {
		return c.ProcessState.ExitCode(), said.String()
	}
	return 255, err.Error() // ssh would not start: not installed
}

// tailBuffer keeps the last 4 KiB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if n := len(t.b) - 4096; n > 0 {
		t.b = t.b[n:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// ssh's own words: stable across OpenSSH versions, and not translated.
var (
	sshUnreachableRE = regexp.MustCompile(`(?i)could not resolve hostname|name or service not known|nodename nor servname|` +
		`connection timed out|operation timed out|no route to host|connection refused|network is (unreachable|down)|host is down|` +
		`executable file not found`)
	sshRefusedRE = regexp.MustCompile(`(?i)permission denied|host key verification failed|too many authentication failures|` +
		`no more authentication methods|host identification has changed|bad owner or permissions|bad configuration option`)
)

// sshExitCode turns how ssh ended into attachCmd's exit codes. 255 is ssh's
// own failure; any other code is the far command's.
func sshExitCode(code int, said string) int {
	switch code {
	case 0, 1, 2, 3:
		return code
	case 126, 127:
		return exitNoZM
	case 255:
		switch {
		case sshRefusedRE.MatchString(said):
			return exitRefused
		case sshUnreachableRE.MatchString(said):
			return exitUnreachable
		}
		return 3 // up, then dropped: attach again
	case -1:
		return 3
	}
	return 1
}

// sshResult is attachCmd's error for how ssh ended. --embed keeps the exit
// code for its host; a person gets the same reason in words, exit 1.
func sshResult(code int, said, machine string, embed bool) error {
	c := sshExitCode(code, said)
	if embed {
		switch c {
		case 0:
			return nil
		case exitUnreachable, exitRefused, exitNoZM:
			return exitWith{c, errors.New(sshReason(c, machine))}
		}
		return exitWith{c, nil}
	}
	switch c {
	case 0:
		return nil
	case 3:
		return fmt.Errorf("connection to %s closed", machine)
	case exitUnreachable, exitRefused, exitNoZM:
		return errors.New(sshReason(c, machine))
	}
	return exitSilently(1) // zm on the far side said why, on this terminal
}

// sshLastLine is what a host app shows when an attach fails with no reason of
// its own: ssh's last line on stderr, as one clipped line of plain text.
func sshLastLine(said string, code int, machine string) string {
	lines := strings.Split(said, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.Map(func(r rune) rune {
			// Control and format characters: a bidi override would make the line read as something else.
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
				return -1
			}
			return r
		}, lines[i])
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > 300 {
				l = string(r[:300])
			}
			return l
		}
	}
	if code == 3 {
		return fmt.Sprintf("zm on %s lost its daemon, or none is running there", machine)
	}
	return fmt.Sprintf("ssh to %s ended without saying why", machine)
}

func sshReason(code int, machine string) string {
	switch code {
	case exitUnreachable:
		return fmt.Sprintf("no answer from %[1]s over ssh. zm runs `ssh %[1]s`: check the machine is on and that this works in a terminal; "+
			"if it goes by another name there, add a Host block to ~/.ssh/config", machine)
	case exitRefused:
		return fmt.Sprintf("ssh to %[1]s was refused. You can only attach to machines you can already ssh to: run `zm ssh %[1]s` and answer what it asks", machine)
	case exitNoZM:
		return fmt.Sprintf("ssh to %s works, but zm is not on its PATH: install zm there", machine)
	case exitUnknownHost:
		return fmt.Sprintf("the name %[1]s comes from a teammate and this Mac has never connected to it. To use it, add a Host entry for exactly that name to your ssh config, "+
			"or, if a wildcard or Match block reaches it, run zm ssh %[1]s once by hand so its key is in known_hosts", machine)
	}
	return ""
}

// sshCmd is `zm ssh <machine>`: plain ssh with the shared connection, so what
// it asks (a passphrase, a new host key) is answered once for every attach
// that follows while the connection stays up.
func sshCmd() *cobra.Command {
	var knownOnly bool
	cmd := &cobra.Command{
		Use:   "ssh <machine> [command...]",
		Short: "ssh to a machine over the connection zm attach shares",
		Long: `Run ssh to a machine by its zm name, over the connection 'zm attach <machine>/<id>'
shares. It uses your own ssh config, keys and agent; zm stores none of them. Use
it once when an attach says ssh was refused: answer what it asks, then attach.
A machine known by another name goes in ~/.ssh/config as a Host block.
A person at a terminal may name any host, for a first connection. Anything else
(a script, or --known-only) is refused a name this Mac does not know as an ssh
host: a machine's name can come from a teammate.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := checkSSHTarget(args[0]); err != nil {
				return err
			}
			if knownOnly || !onATerminal() {
				if err := checkKnownHost(args[0], false); err != nil {
					return err
				}
			}
			bin, err := exec.LookPath("ssh")
			if err != nil {
				return err
			}
			argv := append([]string{"ssh"}, sshArgs(cfg, false)...)
			argv = append(argv, "--", args[0])
			return syscall.Exec(bin, append(argv, args[1:]...), os.Environ())
		},
	}
	cmd.Flags().SetInterspersed(false) // what follows the machine is the command's
	cmd.Flags().BoolVar(&knownOnly, "known-only", false, "refuse a host this Mac has never connected to, even at a terminal")
	return cmd
}

// runOn is `zm run --on <machine>`: spawn on that machine's daemon over ssh
// (it asks, as ever, on its own socket), then attach unless told not to. Every
// flag of the run goes along; --on and --detach are this end's.
func runOn(cmd *cobra.Command, machine string, args []string, detach bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := checkSSHTarget(machine); err != nil {
		return err
	}
	tty := onATerminal()
	if err := checkKnownHost(machine, tty); err != nil {
		return err
	}
	far := []string{"zm", "run", "--detach", "--json"}
	if flagProject != "" {
		far = append(far, "--project", flagProject)
	}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "on", "detach", "json", "as", "project":
		default:
			far = append(far, "--"+f.Name+"="+f.Value.String())
		}
	})
	far = append(far, "--")
	far = append(far, args...)
	for i, w := range far {
		far[i] = shQuote(w)
	}
	sshArgv := append(sshArgs(cfg, !tty), "--", machine, "sh -c "+shQuote(farPath+"exec "+strings.Join(far, " ")))
	var out bytes.Buffer
	code, said := runSSH(sshArgv, os.Stdin, &out)
	if c := sshExitCode(code, said); c != 0 {
		switch c {
		case exitUnreachable, exitRefused, exitNoZM:
			return exitWith{c, errors.New(sshReason(c, machine))}
		}
		var refused struct { // --json: the far zm's reason is on stdout
			Error *api.Error `json:"error"`
		}
		if json.Unmarshal(out.Bytes(), &refused) == nil && refused.Error != nil {
			return refused.Error
		}
		return fmt.Errorf("zm run on %s failed: exit %d", machine, code)
	}
	var ss orchapi.Session
	if err := json.Unmarshal(out.Bytes(), &ss); err != nil {
		return fmt.Errorf("%s answered something that is not a session: %w", machine, err)
	}
	switch {
	case flagJSON:
		return printJSON(ss)
	case detach || !tty:
		fmt.Println(ss.Address)
		return nil
	}
	ss.Machine = machine // the far zm may call itself something else; ssh got here by this name
	return attachOverSSH(ss, false, false, true, "")
}

// shQuote is s as one POSIX shell word: always single-quoted, so the far
// side's shell never expands it.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
