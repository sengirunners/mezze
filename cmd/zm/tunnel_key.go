package main

// zm tunnel key [escrow [--to REF] | clear] (SPEC 17.11 "The home key across
// a restore"): the home key's own path, since a backup set never holds it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/tunnel"
)

func tunnelKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Keep this home's key outside the home, so a restored home keeps it and members keep trusting it",
		Long: `A backup set never holds this daemon's key, so a home restored from one
starts with a new key and every member refuses it. Keep the key outside the
home once, and a restore needs no step:

  zm tunnel key escrow                     the macOS login keychain (the default on a Mac)
  zm tunnel key escrow --to file:/path     a file (0600) on another disk
  zm tunnel key escrow --to env:ZM_HOME_KEY  a variable the daemon's host sets (a container's secret)

The daemon checks the place holds its key before it records it (config
tunnel.key, a reference: a set may carry it). After a restore, the daemon reads
the key from there when the home has none. If it cannot, it starts with no key
rather than a new one; then zm tunnel key escrow --to <where the key is now>
adopts it (never zm tunnel key clear, which would make a new key).

The keychain item is in this Mac's login keychain (in a Time Machine backup;
not synced through iCloud), and a daemon started before login cannot read it
until someone logs in. An older zm drops tunnel.key when it rewrites config:
restore with this zm or newer.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := tunnelKeyClient()
			if err != nil {
				return err
			}
			st, err := c.Tunnel(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(st)
			}
			if st.KeyEscrow == "" {
				fmt.Printf("key %s, kept only in this home: a restore from backup makes a new one (zm tunnel key escrow)\n", st.Fingerprint)
				return nil
			}
			fmt.Printf("key %s, also kept at %s\n", st.Fingerprint, st.KeyEscrow)
			return nil
		},
	}
	var to string
	escrow := &cobra.Command{
		Use:   "escrow",
		Short: "Keep this home's key at --to (default on a Mac: the login keychain), checked by the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := tunnelKeyClient()
			if err != nil {
				return err
			}
			st, err := c.Tunnel(cmd.Context())
			if err != nil {
				return err
			}
			if st.Fingerprint == "" {
				// No key (a restored home whose escrow moved): --to names
				// where the key is, and the daemon adopts it after checking.
				if to == "" {
					return fmt.Errorf("this daemon has no key: %s", st.Error)
				}
				ref, err := secrets.Parse(to)
				if err != nil {
					return err
				}
				st, err = c.TunnelKey(cmd.Context(), ref.String())
				if err != nil {
					return err
				}
				fmt.Printf("key %s taken from %s: members trust this home again\n", st.Fingerprint, st.KeyEscrow)
				return nil
			}
			if to == "" {
				if runtime.GOOS != "darwin" {
					return errors.New("name where to keep the key: --to file:/path (0600, on another disk) or --to env:NAME")
				}
				to = "keychain:zm-home-key:" + st.Fingerprint
			}
			ref, err := secrets.Parse(to)
			if err != nil {
				return err
			}
			_, cfg, err := newClient()
			if err != nil {
				return err
			}
			seed, err := os.ReadFile(filepath.Join(cfg.Home, tunnel.KeyFile))
			if err != nil {
				return fmt.Errorf("reading this home's key: %w", err)
			}
			hexSeed := strings.TrimSpace(string(seed))
			switch ref.Scheme {
			case "keychain":
				if err := keychainPut(ref, hexSeed); err != nil {
					return err
				}
			case "file":
				if !filepath.IsAbs(ref.Name) {
					return errors.New("--to file: needs an absolute path")
				}
				if tunnel.InHome(cfg.Home, ref.Name) {
					return errors.New("--to file: is inside this home, which a restore replaces: pick another disk")
				}
				if err := escrowFile(ref.Name, hexSeed); err != nil {
					return err
				}
			case "env":
				// The daemon's host sets it (a container's secret): the check
				// below fails until it does.
			}
			st, err = c.TunnelKey(cmd.Context(), ref.String())
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(st)
			}
			fmt.Printf("key %s is kept at %s: a restore from backup keeps it\n", st.Fingerprint, st.KeyEscrow)
			return nil
		},
	}
	escrow.Flags().StringVar(&to, "to", "", "where: keychain:SERVICE[:ACCOUNT], file:/path or env:NAME")
	clear := &cobra.Command{
		Use:   "clear",
		Short: "Stop naming the place the key is kept (the copy there stays; remove it yourself)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := tunnelKeyClient()
			if err != nil {
				return err
			}
			st, err := c.TunnelKey(cmd.Context(), "")
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(st)
			}
			fmt.Printf("key %s is kept only in this home again\n", st.Fingerprint)
			return nil
		},
	}
	cmd.AddCommand(escrow, clear)
	return cmd
}

func tunnelKeyClient() (*client.Client, error) {
	c, _, err := newClient()
	if err != nil {
		return nil, err
	}
	if c.Socket == "" {
		return nil, errors.New("zm tunnel key runs on the home's own machine, against its socket (unset ZM_HOST)")
	}
	return c, nil
}

// keychainPut stores v as a generic password with /usr/bin/security,
// handing it the command on stdin (security -i), never in argv.
func keychainPut(ref secrets.Ref, v string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("keychain: is macOS only; use --to file:/path or env:NAME")
	}
	for _, s := range []string{ref.Name, ref.Account, v} {
		if strings.ContainsAny(s, " \t\n\"'\\") {
			return fmt.Errorf("keychain service and account: no spaces or quotes")
		}
	}
	// Never over another value: an item there must hold this key already.
	old, err := secrets.KeychainDriver{}.Get(context.Background(), ref)
	switch {
	case err == nil && old.Reveal() == v:
		return nil
	case err == nil:
		return fmt.Errorf("keychain item %s already holds something else: never written over", ref.Name)
	case !errors.Is(err, secrets.ErrNotFound):
		return err
	}
	line := "add-generic-password -s " + ref.Name
	if ref.Account != "" {
		line += " -a " + ref.Account
	}
	line += " -w " + v + "\n"
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(line)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("writing keychain item %s: %v", ref.Name, err)
	}
	if s := strings.TrimSpace(out.String()); s != "" {
		return fmt.Errorf("writing keychain item %s: security says %s", ref.Name, strings.ReplaceAll(s, v, "[secret]"))
	}
	return nil
}

// escrowFile writes v to path, 0600 in a directory it makes 0700. A file
// already there must hold v: never over another key.
func escrowFile(path, v string) error {
	if b, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(b)) == v {
			return nil
		}
		return fmt.Errorf("%s already holds something else: never written over", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return secrets.WriteFile(path, v)
}
