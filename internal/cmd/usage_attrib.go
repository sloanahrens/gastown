package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
	"github.com/steveyegge/gastown/internal/workspace"
)

// attribZone is the zone --since reads its date in: the town's own zone, so a day
// means the same day the town's operators see.
const attribZone = "America/Chicago"

// usageAttribDeps is what `gt usage attrib` reads from outside itself: the town,
// the clock, the zone, and the reads a scan makes. Production takes the defaults;
// a test builds its own, so no package variable is swapped.
type usageAttribDeps struct {
	townRoot func() (string, error)
	zone     func(name string) (*time.Location, error)
	scan     usageattrib.ScanDeps
}

func defaultUsageAttribDeps() usageAttribDeps {
	return usageAttribDeps{
		townRoot: workspace.FindFromCwdOrError,
		zone:     time.LoadLocation,
		scan: usageattrib.ScanDeps{
			Now: time.Now,
			Events: func(town string) ([]events.Event, error) {
				return events.Read(town)
			},
			Landings: usageattrib.ReadLandings,
			ListSeat: session.CopiedSandboxTranscripts,
		},
	}
}

func init() {
	usageCmd.AddCommand(newUsageAttribCmd(defaultUsageAttribDeps()))
}

func newUsageAttribCmd(deps usageAttribDeps) *cobra.Command {
	attrib := &cobra.Command{
		Use:   "attrib",
		Short: "Attribute session usage to beads",
		Long: `Attribute each polecat session's tokens to the bead it worked, and keep one
usage record per bead under <town>/.runtime/usage/beads/.

Sessions are matched to beads through the town's events (sling, done and
the end events) and its landing records. A seat's transcripts are read from
the copy the daemon keeps under <town>/.runtime/usage/transcripts. That copy
holds one set per seat name, so a later bead dispatched to the same seat name
replaces an earlier bead's transcripts: a bead whose transcripts are gone is
marked incomplete, never shown as zero. Run scan between two runs of one seat
name to keep each bead's transcripts.`,
		RunE: requireSubcommand,
	}
	attrib.AddCommand(newUsageAttribScanCmd(deps), newUsageAttribShowCmd(deps))
	return attrib
}

func newUsageAttribScanCmd(deps usageAttribDeps) *cobra.Command {
	var rebuild bool
	var since string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Write one usage record per bead from the transcripts and events",
		Long: `Scan the seat transcripts and the events, and write each bead's work record.

A record is final once its run's done event is more than 10 minutes old, or,
for a bead with no end event, once its newest transcript is more than 24 hours
old. A final record is left alone unless its session set changed. --rebuild
rewrites every record, final or not.

--since YYYY-MM-DD keeps the runs that ended on or after that day. The day is
read in America/Chicago, the town's zone, from midnight.

Exit status: 0 when the scan ran, 2 when it could not. A malformed
settings/workspaces.json, or an events, landing or transcript file that cannot
be read, stops the scan before any record is written.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUsageAttribScan(cmd, deps, rebuild, since)
		},
	}
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "rewrite every record, including final ones")
	cmd.Flags().StringVar(&since, "since", "", "keep the runs that ended on or after this YYYY-MM-DD day in America/Chicago")
	return cmd
}

func runUsageAttribScan(cmd *cobra.Command, deps usageAttribDeps, rebuild bool, since string) error {
	town, err := deps.townRoot()
	if err != nil {
		return attribFail(cmd, fmt.Errorf("finding the town: %w", err))
	}
	opts := usageattrib.ScanOptions{Town: town, Rebuild: rebuild}
	if since != "" {
		loc, err := deps.zone(attribZone)
		if err != nil {
			return attribFail(cmd, fmt.Errorf("loading zone %s: %w", attribZone, err))
		}
		day, err := time.ParseInLocation("2006-01-02", since, loc)
		if err != nil {
			return attribFail(cmd, fmt.Errorf("--since %q is not a YYYY-MM-DD day", since))
		}
		opts.Since = day
	}
	res, err := usageattrib.Scan(opts, deps.scan)
	if err != nil {
		return attribFail(cmd, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "scanned: %d records written, %d final kept, %d incomplete, %d unmatched sessions\n",
		len(res.Written), len(res.Kept), len(res.Incomplete), res.Unmatched)
	return nil
}

func newUsageAttribShowCmd(deps usageAttribDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "show <bead>",
		Short: "Print one bead's usage record",
		Long: `Print the usage record scan wrote for one bead.

Exit status: 0 when the record was printed, 1 when the bead has no record,
2 when the record cannot be read.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			town, err := deps.townRoot()
			if err != nil {
				return attribFail(cmd, fmt.Errorf("finding the town: %w", err))
			}
			usageDir := filepath.Join(constants.TownRuntimePath(town), "usage")
			st, err := usageattrib.ReadWork(usageDir, args[0])
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(cmd.ErrOrStderr(), "gt usage attrib show: no usage record for bead %s\n", args[0])
				return NewSilentExit(1)
			}
			if err != nil {
				return attribFail(cmd, err)
			}
			printUsageRecord(cmd.OutOrStdout(), st)
			return nil
		},
	}
}

// attribFail prints one line naming the failure and exits 2: a hard error, which the
// scan and show refuse to paper over.
func attribFail(cmd *cobra.Command, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "gt usage attrib: %v\n", err)
	return NewSilentExit(2)
}

// printUsageRecord writes one bead's record, models in name order.
func printUsageRecord(w io.Writer, st usageattrib.Stored) {
	r := st.Record
	seat := "-"
	if r.Seat != nil {
		seat = *r.Seat
	}
	fmt.Fprintf(w, "bead: %s\n", r.Bead)
	fmt.Fprintf(w, "workspace: %s\n", r.Workspace)
	fmt.Fprintf(w, "rig: %s\n", r.Rig)
	fmt.Fprintf(w, "seat: %s\n", seat)
	fmt.Fprintf(w, "complete: %t\n", r.Complete)
	fmt.Fprintf(w, "start: %s\n", timeOrDash(r.Start))
	fmt.Fprintf(w, "end: %s\n", timeOrDash(r.End))
	fmt.Fprintf(w, "sessions: %d\n", len(st.Sessions))
	fmt.Fprintf(w, "calls: %d\n", r.Totals.Calls)
	fmt.Fprintf(w, "input: %d\n", r.Totals.Input)
	fmt.Fprintf(w, "cache_creation: %d\n", r.Totals.CacheCreation)
	fmt.Fprintf(w, "cache_read: %d\n", r.Totals.CacheRead)
	fmt.Fprintf(w, "output: %d\n", r.Totals.Output)
	fmt.Fprintf(w, "uncached: %d\n", r.Totals.Uncached)
	fmt.Fprintf(w, "est_cost_usd: %.4f\n", r.EstCostUSD)
	fmt.Fprintf(w, "wall_s: %d\n", r.WallS)
	if st.Error != "" {
		fmt.Fprintf(w, "error: %s\n", st.Error)
	}
	names := make([]string, 0, len(r.Models))
	for name := range r.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "models:")
	for _, name := range names {
		m := r.Models[name]
		fmt.Fprintf(w, "  %s: calls %d, input %d, cache_creation %d, cache_read %d, output %d\n",
			name, m.Calls, m.Input, m.CacheCreation, m.CacheRead, m.Output)
	}
}

func timeOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
