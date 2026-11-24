package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/specsplit"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/workspace"
)

// specSplitMargin is how far under the split trigger a piece may estimate and
// still pass the dry run's verdict. A piece at or within this many minutes of
// the trigger fails: its estimate is one landing could push over the trigger.
const specSplitMargin = 0.3

// specSplitDryRun is the --dry-run flag of gt spec split.
var specSplitDryRun bool

// specSplitApply is the --apply flag of gt spec split.
var specSplitApply bool

func init() {
	specSplitCmd.Flags().BoolVar(&specSplitApply, "apply", false, "Create the two piece beads, wire B after A and close the original")
}

var specSplitCmd = &cobra.Command{
	Use:   "split <bead-id>",
	Short: "Print the two drafts a bead splits into, or create them with --apply",
	Long: `Split a work bead into two pieces by package, printing or creating them.

Each Constraints bullet and acceptance box is placed in the package directory it
names. The packages are divided between piece A and piece B, which depends on
A. Each draft is the full bead text that piece would carry.

The packages are read from the rig's target branch. A bead that cannot be
split is refused with the reason and exits non-zero.

With --dry-run each draft is then estimated and linted, and nothing is written.
The verdict line says whether both pieces lint ok and estimate under the split
trigger. A piece at or within 0.3 minutes of the trigger fails, and so does a
piece the lint refuses; the command exits 1 when either fails.

With --apply the dry-run checks run first: a draft that fails prints its reason
and creates nothing. Then the two pieces are created: piece A, then piece B whose
PIECE_A placeholder is replaced by A's id, then B's dependency on A, then each
bead that depended on the original moved onto B keeping its relation, then the
original closed naming both ids. A dependent is found by the store's own list and
by the dependency table itself, which keeps an edge stored as external:<rig>:<id>
that the store's list leaves out, and such an edge is removed in the form it is
stored. The pieces keep the original's priority and rig.

One of --dry-run or --apply is required: without one the command refuses.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runSpecSplit,
}

// specSplitRead reads the bead to split.
type specSplitRead func(beadID string) (*beads.Issue, error)

// specSplitTarget is the target branch's inventory for the bead's rig, nil when
// no rig holds the bead.
type specSplitTarget func(beadID string) *specdispatch.TargetTree

// specSplitCheck is what the dry run judges each draft by: the split trigger,
// and the estimate and the lint of one draft. The command wires the production
// run; a test wires fakes.
type specSplitCheck struct {
	// Trigger is story_points.split_over_minutes, the size a bead splits at.
	// Zero is off, and a dry run cannot judge its drafts without a trigger.
	Trigger float64
	// Estimate is the dry-run estimate of one piece's draft. It writes nothing.
	Estimate func(beadID string, draft *beads.Issue) (specEstimateOutcome, error)
	// Lint is the lint verdict of one piece's draft, its estimate labels set.
	Lint func(draft specdispatch.Spec) specdispatch.LintVerdict
}

// specSplitJudgement is one piece's estimate, lint and verdict.
type specSplitJudgement struct {
	Piece   specsplit.Piece
	Outcome specEstimateOutcome
	Verdict specdispatch.LintVerdict
	// Fail is why the piece fails the verdict, "" when it passes.
	Fail string
}

func runSpecSplit(cmd *cobra.Command, args []string) error {
	return runSpecSplitFrom(cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], specSplitDryRun, specSplitApply, workspace.FindFromCwd)
}

// runSpecSplitFrom resolves the town with find, then applies the split or runs
// the dry run with the production estimate and lint. find is a parameter so a
// test can make the town unfindable. errw takes the estimate's warnings.
func runSpecSplitFrom(out, errw io.Writer, beadID string, dryRun, apply bool, find func() (string, error)) error {
	if dryRun && apply {
		return errors.New("pass one of --dry-run or --apply, not both")
	}
	townRoot, err := find()
	if err != nil {
		return fmt.Errorf("cannot find the town: %w", err)
	}
	read := func(id string) (*beads.Issue, error) { return showBead(townRoot, id) }
	target := func(id string) *specdispatch.TargetTree { return specLintTarget(townRoot, id, openRigTree) }
	sd, err := readSpecDispatchConfig(townRoot)
	if err != nil {
		return fmt.Errorf("cannot read patrols.spec_dispatch: %w", err)
	}
	lint, err := specSplitLintCheck(townRoot, beadID, sd, errw)
	if err != nil {
		return fmt.Errorf("cannot judge the drafts of %s: %w", beadID, err)
	}
	check := specSplitCheck{Trigger: sd.StoryPointsSplitOver(), Estimate: specSplitEstimate(townRoot, errw), Lint: lint}
	if apply {
		db := beads.New(resolveBeadDirFromTownRoot(townRoot, beadID))
		return runSpecSplitApplyOn(out, beadID, specBeadRig(townRoot, beadID), read, target, check, db, specSplitSQLDeps{db})
	}
	return runSpecSplitOn(out, beadID, dryRun, read, target, check)
}

// runSpecSplitOn prints the two piece drafts of beadID to out, then each
// piece's minutes and lint, then the verdict. A failed verdict returns a silent
// exit of 1 after the report. The read, the target inventory and the check are
// given, so a test drives it through fakes. Every refusal returns before
// anything is printed: the drafts, their estimates and their lints are all made
// first.
func runSpecSplitOn(out io.Writer, beadID string, dryRun bool, read specSplitRead, target specSplitTarget, check specSplitCheck) error {
	if !dryRun {
		return errors.New("gt spec split needs --dry-run to print the two drafts or --apply to create them")
	}
	issue, tree, plan, err := specSplitPlan(beadID, read, target)
	if err != nil {
		return err
	}
	pieces := []specsplit.Piece{plan.A, plan.B}
	judged, err := specSplitJudgeAll(beadID, issue, tree, pieces, check)
	if err != nil {
		return err
	}
	// The report is built whole and written once, so a failed write reaches
	// the caller instead of leaving half a report on out.
	var report strings.Builder
	for i, piece := range pieces {
		if i > 0 {
			report.WriteString("\n")
		}
		header := fmt.Sprintf("== piece %s: %s", piece.Name, strings.Join(piece.Packages, ", "))
		if piece.DependsOn != "" {
			header += fmt.Sprintf(" (depends on piece %s)", piece.DependsOn)
		}
		report.WriteString(header + " ==\n")
		report.WriteString(piece.Text)
	}
	report.WriteString("\n")
	var failures []string
	for _, j := range judged {
		report.WriteString(specSplitPieceLine(j))
		if j.Fail != "" {
			failures = append(failures, fmt.Sprintf("piece %s: %s", j.Piece.Name, j.Fail))
		}
	}
	if len(failures) > 0 {
		report.WriteString("verdict: fails: " + strings.Join(failures, "; ") + "\n")
	} else {
		report.WriteString(fmt.Sprintf("verdict: both pieces lint ok and estimate under %s min\n", specTriggerText(check.Trigger)))
	}
	if _, err := io.WriteString(out, report.String()); err != nil {
		return fmt.Errorf("cannot print the drafts for %s: %w", beadID, err)
	}
	if len(failures) > 0 {
		return NewSilentExit(specLintExitRefused)
	}
	return nil
}

// specSplitPlan reads beadID, reads the target branch's files and plans the two
// pieces. Both modes refuse here, before anything is written.
func specSplitPlan(beadID string, read specSplitRead, target specSplitTarget) (*beads.Issue, *specdispatch.TargetTree, specsplit.SplitPlan, error) {
	issue, err := read(beadID)
	if err != nil {
		return nil, nil, specsplit.SplitPlan{}, fmt.Errorf("cannot read bead %s: %w", beadID, err)
	}
	tree := target(beadID)
	if tree == nil {
		return nil, nil, specsplit.SplitPlan{}, fmt.Errorf("cannot read the target branch's files for %s", beadID)
	}
	if tree.Err != "" {
		return nil, nil, specsplit.SplitPlan{}, fmt.Errorf("cannot read the target branch's files for %s: %s", beadID, tree.Err)
	}
	spec := specsplit.Spec{
		Constraints: specdispatch.Sections(issue.Description)["constraints"],
		Acceptance:  beads.AcceptanceText(issue),
		Dirs:        specSplitPackageDirs(tree.Files),
		BaseFiles:   specSplitBaseFiles(tree.Files),
		// The target inventory lists files, not symbols, so the command knows
		// of no existing symbol and every symbol A declares keeps its note.
		BaseSymbols: map[string]bool{},
	}
	groups, err := specsplit.Group(spec)
	if err != nil {
		return nil, nil, specsplit.SplitPlan{}, fmt.Errorf("cannot split %s: %w", beadID, err)
	}
	plan, err := specsplit.Plan(spec, groups)
	if err != nil {
		return nil, nil, specsplit.SplitPlan{}, fmt.Errorf("cannot split %s: %w", beadID, err)
	}
	return issue, tree, plan, nil
}

// runSpecSplitApplyOn creates the two pieces of beadID, moves the original's
// dependents onto piece B and closes it: piece A, piece B with PIECE_A replaced
// by A's id, B's dependency on A, the dependents, then the original closed
// naming both ids. The dry-run checks run first, so a failed one creates
// nothing, and the dependents are read before the first write. A write that
// fails before the original is closed undoes the apply: it closes the pieces it
// made, A before B, and puts the dependents it moved back on the original, which
// stays open; the error names the failure and any undo write that failed. raw is
// the dependency table read that finds a dependent the store's own list leaves
// out; nil uses only the store's list.
func runSpecSplitApplyOn(out io.Writer, beadID, rig string, read specSplitRead, target specSplitTarget, check specSplitCheck, db beads.Client, raw specSplitRawDeps) error {
	issue, tree, plan, err := specSplitPlan(beadID, read, target)
	if err != nil {
		return err
	}
	judged, err := specSplitJudgeAll(beadID, issue, tree, []specsplit.Piece{plan.A, plan.B}, check)
	if err != nil {
		return err
	}
	var failures []string
	for _, j := range judged {
		if j.Fail != "" {
			failures = append(failures, fmt.Sprintf("piece %s: %s", j.Piece.Name, j.Fail))
		}
	}
	if len(failures) > 0 {
		if _, err := fmt.Fprintf(out, "verdict: fails: %s\n", strings.Join(failures, "; ")); err != nil {
			return fmt.Errorf("cannot report the failed checks of %s: %w", beadID, err)
		}
		return NewSilentExit(specLintExitRefused)
	}
	dependents, err := specSplitDependents(db, raw, rig, beadID)
	if err != nil {
		return err
	}
	a, err := db.Create(specSplitPieceOptions(issue, plan.A, rig, ""))
	if err != nil {
		return fmt.Errorf("cannot create piece A of %s: %w", beadID, err)
	}
	created := []string{a.ID}
	b, err := db.Create(specSplitPieceOptions(issue, plan.B, rig, a.ID))
	if err != nil {
		return specSplitUndoOn(db, created, nil, beadID, "", fmt.Errorf("created piece A as %s, but cannot create piece B of %s: %w", a.ID, beadID, err))
	}
	created = append(created, b.ID)
	if err := db.AddDependency(b.ID, a.ID); err != nil {
		return specSplitUndoOn(db, created, nil, beadID, b.ID, fmt.Errorf("created piece A as %s and piece B as %s, but cannot wire B after A: %w", a.ID, b.ID, err))
	}
	moved, err := specSplitMoveDependents(db, raw, rig, beadID, b.ID, dependents)
	if err != nil {
		movedText := "none"
		if len(moved) > 0 {
			ids := make([]string, len(moved))
			for i, m := range moved {
				ids[i] = m.id
			}
			movedText = strings.Join(ids, ", ")
		}
		return specSplitUndoOn(db, created, moved, beadID, b.ID, fmt.Errorf("created piece A as %s and piece B as %s, but cannot move the dependents of %s (moved: %s): %w",
			a.ID, b.ID, beadID, movedText, err))
	}
	reason := fmt.Sprintf("split into %s and %s", a.ID, b.ID)
	if err := db.CloseWithReason(reason, beadID); err != nil {
		return specSplitUndoOn(db, created, moved, beadID, b.ID, fmt.Errorf("created piece A as %s and piece B as %s, but cannot close %s: %w", a.ID, b.ID, beadID, err))
	}
	if _, err := fmt.Fprintf(out, "split %s into %s and %s\n", beadID, a.ID, b.ID); err != nil {
		return fmt.Errorf("cannot report the split of %s: %w", beadID, err)
	}
	return nil
}

// specSplitRawDeps is the dependency-table read without the store's join to
// its issues table, so a target in another database still shows. It is how the
// apply sees a dependent whose edge names the original as external:<rig>:<id>,
// which the store's own dependents list leaves out.
type specSplitRawDeps interface {
	// Dependents returns every dependent of id and its relation, an edge
	// stored as external:<rig>:<id> included.
	Dependents(id string) ([]beads.IssueDep, error)
	// Targets returns issue's dependency targets as stored, an external
	// form included.
	Targets(issue string) ([]string, error)
}

// specSplitRelations are the relation types bd stores, blocking first. The raw
// dependents statement selects the id only, so the type is read one relation at
// a time; a dependent whose relation is none of these refuses the apply rather
// than moving with a relation it does not hold.
var specSplitRelations = []string{
	"blocks", "conditional-blocks", "waits-for", "merge-blocks",
	"tracks", "parent-child", "related", "discovered-from", "thread",
}

// specSplitSQLDeps is specSplitRawDeps over a store that runs bd sql.
type specSplitSQLDeps struct{ sql sqlStore }

// Dependents returns one entry per dependent and relation. The unfiltered read
// says whether any dependent exists, so a bead with none costs one statement;
// when some exist, each relation is read in turn to name its type, and a
// dependent none of them classifies refuses rather than moving unclassified.
func (s specSplitSQLDeps) Dependents(id string) ([]beads.IssueDep, error) {
	all, err := s.column(beadsql.RawDeps(id, "up", ""), "issue_id")
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	found := make(map[string]bool, len(all))
	seen := make(map[[2]string]bool, len(all))
	var deps []beads.IssueDep
	for _, rel := range specSplitRelations {
		ids, err := s.column(beadsql.RawDeps(id, "up", rel), "issue_id")
		if err != nil {
			return nil, err
		}
		for _, depID := range ids {
			found[depID] = true
			if seen[[2]string{depID, rel}] {
				continue
			}
			seen[[2]string{depID, rel}] = true
			deps = append(deps, beads.IssueDep{ID: depID, DependencyType: rel})
		}
	}
	for _, depID := range all {
		if !found[depID] {
			return nil, fmt.Errorf("dependent %s has a relation this split does not know, so it cannot be moved keeping its relation", depID)
		}
	}
	return deps, nil
}

// Targets returns issue's dependency targets as stored, an external form
// included.
func (s specSplitSQLDeps) Targets(issue string) ([]string, error) {
	return s.column(beadsql.RawDeps(issue, "down", ""), "depends_on_id")
}

// column reads col from a bd sql --csv answer: a header row, then one row per
// dependency. A missing column refuses the read.
func (s specSplitSQLDeps) column(q beadsql.Query, col string) ([]string, error) {
	rows, err := s.sql.SQLCSV(q)
	if err != nil {
		return nil, fmt.Errorf("reading the dependency table: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	at := -1
	for i, name := range rows[0] {
		if name == col {
			at = i
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("reading the dependency table: no %s column in %q", col, rows[0])
	}
	var out []string
	for _, row := range rows[1:] {
		if at < len(row) && row[at] != "" {
			out = append(out, row[at])
		}
	}
	return out, nil
}

// specSplitDependents returns every dependent of beadID and its relation: the
// ones the store lists, then the ones its list leaves out, read raw when raw is
// not nil. A dependent and relation the store already listed is not repeated.
func specSplitDependents(db beads.Client, raw specSplitRawDeps, rig, beadID string) ([]beads.IssueDep, error) {
	dependents, err := db.DepListUp(beadID, "")
	if err != nil {
		return nil, fmt.Errorf("cannot read the dependents of %s: %w", beadID, err)
	}
	if raw == nil {
		return dependents, nil
	}
	extra, err := raw.Dependents(beadID)
	if err != nil {
		return nil, fmt.Errorf("cannot read the dependents of %s: %w", beadID, err)
	}
	type relation struct{ id, dep string }
	seen := make(map[relation]bool, len(dependents))
	for _, dep := range dependents {
		seen[relation{dep.ID, dep.DependencyType}] = true
	}
	listed := make(map[string]bool, len(dependents))
	for _, dep := range dependents {
		listed[dep.ID] = true
	}
	// The raw read matches an external edge by its id alone, so a dependent
	// only the raw read finds is kept when one of its stored targets names the
	// original, and left alone when the edge names another rig's bead.
	named := make(map[string]bool)
	for _, dep := range extra {
		key := relation{dep.ID, dep.DependencyType}
		if seen[key] {
			continue
		}
		if !listed[dep.ID] {
			ok, checked := named[dep.ID]
			if !checked {
				refs, err := raw.Targets(dep.ID)
				if err != nil {
					return nil, fmt.Errorf("reading the dependencies of %s: %w", dep.ID, err)
				}
				for _, ref := range refs {
					ok = ok || specSplitNamesOriginal(ref, rig, beadID)
				}
				named[dep.ID] = ok
			}
			if !ok {
				continue
			}
		}
		seen[key] = true
		dependents = append(dependents, dep)
	}
	return dependents, nil
}

// splitMoved is a dependent a failed apply puts back on the original.
type splitMoved struct {
	id   string
	rels []string
}

// specSplitMoveDependents moves each dependent off beadID and onto piece B,
// keeping its relation. Every relation a dependent holds gets its B edge before
// any edge to the original is removed, so a failed add, on the first relation or
// a later one, leaves the dependent on the still-open original rather than on
// nothing. The dependents whose B edges are in place are returned with their
// relations, so the undo puts them back and a failure names them.
func specSplitMoveDependents(db beads.Client, raw specSplitRawDeps, rig, beadID, bID string, dependents []beads.IssueDep) ([]splitMoved, error) {
	var order []string
	relations := make(map[string][]string, len(dependents))
	for _, dep := range dependents {
		if _, ok := relations[dep.ID]; !ok {
			order = append(order, dep.ID)
		}
		relations[dep.ID] = append(relations[dep.ID], dep.DependencyType)
	}
	var moved []splitMoved
	for _, id := range order {
		for _, rel := range relations[id] {
			if err := db.AddTypedDependency(id, bID, rel); err != nil {
				return moved, fmt.Errorf("moving dependent %s onto piece B %s: %w", id, bID, err)
			}
		}
		moved = append(moved, splitMoved{id: id, rels: relations[id]})
		if err := specSplitRemoveEdgesTo(db, raw, rig, id, beadID); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// specSplitUndoOn undoes a failed apply: it puts each moved dependent back on
// the original with the relations it held, before its piece B edges go, so it is
// never left on nothing; then it closes the pieces created, A before B so B is
// no longer blocked by an open A. The original stays open, since only the last
// step closes it. cause comes back naming any undo write that failed, so a
// half-done undo never reads as clean.
func specSplitUndoOn(db beads.Client, created []string, moved []splitMoved, beadID, bID string, cause error) error {
	var errs []error
	for _, m := range moved {
		var adds []error
		for _, rel := range m.rels {
			if err := db.AddTypedDependency(m.id, beadID, rel); err != nil {
				adds = append(adds, fmt.Errorf("restoring %s on %s: %w", m.id, beadID, err))
			}
		}
		errs = append(errs, adds...)
		if len(adds) == 0 {
			if err := db.RemoveDependency(m.id, bID); err != nil {
				errs = append(errs, fmt.Errorf("taking %s off piece B %s: %w", m.id, bID, err))
			}
		}
	}
	if err := db.CloseWithReason("split undone", created...); err != nil {
		errs = append(errs, fmt.Errorf("closing the pieces: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w; the undo failed too: %w", cause, err)
	}
	return cause
}

// specSplitNamesOriginal reports whether a stored dependency target names the
// original: its plain id, or external:<x>:<id> where <x> is the id's own prefix
// or the original's rig. An external edge with another segment names a bead of
// the same id in another rig, which this split does not touch.
func specSplitNamesOriginal(ref, rig, beadID string) bool {
	if ref == beadID {
		return true
	}
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 || parts[0] != "external" || parts[2] != beadID {
		return false
	}
	prefix, _, _ := strings.Cut(beadID, "-")
	return parts[1] == prefix || parts[1] == rig
}

// specSplitRemoveEdgesTo removes issue's edges that name beadID, each in the
// form the table stores it: a plain id, or external:<rig>:<id>. A raw read that
// names none falls back to the plain id, the form bd's own removal takes for an
// edge kept outside that table (a wisp dependent's edge is in
// wisp_dependencies). Without a raw read the plain form is the only one the
// store's own list can hold.
func specSplitRemoveEdgesTo(db beads.Client, raw specSplitRawDeps, rig, issue, beadID string) error {
	refs := []string{beadID}
	if raw != nil {
		var err error
		refs, err = raw.Targets(issue)
		if err != nil {
			return fmt.Errorf("reading the dependencies of %s: %w", issue, err)
		}
	}
	removed := false
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if seen[ref] || !specSplitNamesOriginal(ref, rig, beadID) {
			continue
		}
		seen[ref] = true
		removed = true
		if err := db.RemoveDependency(issue, ref); err != nil {
			return fmt.Errorf("moving dependent %s off %s: %w", issue, ref, err)
		}
	}
	if !removed {
		if err := db.RemoveDependency(issue, beadID); err != nil {
			return fmt.Errorf("moving dependent %s off %s: %w", issue, beadID, err)
		}
	}
	return nil
}

// specSplitPieceOptions is the create of one piece: the original's title with the
// piece name, its priority and rig, and the piece's text with PIECE_A replaced by
// aID when the piece depends on A.
func specSplitPieceOptions(issue *beads.Issue, piece specsplit.Piece, rig, aID string) beads.CreateOptions {
	text := piece.Text
	if aID != "" {
		text = strings.ReplaceAll(text, "PIECE_A", aID)
	}
	return beads.CreateOptions{
		Title:       fmt.Sprintf("%s (piece %s)", issue.Title, piece.Name),
		Description: text,
		Type:        "task",
		Priority:    issue.Priority,
		Rig:         rig,
	}
}

// specSplitJudgeAll estimates and lints each piece's draft, nothing written.
// An estimate that errors refuses the whole split: a piece that could not be
// estimated has no minutes to judge. Without a trigger no draft can be judged,
// so that refuses too.
func specSplitJudgeAll(beadID string, issue *beads.Issue, tree *specdispatch.TargetTree, pieces []specsplit.Piece, check specSplitCheck) ([]specSplitJudgement, error) {
	if check.Trigger <= 0 {
		return nil, fmt.Errorf("cannot judge the drafts of %s: no split trigger is set (story_points.split_over_minutes)", beadID)
	}
	judged := make([]specSplitJudgement, 0, len(pieces))
	for _, piece := range pieces {
		draft := &beads.Issue{
			Title:       fmt.Sprintf("%s (piece %s)", issue.Title, piece.Name),
			Description: piece.Text,
			Type:        "task",
			Status:      "open",
			Priority:    issue.Priority,
		}
		outcome, err := check.Estimate(beadID, draft)
		if err != nil {
			return nil, fmt.Errorf("cannot estimate piece %s of %s: %w", piece.Name, beadID, err)
		}
		spec := specWithEstimateLabels(specFromIssue(draft), outcome)
		spec.Target = tree
		verdict := check.Lint(spec)
		judged = append(judged, specSplitJudgement{
			Piece:   piece,
			Outcome: outcome,
			Verdict: verdict,
			Fail:    specSplitFailure(outcome, verdict, check.Trigger),
		})
	}
	return judged, nil
}

// specSplitFailure is why a piece fails the verdict, "" when it passes. The
// lint is read first, then the estimate. A piece at or within specSplitMargin
// under the trigger fails with the trigger, since its estimate could land over
// it.
func specSplitFailure(o specEstimateOutcome, v specdispatch.LintVerdict, trigger float64) string {
	if !v.Clean() {
		return "lint refused: " + specSplitLintReason(v)
	}
	if o.Action != "estimated" {
		reason := o.Reason
		if reason == "" {
			reason = "the estimate was not made"
		}
		return "no estimate: " + reason
	}
	switch {
	case o.Minutes >= trigger:
		return fmt.Sprintf("estimate %s min is at or over the %s-minute trigger", specMinutesText(o.Minutes), specTriggerText(trigger))
	case o.Minutes >= trigger-specSplitMargin:
		return fmt.Sprintf("estimate %s min is within %s min of the %s-minute trigger", specMinutesText(o.Minutes), specTriggerText(specSplitMargin), specTriggerText(trigger))
	}
	return ""
}

// specSplitLintReason is the first reason a lint verdict gives for refusing.
func specSplitLintReason(v specdispatch.LintVerdict) string {
	if v.Reason != "" {
		return v.Reason
	}
	if len(v.Refusals) > 0 {
		return v.Refusals[0].Reason
	}
	return "the lint did not dispatch the draft"
}

// specSplitPieceLine is one piece's minutes and lint result, as the report
// prints them under the drafts.
func specSplitPieceLine(j specSplitJudgement) string {
	minutes := "estimate none"
	if j.Outcome.Action == "estimated" {
		minutes = fmt.Sprintf("estimate %s min", specMinutesText(j.Outcome.Minutes))
	}
	lint := "lint ok"
	if !j.Verdict.Clean() {
		lint = "lint refused: " + specSplitLintReason(j.Verdict)
	}
	return fmt.Sprintf("piece %s: %s, %s\n", j.Piece.Name, minutes, lint)
}

// specSplitEstimate is the production dry-run estimate of one piece's draft:
// the estimate command's own run over a draft that refuses every write.
func specSplitEstimate(townRoot string, errw io.Writer) func(beadID string, draft *beads.Issue) (specEstimateOutcome, error) {
	return func(beadID string, draft *beads.Issue) (specEstimateOutcome, error) {
		return specEstimateBead(context.Background(), beadID, specSplitDraftStore{draft: draft},
			specEstimateOpts{DryRun: true},
			specEstimateRunner{
				Estimate: estimate.Estimate,
				Deps: func() (estimate.Deps, error) {
					return specEstimateDeps(townRoot, specBeadRig(townRoot, beadID), errw)
				},
			})
	}
}

// errSplitDryRunWrites is the write a dry-run draft refuses.
var errSplitDryRunWrites = errors.New("dry run writes nothing")

// specSplitDraftStore reads one draft as the estimate's bead. Both writes are
// refused, so the dry run cannot reach bd through the estimate.
type specSplitDraftStore struct {
	draft *beads.Issue
}

func (s specSplitDraftStore) Show(string) (*beads.Issue, error) { return s.draft, nil }

func (specSplitDraftStore) Comments(string) ([]beads.Comment, error) { return nil, nil }

func (specSplitDraftStore) Update(string, beads.UpdateOptions) error { return errSplitDryRunWrites }

func (specSplitDraftStore) AddComment(string, string) error { return errSplitDryRunWrites }

// specSplitLintCheck is the production lint of one piece's draft: the spec
// lint's own options and template, with the bead's rig and the town's rigs.
// The lint is LintWith, not specLintVerdict, which prints and records the
// outcome. A settings or rig list that cannot be read is an error here, so no
// draft is judged by a lint that did not load.
func specSplitLintCheck(townRoot, beadID string, sd *config.SpecDispatchConfig, errw io.Writer) (func(specdispatch.Spec) specdispatch.LintVerdict, error) {
	opts, err := specLintOptsFor(sd, townRoot, false, false, errw)
	if err != nil {
		return nil, err
	}
	rigs, err := specTownRigs(townRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot read the town's rig list: %w", err)
	}
	tmpl := specdispatch.LoadTemplate(specTemplatePath(sd))
	rig := specBeadRig(townRoot, beadID)
	return func(spec specdispatch.Spec) specdispatch.LintVerdict {
		spec.Rig = rig
		spec.TownRigs = rigs
		return specdispatch.LintWith(spec, tmpl, opts.lintOptions())
	}, nil
}

// specSplitPackageDirs is the directory of every Go file on the target branch:
// the module's package directories, as the planner reads them.
func specSplitPackageDirs(files map[string]struct{}) []string {
	seen := map[string]bool{}
	var dirs []string
	for file := range files {
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		dir := path.Dir(file)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// specSplitBaseFiles is every path on the target branch, so the planner does
// not say a piece creates a file that already exists.
func specSplitBaseFiles(files map[string]struct{}) map[string]bool {
	base := make(map[string]bool, len(files))
	for file := range files {
		base[file] = true
	}
	return base
}
