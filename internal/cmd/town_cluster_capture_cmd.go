package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// clusterCaptureCommandDeps is the capture command's deps: the kubectl runner
// the object is read through, time.Now, the control cluster, and the directory
// getwd names as the one --out resolves under. A getwd that fails is the error,
// never deps for the wrong directory.
func clusterCaptureCommandDeps(getwd func() (string, error)) (clusterCaptureDeps, error) {
	dir, err := getwd()
	if err != nil {
		return clusterCaptureDeps{}, fmt.Errorf("capture: the working directory: %w", err)
	}
	return clusterCaptureDeps{
		kubectl: realRunFunc(),
		now:     time.Now,
		cluster: clusterControl,
		workDir: dir,
	}, nil
}

// newTownClusterCaptureCmd builds `gt town cluster capture`: one <kind>/<name>
// argument, the object's namespace and the required file to write. The command
// does not read or write anything yet: it says capture is not wired
// (gt-ba2krv wires it to runClusterCapture).
func newTownClusterCaptureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capture <kind>/<name>",
		Short: "Capture one live object to a file (not wired yet)",
		Long: `Capture one live cluster object to a file. This command is not wired yet: a
call with the right argument and flags returns an error, reads no cluster and
writes no file.`,
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return errors.New("capture is not wired yet")
		},
	}
	cmd.Flags().StringP("namespace", "n", "", "the object's namespace; empty means kubectl's default")
	cmd.Flags().String("out", "", "the file to write the capture to, under the working directory (required)")
	// The flag is defined on the line above, so marking it cannot fail.
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func init() {
	townClusterCmd.AddCommand(newTownClusterCaptureCmd())
}
