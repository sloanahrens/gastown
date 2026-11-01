// The intervention record: gt intervention record appends one line to the
// town's append-only intervention record, so the weekly report can count how
// often the loop needs a person, and for what, beside its rework figures. The
// writer and the reader are metrics.RecordIntervention and
// metrics.CountInterventions.
package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

// interventionDeps is what an intervention verb reads from outside the process:
// the town, the instant it records at, and the session's actor.
type interventionDeps struct {
	townRoot string
	now      time.Time
	actor    string
}

// interventionFlags are the record's fields as the flags supplied them.
type interventionFlags struct {
	kind, bead, reason, actor string
}

// runInterventionRecord appends one line to the town's intervention record and
// reports what it wrote. The record's rig is the bead's, read from the town's
// route table; --actor names who recorded it when given, the session's own actor
// otherwise.
func runInterventionRecord(d interventionDeps, in interventionFlags, out io.Writer) error {
	actor := strings.TrimSpace(in.actor)
	if actor == "" {
		actor = d.actor
	}
	bead := strings.TrimSpace(in.bead)
	rec := metrics.InterventionRecord{
		TS:     d.now,
		Kind:   strings.TrimSpace(in.kind),
		Bead:   bead,
		Reason: in.reason,
		Actor:  actor,
	}
	// Validating before the bead's rig is resolved reports a bad kind or reason
	// as such, rather than as the route table's answer about a bead.
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("intervention: %w", err)
	}
	if bead != "" {
		rig, err := interventionBeadRig(d.townRoot, bead)
		if err != nil {
			return err
		}
		rec.Rig = rig
	}
	path := metrics.InterventionsPath(d.townRoot)
	if err := metrics.RecordIntervention(path, rec); err != nil {
		return err
	}
	where := "the town"
	if bead != "" {
		where = bead
	}
	fmt.Fprintf(out, "recorded %s intervention for %s at %s in %s\n", rec.Kind, where, rec.TS.UTC().Format(time.RFC3339), path)
	return nil
}

// interventionBeadRig names the rig a bead belongs to: the first segment of the
// path of the route that carries its prefix. A town-level bead, whose route is
// ".", has no rig and returns "". A route table that cannot be read, a prefix
// no route names, and a route whose path has no first segment are errors: an
// intervention is never attributed to a guessed rig.
func interventionBeadRig(townRoot, beadID string) (string, error) {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return "", fmt.Errorf("bead %q names no prefix", beadID)
	}
	routes, err := beads.LoadRoutes(beads.GetTownBeadsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("reading the route table for %s: %w", beadID, err)
	}
	for _, route := range routes {
		if route.Prefix != prefix {
			continue
		}
		if route.Path == "." {
			return "", nil
		}
		rig, _, _ := strings.Cut(route.Path, "/")
		if rig == "" {
			return "", fmt.Errorf("the route for prefix %q names no rig", prefix)
		}
		return rig, nil
	}
	return "", fmt.Errorf("no route names prefix %q (bead %s)", prefix, beadID)
}

// newInterventionCommand builds gt intervention over find, now and actor: find
// resolves the town, now stamps the record, and actor names the recorder when
// --actor is not given. gt registers the command over the working directory, the
// wall clock and detectActor; a test builds its own over a town and values it
// holds, and runs the same command.
func newInterventionCommand(find func() (string, error), now func() time.Time, actor func() string) *cobra.Command {
	var flags interventionFlags
	record := &cobra.Command{
		Use:   "record",
		Short: "Append one intervention to the town's intervention record",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			townRoot, err := find()
			if err != nil {
				return fmt.Errorf("not in a Gas Town workspace: %w", err)
			}
			return runInterventionRecord(interventionDeps{townRoot: townRoot, now: now(), actor: actor()}, flags, cmd.OutOrStdout())
		},
	}
	record.Flags().StringVar(&flags.kind, "kind", "", "What happened: "+strings.Join(metrics.InterventionKinds, ", "))
	record.Flags().StringVar(&flags.bead, "bead", "", "The bead it concerned; optional for hold and restart, required for the rest")
	record.Flags().StringVar(&flags.reason, "reason", "", "One line saying why the loop needed a person")
	record.Flags().StringVar(&flags.actor, "actor", "", "Who intervened; the session's actor when unset")
	// Each flag is defined just above, and MarkFlagRequired's only error is an
	// unknown flag name, so there is nothing to hand an error to here.
	for _, name := range []string{"kind", "reason"} {
		_ = record.MarkFlagRequired(name)
	}
	intervention := &cobra.Command{
		Use:     "intervention",
		GroupID: GroupConfig,
		Short:   "Record when the loop needed a person, so the weekly report can count it",
		Long: `Record one intervention in the town's append-only intervention record.

  gt intervention record   Append one intervention to .runtime/metrics/interventions.jsonl

Run it whenever a person or the overseer steps in where the loop could not: a
waiver, a take-over of a bead, a fix by hand, a hold, a restart, a rescue of a
landing. Each line is the time, the kind, the bead and rig it concerned, why, and
who intervened. The record is what the weekly report counts interventions from,
so a kind is one of the words the command lists, never free text.`,
		RunE: requireSubcommand,
	}
	intervention.AddCommand(record)
	return intervention
}

func init() {
	rootCmd.AddCommand(newInterventionCommand(workspace.FindFromCwdOrError, time.Now, detectActor))
}
