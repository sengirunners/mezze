package main

// `zm sandbox cp`: files and directories in and out of a box you hold, over
// exec (potluck/boxcp). Owned by sandboxes.

import (
	"fmt"
	"github.com/sengirunners/potluck/computeclient"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/kit/api"
)

func sandboxCpCmd() *cobra.Command {
	var maxMiB int
	cmd := &cobra.Command{Use: "cp <local path> <box handle>:<path> | <box handle>:<path> <local path>",
		Short: "Copy a file or directory into or out of a box you hold",
		Long: `Copies through exec, so it works on every box, another machine's among them.
As cp: an existing directory takes the copy by its name, a path ending in /
is made a directory first, else the path is the copy's own name. A box path
is under the box's HOME unless absolute; "<handle>:" alone is HOME.

Up, a directory merges into one already there and a symlink goes as a link.
Down, what comes from the box is data: nothing is run, nothing lands outside
the destination (a name that is absolute or climbs out with .. refuses the
whole copy), symlinks, hard links and devices are left out and listed, a
directory is never merged into one already here (refused) and appears whole
or not at all; a file replaces one here. Each copy is checked against its
SHA-256. A copy cut off on the way up resumes when run again.`,
		Example: `  zm sandbox cp ./data box:linux/b3:work/
  zm sandbox cp box:linux/b3:work/out.json .`, Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if maxMiB <= 0 || int64(maxMiB)<<20 > boxcp.HardMax {
				return fmt.Errorf("--max-mib is from 1 to %d", boxcp.HardMax>>20)
			}
			h0, p0, box0, err := boxcp.Split(args[0])
			if err != nil {
				return err
			}
			h1, p1, box1, err := boxcp.Split(args[1])
			if err != nil {
				return err
			}
			if box0 == box1 {
				return api.Errorf(api.CodeBadRequest, "one side is local and the other box:<target>/<id>:<path>")
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			handle := h0 + h1
			t, id, err := computeclient.BoxHandle(cmd.Context(), c, handle)
			if err != nil {
				return err
			}
			o := boxcp.Options{Max: int64(maxMiB) << 20}
			o.Progress = cpProgress(os.Stderr)
			x := boxpush.Exec(c, t, id)
			var res boxcp.Result
			if box1 {
				res, err = boxcp.Upload(cmd.Context(), x, args[0], p1, o)
			} else {
				res, err = boxcp.Download(cmd.Context(), x, p0, args[1], o)
			}
			if res.Chunks > 4 {
				fmt.Fprintln(os.Stderr)
			}
			for _, l := range res.Left {
				fmt.Fprintln(os.Stderr, "zm: left out", l)
			}
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			fmt.Fprintf(os.Stderr, "zm: copied %s (%d files, %.1f MiB) to %s\n", res.Kind, res.Files, float64(res.Bytes)/(1<<20), res.To)
			return nil
		}}
	cmd.Flags().IntVar(&maxMiB, "max-mib", boxcp.DefaultMax>>20, fmt.Sprintf("refuse a copy over this many MiB (a directory as a tar; at most %d)", boxcp.HardMax>>20))
	return cmd
}

// cpProgress prints chunks done on w once a copy is more than a few.
func cpProgress(w io.Writer) func(done, total int) {
	return func(done, total int) {
		if total > 4 {
			fmt.Fprintf(w, "\rzm: %d/%d chunks", done, total)
		}
	}
}
