package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/workspace"
)

// jevRequestsDeps is what the requests subcommand takes from the host: the
// town it reads. A test supplies the town, so no unit test needs a working
// directory inside one.
type jevRequestsDeps struct {
	findTownRoot func() (string, error)
}

// realJevRequestsDeps is jevRequestsDeps on this host.
func realJevRequestsDeps() jevRequestsDeps {
	return jevRequestsDeps{findTownRoot: workspace.FindFromCwdOrError}
}

// jevRequestsFlags is one requests command's flags; a command owns its own so
// a later run never inherits an earlier run's values.
type jevRequestsFlags struct {
	caller string
	recent int
}

// newJevRequestsCmd builds `gt jev requests`.
func newJevRequestsCmd(d jevRequestsDeps) *cobra.Command {
	var f jevRequestsFlags
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "Print the request log's last lines: a hash and sizes, never the text",
		Long: `Print the last lines of <town>/.runtime/jev/requests.jsonl, where the Jev
client writes one line for every request it sends, answered or not: the caller,
the question ids, the request id, the state's size, the body's size and
SHA-256, and the HTTP status or the error class. A line never holds the state's
text, the body or the key, so it proves what a battery sent without storing it.

--caller keeps one caller's lines and --recent sets how many to print, oldest
first. A missing log is not an error. Lines that do not parse are skipped, and
their count goes to standard error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJevRequests(cmd.OutOrStdout(), cmd.ErrOrStderr(), d, f)
		},
	}
	cmd.Flags().StringVar(&f.caller, "caller", "", "Keep only lines from this caller")
	cmd.Flags().IntVar(&f.recent, "recent", 20, "Number of lines to print")
	return cmd
}

func init() {
	jevCmd.AddCommand(newJevRequestsCmd(realJevRequestsDeps()))
}

func runJevRequests(w, notes io.Writer, d jevRequestsDeps, f jevRequestsFlags) error {
	if f.recent < 1 {
		return fmt.Errorf("--recent must be at least 1, not %d", f.recent)
	}
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return writeJevRequests(w, notes, jev.RequestsPath(townRoot), f.caller, f.recent)
}

// writeJevRequests prints the last recent request lines at path, oldest first,
// one JSON line each, and notes on notes how many unreadable lines it skipped.
func writeJevRequests(w, notes io.Writer, path, caller string, recent int) error {
	records, skipped, err := jev.ReadRequests(path, caller, recent)
	if err != nil {
		return err
	}
	if skipped > 0 {
		fmt.Fprintf(notes, "jev: skipped %d unreadable line(s) in %s\n", skipped, path)
	}
	if len(records) == 0 {
		fmt.Fprintln(w, "no requests logged yet")
		return nil
	}
	enc := json.NewEncoder(w)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("printing a request line: %w", err)
		}
	}
	return nil
}
