package presubmit

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// podEngineFixtureDoc is a pod-engine document with one inventory row. Its
// Evidence cell cites internal/e/e.go:7, and a prose bullet cites
// internal/c/c.go:12, so both are pinned. A prose mention with no line, and a
// bare basename with a line, are not pins.
const podEngineFixtureDoc = "# Pod engine\n\n" +
	"## How to read a row\n\n" +
	"| not | an | inventory | row |\n\n" +
	"## The inventory\n\n" +
	"| File | Function | Line | Source | Asks the engine for | Disposition | Evidence |\n" +
	"| --- | --- | --- | --- | --- | --- | --- |\n" +
	"| `internal/a/a.go` | `F` | 4 | `docker x` | one | mac-job | `internal/e/e.go:7` |\n\n" +
	"## Named, not a call site\n\n" +
	"- `internal/c/c.go:12` names docker in prose.\n" +
	"- `internal/d/d.go` names docker in prose.\n" +
	"- dolt.go:203 names docker in prose.\n"

func TestPinnedFilesReadsTheInventoryAndEveryProseLineRef(t *testing.T) {
	t.Parallel()
	got, err := PinnedFiles(podEngineFixtureDoc)
	if err != nil {
		t.Fatalf("PinnedFiles: %v", err)
	}
	want := []string{"internal/a/a.go", "internal/c/c.go", "internal/e/e.go"}
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
		{name: "the inventory's File column selects", nameStatus: nameStatusPaths("M internal/a/a.go"), want: true},
		{name: "a file a prose line ref cites selects", nameStatus: nameStatusPaths("M internal/c/c.go"), want: true},
		{name: "a file an Evidence line ref cites selects", nameStatus: nameStatusPaths("A internal/e/e.go"), want: true},
		{name: "a file named in prose without a line does not select", nameStatus: nameStatusPaths("M internal/d/d.go"), want: false},
		{name: "a bare basename cited with a line does not select", nameStatus: nameStatusPaths("M dolt.go"), want: false},
		{name: "an unrelated file does not select", nameStatus: nameStatusPaths("M internal/z/z.go"), want: false},
		{name: "a rename away from a pinned file selects", nameStatus: nameStatusPaths("R100 internal/a/a.go internal/a/moved.go"), want: true},
		{name: "a deleted pinned file selects", nameStatus: nameStatusPaths("D internal/c/c.go"), want: true},
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

func TestRecoveryHintPrintsTheRecoveryOnlyWhenAPinnedFileMovedAndTheInventoryFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		nameStatus string
		inventory  error
		want       string
		wantRun    bool
	}{
		{name: "a pinned file moved and the inventory fails", nameStatus: nameStatusPaths("M internal/a/a.go"), inventory: errors.New("FAIL"), want: PodEngineRecovery, wantRun: true},
		{name: "a pinned file moved and the inventory passes", nameStatus: nameStatusPaths("M internal/a/a.go"), inventory: nil, want: "", wantRun: true},
		{name: "a prose-cited file moved and the inventory fails", nameStatus: nameStatusPaths("M internal/c/c.go"), inventory: errors.New("FAIL"), want: PodEngineRecovery, wantRun: true},
		{name: "no pinned file moved", nameStatus: nameStatusPaths("M internal/z/z.go"), inventory: errors.New("FAIL"), want: "", wantRun: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ran := false
			run := func() error {
				ran = true
				return tt.inventory
			}
			got, err := RecoveryHint(tt.nameStatus, podEngineFixtureDoc, run)
			if err != nil {
				t.Fatalf("RecoveryHint: %v", err)
			}
			if got != tt.want {
				t.Fatalf("RecoveryHint = %q, want %q", got, tt.want)
			}
			if ran != tt.wantRun {
				t.Fatalf("inventory run = %v, want %v", ran, tt.wantRun)
			}
		})
	}
}

func TestRecoveryHintIsOneLineThatNamesTheRenumberCommand(t *testing.T) {
	t.Parallel()
	if strings.Contains(PodEngineRecovery, "\n") {
		t.Fatalf("PodEngineRecovery spans more than one line: %q", PodEngineRecovery)
	}
	if !strings.Contains(PodEngineRecovery, "go test ./internal/podengine -update-lines") {
		t.Fatalf("PodEngineRecovery = %q, want the -update-lines command", PodEngineRecovery)
	}
}

func TestRecoveryHintRefusesAnUnreadableInventoryWithoutRunningIt(t *testing.T) {
	t.Parallel()
	const noInventory = "## How to read a row\n\n| a | b |\n| - | - |\n"
	ran := false
	got, err := RecoveryHint(nameStatusPaths("M internal/a/a.go"), noInventory, func() error {
		ran = true
		return errors.New("FAIL")
	})
	if err == nil {
		t.Fatalf("RecoveryHint = %q, nil error; want a refusal", got)
	}
	if ran {
		t.Fatal("RecoveryHint ran the inventory beside an unreadable document")
	}
}
