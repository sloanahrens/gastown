package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	jevRequestsCaller string
	jevRequestsRecent int
)

var jevRequestsCmd = &cobra.Command{
	Use:   "requests",
	Short: "Print the request log's last lines: a hash and sizes, never the text",
	Long: `Print the last lines of <town>/.runtime/jev/requests.jsonl, where the Jev
client writes one line for every request that reaches the network, answered or
not: the caller, the question ids, the request id, the state's size, the body's
size and SHA-256, and the HTTP status or the error class. A line never holds
the state's text, the body or the key, so it proves what a battery sent
without storing it.

--caller keeps one caller's lines and --recent sets how many to print, oldest
first. A missing log is not an error.

Examples:
  gt jev requests
  gt jev requests --caller jev-dispatch --recent 3`,
	Args: cobra.NoArgs,
	RunE: runJevRequests,
}

func init() {
	f := jevRequestsCmd.Flags()
	f.StringVar(&jevRequestsCaller, "caller", "", "Keep only lines from this caller")
	f.IntVar(&jevRequestsRecent, "recent", 20, "Number of lines to print")
	jevCmd.AddCommand(jevRequestsCmd)
}

func runJevRequests(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return writeJevRequests(cmd.OutOrStdout(), jev.RequestsPath(townRoot), jevRequestsCaller, jevRequestsRecent)
}

// writeJevRequests prints the last recent request lines at path, oldest first,
// one JSON line each.
func writeJevRequests(w io.Writer, path, caller string, recent int) error {
	if recent < 1 {
		return fmt.Errorf("--recent must be at least 1, not %d", recent)
	}
	records, err := jev.ReadRequests(path, caller, recent)
	if err != nil {
		return err
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
