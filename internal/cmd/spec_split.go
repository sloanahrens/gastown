package cmd

import (
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/specsplit"
	"github.com/steveyegge/gastown/internal/workspace"
)

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

func runSpecSplit(cmd *cobra.Command, args []string) error {
	return runSpecSplitFrom(cmd.OutOrStdout(), args[0], specSplitDryRun, workspace.FindFromCwd)
}

// runSpecSplitFrom resolves the town with find, then runs runSpecSplitOn on it.
// find is a parameter so a test can make the town unfindable.
func runSpecSplitFrom(out io.Writer, beadID string, dryRun bool, find func() (string, error)) error {
	townRoot, err := find()
	if err != nil {
		return fmt.Errorf("cannot find the town: %w", err)
	}
	return runSpecSplitOn(out, beadID, dryRun,
		func(id string) (*beads.Issue, error) { return showBead(townRoot, id) },
		func(id string) *specdispatch.TargetTree { return specLintTarget(townRoot, id, openRigTree) })
}

// runSpecSplitOn prints the two piece drafts of beadID to out. It is
// runSpecSplit with the flag, the bead read and the target inventory given, so
// a test drives it through fakes. Every refusal returns before anything is
// printed.
func runSpecSplitOn(out io.Writer, beadID string, dryRun bool, read specSplitRead, target specSplitTarget) error {
	if !dryRun {
		return errors.New("gt spec split writes nothing yet: pass --dry-run to print the two drafts")
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
	// The drafts are built whole and written once, so a failed write reaches
	// the caller instead of leaving half a draft on out.
	var drafts strings.Builder
	for i, piece := range []specsplit.Piece{plan.A, plan.B} {
		if i > 0 {
			drafts.WriteString("\n")
		}
		header := fmt.Sprintf("== piece %s: %s", piece.Name, strings.Join(piece.Packages, ", "))
		if piece.DependsOn != "" {
			header += fmt.Sprintf(" (depends on piece %s)", piece.DependsOn)
		}
		drafts.WriteString(header + " ==\n")
		drafts.WriteString(piece.Text)
	}
	if _, err := io.WriteString(out, drafts.String()); err != nil {
		return fmt.Errorf("cannot print the drafts for %s: %w", beadID, err)
	}
	return nil
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
