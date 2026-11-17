package main

// bd context and bd --version: what bd's callers check before they write
// (embeadify's doctor runs `bd --version`, then `bd context --json`, and
// refuses to write unless both succeed and context names a write target).

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
)

// printVersion is `bd version` and `bd --version`.
func printVersion(cmd *cobra.Command) error {
	if asJSON(cmd) {
		return writeJSON(stdout, map[string]any{"version": bdVersion, "build": "bd-zm", "backend": "zm-native"}, true)
	}
	fmt.Fprintf(stdout, "bd version %s (bd-zm: zm-native)\n", bdVersion)
	return nil
}

// contextInfo is bd v1.0.5's ContextInfo with the fields zm has a true value
// for. The database is the zm project (bd's one database, as bd where and
// bd dolt show name it); sync_remote is the project's authority when this
// machine joined it from another (where the issues sync from). Left out,
// having no zm meaning: dolt_mode, server_host/port, data_dir and
// proxied_dir (there is no Dolt), and beads_dir, repo_root, is_redirected
// and is_worktree (zm resolves the project, not a .beads directory, so
// naming one would point callers at a store bd-zm never writes). server and
// issue_prefix are zm's additions.
type contextInfo struct {
	BdVersion   string `json:"bd_version"`
	Backend     string `json:"backend"`
	Database    string `json:"database"`
	ProjectID   string `json:"project_id,omitempty"`
	IssuePrefix string `json:"issue_prefix,omitempty"`
	Server      string `json:"server"`
	SyncRemote  string `json:"sync_remote,omitempty"`
}

func contextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "context",
		Short: "show where bd reads and writes: the zm project, its prefix and authority",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			// zm's answer now, never the cached prefix map: a target zm did not
			// confirm is no target (zm down exits 3, from run).
			m, err := issueProjects(ctx, c, true)
			if err != nil {
				return err
			}
			// No project = no write target: fail, as bd does when it finds no database.
			p, err := projectFor(ctx, c)
			if err != nil {
				return err
			}
			info := contextInfo{BdVersion: bdVersion, Backend: "zm-native", Database: p, Server: servedBy(c)}
			found := false
			for _, ip := range m.ps.Projects {
				if ip.Slug == p || ip.Project == p {
					info.Database, info.ProjectID, info.IssuePrefix, found = ip.Slug, ip.Project, ip.Prefix, true
				}
			}
			// A joined project whose authority did not answer is still the target.
			found = found || slices.ContainsFunc(m.ps.Skipped, func(s api.WorkSkipped) bool { return s.Project == p })
			if r := joined(ctx, c, p); r != nil {
				info.Database, info.ProjectID, info.SyncRemote, found = r.Project.Slug, r.Project.ID, r.Authority, true
			}
			if !found {
				// A project with no issues yet (its first create makes them), or none at all.
				pr, err := c.GetProject(ctx, p) // zm's "no project" is exit 1
				if err != nil {
					return err
				}
				info.Database, info.ProjectID = pr.Slug, pr.ID
			}
			if asJSON(cmd) {
				return writeJSON(stdout, info, true)
			}
			// bd's text layout, the Repository section left out (see contextInfo).
			fmt.Fprintf(stdout, "bd version:     %s (bd-zm: zm-native)\n\nBackend:\n", info.BdVersion)
			fmt.Fprintf(stdout, "  type:         %s\n  database:     %s (zm project)\n", info.Backend, info.Database)
			if info.ProjectID != "" {
				fmt.Fprintf(stdout, "  project id:   %s\n", info.ProjectID)
			}
			if info.IssuePrefix != "" {
				fmt.Fprintf(stdout, "  issue prefix: %s\n", info.IssuePrefix)
			}
			fmt.Fprintf(stdout, "  server:       %s\n", info.Server)
			if info.SyncRemote != "" {
				fmt.Fprintf(stdout, "\nSync:\n  authority:    %s\n", info.SyncRemote)
			}
			return nil
		},
	}
}
