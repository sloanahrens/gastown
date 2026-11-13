package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/usageattrib"
	"github.com/steveyegge/gastown/internal/workspace"
)

// usageReportDeps is what `gt usage report` reads from outside itself: the town, the clock, and the
// zone a day is read in. Production takes the defaults; a test builds its own, so no package variable
// is swapped.
type usageReportDeps struct {
	townRoot func() (string, error)
	now      func() time.Time
	zone     func(name string) (*time.Location, error)
}

func defaultUsageReportDeps() usageReportDeps {
	return usageReportDeps{townRoot: workspace.FindFromCwdOrError, now: time.Now, zone: time.LoadLocation}
}

// The largest day and minute counts a time.Duration holds, so the window's arithmetic cannot overflow.
const (
	maxReportDays    = math.MaxInt64 / int64(24*time.Hour)
	maxReportMinutes = math.MaxInt64 / int64(time.Minute)
)

func init() {
	usageCmd.AddCommand(newUsageReportCmd(defaultUsageReportDeps()))
}

// usageReportFlags is what one report run was asked for.
type usageReportFlags struct {
	days   int
	by     string
	bead   string
	over   int
	asJSON bool
}

func newUsageReportCmd(deps usageReportDeps) *cobra.Command {
	var f usageReportFlags
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Report usage per role, model, bead or day from the usage records",
		Long: `Report the usage the scan wrote, per role, model, bead or day, over the last --days days.

The report reads the work and om records under <town>/.runtime/usage/beads and the
unmatched sessions in unmatched.jsonl. It reads no transcript.

A Claude model is reported in uncached tokens: input, cache creation and output. A
DeepSeek model is reported as an estimated cost, which is an estimate and not a bill.
sub_share is the subagents' uncached tokens over all the group's uncached tokens. It is
shown as - under --by model, where a record's subagent tokens cannot be split by model.

A record counts when its end is inside the window. A record marked incomplete is left
out of the rows and counted as incomplete. A record with no end time is counted as
skipped, since no window holds it. The unmatched and incomplete counts cover the window,
whatever --bead and --over keep.

--bead ID keeps one bead's records. --over MINUTES keeps the beads whose work record ran
longer than MINUTES; every record of a kept bead counts. --json prints the same numbers
as JSON.

Exit status: 0 when the report printed. 2 when it could not be made: the town cannot
be found, the records directory, unmatched.jsonl or a record cannot be read, or a flag
is not valid.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUsageReport(cmd, deps, f)
		},
	}
	cmd.Flags().IntVar(&f.days, "days", 1, "report the last this many days")
	cmd.Flags().StringVar(&f.by, "by", usageattrib.ByRole, "group the rows by role, model, bead or day")
	cmd.Flags().StringVar(&f.bead, "bead", "", "report only this bead's records")
	cmd.Flags().IntVar(&f.over, "over", 0, "keep only the beads whose work ran longer than this many minutes (0 keeps all)")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "print the report as JSON")
	return cmd
}

func runUsageReport(cmd *cobra.Command, deps usageReportDeps, f usageReportFlags) error {
	if f.days < 1 || int64(f.days) > maxReportDays {
		return reportFail(cmd, fmt.Errorf("--days %d: a report covers between 1 and %d days", f.days, maxReportDays))
	}
	if f.over < 0 || int64(f.over) > maxReportMinutes {
		return reportFail(cmd, fmt.Errorf("--over %d: minutes must be between 0 and %d", f.over, maxReportMinutes))
	}
	town, err := deps.townRoot()
	if err != nil {
		return reportFail(cmd, fmt.Errorf("finding the town: %w", err))
	}
	var loc *time.Location
	if f.by == usageattrib.ByDay {
		loc, err = deps.zone(attribZone)
		if err != nil {
			return reportFail(cmd, fmt.Errorf("loading zone %s: %w", attribZone, err))
		}
	}
	usageDir := filepath.Join(constants.TownRuntimePath(town), "usage")
	recs, err := usageattrib.ReadRecords(usageDir)
	if err != nil {
		return reportFail(cmd, err)
	}
	unmatched, err := usageattrib.ReadUnmatched(usageDir)
	if err != nil {
		return reportFail(cmd, err)
	}
	now := deps.now().UTC()
	rep, err := usageattrib.BuildReport(recs, unmatched, usageattrib.ReportOptions{
		From: now.Add(-time.Duration(f.days) * 24 * time.Hour),
		To:   now,
		By:   f.by,
		Bead: f.bead,
		Over: time.Duration(f.over) * time.Minute,
		Zone: loc,
	})
	if err != nil {
		return reportFail(cmd, err)
	}
	if f.asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return reportFail(cmd, fmt.Errorf("encoding the report: %w", err))
		}
		return nil
	}
	if err := printUsageReport(cmd.OutOrStdout(), rep); err != nil {
		return reportFail(cmd, err)
	}
	return nil
}

// reportFail prints one line naming the failure and exits 2: a report that cannot be made is never
// printed as a short or empty one.
func reportFail(cmd *cobra.Command, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "gt usage report: %v\n", err)
	return NewSilentExit(2)
}

// printUsageReport writes the report as a table of rows, then the three counts every report carries.
// Every write to w is checked, so the command never exits 0 over a partial report: the header and the
// counts directly, and the rows at a tabwriter Flush, since a five-cell row reaches w only there.
func printUsageReport(w io.Writer, rep usageattrib.Report) error {
	if _, err := fmt.Fprintf(w, "usage by %s, %s to %s\n", rep.By, rep.From.Format(time.RFC3339), rep.To.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tCALLS\tCLAUDE_UNCACHED\tDEEPSEEK_EST_USD\tSUB_SHARE\n", strings.ToUpper(rep.By))
	for _, r := range rep.Rows {
		share := "-"
		if r.SubShare != nil {
			share = fmt.Sprintf("%.4f", *r.SubShare)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.4f\t%s\n", r.Key, r.Calls, r.ClaudeUncached, r.DeepSeekEstUSD, share)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	for _, c := range []struct {
		label string
		count int
	}{
		{"unmatched sessions", rep.Unmatched},
		{"incomplete records", rep.Incomplete},
		{"records with no end time", rep.Skipped},
	} {
		if _, err := fmt.Fprintf(w, "%s: %d\n", c.label, c.count); err != nil {
			return fmt.Errorf("writing the report: %w", err)
		}
	}
	return nil
}
