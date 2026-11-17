package main

// Owned by the session builder: run, ls, attach, kill, hook, and
// `zm session register|state|stop|input|output|attach|gc|admission|show`.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// detachKey is ctrl-]: it detaches `zm attach` and `zm run`, leaving the session running.
const detachKey = 0x1d

func init() {
	s := &cobra.Command{Use: "session", Short: "Agent sessions on this machine"}
	s.AddCommand(sessionRegisterCmd(), sessionStateCmd(), stopCmd("stop"), sessionInputCmd(), sessionOutputCmd(),
		attachCmd(), sessionGCCmd(), sessionAdmissionCmd(), sessionShowCmd(), sessionHookCmd(), sessionPromptCmd(), sessionPromptsCmd(), sessionMoveCmd(),
		sessionNameCmd())
	s.AddCommand(pipeCmds()...)
	rootCmd.AddCommand(runCmd(), lsCmd(), attachCmd(), sshCmd(), stopCmd("kill"), s)
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sessionTable prints sessions grouped by project (groups with a session
// that needs a human first), needs_human first inside each group.
func sessionTable(ss []orchapi.Session, slugs map[string]string) string {
	if len(ss) == 0 {
		return "no sessions (start one: zm run claude)"
	}
	groups := map[string][]orchapi.Session{}
	urgent := map[string]bool{}
	var names []string
	for _, s := range ss {
		name := slugs[s.Project]
		if name == "" {
			name = s.Project
		}
		if s.Project == api.ProjectNone {
			name = "(no project)"
		}
		if s.Area != "" {
			name += "/" + s.Area
		}
		if _, ok := groups[name]; !ok {
			names = append(names, name)
		}
		groups[name] = append(groups[name], s)
		urgent[name] = urgent[name] || s.State == orchapi.StateNeedsHuman
	}
	sort.SliceStable(names, func(i, j int) bool {
		if urgent[names[i]] != urgent[names[j]] {
			return urgent[names[i]]
		}
		return names[i] < names[j]
	})
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	for i, name := range names {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintf(tw, "%s\n", name)
		for _, s := range groups[name] {
			mark := " "
			if s.State == orchapi.StateNeedsHuman {
				mark = "!"
			}
			what := strings.TrimSpace(s.Name + "  " + s.Title)
			if s.Note != "" {
				what = strings.TrimSpace(what + "  (" + s.Note + ")")
			}
			if s.Unreachable != "" {
				mark = "?"
				what = strings.TrimSpace(what + "  (unreachable: see zm status)")
			}
			if !s.Managed {
				what = strings.TrimSpace("[own terminal] " + what)
			}
			if s.Seen != nil { // a team replica's session, known by its presence
				what = strings.TrimSpace("[presence " + age(*s.Seen) + " ago] " + what)
			}
			fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\t%s\n", mark, s.Address, s.Agent, s.State, age(s.Updated), dash(s.Work), what)
		}
	}
	tw.Flush()
	return textclean.Clean(b.String())
}

// slugMap maps project ULIDs to slugs (best effort: an error shows ULIDs).
func slugMap(ctx context.Context, c *client.Client) map[string]string {
	return slugs(ctx, c)
}

func lsCmd() *cobra.Command {
	var q orchapi.SessionQuery
	cmd := &cobra.Command{
		Use: "ls", Aliases: []string{"sessions", "ps"}, Short: "Sessions you can see, grouped by project, the ones that need you first",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			page, err := orchclient.SessionPage(cmd.Context(), c, q)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(page)
			}
			labels := slugMap(cmd.Context(), c)
			for _, share := range page.Skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s/%s unavailable; session list is incomplete: %s\n",
					textclean.Clean(slugOf(labels, share.Project)), textclean.Clean(share.Source), textclean.Clean(share.Error))
			}
			for _, share := range page.Behind {
				detail := share.State
				if !share.Synced.IsZero() {
					detail += "; last synced " + ago(share.Synced) + " ago"
				}
				if share.Error != "" {
					detail += "; " + share.Error
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s/%s session list is not current (%s)\n",
					textclean.Clean(slugOf(labels, share.Project)), textclean.Clean(share.Source), textclean.Clean(detail))
			}
			fmt.Println(strings.TrimRight(sessionTable(page.Sessions, labels), "\n"))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&q.All, "all", "a", false, "include ended sessions")
	cmd.Flags().StringVar((*string)(&q.State), "state", "", "only this state: running, needs_human, idle, ended")
	cmd.Flags().StringVar(&q.Agent, "agent", "", "only this agent")
	return cmd
}

func runCmd() *cobra.Command {
	var (
		req    orchapi.SpawnRequest
		detach bool
		agent  string
		on     string
		pipe   bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] <agent|program> [args...]",
		Short: "Start an agent in a PTY the daemon owns and attach (detach: ctrl-]; refused without memory/disk headroom)",
		Long: `Start an agent in a pseudo-terminal owned by the zm daemon, then attach to it.
The session keeps running when you detach (ctrl-]) or close the terminal;
reattach with 'zm attach <session>'.

<agent> is claude, codex, opencode, gemini, shell, or any program on your PATH
(config.json "agents" overrides the command for a bare name). Arguments after it
go to the agent unchanged. The agent gets your environment plus ZM_AS and
ZM_SESSION (its address), ZM_SCRATCH (its scratch dir) and ZM_HOME; never
ZM_PROJECT. The project comes from --project, else the repo you are in, else
"none".

Examples:
  zm run claude
  zm run --name auth-fix --worktree --work local:k3x9qa claude --model opus
  zm run -d codex              # start in the background, print the address
  zm run --sandbox claude      # inside srt, writing only the project's repos
  zm run --on mini shell       # on the machine mini, over your own ssh`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.For, req.JobKind = attributionFlags(cmd)
			if on != "" {
				return runOn(cmd, on, args, detach)
			}
			switch {
			case agent != "":
				req.Agent, req.Command = agent, args
			case len(args) == 1 && !strings.Contains(args[0], "/"):
				req.Agent = args[0]
			default:
				req.Agent, req.Command = config.Sanitize(filepath.Base(args[0])), args
			}
			if req.Cwd == "" {
				req.Cwd, _ = os.Getwd()
			} else if abs, err := filepath.Abs(req.Cwd); err == nil {
				req.Cwd = abs
			}
			req.Env = clientEnv()
			if pipe {
				req.Mode = orchapi.SessionModePipe
			} else if req.Approvals != "" || req.Resume != "" || len(req.AllowedTools) > 0 || req.PermissionMode != "" {
				return fmt.Errorf("--approvals, --resume, --allow-tool and --permission-mode are for a structured session (--pipe)")
			}
			tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
			if tty {
				if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					req.Rows, req.Cols = h, w
				}
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ss, err := orchclient.Spawn(cmd.Context(), c, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(ss)
			}
			if pipe {
				fmt.Println(ss.Address)
				if tty {
					fmt.Fprintf(os.Stderr, "structured session: zm session turn %s <text>; zm session events -f %s\n", ss.ID, ss.ID)
				}
				return nil
			}
			if detach || !tty {
				fmt.Println(ss.Address)
				return nil
			}
			return attachTerminal(cmd.Context(), c, ss, false, false)
		},
	}
	cmd.Flags().SetInterspersed(false) // flags after <agent> belong to the agent
	cmd.Flags().StringVarP(&req.Name, "name", "n", "", "a name that reaches the session while it is live (<machine>/<name> in mail); a-z 0-9 . _ -")
	cmd.Flags().StringVar(&req.Title, "title", "", "a title for the session (free text), shown in ls and the cockpit")
	cmd.Flags().StringVar(&req.Area, "area", "", "area of the project")
	cmd.Flags().StringVarP(&req.Cwd, "cwd", "C", "", "directory to run in (default: here)")
	cmd.Flags().StringVar(&req.Model, "model", "", "model to ask the agent for (passed as --model and recorded)")
	cmd.Flags().StringVar(&agent, "agent", "", "agent name to record when the program is a wrapper (default: its basename)")
	cmd.Flags().StringVar(&req.Work, "work", "", "work item the session takes a bound lease on")
	cmd.Flags().StringVar(&req.Prompt, "prompt", "", "first prompt (initial CLI argument for known interactive Codex launches; otherwise typed after startup)")
	cmd.Flags().BoolVar(&req.Worktree, "worktree", false, "give it its own git worktree + branch zm/<session8> (never deleted by zm)")
	cmd.Flags().BoolVar(&req.Shared, "shared", false, "let the team project's members send it prompts (a team project only; each logged)")
	cmd.Flags().BoolVar(&req.Sandbox, "sandbox", false, "run it inside srt (Anthropic's sandbox runtime), writing only the project's repos, worktree and scratch")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "do not attach; print the session address")
	cmd.Flags().StringVar(&on, "on", "", "start it on this machine over ssh (your own config and keys), then attach there")
	cmd.Flags().BoolVar(&pipe, "pipe", false, "a structured session: the agent's adapter on pipes, driven by turns (zm session turn/events), no terminal")
	cmd.Flags().StringVar(&req.Approvals, "approvals", "", "with --pipe: ask (approval events) or never (nothing waits on a person); default: the agent's own settings")
	cmd.Flags().StringVar(&req.Resume, "resume", "", "with --pipe: the agent's own session or thread id to go on from")
	cmd.Flags().StringArrayVar(&req.AllowedTools, "allow-tool", nil, "with --pipe: a tool the agent runs without asking (repeat; Claude specs like Bash or 'Bash(gh pr view:*)'; Codex: a grant that writes is its workspace-write sandbox)")
	cmd.Flags().StringVar(&req.PermissionMode, "permission-mode", "", "with --pipe: default or acceptEdits")
	addAttributionFlags(cmd)
	return cmd
}

// clientEnv is this shell's environment for the agent (the daemon may have
// been started elsewhere, with another PATH). The daemon sets the ZM_* ones.
func clientEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if k != "" && !strings.HasPrefix(k, "ZM_") {
			env[k] = v
		}
	}
	return env
}

func attachCmd() *cobra.Command {
	var readOnly, embed, replayModes bool
	var on, whyFile string
	cmd := &cobra.Command{
		Use:   "attach <session>",
		Short: "Attach your terminal to a session zm started, here or on another machine (detach: ctrl-])",
		Long: `Attach your terminal to a session the daemon owns. You see the last 64 KiB of its
output, then live output; what you type goes to the agent. ctrl-] detaches and
leaves the session running. Several terminals may attach at once.
A session on another machine (<machine>/<id>) attaches over the system ssh, with
your own config, keys and agent: 'ssh <machine>' must work (a Host block in
~/.ssh/config can give the name an address), and zm must be on that machine.
--on <machine> skips this machine's daemon and asks that one, for a session
started there that is not listed here yet, or a machine in no team project.
--read-only watches without typing: keystrokes (except ctrl-]) are dropped, the
terminal size is left alone and a permission prompt stays needs_human.

--embed is for a terminal embedded in another app (one surface per session):
ctrl-] is an ordinary key (the host detaches by closing the surface), nothing is
printed to stderr but errors, and while the scrollback replays (until output,
once it has begun, has been quiet for 250 ms; at most 3 s) input that is
exactly a terminal's answer to a query in the replay (cursor position, device
attributes, device status, colours, version and other DCS replies, mode
reports, kitty keyboard flags) is dropped instead of reaching the agent as
typing; a bracketed paste always passes.
Experimental --replay-modes restores observed screen/paste modes on a new local
--embed surface. It may print an incomplete-replay notice; mouse capture and
complete history are not restored. Ordinary --embed is unchanged. Exit codes:
  0  the session ended (also when it had ended before the attach)
  2  usage, or no such session (or one zm did not start)
  3  the connection to the daemon was lost: attach again
  1  anything else
  4  another machine's session: no answer from it over ssh
  5  another machine's session: ssh refused (keys or host key)
  6  another machine's session: ssh works, zm is not on its PATH
  7  another machine's session: this Mac does not know its name as an ssh host, no ssh ran`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				if embed {
					return exitWith{2, err}
				}
				return err
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if replayModes && (!embed || on != "") {
				return exitWith{2, fmt.Errorf("--replay-modes requires --embed on a new local terminal surface")}
			}
			if on != "" {
				machine, id, found := strings.Cut(args[0], "/")
				if !found {
					id = args[0]
				} else if machine != on {
					err := fmt.Errorf("--on %s, but %s is a session on %s: give the id alone, or name the same machine", on, args[0], machine)
					if embed {
						return exitWith{2, err}
					}
					return err
				}
				return attachOverSSH(orchapi.Session{Machine: on, ID: id}, readOnly, embed, true, whyFile)
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ss, err := orchclient.Session(cmd.Context(), c, args[0])
			if err != nil {
				if embed && (api.IsCode(err, api.CodeNotFound) || api.IsCode(err, api.CodeBadRequest)) {
					return exitWith{2, err}
				}
				return err
			}
			if ss.State == orchapi.StateEnded {
				if embed {
					return nil
				}
				return fmt.Errorf("%s has ended (%s); see its output: zm session output %s", ss.Address, outcomeText(ss.Outcome), ss.ID)
			}
			if !ss.Managed {
				err := fmt.Errorf("%s runs in its own terminal (zm did not start it)", ss.Address)
				if embed {
					return exitWith{2, err}
				}
				return err
			}
			if ss.Seen != nil { // known from its machine's heartbeat: it runs there
				if replayModes {
					return exitWith{2, fmt.Errorf("--replay-modes requires a new local terminal surface")}
				}
				return attachOverSSH(ss, readOnly, embed, false, whyFile)
			}
			return attachTerminalReplay(cmd.Context(), c, ss, readOnly, embed, replayModes)
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "watch only: nothing you type reaches the session (ctrl-] still detaches)")
	cmd.Flags().StringVar(&whyFile, "why-file", "", "for a host app: write why an attach over ssh failed to this file")
	_ = cmd.Flags().MarkHidden("why-file")
	cmd.Flags().StringVar(&on, "on", "", "attach on this machine over ssh, asking its daemon, not this one")
	cmd.Flags().BoolVar(&replayModes, "replay-modes", false, "experimental: restore observed screen/paste modes on a new local --embed surface")
	cmd.Flags().BoolVar(&embed, "embed", false, "for a host app's terminal surface: no ctrl-], no stderr notes, drops replayed query answers, exit 0 ended / 3 connection lost / 2 not found / 4-6 ssh (see --help)")
	return cmd
}

func outcomeText(o *orchapi.Outcome) string {
	if o == nil {
		return "no outcome"
	}
	s := string(o.Kind)
	if o.ExitCode != nil {
		s += fmt.Sprintf(", exit %d", *o.ExitCode)
	}
	if o.Signal != "" {
		s += ", SIG" + o.Signal
	}
	if o.Evidence != "" && o.Kind != orchapi.OutcomeOK {
		s += ": " + o.Evidence
	}
	if o.Note != "" {
		s += " (" + o.Note + ")"
	}
	return s
}

// attachTerminal connects this terminal (raw mode) to a session until the
// user presses ctrl-] or the session ends. embed: see attachCmd (--embed).
func attachTerminal(ctx context.Context, c *client.Client, ss orchapi.Session, readOnly, embed bool) error {
	return attachTerminalReplay(ctx, c, ss, readOnly, embed, false)
}
func attachTerminalReplay(ctx context.Context, c *client.Client, ss orchapi.Session, readOnly, embed, replayModes bool) error {
	bind := func(f func(context.Context, *client.Client, string) (io.ReadWriteCloser, error)) func(context.Context, string) (io.ReadWriteCloser, error) {
		return func(ctx context.Context, id string) (io.ReadWriteCloser, error) { return f(ctx, c, id) }
	}
	attach, how := bind(orchclient.Attach), "attached to"
	framedReplay := false
	if embed {
		attach = bind(orchclient.AttachAll)
	}
	if embed && replayModes { // explicitly opted-in fresh-surface experiment
		attach = func(ctx context.Context, id string) (io.ReadWriteCloser, error) {
			stream, err := orchclient.AttachEmbed(ctx, c, id, readOnly)
			if err != nil {
				return nil, err
			}
			framedReplay = stream.Framed
			return stream, nil
		}
	}
	if readOnly {
		how = "watching (read-only)"
		if !embed || !replayModes {
			attach = bind(orchclient.AttachReadOnly)
		}
	}
	conn, err := attach(ctx, ss.ID)
	if err != nil {
		if embed { // ended, gone or unreachable between the lookup and the attach
			switch now, e := orchclient.Session(context.Background(), c, ss.ID); {
			case e == nil && now.State == orchapi.StateEnded:
				return nil
			case api.IsCode(e, api.CodeNotFound):
				return exitWith{2, e}
			case e != nil:
				return exitWith{3, e}
			}
		}
		return err
	}
	defer conn.Close()
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	tty := term.IsTerminal(in)
	note := func(format string, a ...any) {
		if !embed {
			fmt.Fprintf(os.Stderr, format, a...)
		}
	}
	note("zm: %s %s (%s). Detach: ctrl-]\r\n", how, ss.Address, ss.Agent)
	var window *replyWindow
	if embed {
		window = newReplyWindow(nil) // from the attach: the replay comes first
	}
	restore := func() {}
	if tty {
		old, err := term.MakeRaw(in)
		if err != nil {
			return err
		}
		restore = func() { _ = term.Restore(in, old) }
		defer restore()
		resize := func(shrink int) {
			if readOnly {
				return // a viewer never resizes the session's terminal
			}
			if w, h, err := term.GetSize(out); err == nil && w > shrink {
				_ = orchclient.Resize(ctx, c, ss.ID, orchapi.ResizeRequest{Rows: h, Cols: w - shrink})
			}
		}
		// TUIs repaint on SIGWINCH, which only fires when the size changes:
		// wiggle it so the agent redraws over the replayed scrollback.
		resize(1)
		resize(0)
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for {
				select {
				case <-winch:
					resize(0)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	if embed && replayModes && !framedReplay {
		fmt.Fprintln(os.Stderr, replayDegradedNotice)
	}
	ended := make(chan struct{})
	replayErrors := make(chan error, 1)
	go func() {
		if framedReplay {
			_, err := renderEmbedReplay(os.Stdout, conn, window.output, func(message string) { fmt.Fprintln(os.Stderr, message) })
			replayErrors <- err
		} else if window == nil {
			_, _ = io.Copy(os.Stdout, conn)
		} else {
			buf := make([]byte, 32<<10)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					window.output()
					if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		}
		close(ended)
	}()
	detached := make(chan struct{})
	if embed {
		go embedInput(conn, readOnly, window)
	} else {
		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					chunk := buf[:n]
					if i := bytes.IndexByte(chunk, detachKey); tty && i >= 0 {
						if !readOnly {
							_, _ = conn.Write(chunk[:i])
						}
						close(detached)
						return
					}
					if !readOnly {
						if _, err := conn.Write(chunk); err != nil {
							return
						}
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}

	select {
	case <-detached:
		restore()
		fmt.Fprintf(os.Stderr, "\r\nzm: detached from %s; it keeps running. Reattach: zm attach %s\n", ss.Address, ss.ID)
		return nil
	case <-ctx.Done():
		restore()
		return nil
	case <-ended:
	}
	restore()
	select {
	case replayErr := <-replayErrors:
		if replayErr != nil {
			return exitWith{3, replayErr}
		}
	default:
	}
	for i := 0; i < 20; i++ { // the end is recorded before the stream closes; allow a moment
		now, err := orchclient.Session(context.Background(), c, ss.ID)
		if err == nil && now.State == orchapi.StateEnded {
			note("\r\nzm: %s ended: %s\n", ss.Address, outcomeText(now.Outcome))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if embed {
		return exitWith{3, nil} // the stream closed with the session still live (or no daemon): attach again
	}
	fmt.Fprintf(os.Stderr, "\r\nzm: connection to %s closed\n", ss.Address)
	return nil
}

// replyFlushAfter is how long an unfinished escape sequence is held for the
// rest of it before it passes as typed (a lone Escape key).
const replyFlushAfter = 50 * time.Millisecond

// embedInput copies stdin to the session for --embed: ctrl-] is an ordinary
// key, and while window is open a terminal's answers to replayed queries are
// dropped (replyFilter).
func embedInput(conn io.Writer, readOnly bool, window *replyWindow) {
	chunks := make(chan []byte, 16)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	runEmbedInput(chunks, conn, readOnly, window)
}

// runEmbedInput is embedInput's loop over chunks of input (tests feed it).
func runEmbedInput(chunks <-chan []byte, conn io.Writer, readOnly bool, window *replyWindow) {
	var f replyFilter
	filtering := true
	send := func(b []byte) bool {
		if len(b) == 0 || readOnly {
			return true
		}
		_, err := conn.Write(b)
		return err == nil
	}
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	for {
		var fire <-chan time.Time
		if f.Pending() {
			fire = flush.C
		}
		select {
		case b, ok := <-chunks:
			if !ok {
				send(f.Flush())
				return
			}
			var out []byte
			switch {
			case filtering && window.open():
				out = f.Feed(b)
			case filtering && f.Pending():
				// The window closed with a sequence half read: finish it
				// through the filter (an answer is still dropped, never sent
				// half as keys), then pass everything.
				var rest []byte
				out, rest = f.Resolve(b)
				if !f.Pending() {
					filtering = false
					out = append(out, rest...)
				}
			default:
				filtering = false
				out = b
			}
			if f.Pending() {
				flush.Reset(replyFlushAfter)
			}
			if !send(out) {
				return
			}
		case <-fire:
			if !send(f.Flush()) {
				return
			}
		}
	}
}

func sessionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <session>", Short: "One session (what ran, state, outcome)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Session, error) {
				return orchclient.Session(ctx, c, args[0])
			}, nil)
		},
	}
}

func sessionInputCmd() *cobra.Command {
	var enter bool
	cmd := &cobra.Command{
		Use: "input <session> <text...|->", Short: "Type text into a session zm started ('-' reads stdin)", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := joinArgs(args[1:])
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (struct{}, error) {
				return struct{}{}, orchclient.Input(ctx, c, args[0], orchapi.InputRequest{Data: text, Enter: enter})
			}, func(struct{}) string { return "" })
		},
	}
	cmd.Flags().BoolVar(&enter, "enter", true, "press Enter after the text")
	return cmd
}

func stopCmd(use string) *cobra.Command {
	var sig string
	var force bool
	cmd := &cobra.Command{
		Use:   use + " <session>",
		Short: "Stop a session zm started: TERM, then HUP; --force adds KILL (exit 1 if it still runs)",
		Long: `Stop a session zm started. It sends TERM and waits briefly; a session still
running after 3 s (an interactive shell ignores TERM) gets HUP, as when its terminal
closes, and 1 s more.
--force then sends KILL, which nothing can ignore but gives the agent no chance to
clean up. --signal sends only that one signal. The command exits 1 whenever the
session is still running at the end.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			id := args[0]
			if _, _, named := api.SessionNameRef(id); named {
				// A name reaches only a live session: resolve it once, so each
				// step (and the last read, once it ended) names the address.
				ss, err := orchclient.Session(cmd.Context(), c, id)
				if err != nil {
					return err
				}
				id = ss.Address
			}
			// Another machine's session (one started from here, SPEC 8) takes
			// one plain stop through its authority: no escalation crosses.
			if a, err := api.ParseAddress(id); err == nil && a.Kind == api.KindSession && sig == "" && !force {
				if h, err := c.Health(cmd.Context()); err == nil && h.Machine != "" && a.Machine != h.Machine {
					if _, err := orchclient.Stop(cmd.Context(), c, id, orchapi.StopRequest{}); err != nil {
						return err
					}
					if !flagJSON {
						fmt.Println(id + ": stop sent to " + a.Machine)
					}
					return nil
				}
			}
			stop := func(signal string, wait time.Duration) (orchapi.Session, error) {
				return orchclient.Stop(cmd.Context(), c, id, orchapi.StopRequest{Signal: signal, WaitMS: int(wait / time.Millisecond)})
			}
			get := func() (orchapi.Session, error) { return orchclient.Session(cmd.Context(), c, id) }
			s, sent, err := escalateStop(stop, get, stopPlan(sig, force))
			if err != nil {
				return err
			}
			if flagJSON {
				if err := printJSON(s); err != nil {
					return err
				}
			}
			if s.State != orchapi.StateEnded {
				if !flagJSON {
					hint := "zm " + use + " --force " + s.ID + " sends KILL"
					if force || sig != "" {
						hint = "see what it is doing: zm session output " + s.ID
					}
					fmt.Fprintf(os.Stderr, "zm: %s is still %s after %s; %s\n", s.Address, s.State, strings.Join(sent, ", "), hint)
				}
				return exitSilently(1)
			}
			if !flagJSON {
				how := ""
				if len(sent) > 1 {
					how = " after " + sent[len(sent)-1] + " (" + strings.Join(sent[:len(sent)-1], ", ") + " ignored)"
				}
				fmt.Println(s.Address + " ended" + how + ": " + outcomeText(s.Outcome))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sig, "signal", "", "send only this signal: TERM, INT, HUP or KILL")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "if TERM and HUP do not stop it, send KILL")
	return cmd
}

// stopStep is one signal and how long to wait for the process to exit.
type stopStep struct {
	signal string
	wait   time.Duration
}

// stopPlan is the escalation: TERM, then HUP (what a closing terminal sends;
// interactive shells ignore TERM but not HUP), then KILL only when forced.
func stopPlan(signal string, force bool) []stopStep {
	if signal != "" {
		return []stopStep{{strings.ToUpper(strings.TrimPrefix(strings.ToUpper(signal), "SIG")), 3 * time.Second}}
	}
	// TERM gets the time an agent needs to flush and exit; only the HUP step
	// (for a shell that ignored TERM) is short. Each wait ends with the session.
	plan := []stopStep{{"TERM", 3 * time.Second}, {"HUP", time.Second}}
	if force {
		plan = append(plan, stopStep{"KILL", 3 * time.Second})
	}
	return plan
}

// escalateStop runs plan until the session has ended. It returns the session
// as last seen and the signals sent. A conflict after the first step (the
// session ended between two steps) is the end, not an error: get reads it.
func escalateStop(stop func(signal string, wait time.Duration) (orchapi.Session, error), get func() (orchapi.Session, error),
	plan []stopStep) (orchapi.Session, []string, error) {
	var s orchapi.Session
	var sent []string
	for i, st := range plan {
		next, err := stop(st.signal, st.wait)
		if err != nil {
			if i > 0 && api.IsCode(err, api.CodeConflict) {
				if now, gerr := get(); gerr == nil {
					return now, sent, nil
				}
			}
			return s, sent, err
		}
		s = next
		sent = append(sent, st.signal)
		if s.State == orchapi.StateEnded {
			break
		}
	}
	return s, sent, nil
}

func sessionStateCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use: "state <session> <running|needs_human|idle>", Short: "Report what a session is doing (hooks)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Session, error) {
				return orchclient.SetState(ctx, c, args[0], orchapi.StateRequest{State: orchapi.SessionState(args[1]), Note: note})
			}, func(s orchapi.Session) string { return "" })
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "why")
	return cmd
}

func sessionNameCmd() *cobra.Command {
	return &cobra.Command{
		Use: "name <session> <name>", Short: `Name a session ("" clears it); the name reaches it while it is live, unique on its machine`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Session, error) {
				return orchclient.SetSessionName(ctx, c, args[0], args[1])
			}, func(s orchapi.Session) string {
				if s.Name == "" {
					return s.Address + " has no name"
				}
				return s.Address + " is " + s.Machine + "/" + s.Name
			})
		},
	}
}

func sessionMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "move <session> <project>[/<area>]", Short: "Put a session in another project (or none); refused while it holds a lease",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Session, error) {
				return orchclient.MoveSession(ctx, c, args[0], orchapi.MoveRequest{Project: args[1]})
			}, func(s orchapi.Session) string { return s.Address + " is in " + args[1] })
		},
	}
}

func sessionPromptCmd() *cobra.Command {
	return &cobra.Command{
		Use: "prompt <session> <text>", Short: "Send a prompt to a shared session (logged; a team project's members with operate)",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.SessionPrompt, error) {
				var p orchapi.SessionPrompt
				err := c.Do(ctx, http.MethodPost, "/v0/sessions/"+client.Seg(args[0])+"/prompt", orchapi.PromptRequest{Text: args[1]}, &p)
				return p, err
			}, func(p orchapi.SessionPrompt) string { return p.Session + ": prompt " + p.State })
		},
	}
}

func sessionPromptsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "prompts <session>", Short: "A shared session's prompt log, newest first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PromptList, error) {
				var l orchapi.PromptList
				err := c.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(args[0])+"/prompts", nil, &l)
				return l, err
			}, func(l orchapi.PromptList) string {
				var b strings.Builder
				for _, p := range l.Prompts {
					fmt.Fprintf(&b, "%s  %-9s %s: %s\n", p.At.Local().Format("2006-01-02 15:04"), p.State, p.Who, promptLine(p.Text))
				}
				return b.String()
			})
		},
	}
}

func promptLine(s string) string {
	l, _, more := strings.Cut(s, "\n")
	if more {
		l += " …"
	}
	return l
}

func sessionRegisterCmd() *cobra.Command {
	var req orchapi.RegisterRequest
	cmd := &cobra.Command{
		Use: "register", Short: "Announce an agent session zm did not start (hooks; files it by its cwd, else in project none)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if req.Cwd == "" {
				req.Cwd, _ = os.Getwd()
			}
			req.For, req.JobKind = attributionFlags(cmd)
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Session, error) {
				return orchclient.Register(ctx, c, req)
			},
				func(s orchapi.Session) string { return s.Address })
		},
	}
	cmd.Flags().StringVar(&req.Agent, "agent", "", "claude, codex, opencode, gemini")
	cmd.Flags().StringVar(&req.RuntimeID, "runtime-id", "", "the agent's own session id")
	cmd.Flags().StringVar(&req.Cwd, "cwd", "", "the agent's working directory (default: here)")
	cmd.Flags().IntVar(&req.PID, "pid", 0, "agent pid (the session ends when it exits)")
	cmd.Flags().StringVar(&req.Title, "title", "", "title")
	cmd.Flags().StringVar(&req.Model, "model", "", "model")
	cmd.Flags().StringVar(&req.Version, "version", "", "runtime version")
	cmd.Flags().StringVar((*string)(&req.State), "state", "", "also set its state: running, idle, needs_human")
	addAttributionFlags(cmd)
	return cmd
}

// addAttributionFlags adds --for and --job-kind (SPEC 8 "Usage") to a
// command that starts or registers a session.
func addAttributionFlags(cmd *cobra.Command) {
	cmd.Flags().String("for", "", "the identity this session works for (default $ZM_FOR)")
	cmd.Flags().String("job-kind", "", "interactive, standing, farm or timer (default $ZM_JOB_KIND)")
}

// attributionFlags reads --for and --job-kind, else $ZM_FOR and
// $ZM_JOB_KIND, setting the flags from the environment so a forwarded
// command (zm run --on) carries them.
func attributionFlags(cmd *cobra.Command) (string, string) {
	get := func(flag, env string) string {
		if v, _ := cmd.Flags().GetString(flag); cmd.Flags().Changed(flag) {
			return v
		}
		v := os.Getenv(env)
		if v != "" {
			_ = cmd.Flags().Set(flag, v)
		}
		return v
	}
	return get("for", "ZM_FOR"), get("job-kind", "ZM_JOB_KIND")
}

func sessionOutputCmd() *cobra.Command {
	var since int64
	cmd := &cobra.Command{
		Use: "output <session>", Short: "Recent terminal output of a session zm started (also after it ended)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Output, error) {
				return orchclient.Output(ctx, c, args[0], since)
			},
				func(o orchapi.Output) string { return o.Data })
		},
	}
	cmd.Flags().Int64Var(&since, "since", 0, "byte offset to continue from")
	return cmd
}

func sessionGCCmd() *cobra.Command {
	var transcript string
	var discardRedaction string
	cmd := &cobra.Command{
		Use: "gc", Short: "List ended worktrees and task transcripts; --transcript removes one transcript", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if discardRedaction != "" {
				if transcript != "" {
					return fmt.Errorf("choose either --transcript or --discard-redaction")
				}
				return call(cmd, func(ctx context.Context, c *client.Client) (any, error) {
					return nil, c.Do(ctx, "DELETE", "/v0/tasks/"+discardRedaction+"/transcript?discard_redaction=1", nil, nil)
				}, func(_ any) string { return "task output discarded; redaction released" })
			}
			if transcript != "" {
				return call(cmd, func(ctx context.Context, c *client.Client) (any, error) {
					return nil, c.Do(ctx, "DELETE", "/v0/tasks/"+transcript+"/transcript", nil, nil)
				}, func(_ any) string { return "task transcript removed" })
			}
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.GCCandidate, error) {
				return orchclient.SessionGC(ctx, c)
			},
				func(cs []orchapi.GCCandidate) string {
					if len(cs) == 0 {
						return "no ended sessions with worktrees"
					}
					var b strings.Builder
					for _, x := range cs {
						safe := "keep"
						if x.Safe {
							safe = "safe to delete"
						}
						fmt.Fprintf(&b, "%s  %s  %s: %s\n", x.Session, x.Worktree, safe, strings.Join(x.Reasons, ", "))
						if x.Safe && x.Remove != "" {
							fmt.Fprintf(&b, "    %s\n", x.Remove)
						}
						if x.TranscriptRemove != "" {
							fmt.Fprintf(&b, "    %s\n", x.TranscriptRemove)
						}
					}
					return b.String()
				})
		},
	}
	cmd.Flags().StringVar(&transcript, "transcript", "", "remove an ended task's transcript")
	cmd.Flags().StringVar(&discardRedaction, "discard-redaction", "", "discard output and release an ended task's redaction hold after 30 seconds")
	return cmd
}

func sessionAdmissionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "admission", Short: "Would a new session be admitted now? (memory, swap, disk, session count)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Admission, error) {
				return orchclient.Admission(ctx, c)
			},
				func(a orchapi.Admission) string {
					p := a.Probe
					s := fmt.Sprintf("memory free %d%%, swap used %d MB, disk free %d MB, %d managed sessions",
						p.MemoryFreePercent, p.SwapUsedMB, p.DiskFreeMB, p.ManagedSessions)
					if a.OK {
						return "admitted: " + s
					}
					return "refused (" + a.Reason + ": " + a.Detail + "): " + s
				})
		},
	}
}

// sessionHookCmd turns one agent hook into one register call (state
// included). It never fails and never prints (a Claude Code
// UserPromptSubmit hook's stdout becomes context). `zm hook` (install.go)
// is the integrations builder's entry point; this is the session module's.
func sessionHookCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "hook <event> [payload-json]",
		Short: "For agent hooks: register this session and report its state (never fails, prints nothing)",
		Long: "Events: " + strings.Join(session.HookEvents, ", ") + `.
Claude Code and Gemini CLI pass the payload on stdin; Codex's notify passes it
as the last argument. Inside a session zm started ($ZM_SESSION), the report
goes to that session. Outside, the session is registered by the agent's own id,
in the project its cwd resolves to (else project none).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer func() { _ = recover() }()
			var input []byte
			if len(args) == 2 {
				input = []byte(args[1])
			} else {
				input = readPayload(time.Second)
			}
			req, ok := session.ParseHook(args[0], agent, input, os.Getenv)
			if !ok {
				return nil
			}
			req.PID = integrations.AgentPID(req.Agent)
			c, _, err := newClient()
			if err != nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
			defer cancel()
			_, _ = orchclient.Register(ctx, c, req)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "claude (default), codex (default for codex-notify), gemini, opencode")
	return cmd
}

// readPayload reads a hook payload from stdin, giving up after d (stdin may never close).
func readPayload(d time.Duration) []byte {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	got := make(chan []byte, 1)
	go func() { got <- readCapped(os.Stdin) }()
	select {
	case b := <-got:
		return b
	case <-time.After(d):
		return nil
	}
}
