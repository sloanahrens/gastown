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

var specSplitCmd = &cobra.Command{
	Use:   "split <bead-id>",
	Short: "Print the two drafts a bead splits into, creating nothing",
	Long: `Split a work bead into two pieces by package and print both drafts.

Each Constraints bullet and acceptance box is placed in the package directory it
names. The packages are divided between piece A and piece B, which depends on
A. Each draft is the full bead text that piece would carry.

The packages are read from the rig's target branch. A bead that cannot be
split is refused with the reason and exits non-zero.

Each draft is then estimated and linted, and nothing is written. The verdict
line says whether both pieces lint ok and estimate under the split trigger. A
piece at or within 0.3 minutes of the trigger fails, and so does a piece the
lint refuses; the command exits 1 when either fails.

Only --dry-run is available: without it the command refuses. A dry run writes
nothing and creates no bead.`,
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
	return runSpecSplitFrom(cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], specSplitDryRun, workspace.FindFromCwd)
}

// runSpecSplitFrom resolves the town with find, reads its settings, and runs
// runSpecSplitOn with the production estimate and lint. find is a parameter so
// a test can make the town unfindable. errw takes the estimate's warnings.
func runSpecSplitFrom(out, errw io.Writer, beadID string, dryRun bool, find func() (string, error)) error {
	townRoot, err := find()
	if err != nil {
		return fmt.Errorf("cannot find the town: %w", err)
	}
	sd, err := readSpecDispatchConfig(townRoot)
	if err != nil {
		return fmt.Errorf("cannot read patrols.spec_dispatch: %w", err)
	}
	lint, err := specSplitLintCheck(townRoot, beadID, sd, errw)
	if err != nil {
		return fmt.Errorf("cannot judge the drafts of %s: %w", beadID, err)
	}
	return runSpecSplitOn(out, beadID, dryRun,
		func(id string) (*beads.Issue, error) { return showBead(townRoot, id) },
		func(id string) *specdispatch.TargetTree { return specLintTarget(townRoot, id, openRigTree) },
		specSplitCheck{
			Trigger:  sd.StoryPointsSplitOver(),
			Estimate: specSplitEstimate(townRoot, errw),
			Lint:     lint,
		})
}

// runSpecSplitOn prints the two piece drafts of beadID to out, then each
// piece's minutes and lint, then the verdict. A failed verdict returns a silent
// exit of 1 after the report. The read, the target inventory and the check are
// given, so a test drives it through fakes. Every refusal returns before
// anything is printed: the drafts, their estimates and their lints are all made
// first.
func runSpecSplitOn(out io.Writer, beadID string, dryRun bool, read specSplitRead, target specSplitTarget, check specSplitCheck) error {
	if !dryRun {
		return errors.New("gt spec split writes nothing yet: pass --dry-run to print the two drafts")
	}
	if check.Trigger <= 0 {
		return fmt.Errorf("cannot judge the drafts of %s: no split trigger is set (story_points.split_over_minutes)", beadID)
	}
	issue, err := read(beadID)
	if err != nil {
		return fmt.Errorf("cannot read bead %s: %w", beadID, err)
	}
	tree := target(beadID)
	if tree == nil {
		return fmt.Errorf("cannot read the target branch's files for %s", beadID)
	}
	if tree.Err != "" {
		return fmt.Errorf("cannot read the target branch's files for %s: %s", beadID, tree.Err)
	}
	spec := specsplit.Spec{
		Constraints: specdispatch.Sections(issue.Description)["constraints"],
		Acceptance:  beads.AcceptanceText(issue),
		Dirs:        specSplitPackageDirs(tree.Files),
		BaseFiles:   specSplitBaseFiles(tree.Files),
	}
	groups, err := specsplit.Group(spec)
	if err != nil {
		return fmt.Errorf("cannot split %s: %w", beadID, err)
	}
	plan, err := specsplit.Plan(spec, groups)
	if err != nil {
		return fmt.Errorf("cannot split %s: %w", beadID, err)
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

// specSplitJudgeAll estimates and lints each piece's draft, nothing written.
// An estimate that errors refuses the whole split: a piece that could not be
// estimated has no minutes to judge.
func specSplitJudgeAll(beadID string, issue *beads.Issue, tree *specdispatch.TargetTree, pieces []specsplit.Piece, check specSplitCheck) ([]specSplitJudgement, error) {
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
