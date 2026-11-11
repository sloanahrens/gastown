//go:build integration

package makegate

import (
	"regexp"
	"strings"
	"testing"
)

// The Makefile's gate dry-run shape: one test per case, each pinning one fact
// about `make -n gate`, `make -n lint` or `make -n tier-check`, and each
// comment naming the mutation that turns the test red.

// cachedUnitTierViolations are the recipe shapes that take gate's unit tier off
// go test's result cache; the budget runner's -exec wrapper reran every package
// on every landing (gt-s1vff).
var cachedUnitTierViolations = []struct {
	name  string
	match func(string) bool
}{
	{"the budget runner's -exec", func(s string) bool { return strings.Contains(s, "cmd/budget") }},
	{"-count=1 or -exec", regexp.MustCompile(`-count[= ]1|-exec`).MatchString},
	{"the retired slow tier", regexp.MustCompile(`-slow( |=)|slow\.txt`).MatchString},
}

// TestIntegrationGateDryRunExitsZero checks "make -n gate exits 0".
// Mutation: a syntax error in gate's recipe, or a $(error ...) in one, makes
// make -n exit non-zero.
func TestIntegrationGateDryRunExitsZero(t *testing.T) {
	if out, err := RealMake(repoRoot(t))("gate"); err != nil {
		t.Fatalf("make -n gate: %v\n%s", err, out)
	}
}

// TestIntegrationGateRecipesDoNotRecurseThroughMake checks "gate, test-slow,
// test-integration and test recipes do not recurse through $(MAKE), so -n runs
// nothing".
// Mutation: add a `$(MAKE) -C ...` recipe line to gate; make would run it even
// under -n.
func TestIntegrationGateRecipesDoNotRecurseThroughMake(t *testing.T) {
	text := makefileText(t, repoRoot(t))
	for _, target := range []string{"gate", "test-slow", "test-integration"} {
		if !hasTargetLine(text, target) {
			t.Fatalf("the Makefile has no %s: target, so there is no recipe shape to pin", target)
		}
	}
	if got := recipesRecursingThroughMake(text); len(got) > 0 {
		t.Fatalf("gate-shape recipes recurse through $(MAKE), which make runs even under -n: %q", got)
	}
}

// TestIntegrationGateRunsLintThenBuildThenUnitTier checks "gate runs lint, then
// go build ./..., then the cached unit tier".
// Mutation: swap gate's prerequisites to `gate-test gate-lint`, and lint runs
// after the build.
func TestIntegrationGateRunsLintThenBuildThenUnitTier(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	lint := lineOf(out, "golangci-lint run")
	build := lineOf(out, "go build ./...")
	unit := lineOf(out, "go test -timeout 20m ./...")
	if lint == 0 || build == 0 || unit == 0 || lint >= build || build >= unit {
		t.Fatalf("gate's stages are out of order (lint=%d build=%d unit=%d), want lint < build < unit\n%s", lint, build, unit, out)
	}
}

// TestIntegrationGateLeavesShellTestsToTestSlow checks "gate leaves the shell
// tests (the shelltier runner) to test-slow".
// Mutation: add the shelltier runner to gate's recipe, and every landing runs
// the shell tests too.
func TestIntegrationGateLeavesShellTestsToTestSlow(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if line := lineOf(out, "internal/shelltier/cmd/shelltier"); line != 0 {
		t.Fatalf("gate runs the shelltier runner at line %d; the shell tests belong to test-slow\n%s", line, out)
	}
}

// TestIntegrationGateUnitTierRunsThroughTheCache checks "gate runs the unit tier
// over every package through the test cache (no budget -exec, no -count=1)".
// Mutation: route the unit tier through cmd/budget, add -count=1, or name a
// slow list, and go test's result cache no longer serves a landing.
func TestIntegrationGateUnitTierRunsThroughTheCache(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if !strings.Contains(out, "go test -timeout 20m ./...") {
		t.Fatalf("gate does not run the unit tier over every package\n%s", out)
	}
	for _, bad := range cachedUnitTierViolations {
		if bad.match(out) {
			t.Fatalf("gate's unit tier carries %s, so the result cache no longer serves it\n%s", bad.name, out)
		}
	}
}

// TestIntegrationGateOnlyWarnsOnWallTime checks "gate only warns on wall time;
// make tier-check runs the unit tier with -strict-wall and no lint or build".
// Mutation: add -strict-wall to gate's unit tier, and a loaded host refuses a
// landing (gt-z7qtk); drop it from tier-check, and drift is never caught.
func TestIntegrationGateOnlyWarnsOnWallTime(t *testing.T) {
	m := RealMake(repoRoot(t))
	out := dryGate(t, m)
	if strings.Contains(out, "-strict-wall") {
		t.Fatalf("gate carries -strict-wall, so a loaded host would refuse a landing\n%s", out)
	}
	tout, err := m("tier-check")
	if err != nil {
		t.Fatalf("make -n tier-check: %v\n%s", err, tout)
	}
	if !strings.Contains(tout, "cmd/budget -fast-tier -strict-wall -- -timeout 20m ./...") {
		t.Fatalf("make tier-check does not run the unit tier through cmd/budget with -strict-wall\n%s", tout)
	}
	if regexp.MustCompile(`golangci-lint|go build|slot +run`).MatchString(tout) {
		t.Fatalf("make tier-check carries lint, build or the container slot, which it does not own\n%s", tout)
	}
}

// TestIntegrationGatePrintsItsWallTime checks "gate prints its wall time".
// Mutation: delete gate's final PASSED echo, and the landing log no longer says
// how long the gate took.
func TestIntegrationGatePrintsItsWallTime(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if !strings.Contains(out, "gate: PASSED in ${wall}s wall") {
		t.Fatalf("gate does not print its wall time\n%s", out)
	}
}

// rootLintLockWait is plain make lint's root lint line, matched from the line
// start: the nested-module loop carries its own copy of the retry wrapper, so
// a substring match would keep passing after the root lint lost its retry.
var rootLintLockWait = regexp.MustCompile(`(?m)^bash scripts/lint-lock-wait\.sh 600 golangci-lint run --timeout=5m`)

// TestIntegrationGateLintWaitsOnTheLock checks "gate's lint waits on the lint
// lock; plain make lint retries a contended lint for LINT_LOCK_WAIT".
// Mutation: drop the target-specific --allow-serial-runners from both gate and
// gate-lint, and two worktrees' gates fail instead of waiting their turn
// (gt-uoppq); take the root lint line off scripts/lint-lock-wait.sh, and a
// contended root lint is not retried.
func TestIntegrationGateLintWaitsOnTheLock(t *testing.T) {
	m := RealMake(repoRoot(t))
	out := dryGate(t, m)
	if !strings.Contains(out, "golangci-lint run --timeout=5m --allow-serial-runners") {
		t.Fatalf("gate's lint does not wait on the module lock (no --allow-serial-runners)\n%s", out)
	}
	lout, err := m("lint")
	if err != nil {
		t.Fatalf("make -n lint: %v\n%s", err, lout)
	}
	if strings.Contains(lout, "--allow-serial-runners") {
		t.Fatalf("plain make lint carries --allow-serial-runners; it should retry a contended lint instead\n%s", lout)
	}
	if !rootLintLockWait.MatchString(lout) {
		t.Fatalf("plain make lint's root line does not retry a contended lint through scripts/lint-lock-wait.sh\n%s", lout)
	}
}

// TestIntegrationGateNeverTakesTheSlot checks "gate never takes the container-gate
// slot".
// Mutation: wrap gate's unit tier in `gt slot run`, and the gate competes with
// the pool it must never hold.
func TestIntegrationGateNeverTakesTheSlot(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if slot := regexp.MustCompile(`slot +run`).FindString(out); slot != "" {
		t.Fatalf("gate takes the container-gate slot (%q)\n%s", slot, out)
	}
}

// TestIntegrationGateListsTheSlowTests checks "gate's tierwall run lists each
// test of 5s or more under its package (-slow-tests)".
// Mutation: drop -slow-tests from gate's tierwall line, and the gate log stops
// naming the slow tests (gt-vijwck).
func TestIntegrationGateListsTheSlowTests(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if !strings.Contains(out, "cmd/tierwall -slow-tests") {
		t.Fatalf("gate's tierwall run does not pass -slow-tests, so the gate log does not list the slow tests\n%s", out)
	}
}
