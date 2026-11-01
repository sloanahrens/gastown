//go:build integration

package makegate

import (
	"os"
	"strings"
	"testing"
)

// The seed stage and pins cases: one test per case in the block of
// scripts/makefile-gate_test.sh from its `SEEDTMP` fixture through the stale-pin
// case. Unlike the dry-run families, these drive the real recipe against a
// Sandbox town of temp git checkouts, so a refusal is seen as a non-zero exit
// and the code that names it. Every name carries the TestIntegrationSeed prefix
// the bead's `-run 'TestIntegrationSeed'` covers, and every comment names the
// mutation that turns the test red.

// mustExist fails when path is missing or cannot be read.
func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is not readable, want it staged: %v", path, err)
	}
}

// mustNotExist fails when path exists, and when it cannot be told either way.
func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s exists, want it absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("cannot tell whether %s exists: %v", path, err)
	}
}

// TestIntegrationSeedStagesTheBuildRigAtTheBuildCommit checks "polecat-seed-stage
// stages the build rig at the build commit, not the checkout's HEAD".
// Mutation: pass the canonical checkout's HEAD as -build-commit, and the build
// rig's newer second.txt is staged.
func TestIntegrationSeedStagesTheBuildRigAtTheBuildCommit(t *testing.T) {
	s := newSandbox(t)
	gastownFirst, _ := s.Rig("gastown")
	pinned, _ := s.Rig("fx")
	s.WriteFile("pins", "fx "+pinned+"\n")

	out, err := s.SeedMake(gastownFirst, "polecat-seed-stage")
	if err != nil {
		t.Fatalf("polecat-seed-stage failed at the build commit: %v\n%s", err, out)
	}
	mustExist(t, s.Path("out", "gastown", "first.txt"))
	mustNotExist(t, s.Path("out", "gastown", "second.txt"))
	if want := "gastown at " + gastownFirst[:12] + " (the build commit)"; !strings.Contains(out, want) {
		t.Fatalf("stage does not name %q\n%s", want, out)
	}
}

// TestIntegrationSeedStagesEveryOtherRigAtItsPin checks "polecat-seed-stage
// stages every other rig at its pin, not its newer HEAD".
// Mutation: stage a rig from its checkout HEAD rather than its pin, and fx's
// second.txt is staged.
func TestIntegrationSeedStagesEveryOtherRigAtItsPin(t *testing.T) {
	s := newSandbox(t)
	gastownFirst, _ := s.Rig("gastown")
	pinned, _ := s.Rig("fx")
	s.WriteFile("pins", "fx "+pinned+"\n")

	out, err := s.SeedMake(gastownFirst, "polecat-seed-stage")
	if err != nil {
		t.Fatalf("polecat-seed-stage failed for a readable pin: %v\n%s", err, out)
	}
	mustExist(t, s.Path("out", "fx", "first.txt"))
	mustNotExist(t, s.Path("out", "fx", "second.txt"))
}

// TestIntegrationSeedStopsOnAPinTheCheckoutDoesNotHold checks "polecat-seed-stage
// stops on a pin the checkout does not hold, naming the rig and sha".
// Mutation: treat an unreadable pin as drift-free (skip the guard's readability
// check), and the run either exits 0 or fails later without naming the pin check.
func TestIntegrationSeedStopsOnAPinTheCheckoutDoesNotHold(t *testing.T) {
	s := newSandbox(t)
	gastownFirst, _ := s.Rig("gastown")
	s.Rig("fx")
	s.WriteFile("pins", "fx "+strings.Repeat("0", 40)+"\n")

	out, err := s.SeedMake(gastownFirst, "polecat-seed-stage")
	if err == nil {
		t.Fatalf("polecat-seed-stage exited 0 on a pin the checkout does not hold\n%s", out)
	}
	if want := "rig fx pin 000000000000 is not readable in its canonical checkout"; !strings.Contains(out, want) {
		t.Fatalf("stage does not name %q\n%s", want, out)
	}
}

// TestIntegrationSeedStopsOnABuildCommitTheCheckoutDoesNotHold checks
// "polecat-seed-stage stops on a build commit the checkout does not hold, naming
// the rig and sha".
// Mutation: drop the rig and sha from the stage's archive error, so the failure
// names neither, or stage the commit without reading it, so the seed is partial.
func TestIntegrationSeedStopsOnABuildCommitTheCheckoutDoesNotHold(t *testing.T) {
	s := newSandbox(t)
	s.Rig("gastown")
	pinned, _ := s.Rig("fx")
	s.WriteFile("pins", "fx "+pinned+"\n")
	missing := strings.Repeat("0", 40)

	out, err := s.SeedMake(missing, "polecat-seed-stage")
	if err == nil {
		t.Fatalf("polecat-seed-stage exited 0 on a build commit the checkout does not hold\n%s", out)
	}
	if want := "cannot stage rig gastown at " + missing; !strings.Contains(out, want) {
		t.Fatalf("stage does not name %q\n%s", want, out)
	}
}

// TestIntegrationSeedRefusesABuildCommitItCannotRead checks "polecat-seed-stage
// refuses a build commit it cannot read, naming the reason".
// Mutation: remove the build-commit format guards (BuildRigSeed's and Plan's),
// and the run fails later without naming the reason, or stages a nameless commit.
func TestIntegrationSeedRefusesABuildCommitItCannotRead(t *testing.T) {
	s := newSandbox(t)
	s.Rig("gastown")
	pinned, _ := s.Rig("fx")
	s.WriteFile("pins", "fx "+pinned+"\n")

	out, err := s.SeedMake("", "polecat-seed-stage")
	if err == nil {
		t.Fatalf("polecat-seed-stage exited 0 with an empty build commit\n%s", out)
	}
	if want := "cannot read the build commit"; !strings.Contains(out, want) {
		t.Fatalf("stage does not name %q\n%s", want, out)
	}
}

// TestIntegrationSeedRefusesAnEmptyPinsFile checks "polecat-seed-stage refuses
// an empty pins file".
// Mutation: drop the recipe's `test -s`, and an empty pins file is refused only
// by the stage command's own message, not the recipe's actionable `missing or
// empty` that sends the operator to `make polecat-seed-pins`.
func TestIntegrationSeedRefusesAnEmptyPinsFile(t *testing.T) {
	s := newSandbox(t)
	gastownFirst, _ := s.Rig("gastown")
	s.WriteFile("pins", "")

	out, err := s.SeedMake(gastownFirst, "polecat-seed-stage")
	if err == nil {
		t.Fatalf("polecat-seed-stage exited 0 on an empty pins file\n%s", out)
	}
	if want := "missing or empty"; !strings.Contains(out, want) {
		t.Fatalf("stage does not name %q\n%s", want, out)
	}
}

// TestIntegrationSeedPinsPinsEachRigToItsCheckoutHead checks
// "polecat-seed-pins pins each seeded rig to its checkout's HEAD".
// Mutation: read origin/main (the pin) instead of HEAD, and the file keeps the
// older first commit.
func TestIntegrationSeedPinsPinsEachRigToItsCheckoutHead(t *testing.T) {
	s := newSandbox(t)
	_, newest := s.Rig("fx")

	out, err := s.SeedMake("", "polecat-seed-pins")
	if err != nil {
		t.Fatalf("polecat-seed-pins failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(s.Path("pins"))
	if err != nil {
		t.Fatalf("read the pins file the target wrote: %v", err)
	}
	want := "fx " + newest
	for _, line := range strings.Split(string(data), "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("pins file does not hold %q\n%s", want, data)
}

// TestIntegrationSeedPolecatImageRunsTheStageBeforeDockerBuild checks
// "polecat-image runs the seed stage, with the pin-drift guard, before docker
// build".
// Mutation: move the `$(MAKE) polecat-seed-stage` line after the docker build,
// and the image is built from an unseeded or stale seed directory.
func TestIntegrationSeedPolecatImageRunsTheStageBeforeDockerBuild(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image", "BD_SRC=/somewhere/else")
	stage := lineOf(out, "internal/polecat/seed/cmd/stage")
	build := lineOf(out, "docker build")
	if stage == 0 || build == 0 || stage >= build {
		t.Fatalf("polecat-image does not run the seed stage before docker build (stage line %d, build line %d)\n%s", stage, build, out)
	}
}

// TestIntegrationSeedAStalePinStopsTheStageBeforeAnythingIsStaged checks "a stale
// committed pin stops the seed stage before anything is staged".
// Mutation: drop the pin-drift guard from the stage, and a pin that lacks a
// module main requires seeds the rig and the image builds a cache that cannot
// compile main.
func TestIntegrationSeedAStalePinStopsTheStageBeforeAnythingIsStaged(t *testing.T) {
	s := newSandbox(t)
	fx := s.Checkout("fx")
	pin := s.Commit(fx, "first", map[string]string{
		"go.mod": "module example.invalid/fx\n\ngo 1.21\n",
		"a.txt":  "one\n",
	})
	// main requires a module the pin lacks: the guard must fail on that gap, not
	// on the lag.
	s.Commit(fx, "second", map[string]string{
		"go.mod": "module example.invalid/fx\n\ngo 1.21\n\nrequire github.com/x/missing v1.0.0\n",
		"b.txt":  "two\n",
	})
	s.git(fx, "update-ref", "refs/remotes/origin/main", s.git(fx, "rev-parse", "HEAD"))

	gastown := s.Checkout("gastown")
	build := s.Commit(gastown, "build", map[string]string{"g.txt": "g\n"})
	s.WriteFile("pins", "fx "+pin+"\n")

	out, err := s.SeedMake(build, "polecat-seed-stage")
	if err == nil {
		t.Fatalf("polecat-seed-stage exited 0 on a stale pin\n%s", out)
	}
	for _, want := range []string{"pin-drift guard: rig fx pin", "lacks modules its main", "github.com/x/missing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stage does not name %q\n%s", want, out)
		}
	}
	mustNotExist(t, s.Path("out", "fx"))
}
