// Command zm-connect is the zm side of the outreach connection, which runs as a Cloudflare Worker.
// It runs as the machine's owner on any machine with zm:
//
//	zm-connect bridge init                     make the bridge key; prints the public key for the Worker's BRIDGE_KEYS
//	zm-connect bridge run [--every 1m]         pull the Worker's notices, mail them through zm, report, ack
//	zm-connect bridge once                     one pull (for a timer)
//	zm-connect dbrole-sql --password-file F    the zm_connect role's setup SQL (a SCRAM verifier, never the password)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/connect/bridge"
	"github.com/sengirunners/mezze/internal/connect/dbrole"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/secrets"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zm-connect:", err)
		os.Exit(1)
	}
}

func usage() error {
	fmt.Fprint(os.Stderr, `usage:
  zm-connect bridge init|run|once [--worker https://...] [--project P --mail-to ADDR] [--dir D]
  zm-connect dbrole-sql --password-file F --db DB --agent-role ROLE [--bridged-container]
`)
	return flag.ErrHelp
}

func run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return usage()
	}
	switch args[0] {
	case "bridge":
		if len(args) < 2 {
			return usage()
		}
		return bridgeCmd(ctx, args[1], args[2:])
	case "dbrole-sql":
		return dbroleSQL(args[1:])
	}
	return usage()
}

func defaultDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "zm-connect")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "zm-connect")
}

func bridgeCmd(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	dir := fs.String("dir", defaultDir(), "the bridge's files: bridge.key (0600), worker.pin, worker.url, report.url (0600)")
	worker := fs.String("worker", "", "the Worker's https origin (saved in worker.url on first use)")
	project := fs.String("project", "", "the zm project the mail goes in (saved in project on first use)")
	mailTo := fs.String("mail-to", "", "who gets replies and alerts, e.g. human:<you> (saved in mail-to on first use)")
	every := fs.Duration("every", time.Minute, "how often run pulls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keyFile := filepath.Join(*dir, "bridge.key")
	if verb == "init" {
		pub, err := bridge.Init(keyFile)
		if err != nil {
			return err
		}
		fmt.Println("bridge public key (add it to the Worker's BRIDGE_KEYS; it is not a secret):")
		fmt.Println(pub)
		return nil
	}
	urlFile := filepath.Join(*dir, "worker.url")
	if *worker == "" {
		b, err := os.ReadFile(urlFile)
		if err != nil {
			return fmt.Errorf("say which Worker once: --worker https://<its domain>")
		}
		*worker = strings.TrimSpace(string(b))
	}
	b, err := bridge.New(*worker, keyFile, filepath.Join(*dir, "worker.pin"))
	if err != nil {
		return err
	}
	os.WriteFile(urlFile, []byte(*worker+"\n"), 0o600)
	if err := sticky(project, *dir, "project", "--project <zm project>"); err != nil {
		return err
	}
	if err := sticky(mailTo, *dir, "mail-to", "--mail-to human:<you>"); err != nil {
		return err
	}
	b.VersionFile = filepath.Join(*dir, "worker.version")
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	c := client.New(cfg)
	c.Project = *project
	b.Mail = func(ctx context.Context, body string) error {
		_, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: *mailTo, Body: body})
		return err
	}
	if u, err := secrets.Get(ctx, secrets.File(filepath.Join(*dir, "report.url"))); err == nil && !u.IsZero() {
		b.Report = bridge.Reporter(u)
	}
	once := func() {
		n, err := b.Once(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zm-connect bridge: delivered %d; %v\n", n, err)
		}
	}
	switch verb {
	case "once":
		n, err := b.Once(ctx)
		fmt.Printf("delivered %d\n", n)
		return err
	case "run":
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			once()
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
	return usage()
}

// sticky reads a setting given once (like --worker) back from dir/name when the flag is empty, and saves it
// when given. There is no built-in default: each copy names its own project and recipient.
func sticky(v *string, dir, name, flagHint string) error {
	p := filepath.Join(dir, name)
	if *v != "" {
		os.WriteFile(p, []byte(*v+"\n"), 0o600)
		return nil
	}
	b, err := os.ReadFile(p)
	if *v = strings.TrimSpace(string(b)); err != nil || *v == "" {
		return fmt.Errorf("say which once: %s", flagHint)
	}
	return nil
}

func dbroleSQL(args []string) error {
	fs := flag.NewFlagSet("dbrole-sql", flag.ContinueOnError)
	pwFile := fs.String("password-file", "", "a 0600 file holding zm_connect's password")
	db := fs.String("db", "", "the database (required)")
	agent := fs.String("agent-role", "", "the shared agent role that keeps read access only (required)")
	bridged := fs.Bool("bridged-container", false, "the database runs in a bridged container (its loopback is its own)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pwFile == "" || *db == "" || *agent == "" {
		return fmt.Errorf("--password-file, --db and --agent-role are required")
	}
	pw, err := secrets.Get(context.Background(), secrets.File(*pwFile))
	if err != nil {
		return err
	}
	v, err := dbrole.NewSCRAMVerifier(pw.Reveal())
	if err != nil {
		return err
	}
	sql, err := dbrole.RoleSQL(*db, *agent, v, *bridged)
	if err != nil {
		return err
	}
	fmt.Print(sql)
	return nil
}
