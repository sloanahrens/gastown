package presubmit

import (
	"slices"
	"strings"
	"testing"
)

// podEngineFixtureDoc is a pod-engine document with two inventory rows. Its
// Evidence cell cites internal/e/e.go, and a prose bullet names internal/c/c.go:
// neither is a pinned file, because only the inventory's File column is.
const podEngineFixtureDoc = "# Pod engine\n\n" +
	"## How to read a row\n\n" +
	"| not | an | inventory | row |\n\n" +
	"## The inventory\n\n" +
	"| File | Function | Line | Source | Asks the engine for | Disposition | Evidence |\n" +
	"| --- | --- | --- | --- | --- | --- | --- |\n" +
	"| `internal/a/a.go` | `F` | 4 | `docker x` | one | mac-job | `internal/e/e.go:7` |\n" +
	"| `internal/b/b.go` | `G` | 9 | `docker y` | two | mac-job | `internal/b/b.go:9` |\n\n" +
	"## Named, not a call site\n\n" +
	"- `internal/c/c.go` names docker in prose.\n"

func TestPinnedFilesReadsTheFileColumnOfTheInventoryRows(t *testing.T) {
	t.Parallel()
	got, err := PinnedFiles(podEngineFixtureDoc)
	if err != nil {
		t.Fatalf("PinnedFiles: %v", err)
	}
	want := []string{"internal/a/a.go", "internal/b/b.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("PinnedFiles = %v, want %v", got, want)
	}
}

func TestPinnedFilesRefusesAMalformedInventoryRow(t *testing.T) {
	t.Parallel()
	const header = "## The inventory\n\n" +
		"| File | Function | Line | Source | Asks the engine for | Disposition | Evidence |\n" +
		"| --- | --- | --- | --- | --- | --- | --- |\n"
	tests := []struct {
		name string
		row  string
	}{
		{name: "a six-cell row", row: "| `internal/a/a.go` | `F` | 4 | `docker x` | one | mac-job |\n"},
		{name: "a row with no file", row: "| | `F` | 4 | `docker x` | one | mac-job | `internal/e/e.go:7` |\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := PinnedFiles(header + tt.row); err == nil {
				t.Fatal("PinnedFiles = nil error, want a refusal")
			}
		})
	}
}

func TestPodEngineDirsRefusesAnUnreadableInventoryRatherThanPassingDirsThrough(t *testing.T) {
	t.Parallel()
	dirs := []string{"internal/daemon"}
	const noInventory = "## How to read a row\n\n| a | b |\n| - | - |\n"
	got, err := PodEngineDirs(dirs, nameStatusPaths("M internal/a/a.go"), noInventory)
	if err == nil {
		t.Fatalf("PodEngineDirs = %v, nil error; want a refusal", got)
	}
	if got != nil {
		t.Fatalf("PodEngineDirs returned %v beside its refusal, want nil", got)
	}
}

func TestPinnedFilesRefusesADocWithoutAnInventory(t *testing.T) {
	t.Parallel()
	doc := "## How to read a row\n\n| a | b |\n| - | - |\n| `internal/a/a.go` | 1 |\n"
	if _, err := PinnedFiles(doc); err == nil {
		t.Fatal("PinnedFiles of a document with no inventory rows = nil error, want a refusal")
	}
}

// nameStatusPaths builds one git diff --name-status -z record per change, the
// form land.ChangedPaths reads.
func nameStatusPaths(records ...string) string {
	var b strings.Builder
	for _, r := range records {
		b.WriteString(strings.ReplaceAll(r, " ", "\x00"))
		b.WriteString("\x00")
	}
	return b.String()
}

func TestPodEngineTouchedSelectsOnlyPinnedFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		nameStatus string
		want       bool
	}{
		{name: "the first pinned row selects", nameStatus: nameStatusPaths("M internal/a/a.go"), want: true},
		{name: "the second pinned row selects", nameStatus: nameStatusPaths("A internal/b/b.go"), want: true},
		{name: "a file only the Evidence column cites does not select", nameStatus: nameStatusPaths("M internal/e/e.go"), want: false},
		{name: "a file named only in prose does not select", nameStatus: nameStatusPaths("M internal/c/c.go"), want: false},
		{name: "an unrelated file does not select", nameStatus: nameStatusPaths("M internal/z/z.go"), want: false},
		{name: "a rename away from a pinned file selects", nameStatus: nameStatusPaths("R100 internal/a/a.go internal/a/moved.go"), want: true},
		{name: "a deleted pinned file selects", nameStatus: nameStatusPaths("D internal/b/b.go"), want: true},
		{name: "an empty change does not select", nameStatus: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := PodEngineTouched(tt.nameStatus, podEngineFixtureDoc)
			if err != nil {
				t.Fatalf("PodEngineTouched: %v", err)
			}
			if got != tt.want {
				t.Fatalf("PodEngineTouched = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodEngineTouchedRefusesNameStatusThatIsNotNULSeparated(t *testing.T) {
	t.Parallel()
	// Without -z, git writes tab-separated records, which read as no path at
	// all: that must fail, not select nothing.
	if _, err := PodEngineTouched("M\tinternal/a/a.go\n", podEngineFixtureDoc); err == nil {
		t.Fatal("PodEngineTouched of tab-separated output = nil error, want a refusal")
	}
}

func TestPodEngineDirsAddsThePackageOnlyForAPinnedFile(t *testing.T) {
	t.Parallel()
	dirs := []string{"internal/daemon"}
	tests := []struct {
		name       string
		nameStatus string
		want       []string
	}{
		{name: "a pinned file adds the inventory package", nameStatus: nameStatusPaths("M internal/a/a.go"), want: []string{"internal/daemon", PodEngineDir}},
		{name: "an unpinned file leaves the packages alone", nameStatus: nameStatusPaths("M internal/z/z.go"), want: []string{"internal/daemon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := PodEngineDirs(dirs, tt.nameStatus, podEngineFixtureDoc)
			if err != nil {
				t.Fatalf("PodEngineDirs: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("PodEngineDirs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodEngineDirsDoesNotDuplicateTheInventoryPackage(t *testing.T) {
	t.Parallel()
	dirs := []string{PodEngineDir}
	got, err := PodEngineDirs(dirs, nameStatusPaths("M internal/a/a.go"), podEngineFixtureDoc)
	if err != nil {
		t.Fatalf("PodEngineDirs: %v", err)
	}
	if !slices.Equal(got, []string{PodEngineDir}) {
		t.Fatalf("PodEngineDirs = %v, want the inventory package once", got)
	}
}

func TestPodEngineRecoveryPrintsOneLineOnlyWhenAPinnedFileMoved(t *testing.T) {
	t.Parallel()
	line, err := RecoveryLine(nameStatusPaths("M internal/a/a.go"), podEngineFixtureDoc)
	if err != nil {
		t.Fatalf("RecoveryLine: %v", err)
	}
	if line != PodEngineRecovery {
		t.Fatalf("RecoveryLine = %q, want the recovery line", line)
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("RecoveryLine spans more than one line: %q", line)
	}
	if !strings.Contains(line, "go test ./internal/podengine -update-lines") {
		t.Fatalf("RecoveryLine = %q, want the -update-lines command", line)
	}

	none, err := RecoveryLine(nameStatusPaths("M internal/z/z.go"), podEngineFixtureDoc)
	if err != nil {
		t.Fatalf("RecoveryLine: %v", err)
	}
	if none != "" {
		t.Fatalf("RecoveryLine for an unpinned change = %q, want nothing", none)
	}
}
