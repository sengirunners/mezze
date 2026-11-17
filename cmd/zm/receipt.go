package main

// Owned by the approvals builder: `zm receipt key|verify` (SPEC section 16).

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func init() {
	rc := &cobra.Command{Use: "receipt", Short: "Signed decision receipts: the public key, and verification (offline with --key)"}
	key := &cobra.Command{
		Use: "key", Short: "Print the daemon's public receipt key (commit it for a CI gate)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ReceiptKey, error) {
				return orchclient.ReceiptKey(ctx, c)
			},
				func(k orchapi.ReceiptKey) string { return k.Line })
		},
	}
	var exp orchapi.ReceiptExpect
	var keyArg, keyFile string
	var sigOnly, anyDecision bool
	verify := &cobra.Command{
		Use:   "verify <file|->",
		Short: "Verify a receipt; exit 0 only when it is valid for the given repo, PR and head",
		Long: `Verify a signed receipt (from zm ask receipt, or a PR comment).

With --key or --key-file it runs offline (no daemon: a CI gate). Without, it
asks this daemon (GET /v0/receipts/verify). --repo, --pr and --head are
required unless --work, --question or --signature-only: a receipt for another
PR, head commit or subject must never pass a gate. The decision must be approve unless --decision
names another or --any-decision: a decline receipt never passes a gate by
default. Exit 0 valid, 1 invalid, 3 daemon unreachable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exp, err := gateExpect(exp, sigOnly, anyDecision)
			if err != nil {
				return err
			}
			raw, err := readArg(args[0])
			if err != nil {
				return err
			}
			b, err := receiptJSON(raw)
			if err != nil {
				return err
			}
			var chk orchapi.ReceiptCheck
			if keyArg != "" || keyFile != "" {
				pub, err := loadKey(keyArg, keyFile)
				if err != nil {
					return err
				}
				r, err := approvals.ParseReceipt(b)
				if err != nil {
					chk = orchapi.ReceiptCheck{Reason: "not a receipt"}
				} else {
					chk = approvals.Verify(r, pub, exp)
				}
			} else {
				c, _, err := newClient()
				if err != nil {
					return err
				}
				if chk, err = orchclient.VerifyReceipt(cmd.Context(), c, b, exp); err != nil {
					return err
				}
			}
			if flagJSON {
				if err := printJSON(chk); err != nil {
					return err
				}
			}
			if !chk.Valid {
				if !flagJSON {
					fmt.Fprintln(os.Stderr, "zm: receipt INVALID: "+chk.Reason)
				}
				return exitSilently(1)
			}
			if !flagJSON {
				fmt.Printf("receipt valid (key %s; checked %s)\n", chk.KeyID, strings.Join(append([]string{"signature"}, chk.Checked...), ", "))
			}
			return nil
		},
	}
	f := verify.Flags()
	f.StringVar(&keyArg, "key", "", "public key (ed25519:<base64>, from zm receipt key): verify offline")
	f.StringVar(&keyFile, "key-file", "", "file holding the public key: verify offline")
	f.StringVar(&exp.Project, "for-project", "", "project ULID (or, on the daemon, a slug you can see)")
	f.StringVar(&exp.Ask, "ask", "", "ask id")
	f.StringVar(&exp.Repo, "repo", "", "repository the receipt must be for")
	f.IntVar(&exp.PR, "pr", 0, "pull request number it must be for")
	f.StringVar(&exp.HeadSHA, "head", "", "full head commit id it must be for")
	f.StringVar(&exp.Work, "work", "", "work item it must be for (canonical id, e.g. bd:zm-1)")
	f.StringVar(&exp.Card, "card", "", "required card SHA256")
	f.StringVar(&exp.Question, "question", "", "unique question identity from the ask")
	f.StringVar(&exp.Decision, "decision", "", "required decision (default approve unless --any-decision or --signature-only)")
	f.BoolVar(&anyDecision, "any-decision", false, "accept any decision (never for a merge gate)")
	f.StringVar(&exp.Approver, "approver", "", "required approver: its exact name, or (asking the daemon) the person whose paired device answered")
	f.BoolVar(&sigOnly, "signature-only", false, "check only the signature and form (never for a gate)")
	rc.AddCommand(key, verify)
	rootCmd.AddCommand(rc)
}

// gateExpect completes what a verify requires. Unless --signature-only it
// needs repo, PR and head, and the decision defaults to approve: a gate that
// names only the PR must fail on a decline receipt.
func gateExpect(exp orchapi.ReceiptExpect, sigOnly, anyDecision bool) (orchapi.ReceiptExpect, error) {
	if sigOnly {
		return exp, nil
	}
	onPR, other := exp.Repo != "" || exp.PR != 0 || exp.HeadSHA != "", exp.Work != "" || exp.Question != ""
	if onPR && other {
		return exp, fmt.Errorf("--repo, --pr and --head contradict --work and --question: a receipt has one subject")
	}
	if !other && (exp.Repo == "" || exp.PR == 0 || exp.HeadSHA == "") {
		return exp, fmt.Errorf("--repo, --pr and --head are required, or --work, or --question (or --signature-only)")
	}
	if exp.Work != "" && exp.Ask == "" && exp.Card == "" {
		return exp, fmt.Errorf("--work requires --ask or --card")
	}
	if anyDecision && exp.Decision != "" {
		return exp, fmt.Errorf("--decision and --any-decision contradict each other")
	}
	if exp.Decision == "" && !anyDecision {
		exp.Decision = orchapi.DecisionApprove
	}
	return exp, nil
}

func readArg(a string) ([]byte, error) {
	if a == "-" {
		return io.ReadAll(io.LimitReader(os.Stdin, api.MaxBody))
	}
	return os.ReadFile(a)
}

// receiptJSON accepts a receipt, a decision ({"receipt": ...}) or an ask
// ({"decision": {"receipt": ...}}) and returns the receipt's JSON.
func receiptJSON(b []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("not JSON: %v", err)
	}
	if _, ok := top["sig"]; ok {
		return b, nil
	}
	if r, ok := top["receipt"]; ok {
		return r, nil
	}
	if d, ok := top["decision"]; ok {
		var dec map[string]json.RawMessage
		if json.Unmarshal(d, &dec) == nil && dec["receipt"] != nil {
			return dec["receipt"], nil
		}
	}
	return nil, fmt.Errorf("no receipt in this JSON")
}

func loadKey(arg, file string) (ed25519.PublicKey, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		arg = string(b)
	}
	return approvals.ParsePublicKey(arg)
}
