package main

// zm ci doctor is potluck ci doctor (potluck cli/ci_doctor.go) under zm's
// name: the broker's read-only checks of why a repository's jobs are not
// running. The checks are the broker's (GET /v0/runners/doctor, local); zm
// only prints them. With --probe it runs Potluck's own ci doctor --probe
// (ci_doctor_probe.go).

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/runners"
)

func ciDoctorCmd() *cobra.Command {
	var probe bool
	var po ciProbeOpts
	c := &cobra.Command{
		Use:   "doctor [owner/repo]",
		Short: "Read-only checks explaining why a repository's jobs are not running",
		Long: `Runs the broker's numbered checks on this machine: its settings, GitHub
connection, workflows and queued jobs. It changes nothing. Each failure has
one next step, and any failure exits nonzero. Without a repository it checks
the connected ones (zm runners repos) and those whose jobs came here and were
refused. Workflow and waiting-job reads use this machine owner's gh sign-in
(gh auth login), within a budget of reads a run.

With --probe and one repository, check 18 is Potluck's probe of the whole
path: it writes and prints (never runs) a script that pushes a throwaway branch holding
only a generated one-job workflow, waits for that job on this machine's pool,
prints the machine's OS, Xcode and free disk, and deletes the branch, all with
your own gh sign-in. A public or internal repository needs its owner's yes
first: it comes back as an ask (in --json, "ask"), and only the owner gives it,
with --public.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := newClientContext(cmd.Context())
			if err != nil {
				return err
			}
			repo := ""
			if len(args) > 0 {
				repo = args[0]
			}
			var out ciDoctorOut
			if probe {
				if err := zmDaemonFor(cmd, "ci doctor --probe"); err != nil {
					return err
				}
				out, err = ciDoctorProbe(cmd.Context(), repo, po)
			} else {
				out.DoctorResult, err = orchclient.CIDoctor(cmd.Context(), cl, repo)
			}
			if err != nil {
				return err
			}
			failed := false
			for _, r := range out.Reports {
				failed = failed || r.Failed()
			}
			if flagJSON {
				var v any = out.DoctorResult // without --probe, the broker's JSON as it is
				if probe {
					v = out
				}
				if err := printJSON(v); err != nil {
					return err
				}
			} else {
				printCIDoctor(os.Stdout, out.DoctorResult)
				if out.script != "" { // shown as it is, as Potluck's ci doctor --probe shows it
					body, err := os.ReadFile(out.script)
					if err != nil {
						return err
					}
					fmt.Fprintln(os.Stdout, "The probe script (pushing a workflow file needs gh's workflow scope: gh auth refresh -s workflow):")
					fmt.Fprint(os.Stdout, string(body))
				}
				if out.Ask != nil {
					fmt.Fprintf(os.Stdout, "ask: %s\n", strings.Join(strings.Fields(strings.TrimPrefix(out.Ask.line(), "next: ")), " "))
				}
			}
			if failed {
				return fmt.Errorf("repository checks failed")
			}
			return nil
		},
	}
	c.PreRunE = func(cmd *cobra.Command, args []string) error {
		if probe && len(args) == 0 {
			return fmt.Errorf("--probe needs the repository to probe: zm ci doctor owner/repo --probe")
		}
		for _, f := range []string{"public", "labels", "size", "os"} {
			if !probe && cmd.Flags().Changed(f) {
				return fmt.Errorf("--public, --labels, --size and --os only go with --probe")
			}
		}
		return nil
	}
	c.Flags().BoolVar(&probe, "probe", false, "also check 18: prepare (never run) a script that proves the path with a generated test job (Potluck's probe)")
	c.Flags().BoolVar(&po.public, "public", false, "with --probe: you own this public or internal repository and say yes to the probe (never given for an owner)")
	c.Flags().StringVar(&po.labels, "labels", "", "with --probe: the labels the probe job asks for, comma-separated (default: self-hosted, the OS and this machine's pool label)")
	c.Flags().StringVar(&po.size, "size", "", "with --probe: a pool size to ask for (the label becomes <pool label>:<size>)")
	c.Flags().StringVar(&po.osName, "os", "", "with --probe: the OS label the probe job asks for (default macos)")
	return c
}

func printCIDoctor(w io.Writer, out runners.DoctorResult) {
	plain := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, warning := range out.Warnings {
		fmt.Fprintf(w, "warn: %s\n", plain(warning))
	}
	for _, r := range out.Reports {
		fmt.Fprintln(w, plain(r.Repo))
		for _, c := range r.Checks {
			fmt.Fprintf(w, "%2d %-4s %s\n", c.Number, plain(c.Status), plain(c.Message))
			if c.Fix != "" {
				fmt.Fprintf(w, "   Next: %s\n", plain(c.Fix))
			}
		}
	}
}
