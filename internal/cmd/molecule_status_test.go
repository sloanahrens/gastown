package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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
