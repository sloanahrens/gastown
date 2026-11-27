package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/callercwd"
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

// newTownClusterCaptureCmd builds `gt town cluster capture` with the caller's
// working directory as the one --out resolves under. Require refuses a caller
// path that is not present here, where the capture could not be written.
func newTownClusterCaptureCmd() *cobra.Command {
	return newTownClusterCaptureCmdWithGetwd(callercwd.Require)
}

// newTownClusterCaptureCmdWithGetwd builds `gt town cluster capture`: one
// <kind>/<name> argument, the object's namespace and the required file to
// write. runClusterCapture does the read and the write; the command passes it
// the argument, the namespace, --out and the deps getwd names.
func newTownClusterCaptureCmdWithGetwd(getwd func() (string, error)) *cobra.Command {
	var namespace, out string
	cmd := &cobra.Command{
		Use:   "capture <kind>/<name>",
		Short: "Capture one live object to a file",
		Long: `Capture one live cluster object to a file: the object named by <kind>/<name>
is read through kubectl in the namespace --namespace names (kubectl's default when
empty) and written to --out as indented JSON with mode 0600. A reference that is
not <kind>/<name> or an --out path outside the working directory is refused before
anything is read.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := clusterCaptureCommandDeps(getwd)
			if err != nil {
				return err
			}
			return runClusterCapture(cmd.Context(), d, args[0], namespace, out)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "the object's namespace; empty means kubectl's default")
	cmd.Flags().StringVar(&out, "out", "", "the file to write the capture to, under the working directory (required)")
	// The flag is defined on the line above, so marking it cannot fail.
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func init() {
	townClusterCmd.AddCommand(newTownClusterCaptureCmd())
}
