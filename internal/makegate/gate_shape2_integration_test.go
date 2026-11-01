//go:build integration

package makegate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The second nine gate-shape cases: the container opt-in, the one -timeout, and
// the entry points the gate series retired. Each test is named after its case
// in scripts/makefile-gate_test.sh and its comment names the mutation that
// turns it red. Every name carries the TestIntegrationGate prefix so the bead's
// `-run 'TestIntegrationGate'` covers the family, including the cases whose
// shell title does not mention the gate.

// containerOptInOn matches the container opt-in set to anything but 0, empty
// included: gate must write 0 and never turn containers on.
var containerOptInOn = regexp.MustCompile(`GT_TEST_DOCKER=([^0]|$)`)

// TestIntegrationGateWritesTheContainerOptInOff checks "gate writes the
// container opt-in off where the unit tier starts".
// Mutation: drop GT_TEST_DOCKER=0 from gate-test's unit-tier line, and an
// exported opt-in reaches the tier a landing runs.
func TestIntegrationGateWritesTheContainerOptInOff(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if !strings.Contains(out, "GT_TEST_DOCKER=0 go test -timeout 20m ./...") {
		t.Fatalf("gate does not write the container opt-in off on the unit tier's go test\n%s", out)
	}
}

// TestIntegrationGateNeverTurnsTheContainerOptInOn checks "gate never turns the
// container opt-in on or defaults it".
// Mutation: give any gate recipe line GT_TEST_DOCKER=1, or a reference to the
// environment, and the dry run carries a value other than 0.
func TestIntegrationGateNeverTurnsTheContainerOptInOn(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	if on := containerOptInOn.FindString(out); on != "" {
		t.Fatalf("gate turns the container opt-in on (%q)\n%s", on, out)
	}
}

// TestIntegrationGateIgnoresAnInheritedContainerOptIn checks "an inherited
// GT_TEST_DOCKER=1 does not change the gate".
// Mutation: read the environment instead of writing a literal 0, as
// GT_TEST_DOCKER=$(GT_TEST_DOCKER) on the unit-tier line, and the two shapes
// below differ.
//
// Both runs set the variable explicitly, so a host that exports one cannot make
// the comparison vacuous.
func TestIntegrationGateIgnoresAnInheritedContainerOptIn(t *testing.T) {
	root := repoRoot(t)
	on := gateShape(t, envMake(root, "GT_TEST_DOCKER=1"))
	off := gateShape(t, envMake(root, "GT_TEST_DOCKER=0"))
	if on != off {
		t.Fatalf("an inherited GT_TEST_DOCKER=1 changes the gate\n--- with GT_TEST_DOCKER=1 ---\n%s\n--- with GT_TEST_DOCKER=0 ---\n%s", on, off)
	}
}

// gateStartMask matches the start time make bakes into the gate's recipe when
// it parses the Makefile: the part of a dry run that differs between two runs
// of the same recipe.
var gateStartMask = regexp.MustCompile(`date \+%s\) - [0-9]+`)

// gateShape is `make -n gate` with what varies between two runs removed: the
// gate's start time, and the go and make warnings a dry run prints on stderr.
func gateShape(t *testing.T, m Make) string {
	t.Helper()
	out, err := m("gate")
	if err != nil {
		t.Fatalf("make -n gate: %v\n%s", err, out)
	}
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "go: ") || strings.HasPrefix(line, "Warning: ") {
			continue
		}
		kept = append(kept, gateStartMask.ReplaceAllString(line, "date +%s) - START"))
	}
	return strings.Join(kept, "\n")
}

// oneTimeout matches a -timeout flag on a dry run's recipe line.
var oneTimeout = regexp.MustCompile(`(^| )-timeout[ =]`)

// TestIntegrationGateCarriesOneTimeout checks "gate carries one -timeout, on the
// unit tier's go test (the one gate definition)".
// Mutation: add -timeout to lint or to a second gate line, and the hang budget
// acquires an owner the unit tier's does not name.
func TestIntegrationGateCarriesOneTimeout(t *testing.T) {
	out := dryGate(t, RealMake(repoRoot(t)))
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		count += len(oneTimeout.FindAllString(line, -1))
	}
	if count != 1 {
		t.Fatalf("gate carries %d -timeout flags, want exactly one, on the unit tier's go test\n%s", count, out)
	}
	if !strings.Contains(out, "go test -timeout 20m ./...") {
		t.Fatalf("gate's one -timeout is not on the unit tier's go test\n%s", out)
	}
}

// retiredGateTargets are the entry points the gate series deleted: a caller
// reaching one of these gets no tier at all.
var retiredGateTargets = []string{"test-changed"}

// TestIntegrationGateRetiredTargetsAreGone checks "make test-changed is gone".
// Mutation: leave a test-changed: target, or name it on a .PHONY line, and a
// caller keeps reaching a gate target that runs nothing.
//
// The Makefile is read directly: a deleted target's old recipe recursed through
// $(MAKE), which make runs even under -n.
func TestIntegrationGateRetiredTargetsAreGone(t *testing.T) {
	text := makefileText(t, repoRoot(t))
	for _, target := range retiredGateTargets {
		if hasTargetLine(text, target) {
			t.Fatalf("the Makefile still declares a %s: target", target)
		}
		if declaredPhony(text, target) {
			t.Fatalf("the Makefile still lists %s on a .PHONY line", target)
		}
	}
}

// declaredPhony reports whether text names target as a word of a .PHONY: line.
func declaredPhony(text, target string) bool {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, ".PHONY:")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rest) {
			if field == target {
				return true
			}
		}
	}
	return false
}

// TestIntegrationGateMakeTestOnlyChainsTheTiers checks "make test only chains
// test-slow (the gate, then the shell tests) and test-integration, with no
// recipe of its own".
// Mutation: add a third prerequisite or a tabbed recipe line under test:, and
// `make test` runs a tier the gate never ran.
func TestIntegrationGateMakeTestOnlyChainsTheTiers(t *testing.T) {
	const want = "test: test-slow test-integration"
	text := makefileText(t, repoRoot(t))
	var got []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "test:") {
			got = append(got, line)
		}
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("the Makefile's test: lines are %q, want exactly %q", got, want)
	}
	if recipe := targetRecipe(text, "test"); len(recipe) > 0 {
		t.Fatalf("make test carries a recipe of its own:\n%s", strings.Join(recipe, "\n"))
	}
}

// targetRecipe returns the tab-led lines under the first `target:` line, up to
// the next line make reads as syntax. A blank line and a comment do not end a
// recipe (make resumes at the next tab line), which is why they are skipped.
func targetRecipe(text, target string) []string {
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, target+":") {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var recipe []string
	for _, line := range lines[start+1:] {
		if strings.HasPrefix(line, "\t") {
			recipe = append(recipe, line)
			continue
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			break
		}
	}
	return recipe
}

// slowTierNames matches every reference to the retired slow tier (gt-ik4a1.9).
var slowTierNames = regexp.MustCompile(`SLOW_(LIST|PKGS)|slow\.txt`)

// TestIntegrationGateSlowTierIsGone checks "the slow tier is gone: no
// slow.txt, and the Makefile names none (gt-ik4a1.9)".
// Mutation: restore internal/testpolicy/slow.txt, or name a SLOW_LIST or
// slow.txt in the Makefile, and a tier nothing runs is advertised again.
func TestIntegrationGateSlowTierIsGone(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "internal/testpolicy/slow.txt")); err == nil {
		t.Fatal("internal/testpolicy/slow.txt exists; the slow tier is retired")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat internal/testpolicy/slow.txt: %v (an unreadable path is not an absent one)", err)
	}
	if line := firstLineMatching(makefileText(t, root), slowTierNames); line != "" {
		t.Fatalf("the Makefile still names the slow tier: %s", line)
	}
}

// TestIntegrationGateNoPKGSDefault checks "no PKGS default for a changed-package
// gate".
// Mutation: add `PKGS ?= ./...`, and a target silently widens to every package.
func TestIntegrationGateNoPKGSDefault(t *testing.T) {
	if line := firstLineMatching(makefileText(t, repoRoot(t)), pkgDefault); line != "" {
		t.Fatalf("the Makefile defaults PKGS: %s", line)
	}
}

// pkgDefault matches a PKGS ?= assignment, the changed-package gate's default.
var pkgDefault = regexp.MustCompile(`(^|[^A-Za-z_])PKGS *\?=`)

// TestIntegrationGateNoRecipeDefaultsTheContainerOptIn checks "no recipe
// defaults the container opt-in from the environment".
// Mutation: write GT_TEST_DOCKER=$(GT_TEST_DOCKER:-0), and a caller's exported
// 1 becomes the default on every target.
func TestIntegrationGateNoRecipeDefaultsTheContainerOptIn(t *testing.T) {
	if line := firstLineMatching(makefileText(t, repoRoot(t)), containerOptInDefault); line != "" {
		t.Fatalf("the Makefile defaults the container opt-in from the environment: %s", line)
	}
}

// containerOptInDefault matches a recipe defaulting GT_TEST_DOCKER from the
// environment.
var containerOptInDefault = regexp.MustCompile(`GT_TEST_DOCKER:-`)

// firstLineMatching returns the first line of text that matches re, or "".
func firstLineMatching(text string, re *regexp.Regexp) string {
	for _, line := range strings.Split(text, "\n") {
		if re.MatchString(line) {
			return line
		}
	}
	return ""
}

// envMake returns a Make for the checkout at root that runs with every key
// named in env set to the given value: the test process's own assignment of
// that key is removed first, so an ambient export cannot override the value the
// case means to test.
func envMake(root string, env ...string) Make {
	overridden := make(map[string]bool, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		overridden[key] = true
	}
	var base []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !overridden[key] {
			base = append(base, kv)
		}
	}
	childEnv := append(base, env...)
	return func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "make", append([]string{"-C", root, "--no-print-directory", "-n"}, args...)...)
		cmd.Env = childEnv
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}
