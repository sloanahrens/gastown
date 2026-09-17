package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// addContainerBackedPackage adds a tiny passing package at internal/cmd to
// the verify test module. Its import path ends in "/internal/cmd", which is
// on containerSuitePackages, so the gate must treat it as container-backed
// even though this stand-in spins nothing.
func addContainerBackedPackage(t *testing.T, dir string) {
	t.Helper()
	pkg := filepath.Join(dir, "internal", "cmd")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "cmd.go"), []byte("package cmd\n\nfunc Name() string { return \"cmd\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "cmd_test.go"), []byte("package cmd\n\nimport \"testing\"\n\nfunc TestName(t *testing.T) {\n\tif Name() != \"cmd\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func changePkga(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pkga", "a.go"), []byte("package pkga\n\nfunc Add(a, b int) int { return a + b }\nfunc Triple(a int) int { return a * 3 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunDefaultTestVerification_DefersContainerPackages covers gt-yihz: the
// Docker suite runs once per submission, in the refinery's gate, so gt done's
// own gate leaves container-backed packages alone and needs no slot for the
// rest.
func TestRunDefaultTestVerification_DefersContainerPackages(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	t.Run("container-backed package deferred, plain package runs without a slot", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga and internal/cmd")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/defer-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if len(result.packages) != 1 || !strings.HasSuffix(result.packages[0], "/pkga") {
			t.Errorf("packages = %v, want only pkga", result.packages)
		}
		if len(result.deferredPackages) != 1 || !strings.HasSuffix(result.deferredPackages[0], "/internal/cmd") {
			t.Errorf("deferredPackages = %v, want only internal/cmd", result.deferredPackages)
		}
		if result.slotUsed {
			t.Error("slotUsed = true, want false when nothing in scope spins containers")
		}
		if acquired {
			t.Error("the gate acquired a container-gate slot for a container-free package set")
		}
		log, _ := os.ReadFile(result.logPath)
		if !strings.Contains(string(log), "deferred to the refinery gate") || !strings.Contains(string(log), "container-gate slot: not needed") {
			t.Errorf("verify log does not explain the deferral / no-slot decision:\n%s", log)
		}
	})

	t.Run("only container-backed packages changed: skips with reason, takes no slot", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "only internal/cmd")

		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			t.Error("slot acquired although every changed package was deferred")
			return func() {}, nil
		}, nil)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/all-deferred-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.ran {
			t.Errorf("ran = true, want false: %+v", result)
		}
		if !strings.Contains(result.skipReason, "refinery") || !strings.Contains(result.skipReason, "internal/cmd") {
			t.Errorf("skipReason = %q, want the refinery-owns-it explanation naming the package", result.skipReason)
		}
		if len(result.deferredPackages) != 1 {
			t.Errorf("deferredPackages = %v, want the one container package", result.deferredPackages)
		}
	})

	t.Run("test_verify_include_container_packages restores the slot-gated full run", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch both")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		include := true
		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", TestVerifyIncludeContainerPackages: &include}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/include-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success || len(result.packages) != 2 {
			t.Fatalf("result = %+v, want both packages run", result)
		}
		if len(result.deferredPackages) != 0 || !result.slotUsed || !acquired {
			t.Errorf("include flag: deferred=%v slotUsed=%v acquired=%v, want none/true/true", result.deferredPackages, result.slotUsed, acquired)
		}
	})
}

func TestIsContainerSuitePackage(t *testing.T) {
	for path, want := range map[string]bool{
		"github.com/steveyegge/gastown/internal/cmd":       true,
		"github.com/steveyegge/gastown/internal/beads":     true,
		"internal/refinery":                                true,
		"github.com/steveyegge/gastown/internal/slot":      false,
		"github.com/steveyegge/gastown/internal/cmd/extra": true, // sub-packages inherit the container scope
		"example.test/pkga":                                false,
	} {
		if got := isContainerSuitePackage(path); got != want {
			t.Errorf("isContainerSuitePackage(%q) = %v, want %v", path, got, want)
		}
	}
}
