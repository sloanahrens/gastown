package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// tempGitRepo initializes a throwaway git repo with a committed root file at
// dir.
func tempGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	mustGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating repo dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatalf("writing seed file: %v", err)
	}
	mustGit("init", "-q")
	mustGit("add", "root.txt")
	mustGit("commit", "-q", "-m", "init")
}

// newExposureTown lays out a minimal town root with one rig and five clones
// (mayor, refinery, both witness layouts, one polecat), each a real git repo
// holding an empty .beads/. The probe override is a map keyed by clone path so
// order in the enumeration never matters to the test. It wires beadProbes to
// the map and restores the real probe on cleanup, so tests set the per-clone
// outcome without fabricating broken git repos.
func newExposureTown(t *testing.T) (string, map[string]string, map[string]probeResult) {
	t.Helper()
	town := t.TempDir()
	rigPath := filepath.Join(town, "testrig")
	// A marker dir makes findAllRigs recognize testrig as a rig.
	if err := os.MkdirAll(filepath.Join(rigPath, "refinery"), 0o755); err != nil {
		t.Fatal(err)
	}
	clonePaths := map[string]string{}
	probes := map[string]probeResult{}
	for _, layout := range []struct{ name, sub string }{
		{"mayor", "mayor/rig"},
		{"refinery", "refinery/rig"},
		{"witness-rig", "witness/rig"},
		{"witness-dir", "witness"},
		{"polecat", "polecats/pc1/testrig"},
	} {
		clonePath := filepath.Join(rigPath, layout.sub)
		tempGitRepo(t, clonePath)
		if err := os.MkdirAll(filepath.Join(clonePath, ".beads"), 0o755); err != nil {
			t.Fatalf("creating clone %s: %v", layout.name, err)
		}
		clonePaths[layout.name] = clonePath
	}
	m := probes
	beadProbes = func(p string) probeResult {
		return m[p]
	}
	t.Cleanup(func() { beadProbes = beadsUntrackedAndUnignored })
	return town, clonePaths, probes
}

func TestBeadsExposureCheck_NoRigs(t *testing.T) {
	result := NewBeadsExposureCheck().Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK with no rigs, got %v (%s)", result.Status, result.Message)
	}
}

func TestBeadsExposureCheck_AllProtected(t *testing.T) {
	town, clonePaths, probes := newExposureTown(t)
	for p := range clonePaths {
		probes[p] = probeProtected
	}
	result := NewBeadsExposureCheck().Run(&CheckContext{TownRoot: town})
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when all protected, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "5 checked") {
		t.Errorf("expected all 5 clones checked, got: %s", result.Message)
	}
}

func TestBeadsExposureCheck_ExposedClone(t *testing.T) {
	town, clonePaths, probes := newExposureTown(t)
	probes[clonePaths["mayor"]] = probeExposed
	for p := range clonePaths {
		if _, set := probes[p]; !set {
			probes[p] = probeProtected
		}
	}
	result := NewBeadsExposureCheck().Run(&CheckContext{TownRoot: town})
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "1 clone(s)") {
		t.Errorf("expected one exposed clone, got: %s", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	wantRel := filepath.Join("testrig", "mayor", "rig")
	if !strings.Contains(joined, wantRel) {
		t.Errorf("details should name %q, got: %s", wantRel, joined)
	}
	if result.FixHint == "" {
		t.Error("expected a fix hint on warning")
	}

	// Fix must target exactly the exposed clone and write its exclude file.
	check := NewBeadsExposureCheck()
	check.Run(&CheckContext{TownRoot: town})
	if len(check.exposedClones) != 1 || check.exposedClones[0] != clonePaths["mayor"] {
		t.Fatalf("exposedClones = %v, want [%s]", check.exposedClones, clonePaths["mayor"])
	}
	if err := check.Fix(&CheckContext{TownRoot: town}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	exclude, err := os.ReadFile(filepath.Join(clonePaths["mayor"], ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("reading exclude after fix: %v", err)
	}
	if !strings.Contains(string(exclude), ".beads/") {
		t.Errorf("exclude file should contain .beads/ after Fix, got:\n%s", exclude)
	}
}

func TestBeadsExposureCheck_OnlyUnresolvedIsSkipped(t *testing.T) {
	town, clonePaths, probes := newExposureTown(t)
	probes[clonePaths["mayor"]] = probeUnresolved
	for p := range clonePaths {
		if _, set := probes[p]; !set {
			probes[p] = probeProtected
		}
	}
	result := NewBeadsExposureCheck().Run(&CheckContext{TownRoot: town})
	// A skipped check must never aggregate as a pass (gt-whvu).
	if result.Status != StatusSkipped {
		t.Fatalf("expected StatusSkipped (unknown), got %v: %s", result.Status, result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "UNKNOWN") {
		t.Errorf("details should mark the clone UNKNOWN, got: %s", joined)
	}
	if result.FixHint == "" {
		t.Error("expected a repair hint on skipped")
	}
}

func TestBeadsExposureCheck_ExposedPlusUnresolvedIsWarning(t *testing.T) {
	town, clonePaths, probes := newExposureTown(t)
	probes[clonePaths["mayor"]] = probeExposed
	probes[clonePaths["refinery"]] = probeUnresolved
	for p := range clonePaths {
		if _, set := probes[p]; !set {
			probes[p] = probeProtected
		}
	}
	result := NewBeadsExposureCheck().Run(&CheckContext{TownRoot: town})
	// Proven exposure outranks unknown: the fixable finding is the actionable one.
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "1 clone(s)") {
		t.Errorf("message should count only the proven exposure, got: %s", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "UNKNOWN") {
		t.Errorf("unresolved clone should still appear in details, got: %s", joined)
	}
}

func TestBeadsExposureCheck_NoBeadsDirs(t *testing.T) {
	town, clonePaths, _ := newExposureTown(t)
	for _, p := range clonePaths {
		if err := os.RemoveAll(filepath.Join(p, ".beads")); err != nil {
			t.Fatal(err)
		}
	}
	result := NewBeadsExposureCheck().Run(&CheckContext{TownRoot: town})
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "No .beads/") {
		t.Errorf("expected no-.beads message, got: %s", result.Message)
	}
}

func TestBeadsExposureCheck_FixSkipsUnresolved(t *testing.T) {
	town, clonePaths, probes := newExposureTown(t)
	probes[clonePaths["mayor"]] = probeUnresolved
	probes[clonePaths["refinery"]] = probeUnresolved
	for p := range clonePaths {
		if _, set := probes[p]; !set {
			probes[p] = probeProtected
		}
	}
	check := NewBeadsExposureCheck()
	check.Run(&CheckContext{TownRoot: town})
	// Nothing proven exposed, so Fix must touch nothing.
	if len(check.exposedClones) != 0 {
		t.Fatalf("exposedClones = %v, want empty", check.exposedClones)
	}
	if err := check.Fix(&CheckContext{TownRoot: town}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
}

// TestBeadsExposureProbe_FailingGit is a real-git regression guard for the
// fail-closed change: a repo whose git status fails must come back
// probeUnresolved, never probeProtected.
func TestBeadsExposureProbe_FailingGit(t *testing.T) {
	dir := t.TempDir()
	tempGitRepo(t, dir)
	// Corrupt the refs index so `git status` errors out.
	if err := os.WriteFile(filepath.Join(dir, ".git", "packed-refs"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := beadsUntrackedAndUnignored(dir); got != probeUnresolved {
		t.Errorf("corrupt repo: got %v, want probeUnresolved", got)
	}
	// No git repo at all (no .git) also fails the status call.
	empty := t.TempDir()
	if got := beadsUntrackedAndUnignored(empty); got != probeUnresolved {
		t.Errorf("non-repo dir: got %v, want probeUnresolved", got)
	}
}

// TestBeadsExposureProbe_RealGit exercises detection against live git: an
// untracked .beads/ is exposed; the same .beads/ protected by info/exclude is
// not. A marker file makes the untracked directory non-empty so git status
// reports it.
func TestBeadsExposureProbe_RealGit(t *testing.T) {
	dir := t.TempDir()
	tempGitRepo(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "marker"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := beadsUntrackedAndUnignored(dir); got != probeExposed {
		t.Fatalf("untracked .beads: got %v, want probeExposed", got)
	}
	if err := rig.EnsureLocalExcludePatterns(dir); err != nil {
		t.Fatalf("EnsureLocalExcludePatterns: %v", err)
	}
	if got := beadsUntrackedAndUnignored(dir); got != probeProtected {
		t.Errorf("exclude-protected .beads: got %v, want probeProtected", got)
	}
}

// TestFindBeadsClones_IncludesWitness verifies enumeration covers the witness
// clone in both layouts (witness/rig legacy clone and the witness dir).
func TestFindBeadsClones_IncludesWitness(t *testing.T) {
	rigPath := t.TempDir()
	for _, sub := range []string{"mayor/rig", "refinery/rig", "witness/rig", "witness", "polecats/pc1/testrig"} {
		if err := os.MkdirAll(filepath.Join(rigPath, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var witnessHits int
	seen := map[string]bool{}
	for _, p := range findBeadsClones(rigPath) {
		if seen[p] {
			t.Errorf("duplicate clone path in enumeration: %s", p)
		}
		seen[p] = true
		rel, _ := filepath.Rel(rigPath, p)
		if strings.HasPrefix(rel, "witness") {
			witnessHits++
		}
	}
	if witnessHits != 2 {
		t.Errorf("expected both witness layouts enumerated, got %d witness paths", witnessHits)
	}
}
