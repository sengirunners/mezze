package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
)

func init() { rootCmd.AddCommand(execCmd()) }

// execCmd runs a repository script from a pushed commit on another machine
// of the team project, through its authority (SPEC 8 "Script runs"): no ssh.
func execCmd() *cobra.Command {
	var machine, ref, commit, requestID, cancelID string
	var front bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "exec --machine <name|auto> [--ref main] -- <script> [args...]",
		Short: "Run a repository script from a pushed commit on another machine (no ssh)",
		Long: `Run a script from the project's repository on another zm machine of its team
project, through the authority: no ssh, no keys. The machine resolves --ref on
its checkout's remote (a live lookup, never a local branch), runs <script> (a
path in the repository) from a clean checkout of exactly that commit with the
arguments as given, and removes the checkout after. Only PATH, HOME, LANG, TERM
and TZ (and zm's session variables) reach the script; stdin is empty.

Run it from a zm session (the run is that session's task). The output prints
when the run ends, and zm exec exits with the script's exit code (124: it hit
--timeout; 130: you cancelled it with ctrl-c).

A supervisor that may kill zm exec outright (SIGKILL, OOM) passes its own
--request-id and records it; afterwards, from the same session,
'zm exec --cancel <request-id>' with the run's --ref, --commit, --timeout,
script and arguments (any --machine but this one: it is not compared) cancels
that run and waits until its status confirms it (the run ends killed, or one
not started yet never runs). It never starts a run: an unknown request id is
not found. An unconfirmed cancel says so and exits 1.

With -P, the run goes through that team project's authority instead of the
session's project (one you own or administer; its machines, its checkout).`,
		Example: `  zm exec --machine dev-02 -- scripts/test.sh --unit
  zm exec --machine auto --ref release/1.4 --timeout 30m -- scripts/smoke.sh
  zm exec --cancel ci-4711 --machine auto -- scripts/test.sh --unit`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if machine == "" {
				return errors.New("--machine is required: a machine of the team project, or auto")
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if cancelID != "" {
				if requestID != "" && requestID != cancelID {
					return errors.New("--cancel names the request: leave --request-id out")
				}
				requestID = cancelID
			}
			if requestID == "" {
				requestID = "exec-" + strings.ToLower(ids.New())
			}
			req := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: machine, TimeoutSeconds: int(timeout / time.Second),
				Front: front, ClientRequestID: requestID, Run: &orchapi.TaskRun{Ref: ref, Commit: commit, Script: args[0], Args: args[1:]}}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			tasks := orchclient.TasksIn(c, flagProject)
			if cancelID != "" {
				cctx, cancel := context.WithTimeout(ctx, 45*time.Second) // the lookup, the cancel and 30 s to confirm it
				defer cancel()
				t, err := orchclient.CancelRun(cctx, tasks, req)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(t)
				}
				fmt.Printf("cancelled %s on %s (request %s): %s\n", t.ID, t.Machine, requestID, t.State) // confirmed: ended or refused
				return nil
			}
			fmt.Fprintf(os.Stderr, "zm exec: request %s\n", requestID) // rerun with --request-id to reattach, --cancel to stop
			// A signal during the start cancels the run the start may have queued.
			t, err := orchclient.StartRun(ctx, tasks, req)
			if err != nil && ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "zm exec: %v\n", err)
				return exitWith{code: 130, err: fmt.Errorf("cancelled request %s", requestID)}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "zm exec: %s on %s\n", t.ID, t.Machine)
			for t.State != "ended" && t.State != orchapi.TaskRefused {
				next, err := orchclient.TaskStatus(ctx, c, t.ID, 45)
				if ctx.Err() != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					_, cerr := orchclient.TaskCancel(cctx, c, t.ID)
					cancel()
					if cerr != nil {
						fmt.Fprintf(os.Stderr, "zm exec: cancel %s: %v\n", t.ID, cerr)
					}
					return exitWith{code: 130, err: fmt.Errorf("cancelled %s", t.ID)}
				}
				if err != nil {
					return err
				}
				t = next
			}
			if t.State == orchapi.TaskRefused {
				return fmt.Errorf("%s refused the run: %s", t.Machine, t.Error)
			}
			printed := false
			if !flagJSON {
				read := func(ctx context.Context, id, item string, offset int) (orchapi.TaskHistoryPage, error) {
					return orchclient.TaskRead(ctx, c, id, item, offset)
				}
				printed = execOutput(ctx, read, t.ID)
			}
			if flagJSON {
				if err := printJSON(t); err != nil {
					return err
				}
			}
			if t.Result == nil {
				return exitWith{code: 1, err: errors.New("the run ended with no result")}
			}
			if !printed && t.Result.Text != "" && !flagJSON {
				fmt.Print(t.Result.Text) // the tail, when the whole output is not readable
			}
			o := t.Result.Outcome
			code := 1
			if o.ExitCode != nil {
				code = *o.ExitCode
			}
			if code == 0 && o.Kind != orchapi.OutcomeOK {
				code = 1
			}
			if code != 0 {
				return exitWith{code: code, err: fmt.Errorf("%s: %s", t.ID, o.Evidence)}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&machine, "machine", "", "the machine to run on: a name, or auto (the roomiest)")
	f.StringVar(&ref, "ref", "main", "the pushed branch or tag to run")
	f.StringVar(&commit, "commit", "", "refuse to run unless --ref resolves to this commit")
	f.DurationVar(&timeout, "timeout", 0, "end the run after this long (exit 124); default: the machine's task deadline")
	f.StringVar(&requestID, "request-id", "", "idempotency key: the same key answers the same run")
	f.BoolVar(&front, "front", false, "put it before every start waiting in the authority's line")
	f.StringVar(&cancelID, "cancel", "", "cancel the run this request id started (its --ref, --commit, --timeout, script and arguments), confirm, then exit; never starts one")
	return cmd
}

// execOutput prints a run's whole output to stdout; false when it could not
// be read (still withheld pending redaction, or gone).
func execOutput(ctx context.Context, read func(context.Context, string, string, int) (orchapi.TaskHistoryPage, error), id string) bool {
	off := 0
	for tries := 0; ; {
		page, err := read(ctx, id, orchapi.TaskRunOutput, off)
		if err != nil {
			var ae *api.Error
			if errors.As(err, &ae) && ae.Code == api.CodeConflict && strings.Contains(ae.Message, "withheld") && tries < 12 {
				tries++ // output is held until redaction settles
				select {
				case <-ctx.Done():
					return off > 0
				case <-time.After(5 * time.Second):
				}
				continue
			}
			if off > 0 {
				fmt.Fprintf(os.Stderr, "\nzm exec: output cut short: %v\n", err)
			}
			return off > 0
		}
		for _, it := range page.Items {
			fmt.Print(it.Text)
		}
		if page.Next == "" {
			return true
		}
		n, err := strconv.Atoi(page.Next)
		if err != nil || n <= off {
			return true
		}
		off = n
	}
}
