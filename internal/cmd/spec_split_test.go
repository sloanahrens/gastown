package cmd

import (
	"bytes"
	"errors"
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
	err := runSpecSplitOn(&out, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() })
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
			err := runSpecSplitOn(&out, row.beadID, row.dryRun, db.Show, func(string) *specdispatch.TargetTree { return row.tree })
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

	err := runSpecSplitOn(splitRefusingWriter{}, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() })
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

	err := runSpecSplitFrom(&out, "gt-split", true, noTown)
	if err == nil {
		t.Errorf("runSpecSplitFrom() error = nil with no town, want an error")
	}
	if out.Len() != 0 {
		t.Errorf("runSpecSplitFrom() printed %q before refusing, want nothing", out.String())
	}
}
