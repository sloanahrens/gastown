package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envWith is a process with the given environment and no working directory.
func envWith(kv map[string]string) procEnv {
	return procEnv{
		getenv: func(k string) string { return kv[k] },
		getwd:  func() (string, error) { return "", errors.New("getwd: no such file or directory") },
	}
}

// forbidding is a process whose hermetic harness forbids town.
func forbidding(town string) procEnv {
	return envWith(map[string]string{EnvForbiddenTownRoot: town})
}

// makeTown creates a directory with the primary workspace marker.
func makeTown(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(`{"type":"town","name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFind_ForbiddenRootResolvesToNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	inner := filepath.Join(town, "rig", "polecats", "p", "repo", "internal", "pkg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	// Without the guard the town resolves normally.
	if root, err := envWith(nil).find(inner); err != nil || root != town {
		t.Fatalf("Find without guard = %q, %v; want %q", root, err, town)
	}

	root, err := forbidding(town).find(inner)
	if err != nil {
		t.Fatalf("Find with guard: %v", err)
	}
	if root != "" {
		t.Errorf("Find resolved forbidden town: %q", root)
	}
}

func TestFind_ForbiddenGuardRejectsInnerMarkersToo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	// A rig inside the town has its own mayor/ directory (secondary marker);
	// the guard must not fall back to it.
	rig := filepath.Join(town, "gastown")
	makeTown(t, rig)
	inner := filepath.Join(rig, "internal", "pkg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := forbidding(town).find(inner)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if root != "" {
		t.Errorf("Find resolved a root inside the forbidden town: %q", root)
	}
}

func TestFind_ForbiddenGuardLeavesFixtureTownsAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)

	root, err := forbidding(filepath.Join(t.TempDir(), "other-town")).find(town)
	if err != nil || root != town {
		t.Errorf("Find = %q, %v; fixture town outside the forbidden root must resolve", root, err)
	}
}

func TestIsWorkspace_ForbiddenRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)

	if ok, _ := envWith(nil).isWorkspace(town); !ok {
		t.Fatal("fixture town not recognized without guard")
	}
	if ok, _ := forbidding(town).isWorkspace(town); ok {
		t.Error("IsWorkspace acknowledged the forbidden town")
	}
}

func TestForbiddenRootPredicate(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	inside := filepath.Join(town, "gastown", "polecats", "quartz")

	none := envWith(nil)
	if got := none.forbiddenTownRoot(); got != "" {
		t.Fatalf("ForbiddenTownRoot() = %q without a harness, want empty", got)
	}
	if none.isForbiddenRoot(town) || none.isForbiddenRoot(inside) {
		t.Fatal("IsForbiddenRoot matched without a harness set")
	}

	e := forbidding(town)
	if got := e.forbiddenTownRoot(); got != town {
		t.Errorf("ForbiddenTownRoot() = %q, want %q", got, town)
	}
	for _, dir := range []string{town, inside} {
		if !e.isForbiddenRoot(dir) {
			t.Errorf("IsForbiddenRoot(%q) = false, want true", dir)
		}
	}
	// A sibling whose path merely shares the prefix is not inside the town.
	if e.isForbiddenRoot(town + "-other") {
		t.Error("IsForbiddenRoot matched a path that only shares the prefix")
	}
	if e.isForbiddenRoot("") {
		t.Error("IsForbiddenRoot(\"\") = true")
	}
}

// TestRefuseForbiddenRootFailsLoudUnderTest pins the refusal policy: in a test
// binary a resolver that lands on the live town panics at its call site, which
// is what makes a leak impossible to miss (gt-dr664).
func TestRefuseForbiddenRootFailsLoudUnderTest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	inside := filepath.Join(town, "gastown")

	none := envWith(nil)
	if err := none.refuseForbiddenRoot("fixture", inside); err != nil {
		t.Fatalf("RefuseForbiddenRoot outside the guard = %v, want nil", err)
	}
	if err := none.guardForbiddenRoot("fixture", inside); err != nil {
		t.Fatalf("GuardForbiddenRoot outside the guard = %v, want nil", err)
	}

	e := forbidding(town)
	if err := e.guardForbiddenRoot("fixture", inside); !errors.Is(err, ErrForbiddenTownRoot) {
		t.Errorf("GuardForbiddenRoot = %v, want ErrForbiddenTownRoot", err)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RefuseForbiddenRoot did not fail loud in a test binary")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "HERMETIC VIOLATION") || !strings.Contains(msg, inside) {
			t.Errorf("panic message must name the violation and the path, got: %s", msg)
		}
	}()
	_ = e.refuseForbiddenRoot("fixture", inside)
}

// The exported guards read the process environment: for a fixture town in a
// temp dir, which no harness forbids, they agree with an unguarded process.
func TestForbiddenRootExportedWrappers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	if got, want := ForbiddenTownRoot(), os.Getenv(EnvForbiddenTownRoot); got != want {
		t.Errorf("ForbiddenTownRoot() = %q, want the process's %q", got, want)
	}
	if IsForbiddenRoot(town) {
		t.Errorf("IsForbiddenRoot(%q) = true for a temp fixture town", town)
	}
	if err := GuardForbiddenRoot("fixture", town); err != nil {
		t.Errorf("GuardForbiddenRoot = %v, want nil", err)
	}
	if err := RefuseForbiddenRoot("fixture", town); err != nil {
		t.Errorf("RefuseForbiddenRoot = %v, want nil", err)
	}
}
