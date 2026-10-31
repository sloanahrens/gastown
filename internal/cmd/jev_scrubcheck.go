package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/noise"
	"github.com/steveyegge/gastown/internal/jev/scrubcheck"
	"github.com/steveyegge/gastown/internal/workspace"
)

// jevScrubcheckDeps is what the scrubcheck subcommand takes from the host: the
// town whose check log it reads. A test supplies the lookup, so no unit test
// needs a working directory inside a town.
type jevScrubcheckDeps struct {
	findTownRoot func() (string, error)
}

// realJevScrubcheckDeps is jevScrubcheckDeps on this host.
func realJevScrubcheckDeps() jevScrubcheckDeps {
	return jevScrubcheckDeps{findTownRoot: workspace.FindFromCwdOrError}
}

// jevScrubcheckFlags is one scrubcheck command's flags; a command owns its own
// so a later run never inherits an earlier run's values.
type jevScrubcheckFlags struct {
	caller string
	since  string
	cut    float64
	asJSON bool
}

// newJevScrubcheckCmd builds `gt jev scrubcheck`.
func newJevScrubcheckCmd(d jevScrubcheckDeps) *cobra.Command {
	var f jevScrubcheckFlags
	cmd := &cobra.Command{
		Use:   "scrubcheck",
		Short: "Report what scrubbing the state costs in signal, beside the repeat noise",
		Long: `Read the shadow scrub check log and print, for each caller and question, how far
Jev's answers move when the state is scrubbed. A pair is a sampled state whose
original and scrubbed calls both answered the question. The table gives the
pairs, the states the scrub left unchanged, the states whose scrubbed call did
not answer the question, the mean and largest absolute change of a noul, score
or choice probability, and the crossing share: the share of pairs that cross
--cut, or for a choice the share whose pick changed. It also gives the rule
counts that replaced text in those states.

The baseline is the repeat noise of the same caller and question, read from the
repeat log beside logtriage's battery log. A scrub is free when its crossing
share is at most two points above that baseline, borderline up to five points
above, and costly beyond. A question with fewer than 30 pairs is too few, and one
without repeat data has no baseline. The margins and the minimum are named
constants the operator reviews.

--caller keeps one caller's lines. --since keeps the lines in the window ending
now, given as Nd for days or a Go duration such as 36h, and the baseline reads
the same window. --json prints the same
rows as JSON. The command is read-only and prints no state text.

Examples:
  gt jev scrubcheck
  gt jev scrubcheck --caller jev-logtriage --since 7d --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJevScrubcheck(cmd.OutOrStdout(), d, f, time.Now())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.caller, "caller", "", "Count only this caller's checks (default: all callers)")
	fl.StringVar(&f.since, "since", "", "Count only lines in this window ending now: Nd or a Go duration (default: all time)")
	fl.Float64Var(&f.cut, "cut", noise.DefaultCut, "Threshold a noul or score answer is split at")
	fl.BoolVar(&f.asJSON, "json", false, "Print the rows as JSON")
	return cmd
}

func init() {
	jevCmd.AddCommand(newJevScrubcheckCmd(realJevScrubcheckDeps()))
}

// runJevScrubcheck finds the town and reports the scrub check out of it.
func runJevScrubcheck(w io.Writer, d jevScrubcheckDeps, f jevScrubcheckFlags, now time.Time) error {
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevScrubcheckWithFlags(w, townRoot, f.caller, f.since, f.cut, f.asJSON, now)
}

// jevScrubcheckWithFlags reads the check log and the repeat baseline and prints
// the signal cost of the scrub.
func jevScrubcheckWithFlags(w io.Writer, townRoot, caller, since string, cut float64, asJSON bool, now time.Time) error {
	from, err := parseSince(since, now)
	if err != nil {
		return err
	}
	path := jev.ScrubCheckPath(townRoot)
	lines, err := scrubcheck.Read(path)
	if errors.Is(err, scrubcheck.ErrNoLog) {
		if asJSON {
			return encodeScrubcheck(w, scrubcheck.Report{Cut: cut, Rows: []scrubcheck.Row{}})
		}
		_, err := fmt.Fprintf(w, "no scrub check log at %s: no sampled request has been checked yet\n", path)
		return err
	}
	if err != nil {
		return err
	}
	base, err := scrubcheck.Baseline(townRoot, cut, from)
	if err != nil {
		return err
	}
	rep, err := scrubcheck.Compute(lines, scrubcheck.Options{Since: from, Cut: cut, Caller: caller}, base)
	if err != nil {
		return err
	}
	if asJSON {
		return encodeScrubcheck(w, rep)
	}
	return scrubcheck.WriteText(w, rep)
}

// encodeScrubcheck prints the report as indented JSON.
func encodeScrubcheck(w io.Writer, rep scrubcheck.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
