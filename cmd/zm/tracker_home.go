package main

// zm tracker where|backup (SPEC 17.9): where a tracker's data lives, and
// its backups.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

func trackerWhereCmd() *cobra.Command {
	return &cobra.Command{
		Use: "where [prefix]", Short: "Where a tracker's source of truth lives, its backups and their age (default: every tracker of the project)",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var prefixes []string
			if len(args) == 1 {
				prefixes = args
			} else {
				vs, err := orchclient.Trackers(ctx, c, flagProject)
				if err != nil {
					return err
				}
				slug := ""
				for _, v := range vs {
					if flagProject == "" && slug != "" && v.Slug != slug {
						continue // the resolved project's own trackers (the first listed)
					}
					slug = v.Slug
					prefixes = append(prefixes, v.Prefix)
				}
				if len(prefixes) == 0 {
					fmt.Println("no trackers (zm tracker add native)")
					return nil
				}
			}
			var all []orchapi.TrackerWhere
			for _, p := range prefixes {
				w, err := orchclient.TrackerHome(ctx, c, p)
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				all = append(all, w)
			}
			if flagJSON {
				if len(args) == 1 {
					return printJSON(all[0])
				}
				return printJSON(all)
			}
			for i, w := range all {
				if i > 0 {
					fmt.Println()
				}
				fmt.Print(whereText(w))
			}
			return nil
		},
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func every(sec int) string {
	switch {
	case sec == 3600:
		return "hourly"
	case sec == 86400:
		return "daily"
	}
	return "every " + (time.Duration(sec) * time.Second).String()
}

func whereText(w orchapi.TrackerWhere) string {
	var b strings.Builder
	v := w.Tracker
	fmt.Fprintf(&b, "%s %s (%s)\n", v.Slug, v.Prefix, v.Adapter)
	home := "the " + v.Adapter + " service (backups are export snapshots)"
	if v.Adapter == orchapi.AdapterNative {
		home = "zm's native issues, on this machine (backups: sqlite copies, which zm issues restore reads)"
	}
	fmt.Fprintf(&b, "  home:     %s\n", home)
	if s := v.Sync; s != nil {
		line := s.State
		if !s.Synced.IsZero() {
			line += fmt.Sprintf(", synced %s ago", time.Duration(s.LagSeconds*float64(time.Second)).Round(time.Second))
		}
		fmt.Fprintf(&b, "  mirror:   %s, %d items\n", line, s.Items)
		if s.Error != "" {
			fmt.Fprintf(&b, "            %s: %s\n", failingLabel(s), s.Error)
		}
	}
	if len(w.Backups) == 0 {
		b.WriteString("  backups:  none\n")
	}
	for i, bk := range w.Backups {
		label := "  backups:  "
		if i > 0 {
			label = "            "
		}
		fmt.Fprintf(&b, "%s%s (%s, %s): %s\n", label, bk.ID, bk.Kind, every(bk.EverySeconds), backupStatus(bk))
	}
	for _, n := range w.Notes {
		fmt.Fprintf(&b, "  note:     %s\n", n)
	}
	return b.String()
}

func backupStatus(bk orchapi.BackupView) string {
	var parts []string
	switch {
	case bk.Running:
		parts = append(parts, "running now")
	case bk.LastOK != nil:
		parts = append(parts, "last ok "+ago(*bk.LastOK)+" ago")
	default:
		parts = append(parts, "never ran")
	}
	if bk.Keep > 0 {
		if bk.Newest != nil {
			parts = append(parts, fmt.Sprintf("newest copy %s old (%d kept)", ago(*bk.Newest), bk.Copies))
		} else if bk.LastOK != nil {
			parts = append(parts, "NO COPY ON DISK")
		}
	}
	if bk.Failures > 0 {
		parts = append(parts, fmt.Sprintf("FAILING (%dx): %s", bk.Failures, bk.LastError))
	}
	if bk.ScrubPending {
		parts = append(parts, "OLDER COPIES WAIT FOR A REDACTION")
	}
	if bk.Bytes > 0 {
		parts = append(parts, humanBytes(bk.Bytes))
	}
	if bk.Items > 0 {
		parts = append(parts, fmt.Sprintf("%d items", bk.Items))
	}
	parts = append(parts, bk.Path)
	return strings.Join(parts, ", ")
}

func absFile(p *string) error {
	if *p == "" {
		return nil
	}
	a, err := filepath.Abs(expandTilde(*p))
	if err == nil {
		*p = a
	}
	return err
}

func trackerBackupCmd() *cobra.Command {
	bk := &cobra.Command{Use: "backup", Short: "Back a tracker up on a schedule: sqlite (native issues) or export (JSON Lines, any tracker)",
		Long: `Every tracker can keep "export" backups: JSON Lines snapshots of its items
(with zm's comments) from the mirror, the newest --keep kept. Native issues
also keep sqlite: checked copies of the issue store and project.db on a file:/// target
(never icloud), the newest --keep kept, which zm issues restore reads. A
redaction replaces sqlite copies: a fresh one is written and verified, then the
older ones are deleted.
The daemon runs each on its schedule (hourly by default); zm tracker where
shows the last success and any failure. Targets: icloud[:subdir] (macOS)
and file:///path. To send the whole home off this machine (S3, R2), see
zm backup.`}
	var (
		kind string
		ev   time.Duration
		keep int
		now  bool
	)
	add := &cobra.Command{Use: "add <prefix> <target>", Short: "Schedule a backup (default kind: the tracker's native one, else export)", Args: cobra.ExactArgs(2),
		Example: `  zm tracker backup add bd file:///Volumes/b --now  # native issues: store copies, hourly, first one now
  zm tracker backup add bd icloud --kind export --every 24h --keep 30
  zm tracker backup add gh icloud                  # GitHub issues: export snapshots
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			target := args[1]
			if strings.HasPrefix(target, "~") {
				if err := absFile(&target); err != nil {
					return err
				}
			}
			r, err := orchclient.AddTrackerBackup(cmd.Context(), c, args[0], orchapi.AddBackupRequest{Target: target, Kind: kind, EverySeconds: int(ev.Seconds()), Keep: keep, RunNow: now})
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Printf("scheduled %s backup of %s to %s, %s\n", r.Backup.Kind, args[0], r.Backup.ID, every(r.Backup.EverySeconds))
			fmt.Print(backupResultText(r))
			return nil
		}}
	add.Flags().StringVar(&kind, "kind", "", "sqlite or export (default: sqlite for native issues)")
	add.Flags().DurationVar(&ev, "every", 0, "how often (default 1h)")
	add.Flags().IntVar(&keep, "keep", 0, "copies to keep (export 48, sqlite 24)")
	add.Flags().BoolVar(&now, "now", false, "run the first one now")

	ls := &cobra.Command{Use: "ls <prefix>", Short: "List a tracker's backups", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.BackupView, error) {
				return orchclient.TrackerBackups(ctx, c, args[0])
			}, func(vs []orchapi.BackupView) string {
				if len(vs) == 0 {
					return "no backups (zm tracker backup add " + args[0] + " icloud)"
				}
				var b strings.Builder
				for _, v := range vs {
					fmt.Fprintf(&b, "%s (%s, %s): %s\n", v.ID, v.Kind, every(v.EverySeconds), backupStatus(v))
				}
				return b.String()
			})
		}}
	var rmKind string
	rm := &cobra.Command{Use: "rm <prefix> <id>", Short: "Unschedule a backup (its copies stay)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.BackupView, error) {
				return orchclient.RemoveTrackerBackup(ctx, c, args[0], args[1], rmKind)
			}, func(v orchapi.BackupView) string {
				return fmt.Sprintf("unscheduled the %s backup of %s to %s; its copies stay at %s", v.Kind, args[0], v.ID, v.Path)
			})
		}}
	rm.Flags().StringVar(&rmKind, "kind", "", "sqlite or export, when the target has several")
	var runKind string
	run := &cobra.Command{Use: "run <prefix> [id]", Short: "Run a tracker's backups now (one, or all)", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.BackupResult, error) {
				return orchclient.RunTrackerBackups(ctx, c, args[0], id, runKind)
			}, func(rs []orchapi.BackupResult) string {
				var b strings.Builder
				for _, r := range rs {
					fmt.Fprintf(&b, "%s (%s): ", r.Backup.ID, r.Backup.Kind)
					if r.Ran {
						fmt.Fprintf(&b, "ok in %.1fs\n", r.Seconds)
					} else {
						fmt.Fprintf(&b, "FAILED: %s\n", r.Backup.LastError)
					}
					b.WriteString(backupResultText(r))
				}
				return b.String()
			})
		}}
	run.Flags().StringVar(&runKind, "kind", "", "sqlite or export, when the target has several")
	bk.AddCommand(add, ls, rm, run)
	return bk
}

func backupResultText(r orchapi.BackupResult) string {
	if !r.Ran {
		return ""
	}
	var b strings.Builder
	v := r.Backup
	fmt.Fprintf(&b, "  copy:  %s", v.Path)
	if v.Bytes > 0 {
		fmt.Fprintf(&b, " (%s)", humanBytes(v.Bytes))
	}
	b.WriteString("\n")
	if v.Items > 0 {
		fmt.Fprintf(&b, "  items: %d\n", v.Items)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note:  %s\n", n)
	}
	return b.String()
}
