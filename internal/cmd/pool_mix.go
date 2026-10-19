package cmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// gt pool mix is retired (gt-mukgk): the seat map takes any number of agents,
// so there is no pair of seats and no role_agents.polecat left to keep in step.
// The command stays as a signpost, printing the gt pool set line that replaces
// the mix it was given and exiting 2.

var poolMixCmd = &cobra.Command{
	Use:   "mix <agent>=<cap>...",
	Short: "Retired: prints the gt pool set command that replaces it",
	Long: `gt pool mix is retired. polecat_pool.seats takes any number of agents, so gt
pool set does what the mix did:

  gt pool mix deepseek-flash=2 claude-sonnet=2
  gt pool set deepseek-flash=2 claude-sonnet=2 --total 4

Running the old command prints that line and exits 2, whatever the arguments:
the old --dry-run flag and malformed pairs included. The mix wrote
role_agents.polecat as well; gt pool set leaves that key alone, since the seat
map is the source of truth.`,
	// Flag parsing is off so an old --dry-run, or any other flag, reaches the
	// signpost instead of cobra's exit-1 unknown-flag error.
	DisableFlagParsing: true,
	Args:               cobra.ArbitraryArgs,
	SilenceUsage:       true,
	SilenceErrors:      true,
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, arg := range args {
			if arg == "-h" || arg == "--help" {
				return cmd.Help()
			}
		}
		return runPoolMix(cmd.OutOrStdout(), args)
	},
}

// runPoolMix prints the replacement and exits 2 for every argument list, with
// the writer passed in so a test drives it without the command's globals.
func runPoolMix(w io.Writer, args []string) error {
	fmt.Fprintln(w, poolMixReplacement(args))
	return NewSilentExit(2)
}

func init() {
	poolCmd.AddCommand(poolMixCmd)
}

// poolMixReplacement is the gt pool set line that leaves the seats a mix would
// have left, carrying the max_total the mix set (gt-mukgk). A --dry-run flag is
// dropped: gt pool set has none.
func poolMixReplacement(args []string) string {
	var pairs []string
	for _, arg := range args {
		if arg != "--dry-run" {
			pairs = append(pairs, arg)
		}
	}
	args = pairs
	if len(args) == 0 {
		return "gt pool mix is retired: gt pool set <agent>=<cap>... sets the seats"
	}
	line := "gt pool set " + strings.Join(args, " ")
	total, ok := poolMixTotal(args)
	if ok && total > 0 {
		line += fmt.Sprintf(" --total %d", total)
	}
	return "gt pool mix is retired: run " + line
}

// poolMixTotal is the sum of the mix's caps when every argument parses, which
// is the total the old command would have written.
func poolMixTotal(args []string) (int, bool) {
	total := 0
	for _, arg := range args {
		agent, count, found := strings.Cut(arg, "=")
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if !found || strings.TrimSpace(agent) == "" || err != nil || n < 0 {
			return 0, false
		}
		total += n
	}
	return total, true
}
