package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/noise"
	"github.com/steveyegge/gastown/internal/workspace"
)

// jevNoiseDeps is what the noise subcommand takes from the host: the town
// whose batteries it reads. A test supplies the lookup, so no unit test needs a
// working directory inside a town.
type jevNoiseDeps struct {
	findTownRoot func() (string, error)
}

// realJevNoiseDeps is jevNoiseDeps on this host.
func realJevNoiseDeps() jevNoiseDeps {
	return jevNoiseDeps{findTownRoot: workspace.FindFromCwdOrError}
}

// jevNoiseFlags is one noise command's flags; a command owns its own so a later
// run never inherits an earlier run's values.
type jevNoiseFlags struct {
	battery string
	since   string
	cut     float64
	asJSON  bool
}

// newJevNoiseCmd builds `gt jev noise`.
func newJevNoiseCmd(d jevNoiseDeps) *cobra.Command {
	var f jevNoiseFlags
	cmd := &cobra.Command{
		Use:   "noise",
		Short: "Report how far Jev's answers move when a sampled group is asked again",
		Long: `Read the repeat log beside a battery's log and print, for each question, how far
Jev's answers move when a sampled group is asked again with the same state. Each
repeat pairs with the latest original answer of its key and question version at
or before it. An item counts when its original answered and it has at least
three repeats; any other sampled item is counted as skipped.

For a noul or score question the report gives the items, the mean and largest
sample standard deviation across each item's original and repeat answers, the
share of items whose answers fall on both sides of --cut (a score is compared as
its level index), and the middle 90 percent span: the widest answer range among
the middle 90 percent of items. For a choice question it gives the share of
items whose picked option changed in some repeat.

--battery defaults to logtriage, the only battery that writes repeats. --since
keeps only the items whose original falls in the window ending now, given as Nd
for days or a Go duration such as 36h. --json prints the same rows as JSON. The
command is read-only.

Examples:
  gt jev noise
  gt jev noise --since 7d --cut 0.5 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJevNoise(cmd.OutOrStdout(), d, f, time.Now())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.battery, "battery", "logtriage", "Battery whose repeat log to read")
	fl.StringVar(&f.since, "since", "", "Count only items whose original is in this window ending now: Nd or a Go duration (default: all time)")
	fl.Float64Var(&f.cut, "cut", noise.DefaultCut, "Threshold a noul or score answer is split at")
	fl.BoolVar(&f.asJSON, "json", false, "Print the rows as JSON")
	return cmd
}

func init() {
	jevCmd.AddCommand(newJevNoiseCmd(realJevNoiseDeps()))
}

// runJevNoise finds the town and reports the noise out of it.
func runJevNoise(w io.Writer, d jevNoiseDeps, f jevNoiseFlags, now time.Time) error {
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevNoiseWithFlags(w, townRoot, f.battery, f.since, f.cut, f.asJSON, now)
}

// jevNoiseWithFlags reads the battery's logs and prints the noise of the repeats.
func jevNoiseWithFlags(w io.Writer, townRoot, name, since string, cut float64, asJSON bool, now time.Time) error {
	from, err := parseSince(since, now)
	if err != nil {
		return err
	}
	logPath, err := noise.LogPath(name, townRoot)
	if err != nil {
		return err
	}
	logs, err := noise.Read(logPath)
	if err != nil {
		return err
	}
	if !logs.RepeatLogFound {
		if asJSON {
			return encodeNoise(w, jevNoiseOutput{Report: noise.Report{Battery: name, Cut: cut, Rows: []noise.Row{}}})
		}
		_, err := fmt.Fprintf(w, "no repeat log at %s: no sampled group has been asked again yet\n", battery.ReaskPath(logPath))
		return err
	}
	rep, err := noise.Compute(logs.Originals, logs.Repeats, noise.Options{Since: from, Cut: cut})
	if err != nil {
		return err
	}
	rep.Battery = name
	if asJSON {
		return encodeNoise(w, jevNoiseOutput{Report: rep, RepeatLogFound: true})
	}
	return noise.WriteText(w, rep)
}

// jevNoiseOutput is the JSON the command prints. RepeatLogFound tells a town with
// no repeat log apart from one whose repeats measured no items, since both have
// empty rows.
type jevNoiseOutput struct {
	noise.Report
	RepeatLogFound bool `json:"repeat_log_found"`
}

// encodeNoise prints the output as indented JSON.
func encodeNoise(w io.Writer, out jevNoiseOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
