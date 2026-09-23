package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	town := t.TempDir()
	makeTown(t, town)
	inner := filepath.Join(town, "rig", "polecats", "p", "repo", "internal", "pkg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	// Without the guard the town resolves normally.
	if root, err := Find(inner); err != nil || root != town {
		t.Fatalf("Find without guard = %q, %v; want %q", root, err, town)
	}

	t.Setenv(EnvForbiddenTownRoot, town)
	root, err := Find(inner)
	if err != nil {
		t.Fatalf("Find with guard: %v", err)
	}
	if root != "" {
		t.Errorf("Find resolved forbidden town: %q", root)
	}
}

func TestFind_ForbiddenGuardRejectsInnerMarkersToo(t *testing.T) {
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

	t.Setenv(EnvForbiddenTownRoot, town)
	root, err := Find(inner)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if root != "" {
		t.Errorf("Find resolved a root inside the forbidden town: %q", root)
	}
}

func TestFind_ForbiddenGuardLeavesFixtureTownsAlone(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)

	t.Setenv(EnvForbiddenTownRoot, filepath.Join(t.TempDir(), "other-town"))
	root, err := Find(town)
	if err != nil || root != town {
		t.Errorf("Find = %q, %v; fixture town outside the forbidden root must resolve", root, err)
	}
}

func TestIsWorkspace_ForbiddenRoot(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)

	if ok, _ := IsWorkspace(town); !ok {
		t.Fatal("fixture town not recognized without guard")
	}
	t.Setenv(EnvForbiddenTownRoot, town)
	if ok, _ := IsWorkspace(town); ok {
		t.Error("IsWorkspace acknowledged the forbidden town")
	}
}

func TestForbiddenRootPredicate(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)
	inside := filepath.Join(town, "gastown", "polecats", "quartz")

	if got := ForbiddenTownRoot(); got != "" {
		t.Fatalf("ForbiddenTownRoot() = %q without a harness, want empty", got)
	}
	if IsForbiddenRoot(town) || IsForbiddenRoot(inside) {
		t.Fatal("IsForbiddenRoot matched without a harness set")
	}

	t.Setenv(EnvForbiddenTownRoot, town)
	if got := ForbiddenTownRoot(); got != town {
		t.Errorf("ForbiddenTownRoot() = %q, want %q", got, town)
	}
	for _, dir := range []string{town, inside} {
		if !IsForbiddenRoot(dir) {
			t.Errorf("IsForbiddenRoot(%q) = false, want true", dir)
		}
	}
	// A sibling whose path merely shares the prefix is not inside the town.
	if IsForbiddenRoot(town + "-other") {
		t.Error("IsForbiddenRoot matched a path that only shares the prefix")
	}
	if IsForbiddenRoot("") {
		t.Error("IsForbiddenRoot(\"\") = true")
	}
}

// TestRefuseForbiddenRootFailsLoudUnderTest pins the refusal policy: in a test
// binary a resolver that lands on the live town panics at its call site, which
// is what makes a leak impossible to miss (gt-dr664).
func TestRefuseForbiddenRootFailsLoudUnderTest(t *testing.T) {
	town := t.TempDir()
	makeTown(t, town)
	inside := filepath.Join(town, "gastown")

	if err := RefuseForbiddenRoot("fixture", inside); err != nil {
		t.Fatalf("RefuseForbiddenRoot outside the guard = %v, want nil", err)
	}
	if err := GuardForbiddenRoot("fixture", inside); err != nil {
		t.Fatalf("GuardForbiddenRoot outside the guard = %v, want nil", err)
	}

	t.Setenv(EnvForbiddenTownRoot, town)
	if err := GuardForbiddenRoot("fixture", inside); !errors.Is(err, ErrForbiddenTownRoot) {
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
	_ = RefuseForbiddenRoot("fixture", inside)
}
