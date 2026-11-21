package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// splitBeadDescription is a bead whose Constraints name two packages and whose
// acceptance boxes each test one of them.
const splitBeadDescription = `## Goal

Split two packages.

## Constraints

- Add the parser to internal/alpha/alpha.go.
- Wire the reader in internal/beta/beta.go.

## Acceptance

- [ ] The parser works. Run: go test ./internal/alpha -run TestParse
- [ ] The reader works. Run: go test ./internal/beta -run TestRead
`

// splitOnePackageDescription names one package only, so there is nothing to
// split.
const splitOnePackageDescription = `## Goal

One package.

## Constraints

- Add the parser to internal/alpha/alpha.go.

## Acceptance

- [ ] The parser works. Run: go test ./internal/alpha -run TestParse
- [ ] The lexer works. Run: go test ./internal/alpha -run TestLex
`

// splitUnboxedPackageDescription names beta in a bullet but tests only alpha,
// so beta's file group has no acceptance box for its piece to carry.
const splitUnboxedPackageDescription = `## Goal

Split two packages, one untested.

## Constraints

- Add the parser to internal/alpha/alpha.go.
- Wire the reader in internal/beta/beta.go.

## Acceptance

- [ ] The parser works. Run: go test ./internal/alpha -run TestParse
`

// splitTree is the target branch's inventory the split tests plan against.
func splitTree() *specdispatch.TargetTree {
	return &specdispatch.TargetTree{
		Branch: "main",
		Files: map[string]struct{}{
			"go.mod":                       {},
			"internal/alpha/alpha.go":      {},
			"internal/alpha/alpha_test.go": {},
			"internal/beta/beta.go":        {},
		},
	}
}

// splitJournalLen is the number of journal records the fake has kept, which
// grows with every write.
func splitJournalLen(t *testing.T, db *beadsfake.Fake) int {
	t.Helper()
	page, err := db.EventsTail(0, 0)
	if err != nil {
		t.Fatalf("events tail: %v", err)
	}
	return len(page.Records)
}

// TestSpecSplitDryRunPrints: a dry run of a two-package bead prints piece A and
// piece B, each with its own package and acceptance box, and B names that it
// depends on A. The dry run appends no journal record, so it creates and
// updates no bead.
func TestSpecSplitDryRunPrints(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	before := splitJournalLen(t, db)

	var out bytes.Buffer
	err := runSpecSplitOn(&out, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck())
	if err != nil {
		t.Fatalf("runSpecSplitOn() error = %v, want none", err)
	}
	got := out.String()
	for _, want := range []string{
		"== piece A: internal/alpha ==",
		"== piece B: internal/beta (depends on piece A) ==",
		"- [ ] The parser works. Run: go test ./internal/alpha -run TestParse",
		"- [ ] The reader works. Run: go test ./internal/beta -run TestRead",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if after := splitJournalLen(t, db); after != before {
		t.Errorf("journal records = %d after the dry run, want %d", after, before)
	}
}

// TestSpecSplitRefusesBeforePrinting: every refusal returns an error and prints
// nothing, so a caller never reads a half draft as a split.
func TestSpecSplitRefusesBeforePrinting(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name        string
		dryRun      bool
		beadID      string
		description string
		tree        *specdispatch.TargetTree
	}{
		{name: "without --dry-run", dryRun: false, beadID: "gt-split", description: splitBeadDescription, tree: splitTree()},
		{name: "bead not found", dryRun: true, beadID: "gt-missing", description: splitBeadDescription, tree: splitTree()},
		{name: "no rig holds the bead", dryRun: true, beadID: "gt-split", description: splitBeadDescription, tree: nil},
		{name: "target tree unreadable", dryRun: true, beadID: "gt-split", description: splitBeadDescription, tree: &specdispatch.TargetTree{Branch: "main", Files: splitTree().Files, Err: "cannot read main"}},
		{name: "one package only", dryRun: true, beadID: "gt-split", description: splitOnePackageDescription, tree: splitTree()},
		{name: "package with a bullet and no box", dryRun: true, beadID: "gt-split", description: splitUnboxedPackageDescription, tree: splitTree()},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: row.description})

			var out bytes.Buffer
			err := runSpecSplitOn(&out, row.beadID, row.dryRun, db.Show, func(string) *specdispatch.TargetTree { return row.tree }, splitPassingCheck())
			if err == nil {
				t.Errorf("runSpecSplitOn() error = nil, want a refusal")
			}
			if out.Len() != 0 {
				t.Errorf("output on refusal = %q, want none", out.String())
			}
		})
	}
}

// TestSpecSplitIsRegisteredUnderSpec: gt spec split is reachable from the root
// command, so the dry run is what a user runs.
func TestSpecSplitIsRegisteredUnderSpec(t *testing.T) {
	t.Parallel()
	cmd, _, err := rootCmd.Find([]string{"spec", "split"})
	if err != nil {
		t.Fatalf("Find(spec split) error = %v", err)
	}
	if cmd.Name() != "split" || cmd.Parent() == nil || cmd.Parent().Name() != "spec" {
		t.Errorf("Find(spec split) = %q under %v, want split under spec", cmd.Name(), cmd.Parent())
	}
}

// splitRefusingWriter is an output that refuses every write.
type splitRefusingWriter struct{}

func (splitRefusingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// TestSpecSplitReportsAFailedWrite: a draft that cannot be written to the
// output is an error, not a silent partial print.
func TestSpecSplitReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})

	err := runSpecSplitOn(splitRefusingWriter{}, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck())
	if err == nil {
		t.Errorf("runSpecSplitOn() error = nil on a refused write, want an error")
	}
}

// TestSpecSplitRefusesOutsideATown: when no town can be found, the command
// refuses with an error and prints nothing.
func TestSpecSplitRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	noTown := func() (string, error) { return "", errors.New("no town above the directory") }

	err := runSpecSplitFrom(&out, &bytes.Buffer{}, "gt-split", true, noTown)
	if err == nil {
		t.Errorf("runSpecSplitFrom() error = nil with no town, want an error")
	}
	if out.Len() != 0 {
		t.Errorf("runSpecSplitFrom() printed %q before refusing, want nothing", out.String())
	}
}

// TestSpecSplitRefusesAnUnjudgeableTown: a town whose daemon config, lint
// settings or rig list cannot be read refuses the dry run before any draft is
// printed. The town is a temporary directory, so no bead or host file is read.
func TestSpecSplitRefusesAnUnjudgeableTown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string]string // town-relative path to the file's content
		wantErr string            // the refusal names the unreadable setting
	}{
		{name: "daemon config does not parse", files: map[string]string{"mayor/daemon.json": "{not json"}, wantErr: "cannot read patrols.spec_dispatch"},
		{name: "lint budget is negative", files: map[string]string{"mayor/daemon.json": `{"patrols":{"spec_dispatch":{"story_points":{"lint_budget_max_lines":-5}}}}`}, wantErr: "lint_budget_max_lines -5"},
		{name: "rig list does not parse", files: map[string]string{"mayor/rigs.json": "{not json"}, wantErr: "cannot read the town's rig list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			for rel, content := range tc.files {
				path := filepath.Join(town, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir for %s: %v", rel, err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatalf("write %s: %v", rel, err)
				}
			}

			var out bytes.Buffer
			err := runSpecSplitFrom(&out, &bytes.Buffer{}, "gt-split", true, func() (string, error) { return town, nil })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("runSpecSplitFrom() error = %v, want a refusal naming %q", err, tc.wantErr)
			}
			if out.Len() != 0 {
				t.Errorf("output on refusal = %q, want none", out.String())
			}
		})
	}
}

// splitPieceFake is one piece's estimate and lint as the fakes answer them.
type splitPieceFake struct {
	minutes float64
	// estimateNone is the reason the estimate made no minutes; "" when it did.
	estimateNone string
	// lintRefusal is the reason the lint refused the draft; "" when it passed.
	lintRefusal string
}

// splitCheckFor is a check whose fakes answer for each piece by name. A draft
// whose piece is not in pieces is an error, so a test never judges a piece it
// did not set up.
func splitCheckFor(trigger float64, pieces map[string]splitPieceFake) specSplitCheck {
	return specSplitCheck{
		Trigger: trigger,
		Estimate: func(_ string, draft *beads.Issue) (specEstimateOutcome, error) {
			fake, ok := pieces[splitPieceName(draft.Title)]
			if !ok {
				return specEstimateOutcome{}, errors.New("no fake for " + draft.Title)
			}
			if fake.estimateNone != "" {
				return specEstimateOutcome{ID: "gt-split", Action: "none", Rung: "none", Reason: fake.estimateNone}, nil
			}
			return specEstimateOutcome{ID: "gt-split", Action: "estimated", Rung: "shape", Minutes: fake.minutes}, nil
		},
		Lint: func(draft specdispatch.Spec) specdispatch.LintVerdict {
			fake := pieces[splitPieceName(draft.Title)]
			if fake.lintRefusal != "" {
				return specdispatch.LintVerdict{Route: specdispatch.RouteRefuse, Reason: fake.lintRefusal}
			}
			return specdispatch.LintVerdict{Route: specdispatch.RouteDispatch}
		},
	}
}

// splitPieceName is the piece a draft title names, "" when it names none.
func splitPieceName(title string) string {
	for _, name := range []string{"A", "B"} {
		if strings.HasSuffix(title, "(piece "+name+")") {
			return name
		}
	}
	return ""
}

// splitPassingCheck judges both pieces under a 10-minute trigger, each lint
// clean.
func splitPassingCheck() specSplitCheck {
	return splitCheckFor(10, map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 7}})
}

// TestSpecSplitVerdictPasses: two drafts under the trigger print each piece's
// minutes and lint result and the verdict line, and exit 0. B at 9.6 is 0.4
// under a 10-minute trigger, just outside the margin, so it passes.
func TestSpecSplitVerdictPasses(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	check := splitCheckFor(10, map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 9.6}})

	var out bytes.Buffer
	err := runSpecSplitOn(&out, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, check)
	if err != nil {
		t.Fatalf("runSpecSplitOn() error = %v, want none", err)
	}
	got := out.String()
	for _, want := range []string{
		"piece A: estimate 6.0 min, lint ok",
		"piece B: estimate 9.6 min, lint ok",
		"verdict: both pieces lint ok and estimate under 10 min",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// TestSpecSplitVerdictFails: a piece at the trigger, a piece within 0.3 under
// it (9.7 is exactly 0.3 under), a piece over it, a piece the lint refuses and
// a piece with no estimate each fail the verdict naming the piece and the
// reason, exit 1, and the drafts are still printed.
func TestSpecSplitVerdictFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		pieces map[string]splitPieceFake
		want   string
	}{
		{
			name:   "piece B at the trigger",
			pieces: map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 10}},
			want:   "piece B: estimate 10.0 min is at or over the 10-minute trigger",
		},
		{
			name:   "piece B over the trigger",
			pieces: map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 10.4}},
			want:   "piece B: estimate 10.4 min is at or over the 10-minute trigger",
		},
		{
			name:   "piece B within 0.3 under the trigger",
			pieces: map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 9.7}},
			want:   "piece B: estimate 9.7 min is within 0.3 min of the 10-minute trigger",
		},
		{
			name:   "piece A fails lint",
			pieces: map[string]splitPieceFake{"A": {minutes: 6, lintRefusal: "acceptance box names make lint"}, "B": {minutes: 7}},
			want:   "piece A: lint refused: acceptance box names make lint",
		},
		{
			name:   "piece B has no estimate",
			pieces: map[string]splitPieceFake{"A": {minutes: 6}, "B": {estimateNone: "no text"}},
			want:   "piece B: no estimate: no text",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})

			var out bytes.Buffer
			err := runSpecSplitOn(&out, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, splitCheckFor(10, tc.pieces))
			if code, ok := IsSilentExit(err); !ok || code != 1 {
				t.Fatalf("runSpecSplitOn() error = %v, want a silent exit of 1", err)
			}
			got := out.String()
			if !strings.Contains(got, "== piece A") || !strings.Contains(got, "== piece B") {
				t.Errorf("output lacks the drafts:\n%s", got)
			}
			if want := "verdict: fails: " + tc.want; !strings.Contains(got, want) {
				t.Errorf("output lacks %q:\n%s", want, got)
			}
		})
	}
}

// TestSpecSplitVerdictRefuses: a split the verdict cannot judge is refused
// before anything is printed: no split trigger, or an estimate that errors.
func TestSpecSplitVerdictRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		check func() specSplitCheck
	}{
		{
			name: "no split trigger",
			check: func() specSplitCheck {
				return splitCheckFor(0, map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 7}})
			},
		},
		{
			name: "an estimate that errors",
			check: func() specSplitCheck {
				check := splitPassingCheck()
				check.Estimate = func(string, *beads.Issue) (specEstimateOutcome, error) {
					return specEstimateOutcome{}, errors.New("embedder unreachable")
				}
				return check
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})

			var out bytes.Buffer
			err := runSpecSplitOn(&out, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, tc.check())
			if err == nil {
				t.Errorf("runSpecSplitOn() error = nil, want a refusal")
			}
			if out.Len() != 0 {
				t.Errorf("output on refusal = %q, want none", out.String())
			}
		})
	}
}
