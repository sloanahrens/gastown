package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/cloudcheck"
)

// cloudCheckExitUsage is the exit status of a bad flag or argument.
const cloudCheckExitUsage = 2

// cloudCheckCmd is the `gt cloud-check` job.
var cloudCheckCmd = newCloudCheckCmd()

func init() {
	rootCmd.AddCommand(cloudCheckCmd)
}

// newCloudCheckCmd builds the command; a test builds a fresh one so its flag
// state is its own.
func newCloudCheckCmd() *cobra.Command {
	var req cloudCheckRequest
	c := &cobra.Command{
		Use:     "cloud-check",
		GroupID: GroupDiag,
		Short:   "Check the cloud apps' URLs and report the result",
		Long: `Check every app in the apps file and report one level per check.

The apps file is one check per name=url line, with # comments and blank lines,
and an optional app=<name> line naming the section that follows. A check in an
app=<name> section is off on purpose, and not fetched, when the off directory
holds <name>.off; a check with no app= line reads no marker. A 2xx answer is
ok, a 3xx or 4xx answer is a warning, and a 5xx, a refused connection or a
timeout is a failure. Redirects are not followed, so only the URLs the apps
file names are fetched.

Exit status: 0 when nothing failed, 1 when a check failed or the run could not
be made, 2 for a bad argument.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return &ExitCodeError{Code: cloudCheckExitUsage, Err: fmt.Errorf("cloud-check takes no arguments, got %q", strings.Join(args, " "))}
			}
			return nil
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCloudCheck(cmd, req)
		},
	}
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &ExitCodeError{Code: cloudCheckExitUsage, Err: err}
	})
	c.Flags().BoolVar(&req.quiet, "quiet", false, "Print only warnings and failures")
	c.Flags().StringVar(&req.jsonPath, "json", "", "Write the status as JSON to this file, replaced atomically")
	c.Flags().StringVar(&req.appsPath, "apps", "", "Read the apps list from this file (required)")
	c.Flags().StringVar(&req.offDir, "off-dir", "", "Treat <app>.off markers in this directory as off on purpose")
	return c
}

// cloudCheckRequest is the flags of one run.
type cloudCheckRequest struct {
	quiet    bool
	jsonPath string
	appsPath string
	offDir   string
}

// runCloudCheck runs one check and maps the report onto the exit status.
func runCloudCheck(cmd *cobra.Command, req cloudCheckRequest) error {
	if req.appsPath == "" {
		return &ExitCodeError{Code: cloudCheckExitUsage, Err: errors.New("--apps is required")}
	}
	apps, problems, err := readApps(req.appsPath)
	if err != nil {
		// A run that cannot read its list is a failed run, not a run that
		// leaves the previous healthy status in place.
		return finishCloudCheck(cmd, req, cloudcheck.FailureReport("reading the apps file: "+err.Error()))
	}
	return finishCloudCheck(cmd, req, cloudcheck.Check(cmd.Context(), apps, problems, cloudcheck.Options{OffDir: req.offDir}))
}

// finishCloudCheck prints and, with --json, writes the report, then maps its
// failures onto the exit status.
func finishCloudCheck(cmd *cobra.Command, req cloudCheckRequest, report cloudcheck.Report) error {
	report.Print(cmd.OutOrStdout(), req.quiet)
	if req.jsonPath != "" {
		if err := cloudcheck.Write(req.jsonPath, report); err != nil {
			return fmt.Errorf("writing the status: %w", err)
		}
	}
	if report.Failed > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("%d check(s) failed", report.Failed)}
	}
	return nil
}

// readApps opens and parses the apps file, closing it before it returns.
func readApps(path string) ([]cloudcheck.App, []cloudcheck.Problem, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	return cloudcheck.ParseApps(file)
}
