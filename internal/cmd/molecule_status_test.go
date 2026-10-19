package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// brokenReadClient is a beads.Client whose chosen read fails, so a test can put
// molecule status on the error path without a broken store.
type brokenReadClient struct {
	beads.Client
	showMultiple bool
	list         bool
	err          error
}

func (c brokenReadClient) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if c.showMultiple {
		return nil, c.err
	}
	return c.Client.ShowMultiple(ids)
}

func (c brokenReadClient) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if c.list {
		return nil, c.err
	}
	return c.Client.List(opts)
}

// TestMoleculeCurrentReportsUnknownOnAReadFailure: a molecule whose steps
// cannot be read is neither blocked (open steps missing from the map look like
// unmet dependencies) nor working (zero steps look like an opaque attach). Both
// name a state the read never established, so the status is unknown and carries
// the error that stopped it (gt-abr6v).
func TestMoleculeCurrentReportsUnknownOnAReadFailure(t *testing.T) {
	t.Parallel()

	readErr := errors.New("dial tcp 127.0.0.1:3307: connection refused")

	// seedMolecule returns a store holding a root with one open step child,
	// plus its ID.
	seedMolecule := func(t *testing.T) (*beadsfake.Fake, string) {
		t.Helper()
		fake := beadsfake.New()
		root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
		if err != nil {
			t.Fatalf("create molecule root: %v", err)
		}
		if _, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: root.ID}); err != nil {
			t.Fatalf("create step: %v", err)
		}
		return fake, root.ID
	}

	handoffFor := func(rootID string) *beads.Issue {
		return &beads.Issue{
			ID:    "gt-handoff",
			Title: "handoff",
			Description: beads.SetAttachmentFields(&beads.Issue{}, &beads.AttachmentFields{
				AttachedMolecule: rootID,
			}),
		}
	}

	tests := []struct {
		name   string
		broken brokenReadClient
	}{
		{name: "reading the open steps fails", broken: brokenReadClient{showMultiple: true, err: readErr}},
		{name: "listing the steps fails", broken: brokenReadClient{list: true, err: readErr}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake, rootID := seedMolecule(t)
			client := tt.broken
			client.Client = fake

			info := moleculeCurrentFrom(client, handoffFor(rootID), "gastown/nux")

			if info.Status != "unknown" {
				t.Errorf("status = %q, want unknown — not blocked and not working", info.Status)
			}
			if !strings.Contains(info.Error, readErr.Error()) {
				t.Errorf("Error = %q, want it to carry %q", info.Error, readErr)
			}
		})
	}
}

// A rigs.json that exists but does not decode gives the rig no prefix, so
// buildAgentBeadID returns no id instead of one built from an empty prefix
// (gt-71dgi).
func TestBuildAgentBeadIDCorruptRigsJSONReturnsNoID(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		identity string
		role     Role
	}{
		{"gastown/nux", RolePolecat},
		{"gastown/crew/max", RoleCrew},
		{"gastown/nux", RoleUnknown},
	} {
		if got := buildAgentBeadID(tc.identity, tc.role, townRoot); got != "" {
			t.Errorf("buildAgentBeadID(%q, %v) = %q, want empty", tc.identity, tc.role, got)
		}
	}
}

func TestOutputMoleculeStatus_StandaloneFormulaShowsVars(t *testing.T) {
	t.Parallel()
	status := MoleculeStatusInfo{
		HasWork:         true,
		PinnedBead:      &beads.Issue{ID: "gt-wisp-xyz", Title: "Standalone formula work"},
		AttachedFormula: "mol-release",
		AttachedVars:    []string{"version=1.2.3", "channel=stable"},
	}

	var buf bytes.Buffer
	outputMoleculeStatus(&buf, status, func() {})
	output := buf.String()

	if !strings.Contains(output, "📐 Formula: mol-release") {
		t.Fatalf("expected formula in output, got:\n%s", output)
	}
	if !strings.Contains(output, "--var version=1.2.3") || !strings.Contains(output, "--var channel=stable") {
		t.Fatalf("expected formula vars in output, got:\n%s", output)
	}
}

func TestOutputMoleculeStatus_FormulaWispShowsWorkflowContext(t *testing.T) {
	t.Parallel()

	status := MoleculeStatusInfo{
		HasWork:         true,
		PinnedBead:      &beads.Issue{ID: "tool-wisp-demo", Title: "demo-hello"},
		AttachedFormula: "demo-hello",
		Progress: &MoleculeProgressInfo{
			RootID:     "tool-wisp-demo",
			RootTitle:  "demo-hello",
			TotalSteps: 3,
			DoneSteps:  0,
			ReadySteps: []string{"tool-wisp-step-1"},
		},
		NextAction: "Show the workflow steps: gt prime or gt mol current",
	}

	var buf bytes.Buffer
	outputMoleculeStatus(&buf, status, func() {})
	output := buf.String()

	if !strings.Contains(output, "📐 Formula: demo-hello") {
		t.Fatalf("expected formula line in output, got:\n%s", output)
	}
	if strings.Contains(output, "No molecule attached") {
		t.Fatalf("formula wisp should not be rendered as naked work, got:\n%s", output)
	}
	if strings.Contains(output, "Attach a molecule to start work") {
		t.Fatalf("formula wisp should not suggest gt mol attach, got:\n%s", output)
	}
	if !strings.Contains(output, "Show the workflow steps: gt prime or gt mol current") {
		t.Fatalf("expected workflow next action, got:\n%s", output)
	}
}

// molStatusRecorderGit records the fetch the divergence warning makes, so a
// test can assert it is bounded (gt-2czgm).
type molStatusRecorderGit struct {
	branch     string
	fetched    bool
	fetchURL   string
	fetchSpec  string
	fetchBound time.Duration
}

func (g *molStatusRecorderGit) IsRepo() bool                   { return true }
func (g *molStatusRecorderGit) CurrentBranch() (string, error) { return g.branch, nil }
func (g *molStatusRecorderGit) FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error {
	g.fetched = true
	g.fetchURL, g.fetchSpec, g.fetchBound = remote, refspec, timeout
	return nil
}
func (g *molStatusRecorderGit) CommitsAhead(base, branch string) (int, error) { return 0, nil }
func (g *molStatusRecorderGit) CountCommitsBehind(ref string) (int, error)    { return 0, nil }

// TestShowGitDivergenceWarningBoundsTheFetch: gt mol status refreshes origin
// through a bounded fetch, not a bare `git fetch origin` that can hang on an
// unreachable remote (gt-2czgm).
func TestShowGitDivergenceWarningBoundsTheFetch(t *testing.T) {
	t.Parallel()
	g := &molStatusRecorderGit{branch: "main"}

	showGitDivergenceWarningFor(g)

	if !g.fetched {
		t.Fatal("the divergence warning did not refresh origin")
	}
	if g.fetchURL != "origin" {
		t.Errorf("fetched remote %q, want origin", g.fetchURL)
	}
	if g.fetchBound <= 0 {
		t.Errorf("fetch bound = %v; an unbounded fetch can hang gt mol status", g.fetchBound)
	}
	if !strings.Contains(g.fetchSpec, "refs/heads") {
		t.Errorf("fetch refspec %q does not refresh branch refs", g.fetchSpec)
	}
}

// TestMoleculeProgressShowMultipleFailureNotReady: the dependencies of the open
// steps come from one ShowMultiple read. When it fails, an open step with a
// blocking dependency must not be listed ready — gt mol status would suggest
// claiming it — so the read fails the progress instead of defaulting every open
// step to ready.
func TestMoleculeProgressShowMultipleFailureNotReady(t *testing.T) {
	t.Parallel()

	readErr := errors.New("dial tcp 127.0.0.1:3307: connection refused")

	fake := beadsfake.New()
	root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
	if err != nil {
		t.Fatalf("create molecule root: %v", err)
	}
	first, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: root.ID})
	if err != nil {
		t.Fatalf("create first step: %v", err)
	}
	second, err := fake.Create(beads.CreateOptions{Title: "implement", Parent: root.ID})
	if err != nil {
		t.Fatalf("create second step: %v", err)
	}
	if err := fake.AddDependency(second.ID, first.ID); err != nil {
		t.Fatalf("add dependency: %v", err)
	}

	// With the read intact the second step is blocked: the premise.
	whole, err := getMoleculeProgressInfo(fake, root.ID)
	if err != nil {
		t.Fatalf("progress with a working read: %v", err)
	}
	if !slices.Contains(whole.BlockedSteps, second.ID) {
		t.Fatalf("precondition: BlockedSteps = %v, want it to hold %s", whole.BlockedSteps, second.ID)
	}

	progress, err := getMoleculeProgressInfo(brokenReadClient{Client: fake, showMultiple: true, err: readErr}, root.ID)
	if err == nil {
		t.Fatalf("progress = %+v, want an error naming the failed read", progress)
	}
	if !strings.Contains(err.Error(), readErr.Error()) {
		t.Errorf("error = %q, want it to carry %q", err, readErr)
	}
	if progress != nil && slices.Contains(progress.ReadySteps, second.ID) {
		t.Errorf("ReadySteps = %v lists %s, which depends on an open step", progress.ReadySteps, second.ID)
	}
}

// TestMoleculeCurrentHeldStepIsCurrent: a step a hand-off left pinned or hooked
// is held by an agent, so it is the molecule's current step; counting it in the
// total but in no bucket reported the molecule blocked with no current step.
func TestMoleculeCurrentHeldStepIsCurrent(t *testing.T) {
	t.Parallel()

	for _, held := range []string{beads.StatusPinned, beads.StatusHooked} {
		t.Run(held, func(t *testing.T) {
			t.Parallel()
			assertMoleculeCurrentHeldStep(t, held)
		})
	}
}

func assertMoleculeCurrentHeldStep(t *testing.T, heldStatus string) {
	t.Helper()

	fake := beadsfake.New()
	root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
	if err != nil {
		t.Fatalf("create molecule root: %v", err)
	}
	done, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: root.ID})
	if err != nil {
		t.Fatalf("create closed step: %v", err)
	}
	pinned, err := fake.Create(beads.CreateOptions{Title: "implement", Parent: root.ID})
	if err != nil {
		t.Fatalf("create pinned step: %v", err)
	}
	if err := fake.Close(done.ID); err != nil {
		t.Fatalf("close step: %v", err)
	}
	status := heldStatus
	if err := fake.Update(pinned.ID, beads.UpdateOptions{Status: &status}); err != nil {
		t.Fatalf("set step status %s: %v", heldStatus, err)
	}

	handoff := &beads.Issue{
		ID:    "gt-handoff",
		Title: "handoff",
		Description: beads.SetAttachmentFields(&beads.Issue{}, &beads.AttachmentFields{
			AttachedMolecule: root.ID,
		}),
	}

	info := moleculeCurrentFrom(fake, handoff, "gastown/nux")

	if info.Status != "working" {
		t.Errorf("status = %q, want working", info.Status)
	}
	if info.CurrentStepID != pinned.ID {
		t.Errorf("CurrentStepID = %q, want the pinned step %s", info.CurrentStepID, pinned.ID)
	}
}
