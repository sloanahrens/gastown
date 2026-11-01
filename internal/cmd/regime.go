// The regime record: gt regime record appends one line to the town's
// append-only regime events file, so the metrics analysis marks every series at
// the change without a hand edit to settings/regime-events.json. The reader is
// metrics.ReadRegimeEvents.
package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

// regimeDeps is what a regime verb reads from outside the process: the town, the
// instant it records at, and the session's actor.
type regimeDeps struct {
	townRoot string
	now      time.Time
	actor    string
}

// regimeFlags are the record's fields as the flags supplied them.
type regimeFlags struct {
	kind, beforeRef, afterRef, commit, reason, actor string
}

// runRegimeRecord appends one record to the town's regime events file and
// reports what it wrote. --actor names who recorded it when given, the session's
// own actor otherwise.
func runRegimeRecord(d regimeDeps, in regimeFlags, out io.Writer) error {
	actor := strings.TrimSpace(in.actor)
	if actor == "" {
		actor = d.actor
	}
	kind := strings.TrimSpace(in.kind)
	rec := metrics.RegimeEventRecord{
		TS:        d.now.UTC(),
		Kind:      kind,
		BeforeRef: in.beforeRef,
		AfterRef:  in.afterRef,
		Commit:    in.commit,
		Reason:    in.reason,
		Actor:     actor,
	}
	path := metrics.RegimeEventsPath(d.townRoot)
	if err := metrics.RecordRegimeEvent(path, rec); err != nil {
		return err
	}
	fmt.Fprintf(out, "recorded %s regime event at %s in %s\n", kind, rec.TS.Format(time.RFC3339), path)
	return nil
}

// newRegimeCommand builds gt regime over find, now and actor: find resolves the
// town, now stamps the record, and actor names the recorder when --actor is not
// given. gt registers the command over the working directory, the wall clock and
// detectActor; a test builds its own over a town and values it holds, and runs
// the same command.
func newRegimeCommand(find func() (string, error), now func() time.Time, actor func() string) *cobra.Command {
	var flags regimeFlags
	record := &cobra.Command{
		Use:   "record",
		Short: "Append one regime change to the town's regime events file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			townRoot, err := find()
			if err != nil {
				return fmt.Errorf("not in a Gas Town workspace: %w", err)
			}
			return runRegimeRecord(regimeDeps{townRoot: townRoot, now: now(), actor: actor()}, flags, cmd.OutOrStdout())
		},
	}
	record.Flags().StringVar(&flags.kind, "kind", "", "What changed: "+strings.Join(metrics.RegimeKinds, ", "))
	record.Flags().StringVar(&flags.beforeRef, "before", "", "The ref the regime was before the change")
	record.Flags().StringVar(&flags.afterRef, "after", "", "The ref the regime was after the change")
	record.Flags().StringVar(&flags.commit, "commit", "", "The commit the change landed in, when it has one")
	record.Flags().StringVar(&flags.reason, "reason", "", "One line saying what changed")
	record.Flags().StringVar(&flags.actor, "actor", "", "Who recorded the change; the session's actor when unset")
	// Each flag is defined just above, and MarkFlagRequired's only error is an
	// unknown flag name, so there is nothing to hand an error to here.
	for _, name := range []string{"kind", "before", "after", "reason"} {
		_ = record.MarkFlagRequired(name)
	}
	regime := &cobra.Command{
		Use:     "regime",
		GroupID: GroupConfig,
		Short:   "Record a regime change so the metrics analysis marks it",
		Long: `Record a regime change in the town's append-only regime events file.

  gt regime record   Append one change to .runtime/metrics/regime-events.jsonl

The metrics analysis reads that file with settings/regime-events.json and marks
every series at each change. Run it at an install, directive, gate, seat-model
or pool change: the reason, and the refs either side of it, are what a reader
has to judge the change by.`,
		RunE: requireSubcommand,
	}
	regime.AddCommand(record)
	return regime
}

func init() {
	rootCmd.AddCommand(newRegimeCommand(workspace.FindFromCwdOrError, time.Now, detectActor))
}
