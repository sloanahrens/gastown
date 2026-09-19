package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// initVerifyTestGoRepo builds a tiny Go module with two packages (pkga,
// pkgb) at HEAD "base", so tests can add commits on top and diff against
// that base to exercise changedGoPackages / runDefaultTestVerification.
func initVerifyTestGoRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	mustWrite("go.mod", "module example.test\n\ngo 1.21\n")
	mustWrite("pkga/a.go", "package pkga\n\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite("pkga/a_test.go", "package pkga\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n")
	mustWrite("pkgb/b.go", "package pkgb\n\nfunc Double(a int) int { return a * 2 }\n")
	mustWrite("pkgb/b_test.go", "package pkgb\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"bad double\")\n\t}\n}\n")

	runGit("add", ".")
	runGit("commit", "-q", "-m", "base")
	base = runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", base)
	return dir, base
}

// stubVerifyGate replaces the gate's slot-acquire and suite-runner hooks for
// the duration of a test, so the gate can be driven without the real
// container-gate slot (which would contend with every other suite on a shared
// Gas Town host) and without running `go test` over a real package tree.
func stubVerifyGate(
	t *testing.T,
	acquire func(townRoot, role string, timeout time.Duration) (func(), error),
	run func(ctx context.Context, worktree, script string, env []string, logFile *os.File) error,
) {
	t.Helper()
	if acquire != nil {
		prev := acquireVerifySlot
		acquireVerifySlot = acquire
		t.Cleanup(func() { acquireVerifySlot = prev })
	}
	if run != nil {
		prev := runVerifySuite
		runVerifySuite = run
		t.Cleanup(func() { runVerifySuite = prev })
	}
}

// stubVerifyProgress shortens the gate's progress interval so a test can
// observe progress lines without waiting out the real one.
func stubVerifyProgress(t *testing.T, interval time.Duration) {
	t.Helper()
	prev := testVerifyProgressInterval
	testVerifyProgressInterval = interval
	t.Cleanup(func() { testVerifyProgressInterval = prev })
}

func TestResolveTestVerifyBudgets(t *testing.T) {

	writeMakefile := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	makefileBody := "test:\n\tgo test -timeout 20m ./...\n"

	t.Run("defaults scale with the changed-package count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir() // no Makefile
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test ./..."}, dir, "go test ./...", 2)
		if b.perPackage != defaultTestVerifyPerPackageTimeout {
			t.Errorf("perPackage = %s, want the %s default", b.perPackage, defaultTestVerifyPerPackageTimeout)
		}
		// 20m × 2 packages = 40m, above the 30m floor.
		if b.runTimeout != 40*time.Minute {
			t.Errorf("runTimeout = %s, want 40m (20m per package × 2)", b.runTimeout)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want %s", b.slotTimeout, defaultTestVerifySlotTimeout)
		}
		if !strings.Contains(b.runSource, "2 changed package(s)") {
			t.Errorf("runSource = %q, want it to explain the scaling", b.runSource)
		}
	})

	t.Run("the 30m floor applies to a single cheap package", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test ./..."}, dir, "go test ./...", 1)
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the %s floor (gt-pnkd: a 10m budget was the bug)", b.runTimeout, defaultTestVerifyRunFloor)
		}
	})

	t.Run("the Makefile test target's -timeout wins over the default", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("make"); err != nil {
			t.Skip("make not available")
		}
		dir := writeMakefile(t, makefileBody)
		mq := &config.MergeQueueConfig{TestCommand: "GOFLAGS=-p=6 make test"}
		b := resolveTestVerifyBudgets(mq, dir, mq.TestCommand, 2)
		if b.perPackage != 20*time.Minute {
			t.Errorf("perPackage = %s, want 20m read from the Makefile", b.perPackage)
		}
		if !strings.Contains(b.perSource, "Makefile") {
			t.Errorf("perSource = %q, want it to name the Makefile", b.perSource)
		}
		if b.runTimeout != 40*time.Minute {
			t.Errorf("runTimeout = %s, want 40m", b.runTimeout)
		}
	})

	t.Run("a -timeout in test_command is used when there is no Makefile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test -timeout 25m ./..."}, dir, "go test -timeout 25m ./...", 2)
		if b.perPackage != 25*time.Minute {
			t.Errorf("perPackage = %s, want 25m from test_command", b.perPackage)
		}
		if b.runTimeout != 50*time.Minute {
			t.Errorf("runTimeout = %s, want 50m", b.runTimeout)
		}
	})

	t.Run("rig config overrides both budgets", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "90m",
			TestVerifySlotTimeout: "15m",
		}
		b := resolveTestVerifyBudgets(mq, dir, mq.TestCommand, 3)
		if b.runTimeout != 90*time.Minute {
			t.Errorf("runTimeout = %s, want 90m", b.runTimeout)
		}
		if b.slotSource != "merge_queue.test_verify_slot_timeout" {
			t.Errorf("slotSource = %q", b.slotSource)
		}
		if b.slotTimeout != 15*time.Minute {
			t.Errorf("slotTimeout = %s, want 15m", b.slotTimeout)
		}
	})

	t.Run("invalid durations fall back and say so", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "banana",
			TestVerifySlotTimeout: "-5m",
		}
		b := resolveTestVerifyBudgets(mq, dir, mq.TestCommand, 1)
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the derived floor", b.runTimeout)
		}
		if !strings.Contains(b.runSource, "banana") {
			t.Errorf("runSource = %q, want it to flag the rejected override", b.runSource)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want the default", b.slotTimeout)
		}
		if !strings.Contains(b.slotSource, "-5m") {
			t.Errorf("slotSource = %q, want it to flag the rejected override", b.slotSource)
		}
	})
}

func TestSplitCommandEnvPrefixAndMakeTarget(t *testing.T) {

	tests := []struct {
		command  string
		wantEnv  []string
		wantArgv []string
		wantMake string
		wantOK   bool
	}{
		{"GOFLAGS=-p=6 make test", []string{"GOFLAGS=-p=6"}, []string{"make", "test"}, "test", true},
		{"FOO=1 BAR=2 make -j4 build", []string{"FOO=1", "BAR=2"}, []string{"make", "-j4", "build"}, "build", true},
		{"make", nil, []string{"make"}, "test", true},
		{"go test ./...", nil, []string{"go", "test", "./..."}, "", false},
		{"sh -c 'FOO=1 make test'", nil, []string{"sh", "-c", "'FOO=1", "make", "test'"}, "", false},
		{"1=1 make test", nil, []string{"1=1", "make", "test"}, "", false},
		{"make FOO=bar test", nil, []string{"make", "FOO=bar", "test"}, "test", true},
	}

	for _, tc := range tests {
		env, argv := splitCommandEnvPrefix(tc.command)
		if strings.Join(env, ",") != strings.Join(tc.wantEnv, ",") {
			t.Errorf("splitCommandEnvPrefix(%q) env = %v, want %v", tc.command, env, tc.wantEnv)
		}
		if strings.Join(argv, ",") != strings.Join(tc.wantArgv, ",") {
			t.Errorf("splitCommandEnvPrefix(%q) argv = %v, want %v", tc.command, argv, tc.wantArgv)
		}
		target, ok := makeTarget(tc.command)
		if ok != tc.wantOK || target != tc.wantMake {
			t.Errorf("makeTarget(%q) = (%q, %v), want (%q, %v)", tc.command, target, ok, tc.wantMake, tc.wantOK)
		}
	}
}

func TestTimeoutFromCommandText(t *testing.T) {

	t.Run("reads both flag spellings and keeps the largest", func(t *testing.T) {
		t.Parallel()
		d, ok := timeoutFromCommandText("go test -timeout=5m ./a\ngo test -timeout 20m ./b\n")
		if !ok || d != 20*time.Minute {
			t.Errorf("= (%s, %v), want (20m, true) — the budget must cover the slowest invocation", d, ok)
		}
	})

	t.Run("no flag is not an error", func(t *testing.T) {
		t.Parallel()
		if d, ok := timeoutFromCommandText("pytest -q"); ok {
			t.Errorf("= (%s, true), want ok=false", d)
		}
	})
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
