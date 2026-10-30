package testpolicy

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// maxSweepHost is the number of entries sweep-host.txt holds. The list only
// shrinks: a package leaves it when a container run proves it runs in the
// runner, and the line and this pin move in the same change. A count under
// the pin is how a forgotten pin shows, so CheckSweepHostCount fails both
// directions.
const maxSweepHost = 12

// generatedSeed is the directory make polecat-image stages whole rig
// checkouts in. It mirrors internal/testpolicy/cmd/integrationvet: those are
// copies of other trees, not packages of this module, and walking them would
// count their packages as ours.
const generatedSeed = "deploy/polecat/seed"

// sweepHostOfTree reads the manifest and discovers the integration-tagged
// packages of this tree, which every check below compares.
func sweepHostOfTree(t *testing.T) ([]SweepHostEntry, []string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ReadSweepHost("sweep-host.txt")
	if err != nil {
		t.Fatal(err)
	}
	integration, err := IntegrationPackages(root, generatedSeed)
	if err != nil {
		t.Fatal(err)
	}
	return entries, integration
}

// TestSweepHostMissingManifestFails keeps an unreadable manifest an error: an
// empty list would put every integration package in the runner set, which is
// the classification this file exists to make.
func TestSweepHostMissingManifestFails(t *testing.T) {
	t.Parallel()
	entries, err := ReadSweepHost(filepath.Join(t.TempDir(), "sweep-host.txt"))
	if err == nil {
		t.Fatalf("ReadSweepHost of a missing file = %v, want an error", entries)
	}
}

// TestSweepHostNamesOnlyExistingPackages keeps sweep-host.txt to packages the
// integration tag actually adds a file to: a line naming anything else fails
// with its line number.
func TestSweepHostNamesOnlyExistingPackages(t *testing.T) {
	t.Parallel()
	entries, integration := sweepHostOfTree(t)
	for _, f := range CheckSweepHostPackages(entries, integration) {
		t.Error(f)
	}
}

// TestSweepHostEveryLineHasAReason keeps the seed readable: each line says
// what in that package's tests made the runner skip it.
func TestSweepHostEveryLineHasAReason(t *testing.T) {
	t.Parallel()
	entries, _ := sweepHostOfTree(t)
	for _, f := range CheckSweepHostReasons(entries) {
		t.Error(f)
	}
}

// TestSweepRunnerSetIsTheRest keeps the runner set and the host manifest a
// partition of the discovered packages: sorted, disjoint from the manifest,
// and together exactly the discovered set. The runner set may be short of
// packages while the seed is wide, so this test does not require it to be
// non-empty.
func TestSweepRunnerSetIsTheRest(t *testing.T) {
	t.Parallel()
	entries, integration := sweepHostOfTree(t)
	runner := SweepRunnerPackages(integration, entries)
	if !sort.StringsAreSorted(runner) {
		t.Errorf("runner set is not sorted: %v", runner)
	}
	listed := make(map[string]bool, len(entries))
	union := make(map[string]bool, len(entries)+len(runner))
	for _, e := range entries {
		listed[e.Package] = true
		union[e.Package] = true
	}
	for _, p := range runner {
		if listed[p] {
			t.Errorf("%s is in the runner set and in sweep-host.txt", p)
		}
		union[p] = true
	}
	for _, p := range integration {
		if !union[p] {
			t.Errorf("%s is an integration-tagged package in neither the runner set nor sweep-host.txt", p)
		}
	}
	if len(union) != len(integration) {
		t.Errorf("the two sets hold %d distinct packages and the tree has %d: they must be disjoint and cover every one", len(union), len(integration))
	}
}

// TestSweepHostCount keeps the pin in step with the manifest, so a shrink
// that forgot to lower it fails here rather than being papered over.
func TestSweepHostCount(t *testing.T) {
	t.Parallel()
	entries, _ := sweepHostOfTree(t)
	for _, f := range CheckSweepHostCount(len(entries), maxSweepHost) {
		t.Error(f)
	}
}

// TestSweepHostCountFixtures drives both red branches of the pin with fixture
// counts, so each is seen to fire without editing the real manifest.
func TestSweepHostCountFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		entries    int
		pin        int
		want       string
		wantSilent bool
	}{
		{name: "equal", entries: 3, pin: 3, wantSilent: true},
		{name: "above", entries: 4, pin: 3, want: "above its 3 pin"},
		{name: "below", entries: 2, pin: 3, want: "lower maxSweepHost"},
	} {
		got := strings.Join(CheckSweepHostCount(tc.entries, tc.pin), "\n")
		if tc.wantSilent {
			if got != "" {
				t.Errorf("%s: findings %q, want none", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: findings %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSweepHostFixtures drives the parser, both refusal branches and the
// runner set from literal lines, so every rule fires on input a reader can
// see, independently of the real file's contents.
func TestSweepHostFixtures(t *testing.T) {
	t.Parallel()
	entries := ParseSweepHost([]string{
		"# a whole-line comment",
		"",
		"internal/cmd # launchd plists",
		"internal/doctor",
		"internal/gone # stale line",
		"internal/installer #",
	})
	if len(entries) != 4 {
		t.Fatalf("parsed %d entries, want 4: %v", len(entries), entries)
	}
	wantLines := []int{3, 4, 5, 6}
	for i, e := range entries {
		if e.Line != wantLines[i] {
			t.Errorf("entry %d is line %d, want %d", i, e.Line, wantLines[i])
		}
	}

	got := strings.Join(CheckSweepHostPackages(entries, []string{"internal/cmd", "internal/doctor", "internal/installer"}), "\n")
	if !strings.Contains(got, "sweep-host.txt:5 names internal/gone") {
		t.Errorf("package findings %q lack the stale line and its number", got)
	}
	if strings.Contains(got, "internal/cmd") {
		t.Errorf("a discovered package was reported: %q", got)
	}

	reasons := strings.Join(CheckSweepHostReasons(entries), "\n")
	for _, w := range []string{"sweep-host.txt:4 names internal/doctor without", "sweep-host.txt:6 names internal/installer without"} {
		if !strings.Contains(reasons, w) {
			t.Errorf("reason findings %q lack %q", reasons, w)
		}
	}
	if strings.Contains(reasons, "internal/cmd") || strings.Contains(reasons, "internal/gone") {
		t.Errorf("a line with a reason was reported: %q", reasons)
	}

	// The discovery order puts the two runner packages out of order, so a
	// runner set that skips the sort is caught here rather than passing on a
	// discovery that happened to be sorted.
	runner := SweepRunnerPackages([]string{"internal/zz", "cmd/gt", "internal/doctor", "internal/cmd", "internal/aea"}, entries)
	if got, want := strings.Join(runner, ","), "cmd/gt,internal/aea,internal/zz"; got != want {
		t.Errorf("runner set = %q, want %q", got, want)
	}
}
