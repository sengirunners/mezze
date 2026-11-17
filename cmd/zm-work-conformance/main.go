// Command zm-work-conformance checks a work adapter against the zm/v0
// adapter contract:
//
//	zm-work-conformance <adapter> [--read-only | --write] [--id <id>] [--config k=v]... [--no-serve] [--json]
//	                    [--setup [--setup-option k=v]... [--secret-env NAME]]
//
// <adapter> is a path or a name ("acme" finds zm-work-acme on PATH).
// Read-only by default (--read-only says so explicitly): only describe, list
// and get are sent, never a write verb. --write also claims, releases,
// comments on, sets the status of and links --id (or the first listed item)
// in its real tracker. An adapter whose describe offers serve mode is
// checked again over one long-lived --serve process (unless --no-serve),
// plus pipelining, bad lines, cancel lines and exit on EOF. --setup also
// runs the adapter's setup verb in a throwaway data_dir (a dry run writes
// nothing, a frozen call only probes, undo stays inside data_dir); it runs
// the real setup once, so use it on a tracker that can be set up again.
// --secret-env NAME puts a random secret in the adapter's environment under
// NAME (its secret slot) and fails if any setup answer carries it. Exit 0 = conformant, 1 = a check failed, 2 = usage.
// Owned by the work builder.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/conformance"
)

type kv map[string]string

func (m kv) String() string { return fmt.Sprint(map[string]string(m)) }
func (m kv) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	m[k] = v
	return nil
}

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	fs := flag.NewFlagSet("zm-work-conformance", flag.ContinueOnError)
	opt := conformance.Options{Config: map[string]string{}, SetupOptions: map[string]string{}}
	var asJSON, readOnly, noServe bool
	var secretEnv string
	fs.BoolVar(&opt.Setup, "setup", false, "also check the setup verb in a throwaway data_dir (runs the real setup once)")
	fs.Var(kv(opt.SetupOptions), "setup-option", "setup option key=value for --setup (repeatable)")
	fs.BoolVar(&opt.HasPassword, "has-password", false, "with --setup: tell setup a password is in its secret slot (zm.has_password)")
	fs.StringVar(&secretEnv, "secret-env", "", "with --setup: put a random secret in the adapter's environment under this name and check no setup answer carries it")
	fs.StringVar(&opt.ID, "id", "", "an existing item id to read (and, with --write, change)")
	fs.BoolVar(&opt.Write, "write", false, "also exercise claim, release, comment, set_status and link (changes the item)")
	fs.BoolVar(&readOnly, "read-only", false, "never send a write verb (the default)")
	fs.Var(kv(opt.Config), "config", "tracker config key=value, sent with every request (repeatable)")
	fs.BoolVar(&asJSON, "json", false, "print results as JSON")
	fs.BoolVar(&noServe, "no-serve", false, "skip the serve-mode run (by default an adapter offering serve mode is checked both ways)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: zm-work-conformance <adapter-path-or-name> [--read-only | --write] [--id <id>] [--config k=v]... [--no-serve] [--json]")
		fs.PrintDefaults()
	}
	// Accept flags before or after the adapter argument.
	var target string
	for len(argv) > 0 {
		if err := fs.Parse(argv); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if target != "" {
			fs.Usage()
			return 2
		}
		target, argv = fs.Arg(0), fs.Args()[1:]
	}
	if target == "" || (readOnly && opt.Write) {
		fs.Usage()
		return 2
	}
	path, err := adapter.Find(target, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm-work-conformance:", err)
		return 2
	}
	if secretEnv != "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintln(os.Stderr, "zm-work-conformance:", err)
			return 2
		}
		opt.Secret = "zmconf-" + hex.EncodeToString(b)
		os.Setenv(secretEnv, opt.Secret) // the adapter inherits this environment
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res := conformance.Run(ctx, conformance.Exec(path), opt)
	if !noServe {
		res = append(res, conformance.RunServe(ctx, path, opt)...)
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
	} else {
		fmt.Printf("adapter %s\n%s", path, conformance.Format(res))
	}
	if n := conformance.Failed(res); n > 0 {
		if !asJSON {
			fmt.Printf("%d check(s) failed\n", n)
		}
		return 1
	}
	return 0
}
