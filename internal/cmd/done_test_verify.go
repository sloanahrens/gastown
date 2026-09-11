package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

// defaultTestVerifySlotTimeout bounds how long gt done waits for the
// container-gate slot before giving up on the default test-verify gate.
// Distinct from slot.Acquire's own 60-minute CLI default: gt done is on the
// polecat's critical path to submitting an MR, so it fails fast rather than
// queuing indefinitely behind another rig's suite.
const defaultTestVerifySlotTimeout = 20 * time.Minute

// defaultTestVerifyRunTimeout bounds the test command itself, once the slot
// is held — mirrors preVerificationGateTimeout's reasoning (done.go): a hung
// test must not wedge gt done after the branch is already pushed.
const defaultTestVerifyRunTimeout = 10 * time.Minute

// testVerifyResult is the outcome of runDefaultTestVerification.
type testVerifyResult struct {
	// ran is false when there was nothing to verify (no configured test
	// command, or no changed .go files) — skipReason explains why.
	ran        bool
	skipReason string

	success   bool
	packages  []string // populated when scope == "packages"
	scope     string   // "packages" (Go rig, changed-package scoped) or "full" (rig test_command)
	logPath   string
	logSHA256 string
}

// changedGoPackages resolves the branch's changed .go files (relative to
// verifiedBase) to buildable package import paths via `go list` — gt-h9kf:
// "computes the changed packages (git diff base...HEAD --name-only -> go
// list)". A directory that no longer resolves to a package (e.g. its only
// file was deleted) is dropped rather than treated as an error: go list is
// the authority on what is still a package, not the diff.
//
// changedGoFiles reports whether the diff touched any .go file at all, so
// callers can tell "nothing to verify" (no .go changes) apart from "changes
// exist but none resolved to a package" (which should refuse, not skip).
func changedGoPackages(g *git.Git, worktree, verifiedBase string) (pkgs []string, changedGoFiles bool, err error) {
	files, diffErr := g.DiffNameOnly(verifiedBase, "HEAD")
	if diffErr != nil {
		return nil, false, fmt.Errorf("git diff %s...HEAD: %w", shortSHA(verifiedBase), diffErr)
	}

	dirs := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		changedGoFiles = true
		dirs[filepath.Dir(f)] = true
	}

	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Strings(ordered)

	seen := map[string]bool{}
	for _, d := range ordered {
		importPath, listErr := goListDir(worktree, d)
		if listErr != nil {
			continue
		}
		if importPath != "" && !seen[importPath] {
			seen[importPath] = true
			pkgs = append(pkgs, importPath)
		}
	}
	return pkgs, changedGoFiles, nil
}

func goListDir(worktree, dir string) (string, error) {
	rel := "./" + dir
	if dir == "." {
		rel = "."
	}
	//nolint:gosec // G204: dir comes from the branch's own git diff output, run through `go list` (read-only) in the polecat's own worktree.
	cmd := exec.Command("go", "list", rel)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go list %s: %w: %s", rel, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// readLogTail returns the last maxBytes of the file at path (or its full
// contents when shorter), so a failing test-verify run's own error surfaces
// the actual failure output to the polecat instead of just a log path it
// then has to go read separately.
func readLogTail(path string, maxBytes int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) <= maxBytes {
		return string(data)
	}
	return "... (truncated) ...\n" + string(data[len(data)-maxBytes:])
}

// runDefaultTestVerification is gt done's default, non-opt-in test gate
// (gt-h9kf): polecats were submitting MRs with their own new tests never
// run — 2 of 4 gate rejections in a 90-minute window, each costing a full
// refinery gate cycle plus a redispatch, because the existing verification
// (--pre-verified) is opt-in and polecats weren't passing it.
//
// Unless the caller has already secured a full --pre-verified gate run (see
// resolvePreVerification in done.go) or the polecat explicitly opted out
// with --skip-verify, gt done itself tests at least the branch's changed
// packages before an MR bead can be created: on a Go rig it resolves the
// changed packages via `go list` and runs `go test` over them; on a rig
// without that scoping mechanism it runs the rig's full test_command
// instead. Either way the run happens inside the container-gate slot
// (internal/slot) since a changed package may itself hold a Docker-backed
// suite. Any failure — the run itself failing, timing out, or changed .go
// files that resolve to no buildable package — returns an error and the
// caller must not create the MR bead: that is the refusal gt-h9kf asks for.
func runDefaultTestVerification(g *git.Git, worktree, defaultBranch, target string, mq *config.MergeQueueConfig, townRoot, role string) (testVerifyResult, error) {
	if mq == nil || mq.TestCommand == "" {
		return testVerifyResult{skipReason: "rig has no configured test_command — nothing to verify"}, nil
	}

	verifiedBaseRef := g.CleanBaseRef("origin", defaultBranch, target)
	verifiedBase, baseErr := g.Rev(verifiedBaseRef)
	if baseErr != nil {
		return testVerifyResult{}, fmt.Errorf("gt done: could not resolve %s to compute changed packages for the default test-verify gate: %w", verifiedBaseRef, baseErr)
	}

	isGoRig := false
	if _, statErr := os.Stat(filepath.Join(worktree, "go.mod")); statErr == nil {
		isGoRig = true
	}

	var testCmd, scope string
	var pkgs []string

	if isGoRig {
		resolvedPkgs, changedGo, pkgErr := changedGoPackages(g, worktree, verifiedBase)
		if pkgErr != nil {
			return testVerifyResult{}, fmt.Errorf("gt done: could not compute changed packages for the default test-verify gate: %w", pkgErr)
		}
		if !changedGo {
			return testVerifyResult{skipReason: fmt.Sprintf("no changed .go files since %s — nothing to verify", shortSHA(verifiedBase))}, nil
		}
		if len(resolvedPkgs) == 0 {
			return testVerifyResult{}, fmt.Errorf("gt done: changed .go files since %s did not resolve to any buildable package (go list found none) — refusing to submit an unverified MR; fix the build, or use --skip-verify with justification if this is genuinely not testable", shortSHA(verifiedBase))
		}
		pkgs = resolvedPkgs
		scope = "packages"
		testCmd = "go test " + strings.Join(pkgs, " ")
	} else {
		scope = "full"
		testCmd = mq.TestCommand
	}

	logDir := filepath.Join(worktree, constants.DirRuntime)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return testVerifyResult{}, fmt.Errorf("creating test-verify log dir %s: %w", logDir, err)
	}
	logPath := filepath.Join(logDir, "gt-done-verify.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return testVerifyResult{}, fmt.Errorf("creating test-verify log %s: %w", logPath, err)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "=== gt done default test-verify (%s): %s ===\n", scope, testCmd)

	h, acquireErr := slot.Acquire(townRoot, role, defaultTestVerifySlotTimeout)
	if acquireErr != nil {
		return testVerifyResult{}, fmt.Errorf("gt done: could not acquire the container-gate slot for the default test-verify gate: %w", acquireErr)
	}
	defer h.Release()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestVerifyRunTimeout)
	defer cancel()
	// Trust boundary: testCmd is either `go test` over go-list-resolved
	// packages from the polecat's own branch, or the rig's configured
	// test_command (operator-controlled rig config) — same boundary as
	// runPreVerificationGates.
	cmd := exec.CommandContext(ctx, "sh", "-c", testCmd) //nolint:gosec // G204
	cmd.Dir = worktree
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	util.SetDetachedProcessGroup(cmd)
	runErr := cmd.Run()
	timedOut := ctx.Err() == context.DeadlineExceeded

	if timedOut {
		fmt.Fprintf(logFile, "=== test-verify timed out after %s ===\n", defaultTestVerifyRunTimeout)
		return testVerifyResult{}, fmt.Errorf("gt done: default test-verify timed out after %s running %q — see %s", defaultTestVerifyRunTimeout, testCmd, logPath)
	}
	if runErr != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		tail := readLogTail(logPath, 4000)
		return testVerifyResult{}, fmt.Errorf("gt done: default test-verify failed (exit %d) running %q — fix the failing test(s) before resubmitting (full log: %s):\n%s", exitCode, testCmd, logPath, tail)
	}

	result := testVerifyResult{ran: true, success: true, packages: pkgs, scope: scope, logPath: logPath}
	if logBytes, readErr := os.ReadFile(logPath); readErr == nil {
		sum := sha256.Sum256(logBytes)
		result.logSHA256 = hex.EncodeToString(sum[:])
	}
	return result, nil
}
