package main

// `zm web test-push`: a test alert to the phones paired on the Decisions
// page (SPEC 16 "Phone alerts").

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	cmd := &cobra.Command{Use: "web", Short: "Phone alerts (test-push)"}
	testPush := &cobra.Command{
		Use: "test-push", Short: "Send a test notification to your subscribed devices", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			var result struct {
				Sent int `json:"sent"`
			}
			if err := c.Do(cmd.Context(), "POST", "/v0/web/push/test", struct{}{}, &result); err != nil {
				return err
			}
			fmt.Printf("sent a test notification to %d device(s)\n", result.Sent)
			return nil
		},
	}
	cmd.AddCommand(testPush)
	rootCmd.AddCommand(cmd)
}
