package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupReassignmentRecordFixture stubs bd to log its argv and wires routes so
// gt-zd7c resolves to the gastown rig. The rig directory has no git repo, so
// branch enumeration degrades to "cannot tell" — which is the state the record
// must still be written in.
func setupReassignmentRecordFixture(t *testing.T) (townRoot, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}

	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "mayor", "rig"), 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	writeGastownRoutes(t, townRoot)

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath = filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "` + logPath + `"
echo '[]'
exit 0
`
	_ = writeBDStub(t, binDir, script, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return townRoot, logPath
}

// TestRecordReassignmentCarriesOutgoingAssignee is the gt-zd7c fix: the bead
// itself must name the polecat whose assignee the sling overwrote, or an audit
// that starts from the bead has no way back to the earlier branch.
func TestRecordReassignmentCarriesOutgoingAssignee(t *testing.T) {
	townRoot, logPath := setupReassignmentRecordFixture(t)

	recordReassignment(townRoot, "gt-zd7c", "gastown/polecats/jasper", "gastown/polecats/obsidian", "mayor")

	log := readFileOrFatal(t, logPath)
	for _, want := range []string{
		"comments",
		"add",
		"gt-zd7c",
		"REASSIGNED: gastown/polecats/jasper -> gastown/polecats/obsidian",
		"Branch: (unknown)",
		"By: mayor",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("reassignment record missing %q; got: %q", want, log)
		}
	}
}

// TestRecordReassignmentSkipsNoOps guards the write budget: Dolt takes a
// permanent commit per comment, and neither an unassigned bead nor an
// unchanged assignee has anything to record.
func TestRecordReassignmentSkipsNoOps(t *testing.T) {
	townRoot, logPath := setupReassignmentRecordFixture(t)

	recordReassignment(townRoot, "gt-zd7c", "", "gastown/polecats/obsidian", "mayor")
	recordReassignment(townRoot, "gt-zd7c", "gastown/polecats/obsidian", "gastown/polecats/obsidian", "mayor")

	if got := readFileOrEmpty(logPath); got != "" {
		t.Errorf("expected no bd call for a no-op reassignment, got: %q", got)
	}
}

func readFileOrFatal(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
