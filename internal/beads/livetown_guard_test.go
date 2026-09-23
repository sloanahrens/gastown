package beads

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// forbiddenTownFixture creates a stand-in live town and a directory inside it.
func forbiddenTownFixture(t *testing.T) (town, inner string) {
	t.Helper()
	town = t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"type":"town","name":"live"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inner = filepath.Join(town, "gastown", "polecats", "quartz", "gastown")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	return town, inner
}

// assertRefusedLoudly runs fn and fails unless it panics, which is how
// workspace.RefuseForbiddenRoot refuses inside a test binary.
func assertRefusedLoudly(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s reached the live town instead of refusing loudly", what)
		}
	}()
	fn()
}

// TestFindTownRootRefusesForbiddenTown pins the gt-dr664 guard: the beads
// client's own walk-up used to resolve the live town root regardless of the
// harness, because it never consulted the forbidden-root variable that
// workspace.Find honors.
func TestFindTownRootRefusesForbiddenTown(t *testing.T) {
	town, inner := forbiddenTownFixture(t)

	if got := FindTownRoot(inner); got != town {
		t.Fatalf("FindTownRoot without the guard = %q, want %q", got, town)
	}

	t.Setenv(workspace.EnvForbiddenTownRoot, town)
	assertRefusedLoudly(t, "FindTownRoot", func() { _ = FindTownRoot(inner) })

	// A fixture town outside the forbidden root still resolves.
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "mayor", "town.json"), []byte(`{"type":"town","name":"fixture"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindTownRoot(elsewhere); got != elsewhere {
		t.Errorf("FindTownRoot(fixture) = %q, want %q", got, elsewhere)
	}
}

// TestBeadsClientRefusesForbiddenTownPaths pins that no constructor can point a
// client at the live town, where every subsequent call would open production
// beads and the Dolt server behind them in-process (gt-dr664).
func TestBeadsClientRefusesForbiddenTownPaths(t *testing.T) {
	town, inner := forbiddenTownFixture(t)
	t.Setenv(workspace.EnvForbiddenTownRoot, town)

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"worktree", inner},
		{"beads dir", filepath.Join(town, ".beads")},
		{"town root", town},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRefusedLoudly(t, "New("+tc.dir+")", func() { _ = New(tc.dir) })
			assertRefusedLoudly(t, "NewIsolated("+tc.dir+")", func() { _ = NewIsolated(tc.dir) })
			assertRefusedLoudly(t, "NewWithBeadsDir", func() { _ = NewWithBeadsDir(inner, tc.dir) })
		})
	}

	// Outside the forbidden root nothing changes.
	_ = New(t.TempDir())
}
