//go:build integration

package makegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The node prune and polecat sandbox-target cases: one test per case in the
// block of scripts/makefile-gate_test.sh from the default-off prune case through
// the phony targets case. The dry runs read polecat-image's recipe; the two
// prune cases run polecat-node-prune for real against a stub docker staged under
// the test's temp directory, so no case reaches the host's docker or its nodes.

// pruneStubDocker answers the `docker --context X <verb>` calls nodeprune makes.
// PRUNE_RMI_LOG records each removed id; PRUNE_FAIL_IMAGES makes the node's
// image list unreadable, the fail-closed branch.
const pruneStubDocker = `#!/bin/sh
shift 2
case "$1" in
  ps) echo gt-sbx-worker ;;
  exec)
    shift 2
    shift
    case "$1" in
      images) [ -n "$PRUNE_FAIL_IMAGES" ] && exit 1
        echo '{"images":[{"id":"sha256:old","repoTags":[],"repoDigests":["localhost:5001/gastown-polecat@sha256:f627"]}]}' ;;
      inspecti) echo '{"info":{"imageSpec":{"created":"2026-10-25T16:00:00Z"}}}' ;;
      ps) echo '{"containers":[]}' ;;
      rmi) echo "$2" >>"$PRUNE_RMI_LOG" ;;
    esac ;;
esac
`

// outerMakeFlags are the variables an outer make exports to its recipes. A test
// that runs make itself unsets them, so the flags of the make running the tests
// (-n above all) do not reach the make under test.
var outerMakeFlags = []string{"MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS"}

// envMakeWithout returns a Make for the checkout at root with env appended to
// the process environment and unset removed from it. Unlike envMake it can make
// a variable absent, which a recipe default needs: make's `?=` applies only
// while the variable is unset, and an inherited MAKEFLAGS would carry an outer
// make's flags into the recipe's recursive $(MAKE). A dry run passes -n.
func envMakeWithout(root string, dry bool, unset []string, env ...string) Make {
	drop := make(map[string]bool, len(unset)+len(env))
	for _, key := range unset {
		drop[key] = true
	}
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		drop[key] = true
	}
	var childEnv []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !drop[key] {
			childEnv = append(childEnv, kv)
		}
	}
	childEnv = append(childEnv, env...)
	flags := []string{"-C", root, "--no-print-directory"}
	if dry {
		flags = append(flags, "-n")
	}
	return func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "make", append(append([]string{}, flags...), args...)...)
		cmd.Env = childEnv
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

// stubDocker stages the fake docker in a fresh directory and returns that
// directory as the PATH entry the prune target must reach before the host's
// docker.
func stubDocker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("make stub bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(pruneStubDocker), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
	return bin
}

// pruneMake returns a real (non-dry) Make for root whose PATH reaches the stub
// docker first and whose stub state lives in the test's temp directory: the
// removal log at logPath, and failImages to make the node unreadable.
func pruneMake(t *testing.T, root, stub, logPath string, failImages bool) Make {
	t.Helper()
	env := []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PRUNE_RMI_LOG=" + logPath,
		"POLECAT_IMAGE_KEEP=0",
	}
	if failImages {
		env = append(env, "PRUNE_FAIL_IMAGES=1")
	}
	return envMakeWithout(root, false, outerMakeFlags, env...)
}

// TestIntegrationNodePrunePolecatImageLeavesNodesAloneByDefault checks
// "polecat-image does not call polecat-node-prune by default (gt-06ni7)".
// Mutation: default POLE_NODE_PRUNE to 1, or drop polecat-image's ifeq guard,
// and the dry run prints the prune.
func TestIntegrationNodePrunePolecatImageLeavesNodesAloneByDefault(t *testing.T) {
	m := envMakeWithout(repoRoot(t), true,
		append([]string{"POLE_NODE_PRUNE"}, outerMakeFlags...),
		"POLE_TOWN="+t.TempDir(), "POLE_SEED_DIR="+filepath.Join(t.TempDir(), "seed"))

	out := dryTarget(t, m, "polecat-image", "BD_SRC=/somewhere/else")
	if strings.Contains(out, "polecat-node-prune") {
		t.Fatalf("polecat-image calls polecat-node-prune with POLE_NODE_PRUNE unset\n%s", out)
	}
}

// TestIntegrationNodePrunePolecatImagePrunesAfterDockerBuildOnOptIn checks
// "polecat-image calls polecat-node-prune after docker build with
// POLE_NODE_PRUNE=1".
// Mutation: move the prune above the docker build, or invert the ifeq so the
// flag stops selecting it, and the prune line is missing or earlier.
func TestIntegrationNodePrunePolecatImagePrunesAfterDockerBuildOnOptIn(t *testing.T) {
	out := dryTarget(t, polecatImageMake(t, repoRoot(t)), "polecat-image", "BD_SRC=/somewhere/else", "POLE_NODE_PRUNE=1")
	build := lineOf(out, "docker build")
	prune := lineOf(out, "polecat-node-prune")
	if build == 0 || prune == 0 || prune <= build {
		t.Fatalf("polecat-image does not prune after docker build with POLE_NODE_PRUNE=1 (build line %d, prune line %d)\n%s", build, prune, out)
	}
}

// TestIntegrationNodePruneRemovesTheOldImageWithKeepZero checks "polecat-node-prune
// removes the old polecat image from a node with keep 0".
// Mutation: drop -keep so the command's default 3 keeps the old image, and the
// stub docker records no removal.
func TestIntegrationNodePruneRemovesTheOldImageWithKeepZero(t *testing.T) {
	stub := stubDocker(t)
	logPath := filepath.Join(t.TempDir(), "rmi.log")

	out, err := pruneMake(t, repoRoot(t), stub, logPath, false)("polecat-node-prune")
	if err != nil {
		t.Fatalf("polecat-node-prune failed with keep 0: %v\n%s", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the stub docker recorded no removal with keep 0: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), "sha256:old") {
		t.Fatalf("polecat-node-prune did not remove the old image with keep 0\nlog: %q\n%s", data, out)
	}
}

// TestIntegrationNodePruneLeavesANodeItCannotListAloneAndNamesTheFailure checks
// "polecat-node-prune leaves a node it cannot list alone and names the failure".
// Mutation: read an unreadable node's images as empty so nodeprune exits 0, and
// the recipe's failure line never prints; drop `|| echo`, and make exits
// non-zero instead of naming the failure and carrying on.
func TestIntegrationNodePruneLeavesANodeItCannotListAloneAndNamesTheFailure(t *testing.T) {
	stub := stubDocker(t)
	logPath := filepath.Join(t.TempDir(), "rmi.log")

	out, err := pruneMake(t, repoRoot(t), stub, logPath, true)("polecat-node-prune")
	if err != nil {
		t.Fatalf("polecat-node-prune made an unreadable node fatal to the caller: %v\n%s", err, out)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("a node that cannot list its images had an image removed\n%s", out)
	} else if !os.IsNotExist(err) {
		t.Fatalf("cannot tell whether the stub removed an image: %v", err)
	}
	if !strings.Contains(out, "node prune failed") {
		t.Fatalf("polecat-node-prune does not name the failure\n%s", out)
	}
}

// phonyTargets returns the targets the Makefile declares phony, joining
// backslash continuations as make does.
func phonyTargets(text string) map[string]bool {
	phony := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\\\n", " "), "\n") {
		rest, ok := strings.CutPrefix(line, ".PHONY:")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rest) {
			phony[field] = true
		}
	}
	return phony
}

// TestIntegrationSandboxTargetsAreDeclaredAndPhony checks "the polecat sandbox
// targets are declared and phony".
// Mutation: drop a target from .PHONY, or remove its target line, and a
// directory of the same name shadows the recipe when it is run.
func TestIntegrationSandboxTargetsAreDeclaredAndPhony(t *testing.T) {
	text := makefileText(t, repoRoot(t))
	phony := phonyTargets(text)
	var missing []string
	for _, target := range []string{"polecat-image", "polecat-seed-stage", "polecat-seed-pins", "polecat-caches", "polecat-node-prune", "openshell-providers"} {
		if !hasTargetLine(text, target) {
			missing = append(missing, target+" (not declared)")
		} else if !phony[target] {
			missing = append(missing, target+" (not .PHONY)")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("polecat sandbox targets missing: %s", strings.Join(missing, ", "))
	}
}
