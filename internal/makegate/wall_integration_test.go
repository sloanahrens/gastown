//go:build integration

package makegate

import (
	"regexp"
	"strings"
	"testing"
)

// The test-integration-wall cases: one test per case in lines 663-729 of
// scripts/makefile-gate_test.sh. Every case is a dry run, `make -n
// test-integration-wall`, which prints the recipe and runs nothing, so no case
// starts a container or reaches a network. The wall recipe reruns
// test-integration's two suite commands through one tierwall run, uncached, and
// these cases pin that mapping: the two commands reappear verbatim but for
// -json -count=1, the flag rides after the $(INTEGRATION_GO_TEST) swap, and the
// pipeline keeps go test's failure red under pipefail (gt-gq4gl).
//
// Every name carries the TestIntegrationWall prefix the bead's `-run` covers,
// and every comment names the mutation that turns the test red.
//
// The make calls are processes, so this file is integration tier
// (docs/standards/test-engineering.md, rule 2).

// countLines returns the number of lines of out holding needle as a fixed
// string, as `grep -c -F` does for the shell cases.
func countLines(out, needle string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// wallContainerRun matches a wall suite command that runs go test as JSON with
// containers on.
var wallContainerRun = regexp.MustCompile(`GT_TEST_DOCKER=1.*go test -json`)

// countMatchingLines returns the number of lines of out that match re.
func countMatchingLines(out string, re *regexp.Regexp) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if re.MatchString(line) {
			n++
		}
	}
	return n
}

// wallUncached reports whether out carries exactly the two `go test -json
// -count=1` commands test-integration runs: the count is what keeps the wall an
// uncached measurement (gt-gq4gl).
func wallUncached(out string) bool {
	return countLines(out, "go test -json -count=1") == 2
}

// wallSuiteCommands returns the lines of test-integration's dry run that run go
// test with containers on: the two commands the wall must rerun.
func wallSuiteCommands(out string) []string {
	var cmds []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "GT_TEST_DOCKER=1") && strings.Contains(line, "go test ") {
			cmds = append(cmds, line)
		}
	}
	return cmds
}

// TestIntegrationWallDryRunExitsZero checks "make -n test-integration-wall
// exits 0".
// Mutation: a syntax error or a $(error ...) in test-integration-wall's recipe
// makes make -n exit non-zero.
func TestIntegrationWallDryRunExitsZero(t *testing.T) {
	dryTarget(t, defaultsMake(t, repoRoot(t)), "test-integration-wall")
}

// TestIntegrationWallRunsTestIntegrationsTwoGoTestCommands checks
// "test-integration-wall runs test-integration's two go test commands, each
// -json -count=1".
// Mutation: drop -json from one wall suite command, and the wall measures no
// per-package line; drop -count=1, and the command it was meant to mirror is not
// the one the wall reruns.
func TestIntegrationWallRunsTestIntegrationsTwoGoTestCommands(t *testing.T) {
	root := repoRoot(t)
	wall := dryTarget(t, defaultsMake(t, root), "test-integration-wall")
	integration := dryTarget(t, defaultsMake(t, root), "test-integration")
	cmds := wallSuiteCommands(integration)
	if len(cmds) != 2 {
		t.Fatalf("test-integration runs %d go test commands with containers on, want 2, so the wall cannot mirror the tier\n%s", len(cmds), integration)
	}
	var missing []string
	for _, cmd := range cmds {
		want := strings.Replace(cmd, "go test ", "go test -json -count=1 ", 1)
		if !strings.Contains(wall, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("test-integration-wall does not rerun the configured suite command(s) with -json -count=1: %v\n%s", missing, wall)
	}
}

// TestIntegrationWallIsAnUncachedRun checks "test-integration-wall is an
// uncached run: both commands carry -count=1 (gt-gq4gl)".
// Mutation: drop -count=1 from either wall suite command, and a second run on
// unchanged source is served from the Go test cache, whose wall can sit far
// under a cold run's.
func TestIntegrationWallIsAnUncachedRun(t *testing.T) {
	out := dryTarget(t, defaultsMake(t, repoRoot(t)), "test-integration-wall")
	if !wallUncached(out) {
		t.Fatalf("test-integration-wall does not carry `go test -json -count=1` on both commands, so its wall may be measured from the cache\n%s", out)
	}
}

// TestIntegrationWallCountCheckRejectsThePreGtGq4glRecipe checks "the -count=1
// check rejects the pre-gt-gq4gl wall recipe". The recipe it rejects is made
// from the real one: the wall's dry run with -count=1 taken out of both
// commands, which is the shape the wall had before gt-gq4gl.
// Mutation: return true from wallUncached unconditionally, and the recipe this
// case exists to catch passes the very check that guards the measurement.
func TestIntegrationWallCountCheckRejectsThePreGtGq4glRecipe(t *testing.T) {
	out := dryTarget(t, defaultsMake(t, repoRoot(t)), "test-integration-wall")
	if !wallUncached(out) {
		t.Fatalf("the real wall recipe is not uncached, so there is no recipe to take -count=1 out of\n%s", out)
	}
	preGtGq4gl := strings.ReplaceAll(out, " -count=1", "")
	if preGtGq4gl == out {
		t.Fatal("taking -count=1 out of the wall recipe changed nothing")
	}
	if wallUncached(preGtGq4gl) {
		t.Fatalf("the -count=1 check admits the wall recipe with -count=1 taken out\n%s", preGtGq4gl)
	}
}

// TestIntegrationWallIntegrationGoTestStillSwapsTheRunner checks
// "INTEGRATION_GO_TEST still swaps the wall's runner, and -count=1 rides after
// it (gt-gq4gl)".
// Mutation: move -json -count=1 ahead of $(INTEGRATION_GO_TEST), and a CI
// runner swap collects no JUnit-shaped output and drops the uncached flag.
func TestIntegrationWallIntegrationGoTestStillSwapsTheRunner(t *testing.T) {
	root := repoRoot(t)
	out := dryTarget(t, defaultsMake(t, root), "test-integration-wall", "INTEGRATION_GO_TEST=go test -x")
	if got := countLines(out, "go test -x -json -count=1"); got != 2 {
		t.Fatalf("with INTEGRATION_GO_TEST=go test -x the wall runs `go test -x -json -count=1` on %d commands, want 2\n%s", got, out)
	}
}

// TestIntegrationWallRunnerDefaultCarriesNoCountOne checks "the runner default
// (INTEGRATION_GO_TEST) carries no -count=1; the wall's recipe does".
// Mutation: put -count=1 inside the runner default (INTEGRATION_GO_TEST ?= go
// test -count=1), and a CI runner swap silently restores the cached
// measurement. The recipe's own -count=1 is TestIntegrationWallIsAnUncachedRun.
func TestIntegrationWallRunnerDefaultCarriesNoCountOne(t *testing.T) {
	text := makefileText(t, repoRoot(t))
	var defaults []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "INTEGRATION_GO_TEST") {
			defaults = append(defaults, line)
		}
	}
	if len(defaults) == 0 {
		t.Fatal("the Makefile defines no INTEGRATION_GO_TEST default, so the wall's runner swap has nothing to swap")
	}
	for _, line := range defaults {
		if strings.Contains(line, "-count=1") {
			t.Fatalf("the runner default carries -count=1 (%q), so a runner swap drops the flag the wall's measurement depends on", line)
		}
	}
}

// TestIntegrationWallPipesBothCommandsThroughOneTierwallRun checks
// "test-integration-wall pipes both commands through one tierwall run under
// pipefail, containers on, no -run filter".
// Mutation: drop `set -o pipefail`, and a go test that died before emitting a
// FAIL event reads green; split the pipeline, and the wall is measured twice;
// add a -run filter, and the tier it measures is no longer every
// integration-tagged test.
func TestIntegrationWallPipesBothCommandsThroughOneTierwallRun(t *testing.T) {
	out := dryTarget(t, defaultsMake(t, repoRoot(t)), "test-integration-wall")
	runs := countMatchingLines(out, wallContainerRun)
	tool := countLines(out, "cmd/tierwall")
	if runs != 2 || tool != 1 {
		t.Fatalf("test-integration-wall runs %d suite commands and %d tierwall invocations, want 2 and 1, so the tier is measured by one run\n%s", runs, tool, out)
	}
	if !strings.Contains(out, "set -o pipefail") {
		t.Fatalf("test-integration-wall does not set pipefail, so a go test that died before emitting a FAIL event reads green\n%s", out)
	}
	if !strings.Contains(out, "test-integration-wall: internal/testpolicy/docker.txt lists no package") {
		t.Fatalf("test-integration-wall has no guard refusing a docker.txt that lists no package\n%s", out)
	}
	if !noRunFilter(out) {
		t.Fatalf("test-integration-wall passes a -run name filter, so the tier it measures is not every integration-tagged test\n%s", out)
	}
}

// TestIntegrationWallSetsTheTestDoltInitPoolToTheTownSlotCount checks
// "test-integration-wall sets the test-Dolt init pool to the town slot count on
// both commands".
// Mutation: drop GT_TEST_DOLT_INIT_CONCURRENCY from one wall command, and the
// count is 1; change the number without the channel, and neither line matches.
func TestIntegrationWallSetsTheTestDoltInitPoolToTheTownSlotCount(t *testing.T) {
	root := repoRoot(t)
	out := dryTarget(t, defaultsMake(t, root), "test-integration-wall")
	slots := townSlots(t, root)
	if got := initPoolLines(out, slots); got != 2 {
		t.Fatalf("test-integration-wall sets GT_TEST_DOLT_INIT_CONCURRENCY=%s on %d recipe lines, want 2, one per suite command\n%s", slots, got, out)
	}
}
