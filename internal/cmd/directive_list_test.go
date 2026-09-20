package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func writeDirective(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestDirectiveList_FlagsFilesNoRoleLoads(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	writeDirective(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
	writeDirective(t, filepath.Join(townRoot, "directives", "mayor.md"), "mayor policy")
	writeDirective(t, filepath.Join(townRoot, "directives", config.SharedDirectiveName+".md"), "shared policy")
	writeDirective(t, filepath.Join(townRoot, "myrig", "directives", "testing.md"), "test rules")

	entries, err := listableDirectiveFiles(townRoot)
	if err != nil {
		t.Fatalf("listableDirectiveFiles: %v", err)
	}

	var buf bytes.Buffer
	renderDirectiveList(&buf, entries)
	out := buf.String()

	if !strings.Contains(out, "UNUSED (no such role)") {
		t.Errorf("expected the UNUSED marker for a non-role stem, got:\n%s", out)
	}

	rowFor := func(stem string) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, filepath.Join("directives", stem+".md")) {
				return line
			}
		}
		t.Fatalf("no row for %s.md in:\n%s", stem, out)
		return ""
	}

	if row := rowFor("testing"); !strings.Contains(row, "UNUSED") {
		t.Errorf("misnamed file not flagged: %q", row)
	}
	if row := rowFor("mayor"); strings.Contains(row, "UNUSED") {
		t.Errorf("role-named file flagged as unused: %q", row)
	}
	if row := rowFor(config.SharedDirectiveName); !strings.Contains(row, "every role") {
		t.Errorf("shared file not marked as applying to every role: %q", row)
	}
	if !strings.Contains(out, "never rendered") {
		t.Errorf("expected the list to say what an unused file means, got:\n%s", out)
	}
}

func TestDirectiveList_EmptyTown(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	renderDirectiveList(&buf, nil)

	if !strings.Contains(buf.String(), "No directive files found.") {
		t.Errorf("got:\n%s", buf.String())
	}
}

// A file with no content is left out of the list, but the scan that feeds it
// is the doctor check's too, so the path is still reported there.
func TestDirectiveList_SkipsEmptyFiles(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	writeDirective(t, filepath.Join(townRoot, "directives", "mayor.md"), "   \n")

	entries, err := listableDirectiveFiles(townRoot)
	if err != nil {
		t.Fatalf("listableDirectiveFiles: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0: %+v", len(entries), entries)
	}
}
