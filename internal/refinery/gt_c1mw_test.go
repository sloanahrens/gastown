package refinery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoMergeFailsClosedWhenAutoSaveCheckErrors reproduces gt-c1mw: an om
// major found that when checkpoint.HasAutoSaveCommits itself errors (e.g. a
// transient git failure), doMerge logged a warning and carried on with
// hasAutoSave defaulted to false — the --no-ff path this guard exists to
// block a raw checkpoint_dog/gt-pvx commit from taking (see gt-rswr in
// engineer.go). The check must fail the merge instead of silently assuming
// "no auto-save commits".
//
// This is exercised with a git binary substitute on PATH that fails only the
// exact `git log --format=%s <mergebase>..<head>` invocation HasAutoSaveCommits
// makes — every other git call doMerge needs (staging the target, checking
// conflicts, etc.) still runs the real git binary, so the test isolates this
// one failure without disturbing the rest of the merge machinery.
func TestDoMergeFailsClosedWhenAutoSaveCheckErrors(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "polecat/test/autosave-check-broken"
	createFeatureBranch(t, workDir, branch, "feature.txt", "the whole fix\n")

	installFailingAutoSaveCheckGit(t)

	e := newTestEngineer(t, workDir, g)
	mr := makeMR("mr-autosave-check-broken", branch, "main")
	result := e.doMerge(context.Background(), mr)

	if result.Success {
		t.Fatalf("doMerge succeeded despite the auto-save check erroring; it must fail closed, not fall through to an unsquashed merge:\n%s", engineerOutput(t, e))
	}
	if !strings.Contains(result.Error, "auto-save") {
		t.Errorf("result.Error = %q, want it to name the auto-save check that failed", result.Error)
	}

	history := run(t, workDir, "git", "log", "--oneline", "origin/main")
	if strings.Contains(history, "feature.txt") || strings.Contains(history, branch) {
		t.Errorf("origin/main advanced despite the failed merge:\n%s", history)
	}
}

// installFailingAutoSaveCheckGit prepends a fake `git` to PATH for the
// duration of the test that fails exactly the
// `git log --format=%s <a>..<b>` call and passes every other invocation
// through to the real git binary. t.Setenv restores PATH on cleanup, and
// (being a real testing.T method) refuses to run alongside t.Parallel(),
// which matters here: PATH is process-global, so this test must not overlap
// with any other test's git calls.
func installFailingAutoSaveCheckGit(t *testing.T) {
	t.Helper()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate real git binary: %v", err)
	}

	fakeBinDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "log" ] && [ "$2" = "--format=%%s" ] && [ "$#" -eq 3 ]; then
  case "$3" in
    *..*) echo "fatal: injected failure for gt-c1mw regression test" >&2; exit 128 ;;
  esac
fi
exec %q "$@"
`, realGit)

	wrapperPath := filepath.Join(fakeBinDir, "git")
	if err := os.WriteFile(wrapperPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git wrapper: %v", err)
	}

	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
