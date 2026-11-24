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
			if !row.dryRun {
				return
			}
			// The apply refuses a split the dry run refuses, writing nothing.
			before := splitJournalLen(t, db)
			err = runSpecSplitApplyOn(&out, row.beadID, "gastown", db.Show, func(string) *specdispatch.TargetTree { return row.tree }, splitPassingCheck(), db, nil)
			if err == nil {
				t.Errorf("runSpecSplitApplyOn() error = nil, want a refusal")
			}
			if after := splitJournalLen(t, db); after != before {
				t.Errorf("journal records = %d after the refused apply, want %d", after, before)
			}
		})
	}
}

// TestSpecSplitDryRunRefusal: a planner refusal is reported by its reason and
// exits 1. Nothing is printed, and the fake's journal shows no bead created or
// updated. The two rows are the planner's stages, Plan and Group.
func TestSpecSplitDryRunRefusal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		reason      string
	}{
		{
			name:        "plan refuses a group with a bullet and no box",
			description: splitUnboxedPackageDescription,
			reason:      "file group internal/beta has no acceptance boxes, so its piece would have no tests",
		},
		{
			name:        "group refuses a one-package bead",
			description: splitOnePackageDescription,
			reason:      "spec has only the one file group internal/alpha, so it cannot be split",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: tc.description})
			before := splitJournalLen(t, db)

			var out bytes.Buffer
			err := runSpecSplitOn(&out, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck())
			if err == nil {
				t.Fatalf("runSpecSplitOn() error = nil, want a planner refusal")
			}
			if code := exitCodeForError(err); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %q, want the planner's reason %q", err, tc.reason)
			}
			if out.Len() != 0 {
				t.Errorf("output on refusal = %q, want none", out.String())
			}
			if after := splitJournalLen(t, db); after != before {
				t.Errorf("journal records = %d after the refusal, want %d", after, before)
			}
		})
	}
}

// splitLinkedBeadDescription is splitBeadDescription whose piece B bullet cites
// a symbol piece A declares, so piece B's text carries PIECE_A.
const splitLinkedBeadDescription = `## Goal

Split two packages.

## Constraints

- Add ` + "`func Parse`" + ` to internal/alpha/alpha.go.
- Wire the reader in internal/beta/beta.go, calling ` + "`alpha.Parse`" + `.

## Acceptance

- [ ] The parser works. Run: go test ./internal/alpha -run TestParse
- [ ] The reader works. Run: go test ./internal/beta -run TestRead
`

// splitCreatedPiece is the created piece named name among issues.
func splitCreatedPiece(t *testing.T, issues []*beads.Issue, name string) *beads.Issue {
	t.Helper()
	for _, issue := range issues {
		if strings.HasSuffix(issue.Title, "(piece "+name+")") {
			return issue
		}
	}
	t.Fatalf("no piece %s among %d issues", name, len(issues))
	return nil
}

// TestSpecSplitApplyCreates: --apply creates piece A and piece B, replaces
// PIECE_A in B's text with A's id, wires B to depend on A, and closes the
// original naming both ids. Both pieces keep the original's priority.
func TestSpecSplitApplyCreates(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitLinkedBeadDescription, Priority: 1})

	var out bytes.Buffer
	err := runSpecSplitApplyOn(&out, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), db, nil)
	if err != nil {
		t.Fatalf("runSpecSplitApplyOn() error = %v, want none", err)
	}
	issues, err := db.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(issues) != 3 {
		t.Errorf("issues = %d after the apply, want the original and two pieces", len(issues))
	}
	a, b := splitCreatedPiece(t, issues, "A"), splitCreatedPiece(t, issues, "B")
	for _, piece := range []*beads.Issue{a, b} {
		if piece.Priority != 1 {
			t.Errorf("%s priority = %d, want the original's 1", piece.Title, piece.Priority)
		}
	}
	if strings.Contains(b.Description, "PIECE_A") || !strings.Contains(b.Description, "created by bead "+a.ID) {
		t.Errorf("piece B text has not replaced PIECE_A with %s:\n%s", a.ID, b.Description)
	}
	if len(b.Dependencies) != 1 || b.Dependencies[0].ID != a.ID || b.Dependencies[0].DependencyType != "blocks" {
		t.Errorf("piece B dependencies = %+v, want A %s alone", b.Dependencies, a.ID)
	}
	original, err := db.Show("gt-split")
	if err != nil {
		t.Fatalf("show original: %v", err)
	}
	if original.Status != "closed" {
		t.Errorf("original status = %q, want closed", original.Status)
	}
	if want := "split into " + a.ID + " and " + b.ID; original.CloseReason != want {
		t.Errorf("original close reason = %q, want %q", original.CloseReason, want)
	}
	if want := "split gt-split into " + a.ID + " and " + b.ID; !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want it to name the split %q", out.String(), want)
	}
}

// splitFailingClient is a client whose named write refuses.
type splitFailingClient struct {
	beads.Client
	refuseCreate int // the create number that refuses; 0 for none
	refuseDepend bool
	refuseClose  bool
	creates      int
}

func (c *splitFailingClient) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	c.creates++
	if c.refuseCreate == c.creates {
		return nil, errors.New("create refused")
	}
	return c.Client.Create(opts)
}

func (c *splitFailingClient) AddDependency(issue, dependsOn string) error {
	if c.refuseDepend {
		return errors.New("dependency refused")
	}
	return c.Client.AddDependency(issue, dependsOn)
}

func (c *splitFailingClient) CloseWithReason(reason string, ids ...string) error {
	if c.refuseClose {
		return errors.New("close refused")
	}
	return c.Client.CloseWithReason(reason, ids...)
}

// TestSpecSplitApplyNamesAFailedWrite: a write that fails after an earlier one
// returns an error naming what exists, never a silent partial apply.
func TestSpecSplitApplyNamesAFailedWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fail splitFailingClient
		want []string
	}{
		{name: "piece A", fail: splitFailingClient{refuseCreate: 1}, want: []string{"cannot create piece A of gt-split", "create refused"}},
		{name: "piece B after A", fail: splitFailingClient{refuseCreate: 2}, want: []string{"created piece A as", "cannot create piece B of gt-split"}},
		{name: "the dependency", fail: splitFailingClient{refuseDepend: true}, want: []string{"created piece A as", "piece B as", "cannot wire B after A"}},
		{name: "the close", fail: splitFailingClient{refuseClose: true}, want: []string{"created piece A as", "piece B as", "cannot close gt-split"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
			failing := tc.fail
			failing.Client = db

			var out bytes.Buffer
			err := runSpecSplitApplyOn(&out, "gt-split", "gastown", db.Show,
				func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), &failing, nil)
			if err == nil {
				t.Fatalf("runSpecSplitApplyOn() error = nil, want the failed write reported")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestSpecSplitApplyReportsAFailedOutput: a split whose final report cannot be
// written is an error naming the report, not a silent success.
func TestSpecSplitApplyReportsAFailedOutput(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})

	err := runSpecSplitApplyOn(splitRefusingWriter{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), db, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot report the split of gt-split") {
		t.Errorf("runSpecSplitApplyOn() error = %v on a refused report, want the report failure", err)
	}
}

// TestSpecSplitApplyStops: --apply runs the dry-run checks before its first
// write, so a failed check prints its reason, exits 1 and creates nothing.
func TestSpecSplitApplyStops(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		check specSplitCheck
		want  string
	}{
		{"piece B over the trigger", splitCheckFor(10, map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 10}}),
			"verdict: fails: piece B: estimate 10.0 min is at or over the 10-minute trigger"},
		{"no split trigger", splitCheckFor(0, map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 7}}),
			"no split trigger is set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
			before := splitJournalLen(t, db)

			var out bytes.Buffer
			err := runSpecSplitApplyOn(&out, "gt-split", "gastown", db.Show,
				func(string) *specdispatch.TargetTree { return splitTree() }, tc.check, db, nil)
			if code := exitCodeForError(err); err == nil || code != 1 {
				t.Fatalf("runSpecSplitApplyOn() error = %v, want exit 1", err)
			}
			if after := splitJournalLen(t, db); after != before {
				t.Errorf("journal records = %d after the refused apply, want %d", after, before)
			}
			if got := out.String() + err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("refusal = %q, want it to name %q", got, tc.want)
			}
		})
	}
}

// splitApplyFixture is a bead to split and one downstream bead blocked by it.
func splitApplyFixture(t *testing.T) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	if err := db.AddDependency("gt-downstream", "gt-split"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	return db
}

// splitNoDependents is a store whose dependents read fails.
type splitNoDependents struct{ beads.Client }

func (splitNoDependents) DepListUp(string, string) ([]beads.IssueDep, error) {
	return nil, errors.New("dep list refused")
}

// splitNoMove is a store whose dependent-move add fails.
type splitNoMove struct{ beads.Client }

func (splitNoMove) AddTypedDependency(string, string, string) error {
	return errors.New("dep add refused")
}

// splitNoRemove is a store whose dependency removal fails.
type splitNoRemove struct{ beads.Client }

func (splitNoRemove) RemoveDependency(string, string) error { return errors.New("dep remove refused") }

// splitApplyAndShow runs the apply against store, then returns piece B and the
// downstream dependent as db reads them.
func splitApplyAndShow(t *testing.T, store beads.Client, db *beadsfake.Fake, raw specSplitRawDeps) (*beads.Issue, *beads.Issue) {
	t.Helper()
	if err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), store, raw); err != nil {
		t.Fatalf("runSpecSplitApplyOn() error = %v, want none", err)
	}
	issues, err := db.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	dependent, err := db.Show("gt-downstream")
	if err != nil {
		t.Fatalf("show dependent: %v", err)
	}
	return splitCreatedPiece(t, issues, "B"), dependent
}

// splitWantBAlone asserts that dependent depends on piece B at blocks and on
// nothing else.
func splitWantBAlone(t *testing.T, dependent, b *beads.Issue) {
	t.Helper()
	if len(dependent.Dependencies) != 1 || dependent.Dependencies[0].ID != b.ID || dependent.Dependencies[0].DependencyType != "blocks" {
		t.Errorf("dependent dependencies = %+v, want B %s alone at blocks", dependent.Dependencies, b.ID)
	}
}

// TestSpecSplitApplyMovesDependents: --apply moves a dependent of the original
// onto piece B, so it depends on B afterward and no longer on the original.
func TestSpecSplitApplyMovesDependents(t *testing.T) {
	t.Parallel()
	db := splitApplyFixture(t)
	b, dependent := splitApplyAndShow(t, db, db, nil)
	splitWantBAlone(t, dependent, b)
}

// TestSpecSplitApplyMovesEveryRelation: a dependent holding two relations to
// the original gets both on piece B, and both its edges to the original go.
func TestSpecSplitApplyMovesEveryRelation(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	for _, rel := range []string{"blocks", "tracks"} {
		if err := db.AddTypedDependency("gt-downstream", "gt-split", rel); err != nil {
			t.Fatalf("AddTypedDependency(%s): %v", rel, err)
		}
	}
	b, dependent := splitApplyAndShow(t, db, db, nil)
	got := make(map[string]bool, len(dependent.Dependencies))
	for _, dep := range dependent.Dependencies {
		got[dep.ID+" "+dep.DependencyType] = true
	}
	if len(dependent.Dependencies) != 2 {
		t.Errorf("dependent dependencies = %+v, want the two B edges alone", dependent.Dependencies)
	}
	for _, want := range []string{b.ID + " blocks", b.ID + " tracks"} {
		if !got[want] {
			t.Errorf("dependent dependencies = %+v, want %q", dependent.Dependencies, want)
		}
	}
}

// splitRecordingRemove is a store that records each dependency it removes, so
// a test sees the form an external:<rig>:<id> edge is removed in.
type splitRecordingRemove struct {
	beads.Client
	removed []string
}

func (s *splitRecordingRemove) RemoveDependency(issue, dependsOn string) error {
	s.removed = append(s.removed, issue+" off "+dependsOn)
	return s.Client.RemoveDependency(issue, dependsOn)
}

// splitRawScript is a raw dependency read a test scripts: the dependents and
// targets each call answers, or the error it returns.
type splitRawScript struct {
	dependents []beads.IssueDep
	targets    []string
	depErr     error
	targetErr  error
}

// splitListsNone is a store whose own dependents list leaves everything out,
// so only the raw read can find a dependent.
type splitListsNone struct{ beads.Client }

func (splitListsNone) DepListUp(string, string) ([]beads.IssueDep, error) { return nil, nil }

func (s splitRawScript) Dependents(string) ([]beads.IssueDep, error) {
	if s.depErr != nil {
		return nil, s.depErr
	}
	return s.dependents, nil
}

func (s splitRawScript) Targets(string) ([]string, error) {
	if s.targetErr != nil {
		return nil, s.targetErr
	}
	return s.targets, nil
}

// TestSpecSplitApplyMovesAnExternalEdge: a dependent whose edge names the
// original as external:<rig>:<id> gets the edge to piece B, and the edge to the
// original is removed in the form the table stores it.
func TestSpecSplitApplyMovesAnExternalEdge(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	if err := db.AddTypedDependency("gt-downstream", "external:gt:gt-split", "blocks"); err != nil {
		t.Fatalf("AddTypedDependency: %v", err)
	}
	store := &splitRecordingRemove{Client: db}
	raw := splitRawScript{targets: []string{"external:gt:gt-split"}}
	b, dependent := splitApplyAndShow(t, store, db, raw)
	if want := "gt-downstream off external:gt:gt-split"; len(store.removed) != 1 || store.removed[0] != want {
		t.Errorf("removed = %v, want the stored external ref alone (%q)", store.removed, want)
	}
	splitWantBAlone(t, dependent, b)
}

// TestSpecSplitApplyFindsADependentOnlyTheRawReadSees: a dependent the store's
// list leaves out, whose stored edge names the original as external:<prefix>:<id>,
// is found by the raw read, moved onto B, and loses that edge in its stored form.
func TestSpecSplitApplyFindsADependentOnlyTheRawReadSees(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	if err := db.AddTypedDependency("gt-downstream", "external:gt:gt-split", "blocks"); err != nil {
		t.Fatalf("AddTypedDependency: %v", err)
	}
	store := &splitRecordingRemove{Client: splitListsNone{db}}
	raw := splitRawScript{dependents: []beads.IssueDep{{ID: "gt-downstream", DependencyType: "blocks"}}, targets: []string{"external:gt:gt-split"}}
	b, dependent := splitApplyAndShow(t, store, db, raw)
	if want := "gt-downstream off external:gt:gt-split"; len(store.removed) != 1 || store.removed[0] != want {
		t.Errorf("removed = %v, want %q", store.removed, want)
	}
	splitWantBAlone(t, dependent, b)
}

// TestSpecSplitApplyLeavesAnotherRigsEdgeAlone: the raw read matches an
// external edge by its id, so a dependent whose edge is external:om:gt-split
// names another rig's gt-split. It gets no edge to piece B and no edge is removed.
func TestSpecSplitApplyLeavesAnotherRigsEdgeAlone(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	if err := db.AddTypedDependency("gt-downstream", "external:om:gt-split", "blocks"); err != nil {
		t.Fatalf("AddTypedDependency: %v", err)
	}
	store := &splitRecordingRemove{Client: splitListsNone{db}}
	raw := splitRawScript{dependents: []beads.IssueDep{{ID: "gt-downstream", DependencyType: "blocks"}}, targets: []string{"external:om:gt-split"}}
	if err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), store, raw); err != nil {
		t.Fatalf("runSpecSplitApplyOn() error = %v, want none", err)
	}
	dependent, err := db.Show("gt-downstream")
	if err != nil {
		t.Fatalf("show dependent: %v", err)
	}
	if len(dependent.Dependencies) != 0 {
		t.Errorf("dependent dependencies = %+v, want no edge to piece B for another rig's bead", dependent.Dependencies)
	}
	if len(store.removed) != 0 {
		t.Errorf("removed = %v, want no edge removed", store.removed)
	}
}

// TestSpecSplitApplyKeepsAnotherRigsEdgeOnARealDependent: a dependent of the
// original that also holds external:om:gt-split, another rig's bead of the same
// id, is moved off the original alone and keeps that other edge.
func TestSpecSplitApplyKeepsAnotherRigsEdgeOnARealDependent(t *testing.T) {
	t.Parallel()
	db := splitApplyFixture(t)
	store := &splitRecordingRemove{Client: db}
	raw := splitRawScript{targets: []string{"gt-split", "external:om:gt-split"}}
	b, dependent := splitApplyAndShow(t, store, db, raw)
	if want := "gt-downstream off gt-split"; len(store.removed) != 1 || store.removed[0] != want {
		t.Errorf("removed = %v, want only %q", store.removed, want)
	}
	splitWantBAlone(t, dependent, b)
}

// TestSpecSplitApplyReadsDependentsBDLeavesOut: a dependent whose edge bd's own
// list leaves out is read from the dependency table and moved.
func TestSpecSplitApplyReadsDependentsBDLeavesOut(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	db.OnSQL(func(q string) ([][]string, error) {
		switch {
		case strings.Contains(q, "AS depends_on_id"):
			return [][]string{{"depends_on_id"}, {"gt-split"}}, nil
		case strings.Contains(q, "type = 'blocks'"):
			return [][]string{{"issue_id"}, {"gt-downstream"}}, nil
		case strings.Contains(q, "type ="):
			return [][]string{{"issue_id"}}, nil
		default:
			return [][]string{{"issue_id"}, {"gt-downstream"}}, nil
		}
	})
	b, dependent := splitApplyAndShow(t, db, db, specSplitSQLDeps{db})
	splitWantBAlone(t, dependent, b)
}

// splitMoveRefuses is a store whose dependency add fails for one dependent.
type splitMoveRefuses struct {
	beads.Client
	id string
}

func (s splitMoveRefuses) AddTypedDependency(issue, dependsOn, depType string) error {
	if issue == s.id {
		return errors.New("dep add refused")
	}
	return s.Client.AddTypedDependency(issue, dependsOn, depType)
}

// splitRelationRefuses is a store whose dependency add fails for one relation of
// one dependent, so a test drives a failure after that dependent's earlier
// relation already reached piece B.
type splitRelationRefuses struct {
	beads.Client
	id, rel string
}

func (s splitRelationRefuses) AddTypedDependency(issue, dependsOn, depType string) error {
	if issue == s.id && depType == s.rel {
		return errors.New("dep add refused")
	}
	return s.Client.AddTypedDependency(issue, dependsOn, depType)
}

// TestSpecSplitApplyKeepsTheOriginalEdgesWhenALaterRelationFails: a dependent
// holding blocks and tracks whose tracks add fails keeps both edges to the
// original, and the error does not list it as moved.
func TestSpecSplitApplyKeepsTheOriginalEdgesWhenALaterRelationFails(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	mustBead(t, db, beads.CreateOptions{ID: "gt-downstream", Title: "downstream"})
	for _, rel := range []string{"blocks", "tracks"} {
		if err := db.AddTypedDependency("gt-downstream", "gt-split", rel); err != nil {
			t.Fatalf("AddTypedDependency(%s): %v", rel, err)
		}
	}
	err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), splitRelationRefuses{Client: db, id: "gt-downstream", rel: "tracks"}, nil)
	if err == nil || !strings.Contains(err.Error(), "moved: none") {
		t.Fatalf("runSpecSplitApplyOn() error = %v, want a refusal that lists no dependent as moved", err)
	}
	dependent, showErr := db.Show("gt-downstream")
	if showErr != nil {
		t.Fatalf("Show(gt-downstream): %v", showErr)
	}
	got := make(map[string]bool, len(dependent.Dependencies))
	for _, dep := range dependent.Dependencies {
		got[dep.ID+" "+dep.DependencyType] = true
	}
	for _, want := range []string{"gt-split blocks", "gt-split tracks"} {
		if !got[want] {
			t.Errorf("dependent dependencies = %+v, want the original edge %q kept", dependent.Dependencies, want)
		}
	}
}

// TestSpecSplitApplyNamesTheDependentsItMoved: a move that fails partway names
// the dependents already moved and the one that did not.
func TestSpecSplitApplyNamesTheDependentsItMoved(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
	for _, id := range []string{"gt-downstream-1", "gt-downstream-2"} {
		mustBead(t, db, beads.CreateOptions{ID: id, Title: id})
		if err := db.AddDependency(id, "gt-split"); err != nil {
			t.Fatalf("AddDependency(%s): %v", id, err)
		}
	}
	err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), splitMoveRefuses{Client: db, id: "gt-downstream-2"}, nil)
	if err == nil {
		t.Fatalf("runSpecSplitApplyOn() error = nil, want a refusal")
	}
	for _, want := range []string{"moved: gt-downstream-1", "moving dependent gt-downstream-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

// TestSpecSplitUndoRollsBack: a failure at piece B's creation, at the dependency
// or at a dependent move closes the pieces the apply made, restores a moved
// dependent, leaves the original open and exits 1; an undo failure is named.
func TestSpecSplitUndoRollsBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store func(*beadsfake.Fake) beads.Client
		want  string
		undo  string // the undo write that fails; "" for a clean undo
	}{
		{"piece B's creation", func(db *beadsfake.Fake) beads.Client { return &splitFailingClient{Client: db, refuseCreate: 2} }, "cannot create piece B", ""},
		{"the dependency", func(db *beadsfake.Fake) beads.Client { return &splitFailingClient{Client: db, refuseDepend: true} }, "cannot wire B after A", ""},
		{"a dependent move", func(db *beadsfake.Fake) beads.Client { return splitMoveRefuses{Client: db, id: "gt-downstream-2"} }, "moving dependent gt-downstream-2", ""},
		{"a restore add", func(db *beadsfake.Fake) beads.Client {
			return splitMoveRefuses{Client: splitRestoreAddRefuses{db}, id: "gt-downstream-2"}
		}, "moving dependent gt-downstream-2", "restoring gt-downstream on gt-split"},
		{"a restore remove", func(db *beadsfake.Fake) beads.Client { return splitNoRemove{db} }, "moving dependent gt-downstream off gt-split", "taking gt-downstream off piece B"},
		{"the close", func(db *beadsfake.Fake) beads.Client { return &splitFailingClient{Client: db, refuseClose: true} }, "cannot close gt-split", "closing the pieces"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := splitApplyFixture(t)
			mustBead(t, db, beads.CreateOptions{ID: "gt-downstream-2", Title: "gt-downstream-2"})
			if err := db.AddDependency("gt-downstream-2", "gt-split"); err != nil {
				t.Fatalf("AddDependency: %v", err)
			}
			err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
				func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), tc.store(db), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || exitCodeForError(err) != 1 {
				t.Fatalf("runSpecSplitApplyOn() = %v, want exit 1 naming %q", err, tc.want)
			}
			if tc.undo != "" {
				if !strings.Contains(err.Error(), "the undo failed too") || !strings.Contains(err.Error(), tc.undo) {
					t.Fatalf("runSpecSplitApplyOn() = %v, want the undo failure %q", err, tc.undo)
				}
				return
			}
			issues, err := db.List(beads.ListOptions{Status: "all"})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			byID := make(map[string]*beads.Issue, len(issues))
			pieces, closed := 0, 0
			for _, issue := range issues {
				byID[issue.ID] = issue
				if strings.Contains(issue.Title, "(piece ") {
					pieces++
					if issue.Status == "closed" {
						closed++
					}
				}
			}
			if pieces == 0 || closed != pieces {
				t.Errorf("closed pieces = %d of %d after the undo, want every piece the apply made closed", closed, pieces)
			}
			if original := byID["gt-split"]; original == nil || original.Status != "open" {
				t.Errorf("original after the refusal = %+v, want it open", original)
			}
			for _, id := range []string{"gt-downstream", "gt-downstream-2"} {
				if d := byID[id]; d == nil || len(d.Dependencies) != 1 || d.Dependencies[0].ID != "gt-split" {
					t.Errorf("%s dependencies = %+v after the undo, want the original alone", id, d)
				}
			}
		})
	}
}

// splitRestoreAddRefuses refuses the undo's edge back to the original.
type splitRestoreAddRefuses struct{ beads.Client }

func (s splitRestoreAddRefuses) AddTypedDependency(issue, dependsOn, depType string) error {
	if dependsOn == "gt-split" {
		return errors.New("dep add refused")
	}
	return s.Client.AddTypedDependency(issue, dependsOn, depType)
}

// TestSpecSplitApplyFallsBackToThePlainEdge: a dependent whose raw targets name
// no edge to the original is moved off it by the plain id, the form bd's own
// removal takes for an edge the raw table does not hold.
func TestSpecSplitApplyFallsBackToThePlainEdge(t *testing.T) {
	t.Parallel()
	db := splitApplyFixture(t)
	store := &splitRecordingRemove{Client: db}
	raw := splitRawScript{targets: []string{"gt-other"}}
	if err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), store, raw); err != nil {
		t.Fatalf("runSpecSplitApplyOn() error = %v, want none", err)
	}
	if want := "gt-downstream off gt-split"; len(store.removed) != 1 || store.removed[0] != want {
		t.Errorf("removed = %v, want %q", store.removed, want)
	}
}

// TestSpecSplitApplyRefusesWhatTheRawReadCannotMove: a raw read that fails and
// a dependent whose relation is unknown each refuse the apply, with the
// original left open.
func TestSpecSplitApplyRefusesWhatTheRawReadCannotMove(t *testing.T) {
	t.Parallel()
	bare := func(t *testing.T) *beadsfake.Fake {
		db := beadsfake.New()
		mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})
		return db
	}
	tests := []struct {
		name    string
		db      func(*testing.T) *beadsfake.Fake
		raw     func(*beadsfake.Fake) specSplitRawDeps
		wantErr string
		issues  int
	}{
		{
			name:    "raw dependents read fails",
			db:      splitApplyFixture,
			raw:     func(*beadsfake.Fake) specSplitRawDeps { return splitRawScript{depErr: errors.New("raw refused")} },
			wantErr: "cannot read the dependents",
			issues:  2,
		},
		{
			name:    "raw targets read fails",
			db:      splitApplyFixture,
			raw:     func(*beadsfake.Fake) specSplitRawDeps { return splitRawScript{targetErr: errors.New("raw refused")} },
			wantErr: "reading the dependencies of gt-downstream",
			issues:  4,
		},
		{
			name: "raw relation is unknown",
			db:   bare,
			raw: func(db *beadsfake.Fake) specSplitRawDeps {
				db.OnSQL(func(q string) ([][]string, error) {
					if strings.Contains(q, "type =") || strings.Contains(q, "AS depends_on_id") {
						return [][]string{{"issue_id"}}, nil
					}
					return [][]string{{"issue_id"}, {"gt-downstream"}}, nil
				})
				return specSplitSQLDeps{db}
			},
			wantErr: "does not know",
			issues:  1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := tc.db(t)
			err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
				func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), db, tc.raw(db))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runSpecSplitApplyOn() error = %v, want a refusal naming %q", err, tc.wantErr)
			}
			issues, err := db.List(beads.ListOptions{Status: "all"})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(issues) != tc.issues {
				t.Errorf("issues = %d after the refusal, want %d", len(issues), tc.issues)
			}
			if original, err := db.Show("gt-split"); err != nil || original.Status != "open" {
				t.Errorf("original after the refusal = %+v, %v; want it open", original, err)
			}
		})
	}
}

// TestSpecSplitApplyRefusesABadDependentsPath: a dependents read that fails
// creates nothing; a dependent the move cannot write fails the apply after the
// pieces exist. Every refusal leaves the original open.
func TestSpecSplitApplyRefusesABadDependentsPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		store   func(beads.Client) beads.Client
		wantErr string
		issues  int
	}{
		{"dependents read fails", func(c beads.Client) beads.Client { return splitNoDependents{c} }, "cannot read the dependents", 2},
		{"dependent add fails", func(c beads.Client) beads.Client { return splitNoMove{c} }, "cannot move the dependents", 4},
		{"dependent remove fails", func(c beads.Client) beads.Client { return splitNoRemove{c} }, "cannot move the dependents", 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := splitApplyFixture(t)
			err := runSpecSplitApplyOn(&bytes.Buffer{}, "gt-split", "gastown", db.Show,
				func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck(), tc.store(db), nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runSpecSplitApplyOn() error = %v, want a refusal naming %q", err, tc.wantErr)
			}
			issues, err := db.List(beads.ListOptions{Status: "all"})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(issues) != tc.issues {
				t.Errorf("issues = %d after the refusal, want %d", len(issues), tc.issues)
			}
			if original, err := db.Show("gt-split"); err != nil || original.Status != "open" {
				t.Errorf("original after the refusal = %+v, %v; want it open", original, err)
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
	if cmd.Flags().Lookup("apply") == nil {
		t.Error("gt spec split has no --apply flag")
	}
}

// splitRefusingWriter is an output that refuses every write.
type splitRefusingWriter struct{}

func (splitRefusingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// TestSpecSplitReportsAFailedWrite: a draft or a failed verdict that cannot be
// written to the output is an error, not a silent partial print.
func TestSpecSplitReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	mustBead(t, db, beads.CreateOptions{ID: "gt-split", Title: "split", Description: splitBeadDescription})

	err := runSpecSplitOn(splitRefusingWriter{}, "gt-split", true, db.Show, func(string) *specdispatch.TargetTree { return splitTree() }, splitPassingCheck())
	if err == nil {
		t.Errorf("runSpecSplitOn() error = nil on a refused write, want an error")
	}
	check := splitCheckFor(10, map[string]splitPieceFake{"A": {minutes: 6}, "B": {minutes: 10}})
	err = runSpecSplitApplyOn(splitRefusingWriter{}, "gt-split", "gastown", db.Show,
		func(string) *specdispatch.TargetTree { return splitTree() }, check, db, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot report the failed checks") {
		t.Errorf("runSpecSplitApplyOn() error = %v on a refused write, want the report failure", err)
	}
}

// TestSpecSplitRefusesOutsideATown: when no town can be found, the command
// refuses with an error and prints nothing.
func TestSpecSplitRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	noTown := func() (string, error) { return "", errors.New("no town above the directory") }

	err := runSpecSplitFrom(&out, &bytes.Buffer{}, "gt-split", true, false, noTown)
	if err == nil {
		t.Errorf("runSpecSplitFrom() error = nil with no town, want an error")
	}
	if out.Len() != 0 {
		t.Errorf("runSpecSplitFrom() printed %q before refusing, want nothing", out.String())
	}
}

// TestSpecSplitRefusesBothModes: --dry-run and --apply together are refused
// before the town is read.
func TestSpecSplitRefusesBothModes(t *testing.T) {
	t.Parallel()
	noTown := func() (string, error) { return "", errors.New("no town above the directory") }

	err := runSpecSplitFrom(&bytes.Buffer{}, &bytes.Buffer{}, "gt-split", true, true, noTown)
	if err == nil || !strings.Contains(err.Error(), "--dry-run or --apply") {
		t.Errorf("runSpecSplitFrom() error = %v, want a refusal of both modes", err)
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
			err := runSpecSplitFrom(&out, &bytes.Buffer{}, "gt-split", true, false, func() (string, error) { return town, nil })
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
