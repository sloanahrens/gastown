package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/fixrecurrence"
)

// fix_held.go is the fix-recurrence signal's rate (gt-t9vnda): it reads the
// rig's beads once, the way `gt fix-recurrence scan` does, and hands
// fixrecurrence.Report the bugs the window closed and the claims the store
// already carries against them. `gt fix-held` prints that result (gt-tiaczu).

// The labels Plan writes on a citing bead when it records a recurrence or a
// same-class claim (internal/fixrecurrence/plan.go). Each names a claim from
// that bead to the bead it names, which this file passes to Report as an edge.
const (
	fixHeldRecurrenceLabel = "recurrence-of:"
	fixHeldSameClassLabel  = "same-class-of:"
)

// fixHeldReport reads the rig's beads and returns the fix-held rate over
// [since, until): the closed bugs in the window, and the claims the store
// already carries against them. A failed read is returned before a window is
// computed, so a partial list is never taken as the rig's answer, and a closed
// bug the rows cannot place in time fails the read rather than counting as held.
func fixHeldReport(d fixRecurrenceDeps, since, until time.Time) (fixrecurrence.ReportResult, error) {
	dir, err := d.workDir()
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("finding the rig directory: %w", err)
	}
	raw, err := d.run(dir, fixRecurrenceListArgs)
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("reading the rig's beads: %w", err)
	}
	// The issue type is the one field Bead does not keep, and only a bug counts
	// toward the rate, so the rows are decoded for it first: a list that does
	// not parse fails here.
	issueTypes, err := fixHeldIssueTypes(raw)
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("reading the rig's beads: %w", err)
	}
	// fixRecurrenceBeads is the scan's parse and validation: every row's
	// timestamps are read, and a row the scan would refuse is refused here too.
	listed, state, err := fixRecurrenceBeads(raw)
	if err != nil {
		return fixrecurrence.ReportResult{}, fmt.Errorf("reading the rig's beads: %w", err)
	}
	closed, err := fixHeldClosed(listed, issueTypes)
	if err != nil {
		return fixrecurrence.ReportResult{}, err
	}
	return fixrecurrence.Report(closed, fixHeldEdges(listed, state), since, until), nil
}

// fixHeldIssueTypes is each listed bead's issue type, keyed by its lowercased
// id as Report keys ids. It decodes the rows for the one field Bead does not
// carry, and a list that does not parse fails the read rather than yielding no
// bug.
func fixHeldIssueTypes(raw []byte) (map[string]string, error) {
	var rows []fixRecurrenceRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bd list output does not parse: %w", err)
	}
	issueTypes := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			issueTypes[strings.ToLower(row.ID)] = row.IssueType
		}
	}
	return issueTypes, nil
}

// fixHeldClosed is the closed bugs the rate counts: a bead the store closed and
// typed bug. A closed bug whose closed_at is missing cannot be shown to be in
// the window, and dropping it would inflate the rate, so the read refuses it.
func fixHeldClosed(listed []fixrecurrence.Bead, issueTypes map[string]string) ([]fixrecurrence.Closed, error) {
	var closed []fixrecurrence.Closed
	for _, bead := range listed {
		if bead.Status != "closed" || issueTypes[strings.ToLower(bead.ID)] != "bug" {
			continue
		}
		if bead.ClosedAt.IsZero() {
			return nil, fmt.Errorf("bead %s: closed_at is missing, so it cannot be placed in the window", bead.ID)
		}
		closed = append(closed, fixrecurrence.Closed{ID: bead.ID, ClosedAt: bead.ClosedAt})
	}
	return closed, nil
}

// fixHeldEdges is every claim the store already carries: its typed dependency
// edges and the two labels Plan writes on a citing bead. Report has no
// timestamps and trusts the caller to supply only an edge from a bead created
// after the cited bead closed, so an edge whose order the rows do not show --
// either end unlisted, the cited bead not closed, or the citing bead created at
// or before the close -- is dropped. A pair a label names is one claim: bd
// keeps one "related" type for both a recurrence and a same-class claim, and
// the label on the citing bead says which, so the edge adds no second claim.
func fixHeldEdges(listed []fixrecurrence.Bead, state fixrecurrence.State) []fixrecurrence.Edge {
	type pair struct{ citing, cited string }
	created := make(map[string]time.Time, len(listed))
	closed := make(map[string]time.Time, len(listed))
	for _, bead := range listed {
		key := strings.ToLower(bead.ID)
		created[key] = bead.CreatedAt
		closed[key] = bead.ClosedAt
	}
	var edges []fixrecurrence.Edge
	labeled := make(map[pair]bool)
	for id, labels := range state.Labels {
		for label := range labels {
			rel, old, ok := fixHeldLabel(label)
			if !ok {
				continue
			}
			edges = append(edges, fixrecurrence.Edge{New: id, Old: old, Rel: rel})
			labeled[pair{strings.ToLower(id), strings.ToLower(old)}] = true
		}
	}
	for edge := range state.Deps {
		key := pair{strings.ToLower(edge.New), strings.ToLower(edge.Old)}
		if edge.Rel == fixrecurrence.Recurrence && labeled[key] {
			continue
		}
		edges = append(edges, edge)
	}
	claims := make([]fixrecurrence.Edge, 0, len(edges))
	for _, edge := range edges {
		citingCreated, ok := created[strings.ToLower(edge.New)]
		if !ok {
			continue
		}
		citedClosed, ok := closed[strings.ToLower(edge.Old)]
		if !ok || citedClosed.IsZero() || !citingCreated.After(citedClosed) {
			continue
		}
		claims = append(claims, edge)
	}
	return claims
}

// fixHeldLabel is the relationship and cited bead a claim label names:
// recurrence-of:<id> is a recurrence and same-class-of:<id> a same-class claim.
// Any other label, and one naming no bead, claims nothing here.
func fixHeldLabel(label string) (fixrecurrence.Relationship, string, bool) {
	if old, ok := strings.CutPrefix(label, fixHeldRecurrenceLabel); ok && old != "" {
		return fixrecurrence.Recurrence, old, true
	}
	if old, ok := strings.CutPrefix(label, fixHeldSameClassLabel); ok && old != "" {
		return fixrecurrence.SameClass, old, true
	}
	return "", "", false
}

// fixHeldDefaultSince is the window a run reports without --since.
const fixHeldDefaultSince = "30d"

// fixHeldRelationships is the report's row order: every relationship the
// package defines, so a relationship nothing claimed still shows its zero
// rather than leaving the reader to notice a missing row.
var fixHeldRelationships = []fixrecurrence.Relationship{
	fixrecurrence.Recurrence,
	fixrecurrence.SameClass,
	fixrecurrence.Regression,
	fixrecurrence.FollowUp,
	fixrecurrence.Reference,
}

// fixHeldJSON is the report's --json shape: the same fields the text prints,
// with the per-relationship counts as one object keyed by the relationship.
type fixHeldJSON struct {
	Held            int            `json:"held"`
	Total           int            `json:"total"`
	Rate            float64        `json:"rate"`
	PerRelationship map[string]int `json:"per_relationship"`
	Chronic         []string       `json:"chronic"`
}

func init() {
	rootCmd.AddCommand(newFixHeldCmd(realFixRecurrenceDeps()))
}

// newFixHeldCmd is `gt fix-held`: the report over the rig of the caller, read
// once. The deps are the argument so a test drives it with a fake runner and a
// fixed clock.
func newFixHeldCmd(d fixRecurrenceDeps) *cobra.Command {
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "fix-held",
		GroupID: GroupDiag,
		Short:   "Report the fix-held rate over the bugs a window closed",
		Long: `Read the current rig's beads and report the fix-held rate over the window
ending now: the bugs that closed in it, and the ones no claim of a failed fix
points at. A claim is a typed dependency edge or a label the fix-recurrence plan
writes, from a bead created after the cited bug closed.

--since sets the window as Nd for days or a Go duration such as 36h. The window
includes a bug closed exactly --since ago and stops at the moment the report
runs. A bug whose close time the rows do not carry is an error rather than a bug
counted as held, and a bead read that fails is an error: a partial list is never
reported as the rig's rate.

The report prints the held count and rate, one line per relationship, and the
chronic list -- the bugs two or more citing beads mark as failed fixes. --json
prints the same fields as one JSON object.

Exit status: 0 when the report printed, 2 when it could not be made: the rig
directory or its beads cannot be read, a row is missing a time, --since is not
a valid window, or the report cannot be written.

Examples:
  gt fix-held
  gt fix-held --since 7d
  gt fix-held --json`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFixHeldCmd(cmd, d, since, asJSON)
		},
	}
	cmd.Flags().StringVar(&since, "since", fixHeldDefaultSince, "Report the window ending now: Nd or a Go duration such as 36h")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the report as one JSON object")
	return cmd
}

// runFixHeldCmd makes the report and prints it. A failure is printed by
// fixHeldFail, so no caller sees a partial report and exit 0.
func runFixHeldCmd(cmd *cobra.Command, d fixRecurrenceDeps, since string, asJSON bool) error {
	now := d.now()
	from, err := parseSince(since, now)
	if err != nil {
		return fixHeldFail(cmd, err)
	}
	result, err := fixHeldReport(d, from, now)
	if err != nil {
		return fixHeldFail(cmd, err)
	}
	if asJSON {
		if err := writeFixHeldJSON(cmd.OutOrStdout(), result); err != nil {
			return fixHeldFail(cmd, err)
		}
		return nil
	}
	if err := printFixHeldReport(cmd.OutOrStdout(), result); err != nil {
		return fixHeldFail(cmd, err)
	}
	return nil
}

// fixHeldFail prints a failed run's error on stderr, naming the command, and
// exits 2 the way `gt usage report` does: the command is silent to main, so
// without this line the error would be lost, and a report stream that fails to
// take the report is the same failure as a read that fails, not a usage error.
func fixHeldFail(cmd *cobra.Command, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "gt fix-held: %v\n", err)
	return NewSilentExit(2)
}

// printFixHeldReport writes the report as one line per field, checking every
// write so a report cut short fails instead of looking like a short one.
func printFixHeldReport(w io.Writer, result fixrecurrence.ReportResult) error {
	if _, err := fmt.Fprintf(w, "held %d of %d (rate %.2f)\n", result.Held, result.Total, result.Rate); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	for _, rel := range fixHeldRelationships {
		if _, err := fmt.Fprintf(w, "%s %d\n", rel, result.PerRelationship[rel]); err != nil {
			return fmt.Errorf("writing the report: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "chronic: %s\n", fixHeldChronicLine(result.Chronic)); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// fixHeldChronicLine is the chronic list on one line: the ids Report sorted
// against each other, and "none" for a window no bug failed twice.
func fixHeldChronicLine(chronic []string) string {
	if len(chronic) == 0 {
		return "none"
	}
	return strings.Join(chronic, ", ")
}

// writeFixHeldJSON writes the report as one JSON object holding the fields the
// text prints. Every relationship is a key, with its zero, so a reader of
// either form totals the same claims.
func writeFixHeldJSON(w io.Writer, result fixrecurrence.ReportResult) error {
	perRelationship := make(map[string]int, len(fixHeldRelationships))
	for _, rel := range fixHeldRelationships {
		perRelationship[string(rel)] = result.PerRelationship[rel]
	}
	chronic := result.Chronic
	if chronic == nil {
		chronic = []string{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fixHeldJSON{
		Held:            result.Held,
		Total:           result.Total,
		Rate:            result.Rate,
		PerRelationship: perRelationship,
		Chronic:         chronic,
	}); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}
