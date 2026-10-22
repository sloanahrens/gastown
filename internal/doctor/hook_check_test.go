package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// seedPinnedAttachment makes a pinned bead in db whose attached_molecule is
// moleculeID, the shape HookAttachmentValidCheck reads from a hook.
func seedPinnedAttachment(t *testing.T, db *beadsfake.Fake, moleculeID string) string {
	t.Helper()
	pinned, err := db.Create(beads.CreateOptions{
		Title:       "hook",
		Description: "attached_molecule: " + moleculeID,
		Labels:      []string{"gt:agent"},
		Priority:    -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	status := beads.StatusPinned
	if err := db.Update(pinned.ID, beads.UpdateOptions{Status: &status}); err != nil {
		t.Fatal(err)
	}
	return pinned.ID
}

func TestHookAttachmentValidCheck_ShowUnavailableIsNotInvalid(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := newDoctorTown()
	db := d.db(town)
	mol, err := db.Create(beads.CreateOptions{Title: "polecat work", Labels: []string{"gt:mol"}, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	pinnedID := seedPinnedAttachment(t, db, mol.ID)

	// A Dolt timeout: bd could not answer, so the molecule's existence is unknown.
	db.FailWith("show", errors.New("bd show: context deadline exceeded"))
	ctx := d.ctx(town, "")
	check := NewHookAttachmentValidCheck()
	if r := check.Run(ctx); r.Status == StatusError {
		t.Errorf("Run = %v %q, want a warning or OK when show is unavailable", r.Status, r.Message)
	}
	if len(check.invalidAttachments) != 0 {
		t.Fatalf("invalidAttachments = %+v, want none when show is unavailable", check.invalidAttachments)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	db.FailWith("show", nil)
	after, err := db.Show(pinnedID)
	if err != nil {
		t.Fatal(err)
	}
	if got := beads.ParseAttachmentFields(after); got == nil || got.AttachedMolecule != mol.ID {
		t.Errorf("attachment after Fix = %+v, want the live molecule %s still attached", got, mol.ID)
	}
}

func TestHookAttachmentValidCheck_ShowNotFoundStillRecorded(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := newDoctorTown()
	db := d.db(town)
	// The molecule was never created, so bd answers not-found.
	seedPinnedAttachment(t, db, "gt-gone")

	check := NewHookAttachmentValidCheck()
	check.Run(d.ctx(town, ""))
	if len(check.invalidAttachments) != 1 {
		t.Fatalf("invalidAttachments = %+v, want one not_found entry", check.invalidAttachments)
	}
	if got := check.invalidAttachments[0]; got.moleculeID != "gt-gone" || got.reason != "not_found" {
		t.Errorf("entry = %+v, want molecule gt-gone with reason not_found", got)
	}
}

func TestNewHookAttachmentValidCheck(t *testing.T) {
	t.Parallel()
	check := NewHookAttachmentValidCheck()

	if check.Name() != "hook-attachment-valid" {
		t.Errorf("expected name 'hook-attachment-valid', got %q", check.Name())
	}

	if check.Description() != "Verify attached molecules exist and are not closed" {
		t.Errorf("unexpected description: %q", check.Description())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestHookAttachmentValidCheck_NoBeadsDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	check := NewHookAttachmentValidCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	// No beads dir means nothing to check, should be OK
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no beads dir, got %v", result.Status)
	}
}

func TestHookAttachmentValidCheck_EmptyBeadsDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewHookAttachmentValidCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	// Empty beads dir means no pinned beads, should be OK
	// Note: This may error if bd CLI is not available, but should still handle gracefully
	if result.Status != StatusOK && result.Status != StatusError {
		t.Errorf("expected StatusOK or graceful error, got %v", result.Status)
	}
}

func TestHookAttachmentValidCheck_FormatInvalid(t *testing.T) {
	t.Parallel()
	check := NewHookAttachmentValidCheck()

	tests := []struct {
		inv      invalidAttachment
		expected string
	}{
		{
			inv: invalidAttachment{
				pinnedBeadID: "hq-123",
				moleculeID:   "gt-456",
				reason:       "not_found",
			},
			expected: "hq-123: attached molecule gt-456 not found",
		},
		{
			inv: invalidAttachment{
				pinnedBeadID: "hq-123",
				moleculeID:   "gt-789",
				reason:       "closed",
			},
			expected: "hq-123: attached molecule gt-789 is closed",
		},
	}

	for _, tt := range tests {
		result := check.formatInvalid(tt.inv)
		if result != tt.expected {
			t.Errorf("formatInvalid() = %q, want %q", result, tt.expected)
		}
	}
}

func TestHookAttachmentValidCheck_FindRigBeadsDirs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Only <town>/<rig>/.beads counts: not the town's own .beads, not the
	// mayor's, not one nested deeper, and not a .beads file.
	for _, dir := range []string{".beads", "myrig/.beads", "mayor/.beads", "other/crew/x/.beads", "plain"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "plain", ".beads"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	dirs := NewHookAttachmentValidCheck().findRigBeadsDirs(tmpDir)

	if want := []string{filepath.Join(tmpDir, "myrig", ".beads")}; !slices.Equal(dirs, want) {
		t.Errorf("findRigBeadsDirs = %v, want %v", dirs, want)
	}
}

// Tests for HookSingletonCheck

func TestNewHookSingletonCheck(t *testing.T) {
	t.Parallel()
	check := NewHookSingletonCheck()

	if check.Name() != "hook-singleton" {
		t.Errorf("expected name 'hook-singleton', got %q", check.Name())
	}

	if check.Description() != "Ensure each agent has at most one handoff bead" {
		t.Errorf("unexpected description: %q", check.Description())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestHookSingletonCheck_NoBeadsDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	check := NewHookSingletonCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	// No beads dir means nothing to check, should be OK
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no beads dir, got %v", result.Status)
	}
}

func TestHookSingletonCheck_EmptyBeadsDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewHookSingletonCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	// Empty beads dir means no pinned beads, should be OK
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when empty beads dir, got %v", result.Status)
	}
}

func TestHookSingletonCheck_FormatDuplicate(t *testing.T) {
	t.Parallel()
	check := NewHookSingletonCheck()

	tests := []struct {
		dup      duplicateHandoff
		expected string
	}{
		{
			dup: duplicateHandoff{
				title:   "Mayor Handoff",
				beadIDs: []string{"hq-123", "hq-456"},
			},
			expected: `"Mayor Handoff" has 2 beads: hq-123, hq-456`,
		},
		{
			dup: duplicateHandoff{
				title:   "gastown/crew/joe Handoff",
				beadIDs: []string{"gt-1", "gt-2", "gt-3"},
			},
			expected: `"gastown/crew/joe Handoff" has 3 beads: gt-1, gt-2, gt-3`,
		},
	}

	for _, tt := range tests {
		result := check.formatDuplicate(tt.dup)
		if result != tt.expected {
			t.Errorf("formatDuplicate() = %q, want %q", result, tt.expected)
		}
	}
}

// Tests for OrphanedAttachmentsCheck

func TestNewOrphanedAttachmentsCheck(t *testing.T) {
	t.Parallel()
	check := NewOrphanedAttachmentsCheck()

	if check.Name() != "orphaned-attachments" {
		t.Errorf("expected name 'orphaned-attachments', got %q", check.Name())
	}

	if check.Description() != "Detect handoff beads for non-existent agents" {
		t.Errorf("unexpected description: %q", check.Description())
	}

	// This check is not auto-fixable (uses BaseCheck, not FixableCheck)
	if check.CanFix() {
		t.Error("expected CanFix to return false")
	}
}

func TestOrphanedAttachmentsCheck_NoBeadsDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	check := NewOrphanedAttachmentsCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	// No beads dir means nothing to check, should be OK
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no beads dir, got %v", result.Status)
	}
}

func TestOrphanedAttachmentsCheck_FormatOrphan(t *testing.T) {
	t.Parallel()
	check := NewOrphanedAttachmentsCheck()

	tests := []struct {
		orph     orphanedHandoff
		expected string
	}{
		{
			orph: orphanedHandoff{
				beadID: "hq-123",
				agent:  "gastown/nux",
			},
			expected: `hq-123: agent "gastown/nux" no longer exists`,
		},
		{
			orph: orphanedHandoff{
				beadID: "gt-456",
				agent:  "gastown/crew/joe",
			},
			expected: `gt-456: agent "gastown/crew/joe" no longer exists`,
		},
	}

	for _, tt := range tests {
		result := check.formatOrphan(tt.orph)
		if result != tt.expected {
			t.Errorf("formatOrphan() = %q, want %q", result, tt.expected)
		}
	}
}

func TestOrphanedAttachmentsCheck_AgentExists(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create some agent directories
	polecatDir := filepath.Join(tmpDir, "gastown", "polecats", "nux")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatal(err)
	}

	crewDir := filepath.Join(tmpDir, "gastown", "crew", "joe")
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatal(err)
	}

	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewOrphanedAttachmentsCheck()

	tests := []struct {
		agent    string
		expected bool
	}{
		// Existing agents
		{"gastown/nux", true},
		{"gastown/crew/joe", true},
		{"mayor", true},

		// Non-existent agents
		{"gastown/deleted", false},
		{"gastown/crew/gone", false},
	}

	for _, tt := range tests {
		result := check.agentExists(tt.agent, tmpDir)
		if result != tt.expected {
			t.Errorf("agentExists(%q) = %v, want %v", tt.agent, result, tt.expected)
		}
	}
}
