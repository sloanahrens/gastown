package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func writeDirectiveFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestUnusedDirectiveCheck_Metadata(t *testing.T) {
	check := NewUnusedDirectiveCheck()

	if check.Name() != "unused-directives" {
		t.Errorf("Name() = %q, want %q", check.Name(), "unused-directives")
	}
	if check.Category() != CategoryConfig {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryConfig)
	}
	if check.CanFix() {
		t.Error("CanFix() = true; deleting operator directive files is not a fix")
	}
}

func TestUnusedDirectiveCheck_NoUnusedFiles(t *testing.T) {
	townRoot := t.TempDir()
	writeDirectiveFile(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
	writeDirectiveFile(t, filepath.Join(townRoot, "directives", "mayor.md"), "mayor policy")
	writeDirectiveFile(t, filepath.Join(townRoot, "myrig", "directives", "refinery.md"), "rig policy")

	result := NewUnusedDirectiveCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK: %s", result.Status, result.Message)
	}
}

func TestUnusedDirectiveCheck_WarnsOnMisnamedFile(t *testing.T) {
	townRoot := t.TempDir()
	writeDirectiveFile(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
	writeDirectiveFile(t, filepath.Join(townRoot, "myrig", "directives", "refinery.md"), "rig policy")
	unusedPath := filepath.Join(townRoot, "myrig", "directives", "host-hygiene.md")
	writeDirectiveFile(t, unusedPath, "host rules")

	result := NewUnusedDirectiveCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "1") {
		t.Errorf("Message = %q, want it to count one file", result.Message)
	}

	var named bool
	for _, d := range result.Details {
		if strings.Contains(d, unusedPath) && strings.Contains(d, "host-hygiene") {
			named = true
		}
	}
	if !named {
		t.Errorf("Details = %v, want the file path and its stem", result.Details)
	}
	if result.FixHint == "" {
		t.Error("FixHint is empty; the operator needs the rename path")
	}
}

func TestUnusedDirectiveCheck_SharedNameIsNotUnused(t *testing.T) {
	townRoot := t.TempDir()
	writeDirectiveFile(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
	writeDirectiveFile(t,
		filepath.Join(townRoot, "directives", config.SharedDirectiveName+".md"), "shared policy")

	result := NewUnusedDirectiveCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK for the shared directive: %s / %v",
			result.Status, result.Message, result.Details)
	}
}

func TestUnusedDirectiveCheck_SkipsWhenDirectivesUnreadable(t *testing.T) {
	townRoot := t.TempDir()
	// A regular file where the directory belongs makes the scan fail for a
	// reason other than "nothing there".
	writeDirectiveFile(t, filepath.Join(townRoot, "directives"), "not a directory")

	result := NewUnusedDirectiveCheck().Run(&CheckContext{TownRoot: townRoot, RigName: "myrig"})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want Skipped — a failed scan must not read as a clean pass", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want the %q prefix a skipped check carries", result.Message, "unknown:")
	}
	if len(result.Details) == 0 {
		t.Error("Details is empty; the underlying error belongs there")
	}
}

// The check must never be the thing that removes a directive file. A warning
// whose fix deletes operator prose town-wide is unrecoverable data loss; the
// operator renames, the check only reports.
func TestUnusedDirectiveCheck_FixLeavesFilesOnDisk(t *testing.T) {
	townRoot := t.TempDir()
	writeDirectiveFile(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
	unusedPath := filepath.Join(townRoot, "myrig", "directives", "testing.md")
	writeDirectiveFile(t, unusedPath, "test rules that still matter")

	check := NewUnusedDirectiveCheck()
	ctx := &CheckContext{TownRoot: townRoot, RigName: "myrig"}

	if result := check.Run(ctx); result.Status != StatusWarning {
		t.Fatalf("precondition: Status = %v, want Warning", result.Status)
	}

	if err := check.Fix(ctx); err == nil {
		t.Error("Fix() returned nil; an unfixable check must report that it cannot fix")
	}
	if _, err := os.Stat(unusedPath); err != nil {
		t.Errorf("Fix() removed %s: %v", unusedPath, err)
	}
	if data, err := os.ReadFile(unusedPath); err != nil || string(data) != "test rules that still matter" {
		t.Errorf("Fix() altered %s: %q, %v", unusedPath, data, err)
	}
}
