package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/report"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	reportHour bool
	reportAt   string
	reportJSON bool
)

var reportCmd = &cobra.Command{
	Use:     "report",
	GroupID: GroupDiag,
	Short:   "Summarize the town's last hour from its ledgers",
	Long: `Print one fixed-order plain-text summary of the hour ending at --at
(default: now), assembled from the town's own ledgers and read-only:

  mains              origin/main's tip, read with git ls-remote (never a
                     fetch — the mayor/rig canonical clone is never fetched
                     here), against the installed gt version, and how many
                     commits the installed build is behind. When the tip's
                     objects are not in the local store, the count is made
                     against the clone's own origin/main ref and the row
                     names it
  landings           one row per landing: merge and land times from the
                     daemon's "[land] <bead>: merged/landed" lines, the
                     stages: timing line, and the om verdict from
                     .runtime/landings/<rig>.jsonl
  rejections by kind the MERGE REJECTION notes appended in the window,
                     tallied by failure class
  queue depth        the gt:ready-to-land count per rig
  seats              the intent records under .runtime/agents/ and the
                     refill episodes in .runtime/seat-refill.json
  sweep              each rig's .runtime/tier-sweep/<rig>.json, or "no sweep
                     record" until the daemon's tier sweep has run
  escalations        escalations opened and closed in the window
  spend              the DeepSeek reading in .runtime/watch/spend.json,
                     shown only while it is under 15 minutes old
  attention          the open items in .runtime/attention/state.json

A source that cannot be read prints "<section>: unavailable (<reason>)" and
never fails the command: gt report only reads, exits 0, and writes nothing —
no Dolt writes, no mail, no nudges. When the window's sources are simply
empty, the section prints "(none)".

Timestamps are handled per source: daemon.log is local time, bd JSON is UTC.
The window is computed tz-aware, so a line stamped just inside it counts
whichever zone it was written in.

--json prints the same summary as one JSON object.

--hour names the only window gt report has today, and it is required: a
missing window is an error, not a silent default, so a later --day cannot be
ignored by a script that meant it.

Examples:
  gt report --hour                       # the hour ending now
  gt report --hour --at 2026-10-02T13:00:00-05:00
  gt report --hour --json | jq .attention`,
	Args: cobra.NoArgs,
	RunE: runReport,
}

func init() {
	reportCmd.Flags().BoolVar(&reportHour, "hour", false, "Summarize the hour ending at --at (the only window today)")
	reportCmd.Flags().StringVar(&reportAt, "at", "", "End of the window, RFC3339 (default: now)")
	reportCmd.Flags().BoolVar(&reportJSON, "json", false, "Output the summary as JSON")
	rootCmd.AddCommand(reportCmd)
}

func runReport(cmd *cobra.Command, _ []string) error {
	if !reportHour {
		return errors.New("gt report needs a window: --hour")
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	until := time.Now()
	if reportAt != "" {
		until, err = time.Parse(time.RFC3339, reportAt)
		if err != nil {
			return fmt.Errorf("--at must be RFC3339 (2006-01-02T15:04:05-07:00): %w", err)
		}
	}
	r := report.NewHourRun(townRoot, until, reportInstalledVersion(), version.BuildCommit(),
		func() ([]string, error) { return knownRigNames(townRoot) })
	r.Out = cmd.OutOrStdout()
	doc := r.Collect()
	if reportJSON {
		enc := json.NewEncoder(r.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	r.Render(doc)
	return nil
}

// reportInstalledVersion renders the installed gt's version the way
// `gt version --short` does, so the two never disagree.
func reportInstalledVersion() string {
	return Version + "-" + Build
}
