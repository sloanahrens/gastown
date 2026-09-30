package patrolscan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeExec(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the check under test keys on the execute bit; the file is inert text and is never run
	if err := os.WriteFile(path, []byte("inert: not a program\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestRogueBDNeutralizesFindingsAndSparesBuildOutput(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rogue := filepath.Join(town, "gastown", "polecats", "ruby", "gastown", "bd")
	built := filepath.Join(town, "beads", "crew", "sloan", "bd")
	inert := filepath.Join(town, "gastown", "polecats", "onyx", "bd")
	tooDeep := filepath.Join(town, "gastown", "polecats", "a", "b", "c", "d", "bd")
	shared := filepath.Join(town, "shared-bd")
	link := filepath.Join(town, "gastown", "crew", "max", "bd")
	writeExec(t, rogue, 0o755)
	writeExec(t, built, 0o755)
	writeExec(t, inert, 0o644)
	writeExec(t, tooDeep, 0o755)
	writeExec(t, shared, 0o755)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}

	r := CheckRogueBD(RogueBDAggregates(town), RogueBDOptions{
		IsBuildOutput: func(p string) (bool, error) { return p == built, nil },
	})
	if len(r.WalkErrors) != 0 {
		t.Fatalf("walk errors: %v", r.WalkErrors)
	}
	if got := modeOf(t, rogue).Perm(); got != 0o644 {
		t.Errorf("rogue bd mode = %v, want 0644", got)
	}
	if got := modeOf(t, built).Perm(); got != 0o755 {
		t.Errorf("build output touched: mode %v", got)
	}
	if got := modeOf(t, tooDeep).Perm(); got != 0o755 {
		t.Errorf("walk went past depth %d: mode %v", rogueBDMaxDepth, got)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink not removed: %v", err)
	}
	if got := modeOf(t, shared).Perm(); got != 0o755 {
		t.Errorf("symlink target was modified: mode %v (chmod followed the link)", got)
	}
	if n := len(r.Findings()); n != 2 {
		t.Errorf("findings = %+v, want the rogue file and the link", r.Findings())
	}
	if r.Clean() {
		t.Error("a run with findings reads as clean")
	}
}

func TestRogueBDOnPathIsAlwaysAFinding(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	bd := filepath.Join(town, "beads", "crew", "sloan", "bd")
	writeExec(t, bd, 0o755)
	r := CheckRogueBD(RogueBDAggregates(town), RogueBDOptions{
		PathDirs:      []string{filepath.Dir(bd)},
		IsBuildOutput: func(string) (bool, error) { return true, nil },
	})
	if f := r.Findings(); len(f) != 1 || !f[0].OnPath || f[0].Verdict != RogueBDNeutralized {
		t.Fatalf("findings = %+v", f)
	}
}

func TestRogueBDUnknownClassificationLeavesFileAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	bd := filepath.Join(town, "gastown", "polecats", "ruby", "bd")
	writeExec(t, bd, 0o755)
	r := CheckRogueBD(RogueBDAggregates(town), RogueBDOptions{
		IsBuildOutput: func(string) (bool, error) { return false, errors.New("git: timeout") },
	})
	if got := modeOf(t, bd).Perm(); got != 0o755 {
		t.Fatalf("an unclassified bd was neutralized: mode %v", got)
	}
	if f := r.Findings(); len(f) != 1 || f[0].Verdict != RogueBDUnknown {
		t.Fatalf("findings = %+v", f)
	}
}

func TestRogueBDEmptyTownIsClean(t *testing.T) {
	t.Parallel()
	if r := CheckRogueBD(RogueBDAggregates(t.TempDir()), RogueBDOptions{}); !r.Clean() {
		t.Fatalf("empty town not clean: %+v", r)
	}
}
