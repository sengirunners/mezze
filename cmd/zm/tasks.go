package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

func init() { rootCmd.AddCommand(tasksCmd()) }

// tasksCmd is the remote tasks of a team project and their waiting line at
// the authority (SPEC 8 "Machine choice", "Waiting line"): what hub
// dispatch, tasks and status call.
func tasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tasks -P <project>",
		Short: "List a team project's remote tasks: the waiting line in order, then the running and the newest settled",
		Long: `A task started on another machine (task_start --machine, zm tasks start, zm exec)
waits at the project's authority until a suitable machine is free: one that
joined the project, is trusted there, takes remote tasks and has a free slot
(its remote_task_slots, default 1; a sandbox task uses its sandbox_task_slots
instead). It then starts there within about a
second. --front puts a start first; --machine <name> pins it to that machine.
A task whose machine is lost goes back to the line once, unless zm saw it act
outward (mail, a comment, a status change, a push) or its time is spent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			project := flagProject
			if project == "" {
				project = os.Getenv("ZM_PROJECT")
			}
			if project == "" {
				return errors.New("name the team project: zm tasks -P <project>")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.RemoteTaskList, error) {
				return orchclient.RemoteTasks(ctx, c, project)
			}, func(l orchapi.RemoteTaskList) string {
				if len(l.Tasks) == 0 {
					return "no remote tasks"
				}
				var b strings.Builder
				for _, t := range l.Tasks {
					b.WriteString(taskLine(t) + "\n")
				}
				return b.String()
			})
		},
	}
	cmd.AddCommand(tasksStartCmd(), tasksStatusCmd(), tasksCancelCmd())
	return cmd
}

// taskLine is one task in a line: "#2 waiting  rt-…  codex  on dev-02".
func taskLine(t orchapi.Task) string {
	where := t.Machine
	if where == "" {
		where = "any machine"
	}
	state := t.State
	switch {
	case t.Position > 0:
		state = fmt.Sprintf("#%d waiting", t.Position)
	case t.Lost != "":
		state = "lost"
	}
	agent := t.Agent
	if t.Sandbox != nil {
		agent += " sandbox"
		if t.Sandbox.Size != "" {
			agent += " " + t.Sandbox.Size
		}
	}
	line := fmt.Sprintf("%-11s %s  %s  on %s", state, t.ID, agent, where)
	if t.Work != "" {
		line += "  " + t.Work
	}
	if t.Requeued > 0 {
		line += fmt.Sprintf("  (requeued %d)", t.Requeued)
	}
	if t.Lost != "" {
		line += "  " + t.Lost
	} else if t.Error != "" {
		line += "  " + t.Error
	}
	return line
}

func tasksStartCmd() *cobra.Command {
	var machine, agent, model, work, cwd, requestID, where, size string
	var front, sandbox bool
	var egress []string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start --machine <name|auto> | --where cloud | --sandbox [--size c2m8] [--egress host,...] [--front] [--agent codex] [--work <id>] -- <prompt...>",
		Short: "Start an agent task on another machine of the team project (it waits in the line for a free one)",
		Long: `Run it from a zm session: the task is that session's (at most 3 of a parent's
run at once; more wait). The answer is the task's id (rt-…), printed first.
A prompt of "-" reads stdin. --where cloud runs it on the provider's cloud computer
instead, from this machine's claude or codex login, for a project its owner opted
in (zm project cloud); the cloud has no zm, so the prompt must be self-contained.
--sandbox runs it in a pool box through a machine's task driver (its sandbox_task_cmd,
on its sandbox_task_slots); --machine defaults to auto. --size names the box size;
--egress adds hosts (name.tld or *.name.tld) to the box's egress allowlist for this task.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if size != "" && !sandbox {
				return errors.New("--size goes with --sandbox")
			}
			if len(egress) > 0 && !sandbox {
				return errors.New("--egress goes with --sandbox")
			}
			hosts := egress[:0]
			for _, h := range egress {
				if h = strings.TrimSpace(h); h != "" {
					hosts = append(hosts, h)
				}
			}
			if sandbox && machine == "" && where == "" {
				machine = "auto"
			}
			if machine == "" && where == "" {
				return errors.New("--machine is required: a machine of the team project, or auto (or --where cloud)")
			}
			prompt, err := joinArgs(args)
			if err != nil {
				return err
			}
			req := orchapi.TaskStartRequest{Agent: agent, Model: model, Prompt: prompt, Work: work, Cwd: cwd, Machine: machine,
				Front: front, ClientRequestID: requestID, TimeoutSeconds: int(timeout / time.Second)}
			if sandbox {
				req.Sandbox = &orchapi.TaskSandbox{Size: size, Egress: hosts}
			}
			if where != "" {
				return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.TaskView, error) {
					return orchclient.TaskStartWhere(ctx, c, "", where, req)
				}, func(t orchapi.TaskView) string {
					t.Machine = strings.TrimSuffix(t.Agent, "-cloud") + " cloud" // "on codex cloud"
					return taskLine(t.Task)
				})
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Task, error) {
				return orchclient.TaskStart(ctx, c, req)
			}, taskLine)
		},
	}
	f := cmd.Flags()
	f.StringVar(&machine, "machine", "", "a machine of the team project (pinned: it waits for that one), or auto")
	f.BoolVar(&front, "front", false, "put it before every start waiting in the line")
	f.BoolVar(&sandbox, "sandbox", false, "run it in a pool box through a machine's task driver")
	f.StringVar(&size, "size", "", "with --sandbox: the box size, such as c2m8 (default: the driver's)")
	f.StringSliceVar(&egress, "egress", nil, "with --sandbox: hosts the box may reach beyond the driver's list (name.tld or *.name.tld)")
	f.StringVar(&where, "where", "", "cloud: the provider's cloud computer instead of a machine")
	f.StringVar(&agent, "agent", "codex", "claude or codex")
	f.StringVar(&model, "model", "", "the agent's model")
	f.StringVar(&work, "work", "", "a work item its result is commented on")
	f.StringVar(&cwd, "cwd", "", "an absolute path on that machine (default: its checkout of the project)")
	f.StringVar(&requestID, "request-id", "", "a client request id: a repeat answers the same task")
	f.DurationVar(&timeout, "timeout", 0, "its time budget (default: the machine's task deadline, 4h)")
	return cmd
}

func tasksStatusCmd() *cobra.Command {
	var wait int
	cmd := &cobra.Command{
		Use:   "status <rt-id>",
		Short: "A remote task's status (its place in the line while it waits)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Task, error) {
				return orchclient.TaskStatus(ctx, c, args[0], wait)
			}, taskLine)
		},
	}
	cmd.Flags().IntVar(&wait, "wait", 0, "wait up to this many seconds (at most 45) for a change")
	return cmd
}

func tasksCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <rt-id>",
		Short: "Cancel a remote task: a waiting one leaves the line, a running one stops",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Task, error) {
				return orchclient.TaskCancel(ctx, c, args[0])
			}, taskLine)
		},
	}
}
