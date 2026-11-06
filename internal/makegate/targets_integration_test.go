//go:build integration

package makegate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The test-slow, test-makefile and polecat sandbox cases: one test per case in
// the block of scripts/makefile-gate_test.sh that runs from `echo "test-slow"`
// through the openshell-providers case. Each name carries the
// TestIntegrationTargets prefix so the bead's
// `-run 'TestIntegrationTargets|TestIntegrationPolecatImageDry'` covers the
// whole family, and each comment names the mutation that turns the test red.
//
// Every make call is a dry run: -n prints the recipe and runs nothing. The
// exception is polecat-image's `$(MAKE)` line, which make runs even under -n;
// MAKEFLAGS carries -n into that child, so the seed stage is printed, not run.

// shellTestsLine is the one runner the shell tests have (gt-tqxdd), written as
// test-slow and test-makefile both run it.
const shellTestsLine = "GT_TEST_DOCKER=0 go run ./internal/shelltier/cmd/shelltier --all"

// defaultsMake is a dry-run Make for the checkout at root that runs the way a
// fresh shell would: the outer make's flags are unset, and so is every variable
// the Makefile gives a `?=` default, so an ambient SHELL_TESTS or
// PRESUBMIT_BASE cannot change the recipe a case reads. The names come from the
// Makefile itself, so a default added later is covered without editing this.
func defaultsMake(t *testing.T, root string) Make {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	unset := append([]string{"GT_TEST_DOCKER"}, outerMakeFlags...)
	for _, m := range regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)[ \t]*\?=`).FindAllSubmatch(data, -1) {
		unset = append(unset, string(m[1]))
	}
	return envMakeWithout(root, true, unset)
}

// dryTarget is make -n with args, the output a case reads. make failing fails
// the test: no assertion may run against a partial dry run.
func dryTarget(t *testing.T, m Make, args ...string) string {
	t.Helper()
	out, err := m(args...)
	if err != nil {
		t.Fatalf("make -n %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestIntegrationTargetsTestSlowDryRunExitsZero checks "make -n test-slow exits 0".
// Mutation: a syntax error or a $(error ...) in test-slow's recipe makes make -n
// exit non-zero.
func TestIntegrationTargetsTestSlowDryRunExitsZero(t *testing.T) {
	dryTarget(t, defaultsMake(t, repoRoot(t)), "test-slow")
}

// targetsContainerOptInOn matches the container opt-in set to anything but 0,
// empty included: test-slow must write 0 and never turn containers on.
var targetsContainerOptInOn = regexp.MustCompile(`GT_TEST_DOCKER=([^0]|$)`)

// targetsSlotRun matches a recipe taking the container-gate slot.
var targetsSlotRun = regexp.MustCompile(`slot +run`)

// TestIntegrationTargetsTestSlowRunsTheGateThenTheShellTests checks "test-slow
// runs the gate, then the shell tests, containers off, no slot".
// Mutation: drop test-slow's gate prerequisite, swap the recipe's two stages,
// write GT_TEST_DOCKER=1 on either line, or wrap a line in `gt slot run`.
func TestIntegrationTargetsTestSlowRunsTheGateThenTheShellTests(t *testing.T) {
	out := dryTarget(t, defaultsMake(t, repoRoot(t)), "test-slow")
	gate := lineOf(out, "go test -timeout 20m ./...")
	shell := lineOf(out, shellTestsLine)
	if gate == 0 || shell == 0 || gate >= shell {
		t.Fatalf("test-slow does not run the gate before the shell tests (gate line %d, shell line %d), want both present and the gate first\n%s", gate, shell, out)
	}
	if on := targetsContainerOptInOn.FindString(out); on != "" {
		t.Fatalf("test-slow runs a line with the container opt-in on (%q)\n%s", on, out)
	}
	if slot := targetsSlotRun.FindString(out); slot != "" {
		t.Fatalf("test-slow takes the container-gate slot (%q)\n%s", slot, out)
	}
}

// TestIntegrationTargetsTestMakefileDryRunExitsZero checks "make -n test-makefile
// exits 0".
// Mutation: a syntax error or a $(error ...) in test-makefile's recipe makes
// make -n exit non-zero.
func TestIntegrationTargetsTestMakefileDryRunExitsZero(t *testing.T) {
	dryTarget(t, defaultsMake(t, repoRoot(t)), "test-makefile")
}

// bashScriptCall matches a recipe line running a shell script by hand.
var bashScriptCall = regexp.MustCompile(`bash\s+\S+`)

// TestIntegrationTargetsTestMakefileRunsTheShelltierRunnerAlone checks
// "test-makefile runs the shelltier runner alone, as test-slow does (gt-tqxdd)".
// Mutation: give test-makefile a hand-written list (`bash scripts/x_test.sh`),
// and the list drifts from the runner test-slow uses.
func TestIntegrationTargetsTestMakefileRunsTheShelltierRunnerAlone(t *testing.T) {
	out := dryTarget(t, defaultsMake(t, repoRoot(t)), "test-makefile")
	if call := bashScriptCall.FindString(out); call != "" {
		t.Fatalf("test-makefile runs a shell script by hand (%q); the shell tests have one runner\n%s", call, out)
	}
	if !strings.Contains(out, shellTestsLine) {
		t.Fatalf("test-makefile does not run the shelltier runner test-slow runs\n%s", out)
	}
}

// skippedByTheOldHandList are the manifest entries the hand-written list that
// test-makefile replaced never ran (gt-u560q.6).
var skippedByTheOldHandList = []string{
	".githooks/pre-push_test.sh",
	"scripts/post-land-shell_test.sh",
}

// announcedScript matches the line the shelltier runner writes to stderr before
// it starts a script, capturing the script's path.
var announcedScript = regexp.MustCompile(`(?m)^tier-sweep: shell (.*) \.\.\.$`)

// TestIntegrationTargetsShellTestListRunsTheScriptsTheOldHandListSkipped checks
// "the shelltier runner starts the scripts the old hand list skipped, and the
// test-makefile recipe names no hand list".
// Mutation: drop one of the two entries from internal/shelltier's manifest, and
// a script the runner owns stops running anywhere; restore a hand-written list in
// test-makefile, and it drifts from the manifest again.
//
// The runner is given an empty root, so no script runs: each `bash <path>` finds
// no file, and the runner names every script it starts on stderr before trying.
func TestIntegrationTargetsShellTestListRunsTheScriptsTheOldHandListSkipped(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./internal/shelltier/cmd/shelltier", "--all", "--root", dir, "--logdir", filepath.Join(dir, "logs"))
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The runner exits 1 because no script exists at the empty root; this case
	// reads the scripts it started, not its verdict.
	runErr := cmd.Run()
	started := announcedScript.FindAllStringSubmatch(stderr.String(), -1)
	if len(started) == 0 {
		t.Fatalf("the shelltier runner started no script (go run: %v)\n%s", runErr, stderr.String())
	}
	for _, want := range skippedByTheOldHandList {
		found := false
		for _, m := range started {
			if m[1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the shelltier runner never starts %s\nstarted: %q", want, started)
		}
	}
	if mout := dryTarget(t, defaultsMake(t, root), "test-makefile"); strings.Contains(mout, "test-makefile.sh") {
		t.Fatalf("test-makefile names a hand-written list instead of the runner's manifest\n%s", mout)
	}
}

// polecatImageMake is a dry-run Make for the checkout at root with the town
// paths a polecat-image dry run expands pinned to temp directories: the recipe
// would otherwise read the host's ~/gt, and no case may read the live town tree.
func polecatImageMake(t *testing.T, root string) Make {
	t.Helper()
	return envMake(root, "POLE_TOWN="+t.TempDir(), "POLE_SEED_DIR="+filepath.Join(t.TempDir(), "seed"))
}

// TestIntegrationTargetsPolecatImageDryRunExitsZero checks "make -n polecat-image
// exits 0".
// Mutation: a syntax error or a $(error ...) in polecat-image's recipe makes
// make -n exit non-zero.
func TestIntegrationTargetsPolecatImageDryRunExitsZero(t *testing.T) {
	dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image")
}

// arm64CrossBuild is the line polecat-image uses for each helper it
// cross-compiles for the sandbox image.
const arm64CrossBuild = "GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build"

// TestIntegrationTargetsPolecatImageStagesItsHelpersThroughTheCommand checks
// "polecat-image stages gt, polecat-image, sweep-run and bd through the context
// command, not in the recipe". The command builds them for linux/arm64 with its
// own environment, which the imagectx unit tests check.
// Mutation: put a cross-compile line back in the recipe, and the binaries stop
// coming from the pins' one staging path.
func TestIntegrationTargetsPolecatImageStagesItsHelpersThroughTheCommand(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image")
	if cross := strings.Count(out, arm64CrossBuild); cross != 0 {
		t.Fatalf("polecat-image cross-compiles %d helpers in the recipe, want none: the context command builds them\n%s", cross, out)
	}
	if !strings.Contains(out, "go run ./internal/polecat/imagectx/cmd/build -rev") {
		t.Fatalf("polecat-image does not run the context command\n%s", out)
	}
}

// taggedDockerBuild matches the image build carrying both the short-sha tag and
// :dev, from the Dockerfile in deploy/polecat, over the staged context.
var taggedDockerBuild = regexp.MustCompile(`docker build --platform linux/arm64 .*-t gastown-polecat:[^ ]+ -t gastown-polecat:dev -f deploy/polecat/Dockerfile "\$ctx"`)

// TestIntegrationTargetsPolecatImageBuildsTheImageTaggedShortShaAndDev checks
// "polecat-image builds deploy/polecat/Dockerfile, tagged both the short sha and
// :dev".
// Mutation: drop the :dev tag or the short-sha tag, or build another Dockerfile
// or context, and the sandbox either runs a stale image or the wrong tree.
func TestIntegrationTargetsPolecatImageBuildsTheImageTaggedShortShaAndDev(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image")
	if !taggedDockerBuild.MatchString(out) {
		t.Fatalf("polecat-image does not build deploy/polecat/Dockerfile with both the short-sha tag and :dev\n%s", out)
	}
	dev := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "gastown-polecat:dev") {
			dev++
		}
	}
	if dev != 1 {
		t.Fatalf("polecat-image names the :dev tag on %d lines, want one\n%s", dev, out)
	}
}

// TestIntegrationTargetsPolecatImageBuildsBDFromItsPin checks "polecat-image
// builds bd at its image pin, so BD_SRC names no checkout the image reads".
// Mutation: pass BD_SRC to the context command, and the dry run names the
// checkout the image's bd would be read from.
func TestIntegrationTargetsPolecatImageBuildsBDFromItsPin(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image", "BD_SRC=/somewhere/else")
	if strings.Contains(out, "/somewhere/else") {
		t.Fatalf("polecat-image reads BD_SRC, so bd is not built at its pin\n%s", out)
	}
}

// TestIntegrationTargetsPolecatImageLabelsTheBDCommit checks "polecat-image
// labels the image with the bd commit the context command printed".
// Mutation: label the image from the host bd or a make variable instead of the
// commit the staged context names, and the label no longer matches the bd the
// image carries (gt-7b7a5).
func TestIntegrationTargetsPolecatImageLabelsTheBDCommit(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image")
	for _, want := range []string{
		"s/^label: org.gastown.bd-commit=//p",
		`test -n "$ctx" -a -n "$gt_commit" -a -n "$bd_commit"`,
		`--label org.gastown.bd-commit="$bd_commit"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("polecat-image does not label the image with the bd commit the context command printed (missing %q)\n%s", want, out)
		}
	}
}

// TestIntegrationTargetsPolecatCachesRunsTheGoCommand checks "make -n
// polecat-caches runs the Go command with the pins, the claim and RIGS".
// Mutation: run the retired polecat-caches.sh, or drop the pins, the claim or
// RIGS from the command, and the seed Job either does not run or seeds the wrong
// rigs into the wrong claim.
func TestIntegrationTargetsPolecatCachesRunsTheGoCommand(t *testing.T) {
	out := dryTarget(t, envMake(repoRoot(t), "POLE_TOWN="+t.TempDir()), "polecat-caches", `RIGS=gastown om`)
	for _, want := range []string{
		"go run ./cmd/polecatcaches",
		"-pins deploy/polecat/seed-pins",
		"-claim gt-cache",
		"gastown om",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("polecat-caches does not pass %q to the Go command\n%s", want, out)
		}
	}
	if strings.Contains(out, "polecat-caches.sh") {
		t.Fatalf("polecat-caches still runs the retired shell script\n%s", out)
	}
}

// TestIntegrationTargetsOpenshellProvidersRunsTheGoCommand checks "make -n
// openshell-providers runs the Go command".
// Mutation: drop the go run line, and the target exits 0 having imported no
// provider.
func TestIntegrationTargetsOpenshellProvidersRunsTheGoCommand(t *testing.T) {
	out := dryTarget(t, defaultsMake(t, repoRoot(t)), "openshell-providers")
	if !strings.Contains(out, "go run ./cmd/openshell-providers") {
		t.Fatalf("openshell-providers does not run the Go command\n%s", out)
	}
}
