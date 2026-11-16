package session

import (
	"path/filepath"
	"sort"
	"testing"
)

// A seat reads its task at the path the runtime archive stages it at, so the
// helper is checked against the archive itself rather than a literal: the
// spawn's task text is the body of the entry SandboxTaskFilePath names, under
// the sandbox root the archive extracts to. A move of the staging or of the
// root fails here (gt-w1mzol).
func TestNoBeadAccessSandboxTaskFilePathIsTheArchiveEntry(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	const taskText = "## Goal\n\nRead this from the staged file.\n"
	spec := openshellSpawn(town, work)
	spec.TaskFile = []byte(taskText)
	entries := untarRuntime(t, openshellArchiveBytes(t, spec, sandboxTracker{}))

	entry, err := filepath.Rel(sandboxHome, SandboxTaskFilePath())
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q) = %v", sandboxHome, SandboxTaskFilePath(), err)
	}
	staged, ok := entries[entry]
	if !ok {
		t.Fatalf("the archive stages no entry at %s, the path the seat reads; staged: %v", entry, runtimeEntryNames(entries))
	}
	if staged.body != taskText {
		t.Errorf("%s = %q, want the spawn's task text %q", entry, staged.body, taskText)
	}
}

// runtimeEntryNames lists an archive's entries in a fixed order for a failure
// message.
func runtimeEntryNames(entries map[string]runtimeEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
