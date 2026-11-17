package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// checkTimeout bounds one run of an ask's verify check.
const checkTimeout = 10 * time.Minute

// askCheckCmd is zm ask check: run an approved ask's verify check here, as
// you, and report its exit (SPEC 16 "Approved, not done").
func askCheckCmd() *cobra.Command {
	var giveUp, evidence string
	var yes bool
	cmd := &cobra.Command{
		Use:   "check <id>",
		Short: "Run an approved ask's verify check and report it: only an exit of 0 records DONE",
		Long: `Runs the ask's verify command (the one frozen on its card) with sh -c in this directory, as you,
for at most 10 minutes, and reports its exit and last lines. It runs only once any action finished:
until a check passes then, the ask stays APPROVED, NOT DONE. An action its machine refused or failed may
have been done another way: a passing check records it DONE too. --give-up "<why>" gives the approved
work up instead (also an action that is stuck; zm does not stop a task already started): it fails,
and stays on top of Decisions until an owner confirms it. --evidence "<what shows it>" records it
done without the check (a check that can never pass, or none on a card whose action failed and was done by hand, then by a person only): only its asker or a person who owns or administers the project, and the
DONE record quotes the evidence and names who gave it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			a, err := orchclient.GetAsk(ctx, c, args[0])
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("give-up") && strings.TrimSpace(giveUp) == "" {
				return api.Errorf(api.CodeBadRequest, "--give-up needs a reason")
			}
			if cmd.Flags().Changed("evidence") {
				if strings.TrimSpace(evidence) == "" || giveUp != "" {
					return api.Errorf(api.CodeBadRequest, "--evidence needs one line saying what shows the work is done, and no --give-up")
				}
				out, err := orchclient.CheckAsk(ctx, c, a.ID, orchapi.CheckRequest{CardSHA: a.CardSHA, Evidence: evidence})
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(out)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "ask %s: recorded done with evidence.\n", out.ID)
				return nil
			}
			if a.Card == nil || (a.Card.Verify == nil && (giveUp == "" || a.Card.Action == nil)) {
				return api.Errorf(api.CodeConflict, "ask %s has no verify check", a.ID)
			}
			req := orchapi.CheckRequest{CardSHA: a.CardSHA, GiveUp: giveUp}
			if giveUp == "" {
				switch {
				case a.ActionState == nil:
					return api.Errorf(api.CodeConflict, "ask %s is not approved yet", a.ID)
				case a.ActionState.State == orchapi.ActionQueued || a.ActionState.State == orchapi.ActionRunning:
					return api.Errorf(api.CodeConflict, "ask %s's action is still %s: run the check once it finished", a.ID, a.ActionState.State)
				case a.ActionState.State != orchapi.ActionApproved && a.ActionState.State != orchapi.ActionFailed:
					return api.Errorf(api.CodeConflict, "ask %s's work is already %s", a.ID, a.ActionState.State)
				case a.ActionState.State == orchapi.ActionFailed && (a.ActionState.Task == "" || strings.HasPrefix(a.ActionState.Reason, orchapi.ActionGaveUpPrefix)):
					// It never started, or its asker gave it up: the server refuses the check, so do not run the command first.
					return api.Errorf(api.CodeConflict, "ask %s's work failed for good: %s", a.ID, a.ActionState.Reason).WithHint("ask again, naming ask " + a.ID)
				}
				if w, err := c.Whoami(ctx); (err != nil || w.As == "" || w.As != a.AskedBy) && !yes {
					return fmt.Errorf("ask %s's check is its asker's command: %q. Run it as the asker, or add --yes to run it as you", a.ID, a.Card.Verify.Command)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "running: %s\n", a.Card.Verify.Command)
				exit, output := runCheck(ctx, a.Card.Verify.Command)
				req.Exit, req.Output = &exit, output
			}
			out, err := orchclient.CheckAsk(ctx, c, a.ID, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			if giveUp != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "ask %s: given up. %s\n", out.ID, out.ActionStateText)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ask %s: check exited %d. %s\n", out.ID, *req.Exit, out.ActionStateText)
			if out.ActionState == nil || out.ActionState.State != orchapi.ActionDone {
				return exitSilently(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&giveUp, "give-up", "", "give the approved work up, with a plain reason (fails it; nothing runs)")
	cmd.Flags().BoolVar(&yes, "yes", false, "run the check although you are not its asker")
	cmd.Flags().StringVar(&evidence, "evidence", "", "record the approved work done without its check, with one line saying what shows it (asker, or a project owner or admin)")
	return cmd
}

// runCheck runs command with sh -c and returns its exit status and the
// tail of its combined output (-1 when it could not run or timed out).
func runCheck(ctx context.Context, command string) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var buf bytes.Buffer
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Stdout, c.Stderr = &buf, &buf
	// Its own process group, all of it ended at the timeout; and a child
	// left holding the output open never keeps the run waiting.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 5 * time.Second
	err := c.Run()
	out := buf.String()
	if len(out) > orchapi.MaxCheckOutput {
		out = strings.ToValidUTF8(out[len(out)-orchapi.MaxCheckOutput:], "")
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, out
	case ctx.Err() != nil:
		return -1, out + "\n(timed out after " + checkTimeout.String() + ")"
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return ee.ExitCode(), out
	}
	return -1, out + "\n" + err.Error()
}
