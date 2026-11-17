package main

// zm backup (SPEC 17.9 "Off-site home backups"): the whole home, on a
// schedule, to S3 (Cloudflare R2 and the like) or a directory; the watchdog
// heartbeat; and restoring a set into a fresh home.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/secrets"
)

func init() { rootCmd.AddCommand(backupCmd()) }

func backupCmd() *cobra.Command {
	bk := &cobra.Command{Use: "backup", Short: "Back the whole home up off this machine (S3/R2 or a directory), and restore it",
		Long: `The daemon copies the whole home (registry, every project's databases and
config) with VACUUM INTO every --every (15m by default), gzips it, and
writes it as one set to the target. Every set is encrypted on this machine
with the home's backup key, secrets/backup.key (made when the backup is set):
keep a copy of that file off the machine, because without it no set can be
read. --encrypt and --encrypt-salt add the target's own encryption (rclone
crypt, which also hides file names) on top. Unchanged files are copied on the
target, not sent again. Retention keeps the newest --keep sets plus hourly and daily
ones. Secrets (tracker passwords, tokens, keys, tunnel.edge_secret, webhook
and GitHub-link signing secrets) are never sent in new sets; a restored home
re-adds its webhooks. Existing older sets are not scrubbed by this change.

Keys are secret references, never the secrets: keychain:SERVICE[:ACCOUNT]
(an interactive Mac), file:/abs/path (chmod 600) or env:NAME.

A watchdog (zm backup watchdog) gets a signed heartbeat every minute with
this home's sync and backup status, so a home that stops, or stops backing
up, is noticed from outside.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return offsiteStatus(cmd) }}

	var (
		req             orchapi.OffsiteRequest
		ev              time.Duration
		noDownload, yes bool
	)
	set := &cobra.Command{Use: "set <target>", Short: "Set the off-site backup (checks the target answers first)", Args: cobra.ExactArgs(1),
		Example: `  zm backup set s3://zm-backups/homes --home team-home \
      --endpoint https://<account id>.r2.cloudflarestorage.com \
      --access-key file:/home/me/.zm-secrets/r2-access-key-id \
      --secret-key file:/home/me/.zm-secrets/r2-secret-access-key \
      --encrypt file:/home/me/.zm-secrets/backup-passphrase \
      --encrypt-salt file:/home/me/.zm-secrets/backup-salt --now
  zm backup set file:///Volumes/backup --home laptop --every 1h`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cfg, err := newClient()
			if err != nil {
				return err
			}
			req.Target, req.EverySeconds = args[0], int(ev.Seconds())
			oc := offsite.Config{URL: req.Target, Encrypt: req.Encrypt}
			if (oc.Kind() == "s3" || req.Encrypt != "") && os.Getenv("ZM_HOST") == "" {
				if err := provisionNames(cmd.Context(), os.Stderr, cfg.Home, []string{tools.Rclone}, !noDownload, yes, interactive()); err != nil {
					return err
				}
			}
			v, err := orchclient.SetBackup(cmd.Context(), c, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(v)
			}
			fmt.Println(offsiteText(v))
			return nil
		}}
	f := set.Flags()
	f.StringVar(&req.Home, "home", "", "this home's id: its folder in the target (required; lowercase, digits, . _ -)")
	f.StringVar(&req.Endpoint, "endpoint", "", "the S3 API URL (R2: https://<account id>.r2.cloudflarestorage.com)")
	f.StringVar(&req.Region, "region", "", "the S3 region (default auto, R2's)")
	f.StringVar(&req.AccessKey, "access-key", "", "secret reference of the S3 access key id")
	f.StringVar(&req.SecretKey, "secret-key", "", "secret reference of the S3 secret access key")
	f.StringVar(&req.Encrypt, "encrypt", "", "secret reference of a passphrase: encrypt names and contents here (rclone crypt)")
	f.StringVar(&req.EncryptSalt, "encrypt-salt", "", "secret reference of a second secret, the crypt salt (required with --encrypt)")
	f.DurationVar(&ev, "every", 0, "how often (default 15m, at least 1m)")
	f.IntVar(&req.Keep, "keep", 0, "newest sets to keep (default 8)")
	f.IntVar(&req.KeepHourly, "keep-hourly", 0, "hours to keep one set each (default 24)")
	f.IntVar(&req.KeepDaily, "keep-daily", 0, "days to keep one set each (default 30)")
	f.BoolVar(&req.RunNow, "now", false, "take the first set now")
	f.BoolVar(&noDownload, "no-download", false, "never fetch the pinned rclone")
	f.BoolVarP(&yes, "yes", "y", false, yesHelp)
	set.MarkFlagRequired("home")

	status := &cobra.Command{Use: "status", Short: "The off-site backup and the watchdog: last set, its age, failures", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return offsiteStatus(cmd) }}
	run := &cobra.Command{Use: "run", Short: "Take and send a set now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OffsiteRunResult, error) {
				return orchclient.RunBackup(ctx, c)
			}, func(r orchapi.OffsiteRunResult) string {
				s := fmt.Sprintf("set %s: %d files (%d sent, %d unchanged), %s stored, in %.1fs", r.Set, r.Files, r.Uploaded, r.Copied, mb(r.Bytes), r.Seconds)
				if len(r.Deleted) > 0 {
					s += fmt.Sprintf("; retention removed %d older sets", len(r.Deleted))
				}
				return s
			})
		}}
	ls := &cobra.Command{Use: "ls", Short: "List the sets on the target", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OffsiteSets, error) {
				return orchclient.BackupSets(ctx, c)
			}, func(r orchapi.OffsiteSets) string {
				var b strings.Builder
				fmt.Fprintf(&b, "%s, home %s: %d sets\n", r.Target, r.Home, len(r.Sets))
				for _, x := range r.Sets {
					note := ""
					if !x.Complete {
						note = "  (incomplete: no manifest; the next run removes it)"
					}
					fmt.Fprintf(&b, "  %s  %s%s\n", x.Stamp, mb(x.Bytes), note)
				}
				return b.String()
			})
		}}
	off := &cobra.Command{Use: "off", Short: "Stop the off-site backup (its sets stay on the target)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
				return orchclient.BackupOff(ctx, c)
			}, func(v orchapi.OffsiteView) string {
				return "off-site backup stopped; its sets stay at " + v.Target
			})
		}}
	bk.AddCommand(set, status, run, ls, off, watchdogCmd(), restoreHomeCmd())
	return bk
}

// restoreKey is the backup key a restore opens sets with: --key, else the
// key already in --to, else this machine's home's; nil when there is none
// (a plain set from before encryption still restores; an encrypted one
// says it needs --key).
func restoreKey(ctx context.Context, ref, to string) (*bkcrypt.Key, error) {
	if ref != "" {
		return bkcrypt.Load(ctx, ref)
	}
	homes := []string{to}
	if cfg, err := config.LoadClient(); err == nil && cfg.Home != to {
		homes = append(homes, cfg.Home)
	}
	for _, h := range homes {
		if _, err := os.Lstat(bkcrypt.KeyPath(h)); err == nil {
			return bkcrypt.Open(h)
		}
	}
	return nil, nil
}

func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func offsiteStatus(cmd *cobra.Command) error {
	if flagProject != "" {
		// A joined project's backup is its authority's: this machine sees what
		// the authority's sync answers (a whole-project reader).
		c, _, err := newClient()
		if err != nil {
			return err
		}
		rows, err := orchclient.WorkSources(cmd.Context(), c, "")
		if err != nil {
			return err
		}
		for _, s := range rows {
			if s.Name != "authority" {
				continue
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(s)
			}
			f := authorityFacts(s)
			if s.Backup == nil {
				f = append(f, "no backup reported (a whole-project reader sees the authority's)")
			}
			fmt.Println(flagProject + ": " + strings.Join(f, ", "))
			return nil
		}
	}
	return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
		return orchclient.Backup(ctx, c)
	}, offsiteText)
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

func offsiteText(v orchapi.OffsiteView) string {
	var b strings.Builder
	if !v.Configured {
		b.WriteString("off-site backup: none (zm backup set)\n")
	} else {
		fmt.Fprintf(&b, "off-site backup: %s, home %s, %s\n", v.Target, v.Home, every(v.EverySeconds))
		st := v.Status
		switch {
		case st == nil || st.LastOK == nil:
			b.WriteString("  last set: none yet")
		default:
			fmt.Fprintf(&b, "  last set: %s, %s ago, %s", v.Set, age(*st.LastOK), mb(st.Bytes))
			if time.Since(*st.LastOK) > 2*time.Duration(v.EverySeconds)*time.Second {
				b.WriteString(" OVERDUE")
			}
		}
		if v.Running {
			b.WriteString(" (running now)")
		}
		b.WriteString("\n")
		if st != nil && st.Failing {
			fmt.Fprintf(&b, "  FAILING (%d x): %s\n", st.Failures, st.Error)
		}
		if v.ScrubDue != nil {
			b.WriteString("  REDACTION WAITING: older sets go once a fresh set is sent\n")
		}
		fmt.Fprintf(&b, "  keep: %d newest, %d hourly, %d daily\n", v.Keep, v.KeepHourly, v.KeepDaily)
		b.WriteString("  encrypted with the home's backup key, secrets/backup.key: keep a copy off this machine\n")
	}
	if w := v.Watchdog; w != nil {
		fmt.Fprintf(&b, "watchdog: %s as %s", w.URL, w.Home)
		if w.LastSent != nil {
			fmt.Fprintf(&b, ", last heartbeat %s ago", age(*w.LastSent))
		}
		b.WriteString("\n")
		if w.LastError != "" {
			fmt.Fprintf(&b, "  FAILING (%d x): %s\n", w.Failures, w.LastError)
		}
	} else {
		b.WriteString("watchdog: none (zm backup watchdog <url>)\n")
	}
	return b.String()
}

func watchdogCmd() *cobra.Command {
	var req orchapi.WatchdogRequest
	wd := &cobra.Command{Use: "watchdog <url>|off", Short: "Send a signed heartbeat to a watchdog every minute (sends one now)", Args: cobra.ExactArgs(1),
		Example: `  zm backup watchdog https://<watchdog>/v1/heartbeat --home team-home --secret file:/home/me/.zm-secrets/watchdog-secret
  zm backup watchdog off`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "off" {
				return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
					return orchclient.WatchdogOff(ctx, c)
				}, func(orchapi.OffsiteView) string { return "heartbeat stopped" })
			}
			req.URL = args[0]
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.OffsiteView, error) {
				return orchclient.SetWatchdog(ctx, c, req)
			}, offsiteText)
		}}
	wd.Flags().StringVar(&req.Secret, "secret", "", "secret reference of the whsec_ signing secret")
	wd.Flags().StringVar(&req.Home, "home", "", "this home's id in the heartbeat")
	return wd
}

func restoreHomeCmd() *cobra.Command {
	var (
		c               offsite.Config
		home, at, to    string
		keyRef          string
		dry             bool
		noDownload, yes bool
	)
	cmd := &cobra.Command{Use: "restore", Short: "Restore a set from the target into a fresh home (no daemon needed)", Args: cobra.NoArgs,
		Long: `Fetches a set (the newest, or --at) into --to, which must be empty or not
exist, checking every file against the set's manifest (size, sha256, and
each database's integrity) before any is put in place. --dry-run checks a
set without keeping it. A set is encrypted with the backup key of the home
that wrote it: --key names a copy of that key file (by default this
machine's secrets/backup.key, or --to's); a restored home gets the key, so
its own sets go on using it. Set up the restored home's tracker passwords
(secrets/), receipt.key and team tokens (remotes/) again. New sets omit
tunnel.edge_secret and disable a tunnel listener that used it: reconfigure
the listener and its edge trust before enabling it. Older sets may still
contain tunnel.edge_secret; this restore does not scrub them.
Then start it: ZM_HOME=<dir> zm daemon.`,
		Example: `  zm backup restore --from s3://zm-backups/homes --home team-home --to ~/.zm \
      --endpoint https://<account id>.r2.cloudflarestorage.com \
      --access-key file:/home/me/.zm-secrets/r2-access-key-id \
      --secret-key file:/home/me/.zm-secrets/r2-secret-access-key \
      --encrypt file:/home/me/.zm-secrets/backup-passphrase \
      --encrypt-salt file:/home/me/.zm-secrets/backup-salt`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if to == "" && !dry {
				return api.Errorf(api.CodeBadRequest, "--to <empty directory> is required (or --dry-run)")
			}
			if to == "" {
				d, err := os.MkdirTemp("", "zm-restore-check")
				if err != nil {
					return err
				}
				defer os.RemoveAll(d)
				to = d
			}
			if err := absFile(&to); err != nil {
				return err
			}
			if err := os.MkdirAll(to, 0o700); err != nil {
				return err
			}
			scratch, err := os.MkdirTemp("", "zm-rclone")
			if err != nil {
				return err
			}
			defer os.RemoveAll(scratch)
			o := offsite.Opener{Secrets: secrets.Default(), Scratch: scratch}
			if c.Kind() == "s3" || c.Encrypt != "" {
				// The pinned rclone goes into this CLI's own home (on a fresh
				// machine, the default one, which may be --to itself).
				cfg, err := config.LoadClient()
				if err != nil {
					return err
				}
				if err := provisionNames(ctx, os.Stderr, cfg.Home, []string{tools.Rclone}, !noDownload, yes, interactive()); err != nil {
					return err
				}
				p, err := tools.Verify(cfg.Home, tools.Rclone)
				if err != nil {
					return err
				}
				o.Bin = p
			}
			key, err := restoreKey(ctx, keyRef, to)
			if err != nil {
				return err
			}
			t, err := o.Open(ctx, c)
			if err != nil {
				return err
			}
			sets, err := offsite.Sets(ctx, t, home)
			if err != nil {
				return err
			}
			stamp, err := offsite.Pick(sets, at)
			if err != nil {
				return err
			}
			m, err := offsite.Restore(ctx, t, home, stamp, to, dry, key)
			if err != nil {
				return err
			}
			if !dry && m.Cipher != "" {
				// The restored home seals its sets with the key its old ones
				// open with (an existing key there is left alone).
				if err := bkcrypt.Write(bkcrypt.KeyPath(to), key); err != nil && !errors.Is(err, os.ErrExist) {
					return fmt.Errorf("restored, but the backup key was not put in %s: %w", to, err)
				}
			}
			if flagJSON {
				return printJSON(m)
			}
			if dry {
				fmt.Printf("set %s of %s checks out: %d files, written by zm %s; nothing kept\n", stamp, home, len(m.Files), m.Version)
				return nil
			}
			fmt.Printf("restored set %s of %s into %s: %d files, written by zm %s\n", stamp, home, to, len(m.Files), m.Version)
			fmt.Println("set up secrets again: tracker passwords (secrets/), receipt.key, team tokens (remotes/)")
			fmt.Println("new sets omit tunnel.edge_secret and disable its listener: reconfigure the listener and edge trust before enabling it")
			fmt.Println("older sets may still contain tunnel.edge_secret; this restore does not scrub them")
			fmt.Printf("then: ZM_HOME=%s zm daemon\n", to)
			return nil
		}}
	f := cmd.Flags()
	f.StringVar(&c.URL, "from", "", "the target: s3://bucket[/folder] or file:///path (required)")
	f.StringVar(&home, "home", "", "the home id the sets were written under (required)")
	f.StringVar(&to, "to", "", "the new home: an empty directory")
	f.StringVar(&at, "at", "", "the set whose stamp starts with this (default: the newest)")
	f.StringVar(&c.Endpoint, "endpoint", "", "the S3 API URL")
	f.StringVar(&c.Region, "region", "", "the S3 region (default auto)")
	f.StringVar(&c.AccessKey, "access-key", "", "secret reference of the S3 access key id")
	f.StringVar(&c.SecretKey, "secret-key", "", "secret reference of the S3 secret access key")
	f.StringVar(&c.Encrypt, "encrypt", "", "secret reference of the passphrase the sets were encrypted with")
	f.StringVar(&c.EncryptSalt, "encrypt-salt", "", "secret reference of the salt the sets were encrypted with")
	f.StringVar(&keyRef, "key", "", "secret reference of the backup key the sets were encrypted with (default: --to's or this machine's secrets/backup.key)")
	f.BoolVar(&dry, "dry-run", false, "check the set, keep nothing")
	f.BoolVar(&noDownload, "no-download", false, "never fetch the pinned rclone")
	f.BoolVarP(&yes, "yes", "y", false, yesHelp)
	cmd.MarkFlagRequired("from")
	cmd.MarkFlagRequired("home")
	return cmd
}
