package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// provisionedTown is a town root whose .claude/commands/ holds the bodies this
// binary embeds.
func provisionedTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := templates.ProvisionCommands(town); err != nil {
		t.Fatalf("provisioning commands: %v", err)
	}
	return town
}

func commandPath(town, name string) string {
	return filepath.Join(town, ".claude", "commands", name+".md")
}

func writeCommand(t *testing.T, town, name, body string) {
	t.Helper()
	if err := os.WriteFile(commandPath(town, name), []byte(body), 0644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func TestCommandsCheck_Current(t *testing.T) {
	t.Parallel()
	town := provisionedTown(t)

	result := NewCommandsCheck().Run(&CheckContext{TownRoot: town})
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for a provisioned town, got %v: %s", result.Status, result.Message)
	}
}

// A copy that predates a template edit must warn, or Fix never runs and the
// agent keeps reading the old body (gt-w9afa).
func TestCommandsCheck_StaleCommandIsReportedAndFixed(t *testing.T) {
	t.Parallel()
	town := provisionedTown(t)
	writeCommand(t, town, "done", "---\ndescription: old\n---\n\nold body\n")

	check := NewCommandsCheck()
	result := check.Run(&CheckContext{TownRoot: town})
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for a stale command, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "stale: done") {
		t.Errorf("message %q does not name the stale command", result.Message)
	}
	if strings.Contains(result.Message, "missing") {
		t.Errorf("message %q reports a missing command in a town that has them all", result.Message)
	}

	if err := check.Fix(&CheckContext{TownRoot: town}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	data, err := os.ReadFile(commandPath(town, "done"))
	if err != nil {
		t.Fatalf("reading done.md: %v", err)
	}
	if strings.Contains(string(data), "old body") {
		t.Error("Fix left the stale body in place")
	}

	after := check.Run(&CheckContext{TownRoot: town})
	if after.Status != StatusOK {
		t.Errorf("expected StatusOK after Fix, got %v: %s", after.Status, after.Message)
	}
}

func TestCommandsCheck_MissingAndStale(t *testing.T) {
	t.Parallel()
	town := provisionedTown(t)
	writeCommand(t, town, "handoff", "stale")
	if err := os.Remove(commandPath(town, "review")); err != nil {
		t.Fatal(err)
	}

	check := NewCommandsCheck()
	result := check.Run(&CheckContext{TownRoot: town})
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
	for _, want := range []string{"missing: review", "stale: handoff"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q lacks %q", result.Message, want)
		}
	}

	if err := check.Fix(&CheckContext{TownRoot: town}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if after := check.Run(&CheckContext{TownRoot: town}); after.Status != StatusOK {
		t.Errorf("expected StatusOK after Fix, got %v: %s", after.Status, after.Message)
	}
}

// A town with nothing provisioned reports missing names only — Stale owns the
// differs-from-template case and must not double-report absent files.
func TestCommandsCheck_EmptyTownReportsOnlyMissing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	result := NewCommandsCheck().Run(&CheckContext{TownRoot: town})
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for an unprovisioned town, got %v", result.Status)
	}
	if strings.Contains(result.Message, "stale") {
		t.Errorf("message %q reports stale commands in an empty town", result.Message)
	}
	if !strings.Contains(result.Message, "missing:") {
		t.Errorf("message %q does not report the missing commands", result.Message)
	}
}

// Fix is driven by what Run cached; with nothing cached it must not touch the
// workspace at all.
func TestCommandsCheck_FixWithoutDriftIsANoOp(t *testing.T) {
	t.Parallel()
	town := provisionedTown(t)

	stamp := commandPath(town, "done")
	before, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}

	if err := NewCommandsCheck().Fix(&CheckContext{TownRoot: town}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	after, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("Fix rewrote a current command file")
	}
}
