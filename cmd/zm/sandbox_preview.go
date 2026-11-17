package main

import (
	"strconv"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/spf13/cobra"
)

// Preview commands are a door onto Potluck's holder-checked preview API.
func sandboxPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "preview", Short: "Publish, list and revoke HTTPS application previews"}
	var public bool
	publish := &cobra.Command{Use: "publish <box handle> <port> --public", Short: "Expose an HTTP port publicly until revoked or the sandbox ends", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		c, _, err := newClient()
		if err != nil {
			return err
		}
		target, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		out, err := computeclient.PublishRunTargetPreview(cmd.Context(), c, target, id, computeapi.PreviewRequest{Port: port, Public: public})
		if err != nil {
			return err
		}
		return printJSON(out)
	}}
	publish.Flags().BoolVar(&public, "public", false, "allow anyone with the returned URL to reach this HTTP port")
	list := &cobra.Command{Use: "ls <box handle>", Short: "List a box's HTTPS previews", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		target, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		out, err := computeclient.RunTargetPreviews(cmd.Context(), c, target, id)
		if err != nil {
			return err
		}
		return printJSON(out)
	}}
	revoke := &cobra.Command{Use: "revoke <box handle> <preview-id>", Short: "End a public preview", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		target, id, err := computeclient.BoxHandle(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		if err := computeclient.RevokeRunTargetPreview(cmd.Context(), c, target, id, args[1]); err != nil {
			return err
		}
		return printJSON(struct {
			Preview string `json:"preview"`
			Revoked bool   `json:"revoked"`
		}{args[1], true})
	}}
	configure := &cobra.Command{Use: "configure <existing-domain>", Short: "Use an existing wildcard HTTPS preview domain; empty disables publishing", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		var out api.TunnelStatus
		if err := c.Do(cmd.Context(), "POST", "/v0/tunnel/preview-domain", api.TunnelPreviewDomainRequest{Domain: args[0]}, &out); err != nil {
			return err
		}
		return printJSON(out)
	}}
	cmd.AddCommand(publish, list, revoke, configure)
	return cmd
}
