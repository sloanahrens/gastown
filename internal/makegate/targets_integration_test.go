//go:build integration

package makegate

import (
	"bytes"
	"context"
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
	dryTarget(t, RealMake(repoRoot(t)), "test-slow")
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
	out := dryTarget(t, RealMake(repoRoot(t)), "test-slow")
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
	dryTarget(t, RealMake(repoRoot(t)), "test-makefile")
}

// bashScriptCall matches a recipe line running a shell script by hand.
var bashScriptCall = regexp.MustCompile(`bash\s+\S+`)

// TestIntegrationTargetsTestMakefileRunsTheShelltierRunnerAlone checks
// "test-makefile runs the shelltier runner alone, as test-slow does (gt-tqxdd)".
// Mutation: give test-makefile a hand-written list (`bash scripts/x_test.sh`),
// and the list drifts from the runner test-slow uses.
func TestIntegrationTargetsTestMakefileRunsTheShelltierRunnerAlone(t *testing.T) {
	out := dryTarget(t, RealMake(repoRoot(t)), "test-makefile")
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
	"scripts/town_metrics_test.sh",
}

// announcedScript matches the line the shelltier runner writes to stderr before
// it starts a script, capturing the script's path.
var announcedScript = regexp.MustCompile(`(?m)^tier-sweep: shell (.*) \.\.\.$`)

// TestIntegrationTargetsShellTestListRunsTheScriptsTheOldHandListSkipped checks
// "the shelltier runner starts the scripts the old hand list skipped, and the
// test-makefile recipe names no hand list".
// Mutation: drop one of the three entries from internal/shelltier's manifest, and
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
	if mout := dryTarget(t, RealMake(root), "test-makefile"); strings.Contains(mout, "test-makefile.sh") {
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

// TestIntegrationTargetsPolecatImageCrossCompilesForArm64 checks "polecat-image
// cross-compiles gt and the cache-trim helper for linux/arm64 and builds bd
// through the bd build helper".
// Mutation: drop either go build line, point one at another package, or build bd
// outside the helper, and the image loses a binary or its commit provenance.
func TestIntegrationTargetsPolecatImageCrossCompilesForArm64(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image")
	cross := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, arm64CrossBuild) {
			cross++
		}
	}
	if cross != 2 {
		t.Fatalf("polecat-image cross-compiles %d helpers, want gt and the cache-trim helper\n%s", cross, out)
	}
	for _, want := range []string{"./cmd/gt", "./plugins/polecat-image", "go run ./internal/polecat/bdbuild/cmd/build"} {
		if !strings.Contains(out, want) {
			t.Fatalf("polecat-image does not build %s\n%s", want, out)
		}
	}
}

// taggedDockerBuild matches the image build carrying both the short-sha tag and
// :dev, from the Dockerfile in deploy/polecat.
var taggedDockerBuild = regexp.MustCompile(`docker build --platform linux/arm64 .*-t gastown-polecat:[^ ]+ -t gastown-polecat:dev -f deploy/polecat/Dockerfile deploy/polecat`)

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

// TestIntegrationTargetsPolecatImageTakesBDSrc checks "polecat-image takes
// BD_SRC pointing at another beads checkout".
// Mutation: stop passing BD_SRC to the bd build helper, and the image's bd comes
// from wherever the host's checkout happens to be.
func TestIntegrationTargetsPolecatImageTakesBDSrc(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image", "BD_SRC=/somewhere/else")
	if !strings.Contains(out, `-src "/somewhere/else"`) {
		t.Fatalf("polecat-image does not build bd from BD_SRC\n%s", out)
	}
}

// TestIntegrationTargetsPolecatImageLabelsTheBDCommit checks "polecat-image
// labels the image with the bd commit the helper built".
// Mutation: label the image from the host bd instead of the file the helper
// wrote, and a checkout that predates it leaves the label naming another commit
// (gt-7b7a5).
func TestIntegrationTargetsPolecatImageLabelsTheBDCommit(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image", "BD_SRC=/somewhere/else", "BD_COMMIT=3b90ab0")
	for _, want := range []string{
		`-commit "3b90ab0"`,
		"bd_commit=$(go run ./internal/polecat/bdbuild/cmd/build",
		`test -n "$bd_commit"`,
		"bd_commit=$(cat deploy/polecat/bin/bd.commit)",
		`--label org.gastown.bd-commit="$bd_commit"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("polecat-image does not label the image with the bd commit the helper built (missing %q)\n%s", want, out)
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
	out := dryTarget(t, RealMake(repoRoot(t)), "openshell-providers")
	if !strings.Contains(out, "go run ./cmd/openshell-providers") {
		t.Fatalf("openshell-providers does not run the Go command\n%s", out)
	}
}
