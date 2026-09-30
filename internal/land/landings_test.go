package land

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLandingsFileAppendsJSONLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(town, ".runtime", "landings", "gastown.jsonl"); f.Path != want {
		t.Fatalf("path = %s, want %s", f.Path, want)
	}
	for _, c := range []string{"aaa", "bbb"} {
		if err := f.Append(LandingRecord{BeadID: "gt-" + c, LandedCommit: c}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil || rec.LandedCommit != "bbb" {
		t.Fatalf("second line = %q (%v)", lines[1], err)
	}
	if info, _ := os.Stat(f.Path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(f.Path)); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode().Perm())
	}
}

func TestLandingsFileRefusesUnsafeDirAndNames(t *testing.T) {
	t.Parallel()
	if _, err := RigLandingsFile(t.TempDir(), "../evil"); err == nil {
		t.Error("rig name with a path separator accepted")
	}
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is a directory, not a file: the test needs a world-writable landings dir
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{}); err == nil {
		t.Error("world-writable landings dir accepted")
	}
	town2 := t.TempDir()
	dir2 := filepath.Join(town2, ".runtime", "landings")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(town2, "elsewhere"), filepath.Join(dir2, "gastown.jsonl")); err != nil {
		t.Fatal(err)
	}
	f2, _ := RigLandingsFile(town2, "gastown")
	if err := f2.Append(LandingRecord{}); err == nil {
		t.Error("symlinked landings file accepted")
	}
}
