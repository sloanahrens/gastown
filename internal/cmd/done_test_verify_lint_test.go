//go:build integration

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

// TestRunDefaultTestVerification_Lint covers the rig lint_command inside gt
// done's default gate: it runs slot-free before the tests, a failure refuses
// the submission without spending a suite run, and it still runs when every
// changed package was deferred to the refinery.
func TestRunDefaultTestVerification_Lint(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	t.Run("lint passes, then tests run; recorded on the result", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		marker := filepath.Join(dir, "lint-ran")
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: "echo ok > '" + marker + "'"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.lintRan || result.lintCommand == "" {
			t.Errorf("lint not recorded: %+v", result)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("lint command did not run: %v", statErr)
		}
		if !result.ran || !result.success || len(result.packages) != 1 {
			t.Errorf("tests did not run after lint: %+v", result)
		}
		if acquired {
			t.Error("lint/test of a container-free package took a slot")
		}
		log, _ := os.ReadFile(result.logPath)
		if !strings.Contains(string(log), "lint passed in") {
			t.Errorf("verify log does not record the lint pass:\n%s", log)
		}
	})

	t.Run("lint fails: refuse with the findings, tests never run", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		testMarker := filepath.Join(dir, "tests-ran")
		lint := "echo 'pkga/a.go:1:1: something is wrong (fakelint)'; exit 3"
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: lint, TestVerifyCommand: "echo ran > '" + testMarker + "'"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-fail-role")
		if err == nil {
			t.Fatalf("expected a lint refusal, got result=%+v", result)
		}
		for _, want := range []string{"lint-verify failed", "exit 3", "fakelint", "no tests were run"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
		if _, statErr := os.Stat(testMarker); statErr == nil {
			t.Error("tests ran despite the lint failure")
		}
	})

	t.Run("every changed package deferred: lint still runs, tests skipped", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "only internal/cmd")

		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			t.Error("slot acquired although every changed package was deferred")
			return func() {}, nil
		}, nil)

		marker := filepath.Join(dir, "lint-ran")
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", LintCommand: "echo ok > '" + marker + "'"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/lint-deferred-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.lintRan {
			t.Errorf("lint did not run for an all-deferred change: %+v", result)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("lint command did not run: %v", statErr)
		}
		if result.ran || !strings.Contains(result.skipReason, "refinery") || len(result.deferredPackages) != 1 {
			t.Errorf("tests should be skipped with the refinery reason: %+v", result)
		}
		if result.slotUsed {
			t.Error("slotUsed = true for a lint-only gate")
		}
	})

	t.Run("no lint_command configured: gate unchanged", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/no-lint-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.lintRan || result.lintCommand != "" {
			t.Errorf("lint recorded although none was configured: %+v", result)
		}
		if !result.ran || !result.success {
			t.Errorf("tests did not run: %+v", result)
		}
	})
}
