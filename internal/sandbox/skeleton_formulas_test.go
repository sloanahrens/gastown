package sandbox

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// formulaTown opens a town written by writeTown the way skeletonEntries does.
func formulaTown(t *testing.T, files map[string]string) townReader {
	t.Helper()
	root, err := os.OpenRoot(writeTown(t, files))
	if err != nil {
		t.Fatalf("open town root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return townReader{root: root}
}

func formulaFiles(entries []skeletonEntry) map[string]string {
	got := map[string]string{}
	for _, e := range entries {
		got[e.name] = string(e.data)
	}
	return got
}

func TestSkeletonFormulasReturnsEachFormula(t *testing.T) {
	t.Parallel()

	town := formulaTown(t, map[string]string{
		".beads/formulas/mol-polecat-work.formula.toml":   "formula = \"mol-polecat-work\"\n",
		".beads/formulas/mol-polecat-review.formula.toml": "formula = \"mol-polecat-review\"\n",
	})
	entries, err := skeletonFormulas(town)
	if err != nil {
		t.Fatalf("skeletonFormulas: %v", err)
	}
	want := map[string]string{
		"gt/.beads/formulas/mol-polecat-review.formula.toml": "formula = \"mol-polecat-review\"\n",
		"gt/.beads/formulas/mol-polecat-work.formula.toml":   "formula = \"mol-polecat-work\"\n",
	}
	if got := formulaFiles(entries); !maps.Equal(got, want) {
		t.Errorf("formulas = %q, want %q", got, want)
	}
}

func TestSkeletonFormulasSkipsOtherFiles(t *testing.T) {
	t.Parallel()

	town := formulaTown(t, map[string]string{
		".beads/formulas/mol-keep.formula.toml":       "formula = \"mol-keep\"\n",
		".beads/formulas/.installed.json":             "{}\n",
		".beads/formulas/mol-other.toml":              "formula = \"mol-other\"\n",
		".beads/formulas/mol-json.formula.json":       "{}\n",
		".beads/formulas/sub/mol-nested.formula.toml": "formula = \"mol-nested\"\n",
	})
	if err := os.Symlink("mol-keep.formula.toml", filepath.Join(town.root.Name(), ".beads/formulas/mol-linked.formula.toml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	entries, err := skeletonFormulas(town)
	if err != nil {
		t.Fatalf("skeletonFormulas: %v", err)
	}
	want := map[string]string{"gt/.beads/formulas/mol-keep.formula.toml": "formula = \"mol-keep\"\n"}
	if got := formulaFiles(entries); !maps.Equal(got, want) {
		t.Errorf("formulas = %q, want %q", got, want)
	}
}

func TestSkeletonFormulasAbsentDirectory(t *testing.T) {
	t.Parallel()

	town := formulaTown(t, map[string]string{"gastown/metadata.json": `{"rig": "gastown"}`})
	entries, err := skeletonFormulas(town)
	if err != nil || len(entries) != 0 {
		t.Errorf("skeletonFormulas = %q, %v; want no entries and no error", formulaFiles(entries), err)
	}
}

func TestSkeletonFormulasRefusesANonDirectoryPath(t *testing.T) {
	t.Parallel()

	for _, c := range [][2]string{
		{".beads", "read .beads/formulas"},
		{".beads/formulas", ".beads/formulas is not a directory"},
	} {
		_, err := skeletonFormulas(formulaTown(t, map[string]string{c[0]: "not a directory\n"}))
		if err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("skeletonFormulas error = %v, want one containing %q", err, c[1])
		}
	}
}

func TestSkeletonFormulasUnreadableDirectory(t *testing.T) {
	t.Parallel()

	town := formulaTown(t, map[string]string{".beads/formulas/mol-keep.formula.toml": "formula = \"mol-keep\"\n"})
	dir := filepath.Join(town.root.Name(), ".beads/formulas")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	//testpolicy:allow no-exec-files — a directory needs its search bit to be listed, so it is restored before the temporary town is removed
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := skeletonFormulas(town)
	if err == nil || !strings.Contains(err.Error(), ".beads/formulas") {
		t.Errorf("skeletonFormulas error = %v, want an error naming .beads/formulas", err)
	}
}

func TestSkeletonFormulasUnreadableFile(t *testing.T) {
	t.Parallel()

	town := formulaTown(t, map[string]string{".beads/formulas/mol-locked.formula.toml": "formula = \"mol-locked\"\n"})
	file := filepath.Join(town.root.Name(), ".beads/formulas/mol-locked.formula.toml")
	if err := os.Chmod(file, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })

	_, err := skeletonFormulas(town)
	if err == nil || !strings.Contains(err.Error(), ".beads/formulas/mol-locked.formula.toml") {
		t.Errorf("skeletonFormulas error = %v, want an error naming the unreadable formula", err)
	}
}
