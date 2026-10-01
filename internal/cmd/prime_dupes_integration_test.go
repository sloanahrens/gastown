//go:build integration

package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
)

// buildGitRootFixture seeds a git repo in root whose origin/main carries a
// fresh commit touching cmd/gt/hermetic_main_test.go, and returns root.
func buildGitRootFixture(t *testing.T, root string) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	f := filepath.Join(root, "cmd", "gt", "hermetic_main_test.go")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("package gt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "TestRunPrimeExternalTools_BoundsSlowMailCheck: fix the slow-mail bound (gt-abc)")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	return root
}

// TestIntegrationHookedPathDupesReadsRealGit runs the pre-work duplicate
// check (gt-csng) against a real repository: it warns on a fresh origin/main
// commit naming the bead's path, and fails closed to silence without
// origin/main or outside a repository. The prime clock is fake and never
// advanced, so the external-tool deadline cannot fire on a slow host.
// TestCheckHookedPathDupes covers the decisions over canned git output.
func TestIntegrationHookedPathDupesReadsRealGit(t *testing.T) {
	t.Parallel()
	bead := &beads.Issue{
		ID:    "gt-3vr",
		Title: "Fix the slow-mail bound",
		Description: "cmd/gt/hermetic_main_test.go fails in " +
			"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
	}
	run := func(workDir string) string {
		var out bytes.Buffer
		p := primeTools{clock: clockwork.NewFakeClockAt(primeTestEpoch), out: &out}
		p.hookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: workDir}, bead)
		return out.String()
	}

	root := buildGitRootFixture(t, t.TempDir())
	out := run(root)
	for _, want := range []string{"cmd/gt/hermetic_main_test.go", "test name your bead names", "check before duplicating this work"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	if out := run(t.TempDir()); strings.TrimSpace(out) != "" {
		t.Fatalf("a workdir outside a repo must not be reported on, got:\n%s", out)
	}

	drop := exec.Command("git", "update-ref", "-d", "refs/remotes/origin/main")
	drop.Dir = root
	if b, err := drop.CombinedOutput(); err != nil {
		t.Fatalf("drop origin/main: %v\n%s", err, b)
	}
	if out := run(root); strings.TrimSpace(out) != "" {
		t.Fatalf("no origin/main must fail closed, got:\n%s", out)
	}
}
