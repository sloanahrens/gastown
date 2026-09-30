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

func TestLandingsFileFind(t *testing.T) {
	t.Parallel()
	f, _ := RigLandingsFile(t.TempDir(), "gastown")
	if _, ok, err := f.Find("gt-a", "h1"); ok || err != nil {
		t.Fatalf("Find on a missing file = %v, %v", ok, err)
	}
	for _, r := range []LandingRecord{{BeadID: "gt-a", Head: "h0", LandedCommit: "c0"}, {BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}, {BeadID: "gt-b", Head: "h1", LandedCommit: "c2"}} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := f.Find("gt-a", "h1")
	if err != nil || !ok || got.LandedCommit != "c1" {
		t.Fatalf("Find = %+v, %v, %v", got, ok, err)
	}
}

func TestLandingsLatestForBeadAndRecent(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.LatestForBead("gt-a"); ok || err != nil {
		t.Fatalf("absent file: ok=%v err=%v", ok, err)
	}
	for _, r := range []LandingRecord{
		{BeadID: "gt-a", Head: "h1", LandedCommit: "l1"},
		{BeadID: "gt-b", Head: "h2", LandedCommit: "l2"},
		{BeadID: "gt-a", Head: "h3", LandedCommit: "l3"},
	} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	rec, ok, err := f.LatestForBead("gt-a")
	if err != nil || !ok || rec.Head != "h3" {
		t.Fatalf("LatestForBead = %+v %v %v; want the h3 record", rec, ok, err)
	}
	recent, err := f.Recent(2)
	if err != nil || len(recent) != 2 || recent[0].BeadID != "gt-b" || recent[1].Head != "h3" {
		t.Fatalf("Recent(2) = %+v %v", recent, err)
	}
	// Find keeps its exact bead+head match.
	if rec, ok, _ := f.Find("gt-a", "h1"); !ok || rec.LandedCommit != "l1" {
		t.Fatalf("Find(gt-a, h1) = %+v %v", rec, ok)
	}
}
