//go:build integration

package makegate

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildLintpin builds cmd/lintpin from the checkout under test into the
// sandbox's stub directory, so the recipe's `go run ./cmd/lintpin` reaches a
// real check against the case's stub linter instead of the stub go. The
// check's own branches are unit-tested in internal/lintpin and cmd/lintpin.
func buildLintpin(t *testing.T, g *GateSandbox) string {
	t.Helper()
	bin := filepath.Join(g.bin, "lintpin")
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/lintpin")
	cmd.Dir = repoRoot(t)
	cmd.Env = sandboxEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/lintpin: %v\n%s", err, out)
	}
	return bin
}

// TestIntegrationGolangciLintVersionPrintsThePin checks the image build's seam:
// `make -s golangci-lint-version` prints the pin from its one home, so the
// image recipe and the gate cannot install and require different versions
// (gt-zzo1g2). Mutation: print another value and the image ships a linter the
// gate refuses.
func TestIntegrationGolangciLintVersionPrintsThePin(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("golangci-lint-version", nil)
	if r.Code != 0 {
		t.Fatalf("make golangci-lint-version exit %d, want 0\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if got := strings.TrimSpace(r.Stdout); got != golangciPin(t) {
		t.Fatalf("make golangci-lint-version printed %q, want the Makefile pin %q", got, golangciPin(t))
	}
}

// TestIntegrationLintToolsSkipsAnAlreadyPinnedLinter checks that `make
// lint-tools` compiles nothing when PATH already carries the pin, which is how
// the gate runner gets its linter from the CI base image (gt-zzo1g2).
// Mutation: install before checking, and every gate run compiles the linter.
func TestIntegrationLintToolsSkipsAnAlreadyPinnedLinter(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("lint-tools", nil, "STUB_LINTPIN="+buildLintpin(t, g))
	if r.Code != 0 {
		t.Fatalf("make lint-tools exit %d, want 0 when PATH carries the pin\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stdout, "already on PATH") {
		t.Fatalf("make lint-tools did not report that the pinned linter is already on PATH\nstdout:\n%s", r.Stdout)
	}
	if r.hasCallWithPrefix("go install") {
		t.Fatalf("make lint-tools compiled the linter while PATH already carries %s\ncalls:\n%s", golangciPin(t), r.callLog())
	}
}

// TestIntegrationLintToolsInstallsAnotherVersion checks the developer path: a
// checkout whose PATH carries another version still installs the pin.
// Mutation: skip the install on any PATH, and a developer keeps linting with
// an unpinned linter.
func TestIntegrationLintToolsInstallsAnotherVersion(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("lint-tools", nil, "STUB_LINTPIN="+buildLintpin(t, g), "STUB_LINT_VERSION=2.0.0")
	if r.Code != 0 {
		t.Fatalf("make lint-tools exit %d, want 0 while installing the pin\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if !r.hasCallWithPrefix("go install") {
		t.Fatalf("make lint-tools did not install the pin over the linter on PATH (2.0.0)\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationLintRefusesALinterAtAnotherVersion checks the recipe's half of
// the contract: `make lint` reads the linter from PATH and refuses one at
// another version, naming the pin, before any lint runs (gt-zzo1g2).
// Mutation: drop the check from the recipe and the lint proceeds with 2.0.0.
func TestIntegrationLintRefusesALinterAtAnotherVersion(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("lint", nil, "STUB_LINTPIN="+buildLintpin(t, g), "STUB_LINT_VERSION=2.0.0")
	if r.Code == 0 {
		t.Fatalf("make lint exit 0 with golangci-lint 2.0.0 on PATH, want a refusal\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if out := r.Stdout + r.Stderr; !strings.Contains(out, golangciPin(t)) || !strings.Contains(out, "2.0.0") {
		t.Fatalf("the refusal does not name the linter it found (2.0.0) and the pin (%s)\nstdout:\n%s\nstderr:\n%s", golangciPin(t), r.Stdout, r.Stderr)
	}
	if r.hasCallWithPrefix("golangci-lint run") {
		t.Fatalf("make lint linted with the wrong version instead of refusing\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationLintRefusesAMissingLinter checks that an absent linter is a
// refusal that names the pin, never a silent pass (gt-zzo1g2). Mutation: read
// the failed lookup as the pin, or drop the check, and lint runs.
func TestIntegrationLintRefusesAMissingLinter(t *testing.T) {
	g := newGateSandbox(t)
	r := g.runTarget("lint", nil, "STUB_LINTPIN="+buildLintpin(t, g), "STUB_LINT_MISSING=1")
	if r.Code == 0 {
		t.Fatalf("make lint exit 0 with no golangci-lint on PATH, want a refusal\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if out := r.Stdout + r.Stderr; !strings.Contains(out, golangciPin(t)) {
		t.Fatalf("the refusal does not name the pin %s\nstdout:\n%s\nstderr:\n%s", golangciPin(t), r.Stdout, r.Stderr)
	}
	if r.hasCallWithPrefix("golangci-lint run") {
		t.Fatalf("make lint linted with no linter on PATH instead of refusing\ncalls:\n%s", r.callLog())
	}
}
