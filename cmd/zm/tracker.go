package main

// Owned by stream A2: `zm tracker add|ls|test|rm` (SPEC 17.2).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/secrets"
)

func init() {
	tr := &cobra.Command{Use: "tracker", Short: "Bind trackers (native issues, or a zm-work-<name> adapter) to a project, live",
		Long: `Trackers feed a project's work items. zm tracker add binds one in a single
command: it writes the binding into project.toml (no daemon restart), runs the
read-only conformance checks and the first mirror sync, and rolls back if a
check fails. "native" is zm's own issue store; any other name runs the
adapter zm-work-<name> (on the daemon's PATH, or config.json work_adapters).
Adapters run with an environment allowlist (--env inherit opts a tracker out).`}
	tr.AddCommand(trackerAddCmd(), trackerLsCmd(), trackerTestCmd(), trackerRmCmd(),
		trackerWhereCmd(), trackerBackupCmd()) // homes: tracker_home.go
	rootCmd.AddCommand(tr)
}

func trackerAddCmd() *cobra.Command {
	var (
		req     orchapi.AddTrackerRequest
		newDB   bool
		cfgKV   []string
		envFlag string
		wait    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "add native|<name>",
		Short: "Bind a tracker to the project: native issues, or the adapter zm-work-<name>",
		Example: `  zm tracker add native --prefix bd           # zm's own issues, under the bd: prefix
  zm tracker add acme --config token_file=~/.config/acme/token   # runs zm-work-acme`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			req.Adapter = args[0]
			if newDB {
				req.Mode = orchapi.TrackerModeNew
			}
			if envFlag != "" {
				req.Env = envFlag
			}
			req.WaitSeconds = int(wait.Seconds())
			if req.Config == nil {
				req.Config = map[string]string{}
			}
			for _, kv := range cfgKV {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("--config wants key=value, got %q", kv)
				}
				req.Config[k] = v
			}
			for _, p := range []*string{&req.Dir, &req.PasswordFile} {
				if *p != "" {
					if *p, err = filepath.Abs(expandTilde(*p)); err != nil {
						return err
					}
				}
			}
			start := time.Now()
			res, err := orchclient.AddTracker(cmd.Context(), c, req)
			if err != nil {
				printFailedChecks(err)
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			fmt.Print(addText(res, time.Since(start)))
			return nil
		},
	}
	// The setup options are SPEC 11's: an adapter takes the ones its setup
	// declares (zm refuses the rest).
	f := cmd.Flags()
	f.BoolVar(&newDB, "new", false, "setup option: a new tracker database under $ZM_HOME/trackers")
	f.StringVar(&req.Dir, "dir", "", "setup option: bind an existing tracker directory")
	f.StringVar(&req.Server, "server", "", "setup option: bind a shared server (host:port)")
	f.StringVar(&req.Database, "database", "", "setup option (--server): the database")
	f.StringVar(&req.User, "user", "", "setup option (--server): the user")
	f.StringVar(&req.PasswordFile, "password-file", "", "--server: a file holding the password (copied to $ZM_HOME/secrets, 0600)")
	f.BoolVar(&req.AllowMigrate, "allow-migrate", false, "setup option (--server): let the adapter migrate an older schema (otherwise a mismatch binds read-only)")
	f.StringVar(&req.Prefix, "prefix", "", "id prefix for its items (default: the adapter name)")
	f.StringVar(&req.Area, "area", "", "file its items in this area by default")
	f.BoolVar(&req.Readonly, "readonly", false, "refuse every write to the tracker")
	f.StringVar(&envFlag, "env", "", `"inherit": give the adapter the daemon's whole environment`)
	f.StringArrayVar(&cfgKV, "config", nil, "adapter config key=value (repeatable)")
	f.DurationVar(&wait, "wait", 5*time.Minute, "how long to wait for the first sync (a longer one goes on in the background)")
	return cmd
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// yesHelp is --yes on the commands that fetch pinned tools (SPEC 17.2: zm
// offers the download; off a terminal there is no one to ask).
const yesHelp = "fetch missing or outdated pinned tools without asking (when stdin or stderr is not a terminal zm fetches without asking anyway; --no-download refuses)"

// provisionNames fetches the named pinned tools that home lacks, asking
// first on a terminal.
func provisionNames(ctx context.Context, stderr io.Writer, home string, names []string, download, yes, interactive bool) error {
	var missing []string
	for _, t := range names {
		if _, err := tools.Verify(home, t); err != nil {
			if !errors.Is(err, tools.ErrMissing) {
				return fmt.Errorf("%v (remove it and run again to fetch the pinned release)", err)
			}
			pin, _ := tools.PinFor(t)
			missing = append(missing, t+" "+pin.Version)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !download {
		return api.Errorf(api.CodeUnavailable, "%s not in %s and --no-download is set", strings.Join(missing, " and "), tools.Dir(home)).
			WithHint("run without --no-download to fetch the pinned releases (sha256-checked)")
	}
	switch {
	case !yes && interactive:
		fmt.Fprintf(stderr, "zm: fetch %s (pinned releases from github.com, sha256-checked) into %s? [Y/n] ",
			strings.Join(missing, " and "), tools.Dir(home))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "" && a != "y" && a != "yes" {
			return fmt.Errorf("not fetched")
		}
	case !yes:
		fmt.Fprintf(stderr, "zm: not on a terminal, so fetching %s (pinned releases from github.com, sha256-checked) into %s without asking; --no-download refuses instead\n",
			strings.Join(missing, " and "), tools.Dir(home))
	}
	_, err := tools.Ensure(ctx, home, names, tools.Options{Download: true, Progress: stderr})
	return err
}

func printFailedChecks(err error) {
	var e *api.Error
	if !errors.As(err, &e) || len(e.Data) == 0 || flagJSON {
		return
	}
	var res orchapi.TrackerResult
	if json.Unmarshal(e.Data, &res) != nil {
		return
	}
	for _, c := range res.Checks {
		if !c.OK && !c.Skipped {
			fmt.Fprintf(os.Stderr, "  FAIL  %s: %s\n", c.Name, c.Detail)
		}
	}
}

func checksLine(cs []orchapi.TrackerCheck) string {
	pass, skip, fail := 0, 0, 0
	for _, c := range cs {
		switch {
		case c.Skipped:
			skip++
		case c.OK:
			pass++
		default:
			fail++
		}
	}
	s := fmt.Sprintf("%d passed", pass)
	if skip > 0 {
		s += fmt.Sprintf(", %d skipped", skip)
	}
	if fail > 0 {
		s += fmt.Sprintf(", %d FAILED", fail)
	}
	return s
}

func describeBinding(v orchapi.TrackerView) string {
	s := v.Adapter
	switch v.Mode {
	case orchapi.TrackerModeNew:
		s += ", embedded database"
	case orchapi.TrackerModeDir:
		s += ", " + v.Config["dir"]
	case orchapi.TrackerModeServer:
		s += fmt.Sprintf(", server %s database %s as %s", v.Config["server"], v.Config["database"], v.Config["user"])
		if v.Password {
			s += " (password stored)"
		}
	}
	if v.Readonly {
		s += ", read-only"
	}
	if v.Env == api.TrackerEnvInherit {
		s += ", env inherit"
	}
	return s
}

func addText(r orchapi.TrackerResult, took time.Duration) string {
	var b strings.Builder
	v := r.Tracker
	// created gates the adapter's undo; only a new database is "created"
	// to a person (a --server add made just its local metadata).
	verb := "bound"
	if r.Created && v.Mode == orchapi.TrackerModeNew {
		verb = "created and bound"
	}
	fmt.Fprintf(&b, "%s %s to %s: %s\n", verb, v.Prefix, v.Slug, describeBinding(v))
	if v.Data != "" {
		fmt.Fprintf(&b, "  data:   %s\n", v.Data)
	}
	if r.Schema != nil && r.Schema.Pinned == 0 {
		fmt.Fprintf(&b, "  schema: server v%d, not compared\n", r.Schema.Server)
	} else if r.Schema != nil {
		fmt.Fprintf(&b, "  schema: server v%d, the adapter expects v%d", r.Schema.Server, r.Schema.Pinned)
		switch {
		case r.Schema.Migrated:
			b.WriteString(" (migrated)")
		case r.Schema.Readonly:
			b.WriteString(" (mismatch: read-only; --allow-migrate to migrate)")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  checks: %s (read-only conformance)\n", checksLine(r.Checks))
	if r.Synced {
		fmt.Fprintf(&b, "  sync:   %d items in %.1fs\n", r.Items, r.SyncSeconds)
	} else {
		fmt.Fprintf(&b, "  sync:   %d items so far after %.1fs, still syncing in the background\n", r.Items, r.SyncSeconds)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note:   %s\n", n)
	}
	fmt.Fprintf(&b, "done in %.1fs. next: zm work ls -P %s\n", took.Seconds(), v.Slug)
	return b.String()
}

// backupBrief is tracker ls's backup column: the newest copy's age, and what
// needs someone's attention. The age is of a file on disk, never a record.
func backupBrief(v orchapi.TrackerView) string {
	bk := v.Backup
	if bk == nil {
		return "backup: none"
	}
	s := "backup: no copy on disk"
	if bk.Newest != nil {
		s = "backup: " + ago(*bk.Newest) + " old (" + bk.Kind + ")"
	}
	if bk.Overdue {
		s += " OVERDUE"
	}
	if bk.Failing {
		s += " FAILING"
	}
	if bk.ScrubPending {
		s += " REDACTION WAITING"
	}
	if v.Adapter == orchapi.AdapterNative && bk.Kind != orchapi.BackupSqlite {
		s += " (cannot restore the store)"
	}
	return s
}

func trackerLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List tracker bindings (every project; -P narrows)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.TrackerView, error) {
				return orchclient.Trackers(ctx, c, flagProject)
			}, func(vs []orchapi.TrackerView) string {
				if len(vs) == 0 {
					return "no trackers (zm tracker add native)"
				}
				var b strings.Builder
				for _, v := range vs {
					sync := "-"
					if v.Sync != nil {
						sync = v.Sync.State
						if !v.Sync.Synced.IsZero() {
							sync += fmt.Sprintf(" %s ago", time.Duration(v.Sync.LagSeconds*float64(time.Second)).Round(time.Second))
						}
					}
					items := 0
					if v.Sync != nil {
						items = v.Sync.Items
					}
					fmt.Fprintf(&b, "%-12s %-6s %-10s %-16s %6d items  env %-9s %s  %s\n", v.Slug, v.Prefix, v.Mode+"/"+v.Adapter, sync, items, v.Env, describeBinding(v), backupBrief(v))
					if v.Sync != nil && v.Sync.Error != "" {
						fmt.Fprintf(&b, "%-12s %-6s %s: %s\n", "", "", failingLabel(v.Sync), v.Sync.Error)
					}
				}
				return b.String()
			})
		},
	}
}

func trackerTestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "test <prefix>", Short: "Run a tracker's read-only checks and a sync now (exit 1 if one fails)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			res, err := orchclient.TestTracker(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				if err := printJSON(res); err != nil {
					return err
				}
			} else {
				for _, ch := range res.Checks {
					st := "PASS"
					switch {
					case ch.Skipped:
						st = "SKIP"
					case !ch.OK:
						st = "FAIL"
					}
					line := fmt.Sprintf("%s  %s", st, ch.Name)
					if ch.Detail != "" && st != "PASS" {
						line += ": " + ch.Detail
					}
					fmt.Println(line)
				}
				for _, n := range res.Notes {
					fmt.Println("note: " + n)
				}
				fmt.Printf("%s %s: %s; %d items, synced in %.1fs\n", res.Tracker.Slug, res.Tracker.Prefix, checksLine(res.Checks), res.Items, res.SyncSeconds)
			}
			if !res.OK {
				return exitSilently(1)
			}
			return nil
		},
	}
	return cmd
}

func trackerRmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <prefix>", Short: "Unbind a tracker (its mirror goes; its data stays)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.RemoveTrackerResult, error) {
				return orchclient.RemoveTracker(ctx, c, args[0])
			}, func(r orchapi.RemoveTrackerResult) string {
				return fmt.Sprintf("unbound %s from %s", r.Tracker.Prefix, r.Tracker.Slug)
			})
		},
	}
}

// failingLabel is "FAILING for 1h2m (37 syncs)" for a mirror whose syncs
// keep failing, else "error".
func failingLabel(s *orchapi.WorkSync) string {
	if s == nil || s.FailingSince == nil {
		return "error"
	}
	return fmt.Sprintf("FAILING for %s (%d syncs in a row)", time.Since(*s.FailingSince).Round(time.Second), s.Failures)
}

// readSecretFile reads a secret file named on the command line through the
// secrets brick (owner-only permissions, trimmed), ~ and relative paths
// resolved here.
func readSecretFile(ctx context.Context, p string) (secrets.Value, error) {
	abs, err := filepath.Abs(expandTilde(p))
	if err != nil {
		return secrets.Value{}, err
	}
	v, err := secrets.Get(ctx, secrets.File(abs))
	return v, secrets.Recode(err, api.CodeBadRequest)
}
