package seed

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestSeedPinsNameEverySeededRig: the Makefile's POLE_SEED_RIGS default and
// deploy/polecat/seed-pins name the same rigs. The image's seed RUN and the
// shared cache's seed Job both read the pins file alone, so a rig the default
// names without a pin line is a rig neither seeds (hq-smfiv).
func TestSeedPinsNameEverySeededRig(t *testing.T) {
	t.Parallel()
	root := seedRepoRoot(t)
	rigs := seedRigsDefault(t, filepath.Join(root, "Makefile"))
	if !slices.Contains(rigs, "beads") {
		t.Errorf("the Makefile's POLE_SEED_RIGS default (%q) omits beads: no other seeded rig needs github.com/DATA-DOG/go-sqlmock, so a beads polecat cannot build internal/storage/uow offline (hq-smfiv)", rigs)
	}
	pins, err := Pins(seedRepoFile(t, filepath.Join(root, "deploy", "polecat", "seed-pins")))
	if err != nil {
		t.Fatalf("deploy/polecat/seed-pins: %v", err)
	}
	pinned := make(map[string]string, len(pins))
	for _, p := range pins {
		pinned[p.Rig] = p.Commit
	}
	for _, rig := range rigs {
		if _, ok := pinned[rig]; !ok {
			t.Errorf("rig %q is in the Makefile's POLE_SEED_RIGS default but has no line in deploy/polecat/seed-pins, so neither the image nor the shared cache seeds its modules", rig)
		}
	}
	for _, p := range pins {
		if !slices.Contains(rigs, p.Rig) {
			t.Errorf("deploy/polecat/seed-pins pins rig %q, which the Makefile's POLE_SEED_RIGS default does not name: the next make polecat-seed-pins drops that line", p.Rig)
		}
	}
}

// seedRepoRoot is the repository root, found from this file's directory so the
// test reads the same tree however it is invoked.
func seedRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("look for %s: %v", filepath.Join(dir, "go.mod"), err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod in %s or any directory above it", filepath.Dir(file))
		}
		dir = parent
	}
}

// seedRigsDefault is the rig list the Makefile's POLE_SEED_RIGS assignment
// names, not a recipe line that expands the variable.
func seedRigsDefault(t *testing.T, path string) []string {
	t.Helper()
	for _, line := range strings.Split(string(seedRepoFile(t, path)), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "POLE_SEED_RIGS ?=")
		if ok {
			return strings.Fields(rest)
		}
	}
	t.Fatalf("%s has no POLE_SEED_RIGS ?= default", path)
	return nil
}

func seedRepoFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
