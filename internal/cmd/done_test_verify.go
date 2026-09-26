package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

// gt-pnkd: the budgets below used to be hardcoded at 20m (slot) and 10m
// (run). Both were smaller than reality on a loaded host — internal/cmd alone
// measures 505-602s, and up to three suites queue behind each other for the
// one container-gate slot — so the gate refused for infrastructure reasons on
// branches whose tests were never run at all. Four one-shot --skip-verify
// exceptions in 24h were the result, i.e. the exceptions became the process.
// The fix is not "raise the numbers": it is that the budgets are surfaceable
// in the verify log and overridable per rig in settings
// (merge_queue.test_verify_*). gt-btw1: the gate now runs the rig's full
// hermetic test_command, so the run budget is a floor, not a per-package
// derivation.

// defaultTestVerifySlotTimeout bounds how long gt done waits for the
// container-gate slot before giving up on the default test-verify gate.
// Deliberately identical to the container-gate slot's own CLI default
// (`gt slot run --timeout`, internal/cmd/slot.go): a queue of N suites each
// holding the slot for its full budget is a legitimate wait, not a failure,
// and the old 20m cap was reached by a *single* 17m35s holder plus one queued
// suite. Raise it per rig with merge_queue.test_verify_slot_timeout.
const defaultTestVerifySlotTimeout = 60 * time.Minute

// defaultTestVerifyRunFloor is the floor for the run budget — the wall-clock
// bound on the test command once the slot is held. Below this the gate starts
// reporting legitimate-but-slow suites as hung, which is the bug.
const defaultTestVerifyRunFloor = 30 * time.Minute

// defaultLintVerifyTimeout bounds the rig's lint_command inside gt done's
// default gate. Lint is static analysis (no Docker, no Dolt, ~7 s on gastown),
// so this is a safety net against a hung tool, not a budget to tune. The gate
// now spends it waiting for a held lock as well as linting (gt-taoz), which is
// why a budget that runs out is attributed rather than reported as findings.
const defaultLintVerifyTimeout = 10 * time.Minute

// lintVerifyTimeout is the budget the lint gate spends, a var so a test can
// drive a real expiry instead of waiting out the default.
var lintVerifyTimeout = defaultLintVerifyTimeout

// testVerifyPackagesPlaceholder is the token merge_queue.test_verify_command
// may embed to receive the resolved changed-package list, e.g.
// "make test-changed PKGS='{packages}'".
const testVerifyPackagesPlaceholder = "{packages}"

// testVerifyProgressInterval is how often a *waiting* (slot) or *running*
// (suite) gate emits a progress line. gt-pnkd's victims could only show a
// frozen 144-byte verify log and 0.0% CPU across 13 minutes of silence — the
// single most expensive part of diagnosing this bug. A var so tests can
// shorten it.
var testVerifyProgressInterval = 2 * time.Minute

// acquireVerifySlot acquires the container-gate slot, returning a release
// func. A var so tests can drive the gate without the real slot (gt-pnkd
// asked for a fake slot + fake runner): slot.Handle's fields are unexported,
// so this deliberately returns just the release closure rather than a
// *slot.Handle a fake could not construct.
var acquireVerifySlot = func(townRoot, role string, timeout time.Duration) (func(), error) {
	h, err := slot.AcquirePool(townRoot, role, timeout, containerGatePool(townRoot))
	if err != nil {
		return nil, err
	}
	return func() { _ = h.Release() }, nil
}

// isContainerSuitePackage reports whether importPath is one of the
// Dolt/testcontainers-backed packages (containerSuitePackages, the same list
// the container-suite guard enforces) or lives under one. Sub-packages are
// treated as container-backed too, matching the guard's prefix-scope reading
// (containerSuiteTarget): erring that way only sends a package to
// the refinery's gate, erring the other way would run an unwrapped suite.
func isContainerSuitePackage(importPath string) bool {
	for _, p := range containerSuitePackages {
		if importPath == p || strings.HasSuffix(importPath, "/"+p) ||
			strings.HasPrefix(importPath, p+"/") || strings.Contains(importPath, "/"+p+"/") {
			return true
		}
	}
	return false
}

// runVerifySuite runs one shell command for the gate with the given
// environment, streaming combined output to logFile. A var so tests can
// substitute a fake runner: the real one shells out to the rig's full
// hermetic test_command (or its test_verify_command override), which no
// unit test may do.
var runVerifySuite = func(ctx context.Context, worktree, script string, env []string, logFile *os.File) error {
	// Trust boundary: script is the rig's configured test_command or the
	// operator's merge_queue.test_verify_command — same boundary as
	// runPreVerificationGates.
	cmd := exec.CommandContext(ctx, "sh", "-c", script) //nolint:gosec // G204
	cmd.Dir = worktree
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// SetProcessGroup, not SetDetachedProcessGroup: only its Cancel hook
	// reaches the script's own children, which would otherwise outlive both
	// the gate's budget and this process (gt-ypkc).
	util.SetProcessGroup(cmd)
	return cmd.Run()
}

// testVerifyResult is the outcome of runDefaultTestVerification.
type testVerifyResult struct {
	// ran is false when there was nothing to verify (no configured test
	// command, or no changed .go files) — skipReason explains why.
	ran        bool
	skipReason string

	success bool
	// the branch's changed packages, recorded for the MR bead (gt-btw1);
	// ["."] stands in when the change is a whole-package deletion verified
	// by `go build ./...` (gt-ytjh) and there is no named package to record.
	packages []string
	scope    string // "full": the gate runs the rig's full hermetic test_command
	// slotUsed is true only when the gate's run can start a container-backed
	// suite (gt-wx53): a Go rig whose command does not turn the container
	// opt-in on runs its whole suite with those tests skipping, so it takes no
	// slot and never queues behind the town's one gate.
	slotUsed bool
	// containersOptedOut is true when the gate wrote the container opt-in off
	// for its run, which is what makes slotUsed false on a Go rig. Recorded
	// (and written to the log header) so a reader can tell a gate that
	// deliberately skipped the Docker suite apart from one whose rig never had
	// containers to run.
	containersOptedOut bool
	// lintRan / lintCommand / lintElapsed record the rig's lint_command run
	// (gt-yihz follow-up): lint is part of the default gate whenever the rig
	// configures it, slot-free, before the tests.
	lintRan     bool
	lintCommand string
	lintElapsed time.Duration
	logPath     string
	logSHA256   string

	// Budgets and timings, resolved and measured, so the MR bead records what
	// the gate was actually allowed and what it actually cost (gt-pnkd). The
	// provenance of each budget (and the resolved command and environment)
	// lives in the verify log's header, which logSHA256 covers.
	runBudget   time.Duration
	slotWait    time.Duration
	runElapsed  time.Duration
	slotTimeout time.Duration
}

// testVerifyBudgets is the resolved budget pair for one gate run, with the
// provenance of each number so the verify log can explain itself — a log that
// says "run budget: 30m (derived floor; the rig's own per-package -timeout
// cannot scale a full-suite run)" answers the question gt-pnkd's victims
// could not answer from a frozen header.
type testVerifyBudgets struct {
	slotTimeout time.Duration
	slotSource  string
	runTimeout  time.Duration
	runSource   string
}

// unresolvedChangedDir is a changed .go directory that still holds .go files
// in the worktree but that `go list` would not turn into a package. listErr
// keeps the tool's own words so a refusal can quote them.
type unresolvedChangedDir struct {
	dir     string
	listErr error
}

// nestedChangedDir is a changed .go directory that belongs to a module nested
// inside the worktree — a directory below the worktree root with its own
// go.mod. The worktree's own module contains neither the directory nor a
// package for it, so its `go list` fails on a file that is perfectly good;
// moduleRoot and importPath name where the file does resolve.
type nestedChangedDir struct {
	dir        string
	moduleRoot string
	importPath string
}

// changedGoResolution is the outcome of resolving a branch's changed .go files
// against the worktree.
type changedGoResolution struct {
	// packages are the import paths the branch's changed .go files resolve
	// to. A directory `go list` cannot resolve contributes nothing.
	packages []string
	// deletedDirs are changed .go directories that no longer hold any .go
	// file: the whole-package deletion shape, where go list failing is the
	// deletion talking and not an unbuildable change. The module still has to
	// build, and only a whole-module build can say so.
	deletedDirs []string
	// nestedModules are changed .go directories that belong to a nested
	// module. They hold .go files this module cannot name, but nothing is
	// wrong with the files: they are resolved, built and logged as the other
	// module's, not refused as this one's broken change.
	nestedModules []nestedChangedDir
	// unresolvable are changed .go directories that still hold a .go file but
	// that `go list` could not resolve — a file that no longer compiles, a
	// file whose build constraints exclude it, a stray .go file outside any
	// package. These are NOT deletions, and `go build ./...` will happily
	// succeed for some of them (see the refusal at the gate).
	unresolvable []unresolvedChangedDir
	// changedGoFiles reports whether the diff touched any .go file at all, so
	// callers can tell "nothing to verify" (no .go changes) apart from
	// "changes exist but none resolved to a package" (which gt-h9kf says must
	// refuse, not skip).
	changedGoFiles bool
}

// changedGoPackages resolves the branch's changed .go files (relative to
// verifiedBase) into what the gate can act on — gt-h9kf: "computes the changed
// packages (git diff base...HEAD --name-only -> go list)". `go list`, not the
// diff, is the authority on what is still a package, so a changed directory it
// will not resolve is classified rather than dropped: deletedDirs when the
// directory holds no .go file any more, nestedModules when the directory
// belongs to another module inside the worktree, and unresolvable when a .go
// file is still there that this module will not make a package of — one that
// no longer compiles, one its build constraints exclude, a stray .go file
// outside any package. Only the last is a refusal: nothing here builds or
// tests a file the module cannot name, so no check can stand in for it.
func changedGoPackages(g *git.Git, worktree, verifiedBase string) (changedGoResolution, error) {
	var res changedGoResolution

	files, diffErr := g.DiffNameOnly(verifiedBase, "HEAD")
	if diffErr != nil {
		return res, fmt.Errorf("git diff %s...HEAD: %w", shortSHA(verifiedBase), diffErr)
	}

	dirs := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		res.changedGoFiles = true
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
		if listErr == nil {
			if importPath != "" && !seen[importPath] {
				seen[importPath] = true
				res.packages = append(res.packages, importPath)
			}
			continue
		}
		// go list is the authority on what is a package, and it said no.
		// Which of the three reasons it said no for decides what the caller
		// may do about it.
		if !dirHoldsGoFiles(worktree, d) {
			res.deletedDirs = append(res.deletedDirs, d)
			continue
		}
		// The directory still holds .go files. A nested module is the one
		// reason that is not a broken change: the file is fine, it just
		// belongs to a module this one does not contain, so resolve it
		// there instead of reading this module's refusal as the file's.
		moduleRoot, moduleErr := nestedModuleRoot(worktree, d)
		if moduleErr != nil {
			res.unresolvable = append(res.unresolvable, unresolvedChangedDir{dir: d, listErr: moduleErr})
			continue
		}
		if moduleRoot == "" {
			res.unresolvable = append(res.unresolvable, unresolvedChangedDir{dir: d, listErr: listErr})
			continue
		}
		nestedPath, nestedErr := goListDirInModule(worktree, moduleRoot, d)
		if nestedErr != nil {
			// The module that owns the file will not make a package of it
			// either, so nothing anywhere here could compile or test it:
			// still a refusal, in the words of the module that owns it.
			res.unresolvable = append(res.unresolvable, unresolvedChangedDir{dir: d, listErr: nestedErr})
			continue
		}
		res.nestedModules = append(res.nestedModules, nestedChangedDir{dir: d, moduleRoot: moduleRoot, importPath: nestedPath})
	}
	return res, nil
}

// nestedModuleRoot returns the worktree-relative directory of the module that
// owns dir, when that module is nested inside the worktree — a directory below
// the worktree root holding its own go.mod (plugins/dolt-snapshots, say). The
// worktree's own module does not own any of them, so the walk stops before the
// root and returns "" for a directory the root module owns; the root module's
// packages are resolved, not skipped.
//
// A walk that cannot read a candidate go.mod reports it rather than guessing at
// a module boundary: the caller turns that into the gate's refusal.
func nestedModuleRoot(worktree, dir string) (string, error) {
	for d := dir; d != "."; d = filepath.Dir(d) {
		_, err := os.Stat(filepath.Join(worktree, d, "go.mod"))
		if err == nil {
			return d, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("looking for a go.mod above %s: %w", dir, err)
		}
	}
	return "", nil
}

// goListDirInModule resolves dir, a worktree-relative path inside the module at
// moduleRoot, to a package import path by running `go list` from that module's
// own root. A module that does not resolve dir to a package — including one it
// resolves to nothing at all — is an error, quoting the tool.
func goListDirInModule(worktree, moduleRoot, dir string) (string, error) {
	pattern := "."
	if dir != moduleRoot {
		rel, relErr := filepath.Rel(moduleRoot, dir)
		if relErr != nil {
			return "", fmt.Errorf("locating %s within module %s: %w", dir, moduleRoot, relErr)
		}
		pattern = "./" + filepath.ToSlash(rel)
	}
	//nolint:gosec // G204: the directory comes from the branch's own git diff, run through a read-only `go list` in the polecat's own worktree.
	cmd := exec.Command("go", "list", pattern)
	cmd.Dir = filepath.Join(worktree, moduleRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go list %s in the nested module %s: %w: %s", pattern, moduleRoot, err, strings.TrimSpace(string(out)))
	}
	importPath := strings.TrimSpace(string(out))
	if importPath == "" {
		return "", fmt.Errorf("go list %s in the nested module %s named no package", pattern, moduleRoot)
	}
	return importPath, nil
}

// dirHoldsGoFiles reports whether dir (relative to worktree) still contains a
// .go file, which is what tells a deleted package directory apart from one
// `go list` refuses to resolve. A directory that does not exist holds none.
func dirHoldsGoFiles(worktree, dir string) bool {
	entries, err := os.ReadDir(filepath.Join(worktree, dir))
	if err != nil {
		// A directory that is gone holds nothing left to resolve, but any
		// other reason the listing failed is not evidence of a deletion —
		// that is the weaker claim this function exists to make, so report
		// the stronger one and let the gate refuse (gt-7rds: fail closed).
		return !errors.Is(err, fs.ErrNotExist)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
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

// goBuildWholeModule builds the module rooted at dir, which is the check that
// distinguishes a whole-package deletion that still compiles (legitimate, and
// verifiable — the deletion is either consistent or the build breaks, and the
// build is the authority) from one whose importers are now broken (gt-ytjh). A
// var so tests can stub it: the real one shells out to `go build`, which no
// unit test may do.
//
// dir is the module to build: the worktree for a change to this module, and a
// nested module's own directory for a change to that one. Building anything
// else verifies the wrong tree — gt done takes a worktree argument, and the
// process's own directory is the host checkout, not the branch under test —
// and this module's `go build ./...` does not reach a nested module at all.
var goBuildWholeModule = func(dir string) error {
	buildArgs := []string{"build", "./..."}
	// `go build ./...` discards the objects of a package list, and writes an
	// executable only when that list resolves to exactly one main package —
	// named after the package's source directory. So a single-binary module
	// whose last library package this diff deletes either drops that binary
	// into the tree about to be handed to the refinery, or, when the name is
	// the directory it came from, fails the build outright ("build output
	// \"cmd\" already exists and is a directory") and refuses a deletion that
	// compiles. -o sends the executables to a temp dir that is removed again
	// instead. A module with no main packages has nothing to write and rejects
	// -o ("go: no main packages to build"), which would be a false refusal for
	// every library-only rig and test fixture, so the plain form runs there;
	// with no main package it writes nothing.
	if moduleHasMainPackage(dir) {
		outDir, err := os.MkdirTemp("", "gt-whole-module-build-")
		if err != nil {
			return fmt.Errorf("creating a temp dir for the whole-module build output: %w", err)
		}
		defer os.RemoveAll(outDir)
		buildArgs = []string{"build", "-o", outDir, "./..."}
	}

	//nolint:gosec // G204: fixed arguments, run in the module under test.
	cmd := exec.Command("go", buildArgs...)
	cmd.Dir = dir
	out, buildErr := cmd.CombinedOutput()
	if buildErr != nil {
		// The compiler's own words, not just "fix the build" (gt-7rds):
		// every other refusal in the gate quotes the failure it saw.
		return fmt.Errorf("go build ./...: %w: %s", buildErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// moduleHasMainPackage reports whether the module in worktree has a package
// main for `go build ./...` to link, which is what decides whether that build
// needs an output directory to keep the binaries out of the worktree.
//
// A listing that fails is not this function's to report: it returns false and
// lets the build that follows surface the same problem, in the compiler's own
// words, as the refusal the polecat needs to read.
func moduleHasMainPackage(worktree string) bool {
	//nolint:gosec // G204: fixed arguments, read-only `go list` in the worktree under test.
	cmd := exec.Command("go", "list", "-f", `{{if eq .Name "main"}}{{.ImportPath}}{{end}}`, "./...")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
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

// readLogFrom returns the part of the log at path written since byte offset
// from, so a gate can inspect just the slice one attempt produced instead of
// the whole log it shares with earlier attempts (gt-xsty's lint retry: a
// marker from a previous attempt must not decide the current one).
func readLogFrom(path string, from int64) string {
	data, err := os.ReadFile(path)
	if err != nil || from < 0 || from >= int64(len(data)) {
		return ""
	}
	return string(data[from:])
}

// fileSize is the current size of an open gate log, used as the offset to hand
// readLogFrom. Stat on the *os.File (never a buffered writer: the gates stream
// straight to the fd) reports exactly what a reader of the path can see.
func fileSize(f *os.File) int64 {
	if f == nil {
		return 0
	}
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

// humanDuration renders a duration the way it is written in config and in a
// Makefile ("20m", "1h", "90m") rather than Go's "20m0s", so a budget quoted
// in the verify log is greppable in the rig's own files.
func humanDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0 && d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// splitCommandEnvPrefix splits a shell command string into its leading
// VAR=value environment assignments and the remaining argv words, mirroring
// how a POSIX shell applies a command-scoped environment: in
// `GOFLAGS=-p=6 make test`, make runs with GOFLAGS set. This is how the
// default test-verify gate inherits the rig's configured test_command
// environment without running the full suite (gt-fa3s: the gate used to run a
// bare `go test <pkgs>`, diverging from the refinery's suite).
//
// Tokens are split on whitespace, so a quoted value containing spaces is not
// interpreted — for that, set merge_queue.test_verify_command.
func splitCommandEnvPrefix(command string) (env, argv []string) {
	tokens := strings.Fields(command)
	i := 0
	for ; i < len(tokens); i++ {
		name, _, isAssign := strings.Cut(tokens[i], "=")
		if !isAssign || !isEnvVarName(name) {
			break
		}
		env = append(env, tokens[i])
	}
	return env, tokens[i:]
}

// isEnvVarName reports whether s is a valid POSIX environment variable name
// (letters, digits and underscore, not starting with a digit) — the guard
// that keeps splitCommandEnvPrefix from mistaking a bare argument such as
// `1=1` or a stray `=` for an assignment.
func isEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// containerOptInRequested reports whether any of the given command texts turns
// the container opt-in (testutil.DockerTestsEnv) on inline — "GT_TEST_DOCKER=1
// make test", "env GT_TEST_DOCKER=1 go test ...", "export GT_TEST_DOCKER=1;
// ...". It reuses the container-suite guard's tokenizer (same package), so the
// gate and the tap guard agree on what "this command wants containers" means.
func containerOptInRequested(commands ...string) bool {
	for _, c := range commands {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if commandEnablesDockerTests(shellTokenize(c)) {
			return true
		}
	}
	return false
}

// containerSwitch is the gate's container opt-in decision for the run it is
// about to make: the value written into the run's environment, and whether that
// value means the gate must hold the town-wide container-gate slot (gt-wx53).
//
// The two travel together (gt-0hbm): what decides whether a container starts is
// the environment the child reads, so the gate writes the value it decided into
// that environment (verifyGateEnv) instead of scanning for a slot and letting
// whatever the session exported reach the child.
type containerSwitch struct {
	// value is the GT_TEST_DOCKER value the gate writes into the run's
	// environment, "0" or "1". Empty leaves the rig's own environment alone.
	value string
	// slot is true when the run can start a container-backed suite, and so
	// must hold the container-gate slot.
	slot bool
}

// optedOut reports whether the gate's run is made with the container opt-in
// written off — the case the log header and the MR bead call "containers opted
// out" (gt-wx53).
func (s containerSwitch) optedOut() bool { return s.value == "0" }

// resolveContainerSwitch decides the container opt-in for the gate's run from
// the rig's own command text, never from this process's ambient value.
//
// gt-wx53: the slot is town-wide and one suite wide, so taking it for a run
// that cannot start a container makes every `gt done` in the town queue behind
// the daemon's main-branch patrol and the refinery's batch gate — mica's gt
// done on gt-jqif waited out the full 20m cap for a gate that never ran a test
// (gt-pnkd). A Go rig asks for containers in its own command or not at all; the
// rest run the rig's whole suite with the switch written off, their
// container-backed tests skipping, and the refinery's slot-holding gate runs
// those once per submission (gt-yihz, operator decision 14:44 2026-09-17). A
// non-Go rig's command is opaque, so it keeps the slot: the conservative
// direction.
//
// The ambient value stays out of the decision (gt-0hbm) because it does not
// describe this run: the Makefile's test recipe defaults GT_TEST_DOCKER to 1,
// so every process under `make test` — the daemon's patrol and the integration
// suite, which hold the slot themselves, included — carries =1 without anyone
// asking for containers, and reading it would put the gate behind a slot its
// own ancestor holds. Writing the resolved value into the run's environment is
// what keeps the gate and the child's reading of the switch in step.
func resolveContainerSwitch(isGoRig bool, commands ...string) containerSwitch {
	if !isGoRig {
		return containerSwitch{slot: true}
	}
	if containerOptInRequested(commands...) {
		return containerSwitch{value: "1", slot: true}
	}
	return containerSwitch{value: "0"}
}

// verifyGateEnv builds the child environment for the gate's lint and suite
// runs: this process's environment, then the rig's test_command env prefix
// (gt-fa3s), then the gate's resolved container switch (gt-0hbm).
//
// Every inherited and rig-supplied value of the switch's name is dropped before
// the resolved one is appended, rather than left for the child to resolve: a
// duplicate entry is read differently by different readers (Go's os.Getenv
// takes the last, a libc getenv the first), and this value decides whether a
// container starts outside the container-gate slot.
func verifyGateEnv(envPrefix []string, switchValue string) []string {
	env := make([]string, 0, len(os.Environ())+len(envPrefix)+1)
	keep := func(kv string) bool {
		return switchValue == "" || !strings.HasPrefix(kv, dockerTestsEnv+"=")
	}
	for _, kv := range os.Environ() {
		if keep(kv) {
			env = append(env, kv)
		}
	}
	for _, kv := range envPrefix {
		if keep(kv) {
			env = append(env, kv)
		}
	}
	if switchValue != "" {
		env = append(env, dockerTestsEnv+"="+switchValue)
	}
	return env
}

// resolveTestVerifyBudgets turns the rig's configuration into the two budgets
// the gate runs under: how long to wait for the slot, and how long the suite
// may run once it has it. Explicit rig config wins; otherwise the slot cap
// takes the slot's own CLI default and the run budget takes the 30m floor —
// the gate now runs the rig's full test_command, so there is no changed-
// package count to scale by, and a per-package -timeout cannot bound a whole
// suite (gt-btw1).
//
// The two are deliberately independent (gt-pnkd: "never count slot wait
// against the run budget"): the run timer starts only after the slot is held,
// so a deep queue can never eat the budget the suite itself needs.
func resolveTestVerifyBudgets(mq *config.MergeQueueConfig) testVerifyBudgets {
	b := testVerifyBudgets{
		slotTimeout: defaultTestVerifySlotTimeout,
		slotSource:  "default — matches `gt slot run --timeout`",
	}
	if mq != nil {
		if raw := strings.TrimSpace(mq.TestVerifySlotTimeout); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				b.slotTimeout, b.slotSource = d, "merge_queue.test_verify_slot_timeout"
			} else {
				b.slotSource = fmt.Sprintf("default — merge_queue.test_verify_slot_timeout %q is not a positive duration, so the `gt slot run --timeout` default applies", raw)
			}
		}
	}

	if mq != nil && strings.TrimSpace(mq.TestVerifyRunTimeout) != "" {
		raw := strings.TrimSpace(mq.TestVerifyRunTimeout)
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			b.runTimeout, b.runSource = d, "merge_queue.test_verify_run_timeout"
			return b
		}
		b.runSource = fmt.Sprintf("derived — merge_queue.test_verify_run_timeout %q is not a positive duration; ", raw)
	}

	// The gate runs the rig's full test_command, so the run budget is the
	// floor: a per-package -timeout cannot scale a whole-suite run (gt-btw1).
	// Rigs whose suite legitimately needs longer set
	// merge_queue.test_verify_run_timeout.
	b.runTimeout = defaultTestVerifyRunFloor
	b.runSource += fmt.Sprintf("derived floor %s (the gate runs the rig's full test_command, so there is no changed-package count to scale by)", humanDuration(defaultTestVerifyRunFloor))
	return b
}

// runWithProgress runs fn in a goroutine, calling progress with the elapsed
// time every interval until fn returns, and returns fn's error plus how long
// fn took. fn must return promptly once its own context expires — the
// progress loop has no way to cancel it.
func runWithProgress(interval time.Duration, fn func() error, progress func(time.Duration)) (time.Duration, error) {
	start := time.Now()
	if interval <= 0 {
		err := fn()
		return time.Since(start), err
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return time.Since(start), err
		case <-ticker.C:
			progress(time.Since(start))
		}
	}
}

// reportVerifyProgress writes one progress line to both the verify log and
// the polecat's pane. The log matters because the log is the evidence a
// bystander reads after the fact (gt-pnkd's frozen 144-byte log); the pane
// matters because a silent terminal is indistinguishable from a hung agent
// (gt-hkhu).
func reportVerifyProgress(logFile *os.File, msg string) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
	if logFile != nil {
		_, _ = fmt.Fprint(logFile, line)
	}
	_, _ = fmt.Fprint(os.Stdout, line)
}

// currentSlotHolder renders the container-gate slot's current holder for a
// progress line, read from the slot package's display-only owner file (see
// internal/slot's package doc: it is decoration, never authoritative, and is
// used here for nothing but text). Best-effort — any error yields "".
// Deliberately not slot.Status, which shells out to `docker ps` and would add
// a docker round-trip to every progress tick.
func currentSlotHolder(townRoot string) string {
	data, err := os.ReadFile(slot.OwnerPath(townRoot))
	if err != nil {
		return ""
	}
	var o slot.Owner
	if err := json.Unmarshal(data, &o); err != nil || o.Role == "" {
		return ""
	}
	held := ""
	if !o.AcquiredAt.IsZero() {
		held = fmt.Sprintf(", held %s", time.Since(o.AcquiredAt).Round(time.Second))
	}
	return fmt.Sprintf("%s (pid %d%s)", o.Role, o.PID, held)
}

// acquireVerifySlotWithProgress acquires the container-gate slot, emitting a
// progress line every testVerifyProgressInterval while it waits, and reports
// how long the wait took. Release is non-nil only when err is nil.
func acquireVerifySlotWithProgress(townRoot, role string, timeout time.Duration, logFile *os.File) (release func(), waited time.Duration, err error) {
	waited, err = runWithProgress(testVerifyProgressInterval, func() error {
		var acquireErr error
		release, acquireErr = acquireVerifySlot(townRoot, role, timeout)
		return acquireErr
	}, func(elapsed time.Duration) {
		msg := fmt.Sprintf("still waiting for the container-gate slot (%s elapsed, cap %s)",
			elapsed.Round(time.Second), humanDuration(timeout))
		if holder := currentSlotHolder(townRoot); holder != "" {
			msg += " — holder: " + holder
		}
		reportVerifyProgress(logFile, msg)
	})
	if err != nil {
		return nil, waited, err
	}
	return release, waited, nil
}

// testVerifyLogPath is where the default test-verify gate writes its log —
// inside the worktree, so the artifact survives the session that produced it
// and a bystander can read the same evidence the failure message cites.
func testVerifyLogPath(worktree string) string {
	return filepath.Join(worktree, constants.DirRuntime, "gt-done-verify.log")
}

// runDefaultTestVerification is gt done's default, non-opt-in test gate
// (gt-h9kf): polecats were submitting MRs with their own new tests never
// run — 2 of 4 gate rejections in a 90-minute window, each costing a full
// refinery gate cycle plus a redispatch, because the existing verification
// (--pre-verified) is opt-in and polecats weren't passing it.
//
// Unless the caller has already secured a full --pre-verified gate run (see
// resolvePreVerification in done.go) or the polecat explicitly opted out
// with --skip-verify, gt done itself runs the rig's hermetic test_command
// before an MR bead can be created (gt-btw1): the gate used to run a bare
// `go test` over the changed packages, which bypassed the rig's hermetic
// test environment (BEADS_TEST_MODE, test-env.sh, Makefile CGO flags) and
// failed every test-only change in a rig whose package is red on main for
// ambient-env reasons. Where the run happens depends on whether it can start
// containers (gt-wx53, see gateNeedsSlot): a run that can holds a
// container-gate slot (internal/slot) for its whole duration, and a run that
// cannot — the common case, since the container opt-in is off unless the rig
// asks for it — runs the same hermetic command with those tests skipping and
// takes no slot at all. Any failure — the run itself failing, timing out,
// changed .go files that are still present but that `go list` will not resolve
// to a package, or a whole-module build that confirms a package deletion broke
// the build — returns an error and the caller must not create the MR bead:
// that is the refusal gt-h9kf asks for. A change this module cannot name for a
// reason that is not a broken file is not a refusal: a whole-package deletion
// that still compiles (gt-ytjh) is verified by `go build ./...` before the slot
// and then runs the suite; a nested module's files are built in that module's
// own directory, and the run's log header records the switch.
//
// gt-pnkd: the budgets and the command are now resolved rather than
// hardcoded, and every resolved value is written to the verify log header
// before the gate does anything, so a gate that cannot run says why in the
// only artifact left behind. A slot-acquire failure is reported as slot
// contention — explicitly not a test failure, and explicitly counted
// separately from the run budget.
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

	// The rig's configured test_command environment (e.g. `GOFLAGS=-p=6 make
	// test`) applies to the gate too, so the gate cannot diverge from the
	// refinery's suite (gt-fa3s).
	envPrefix, _ := splitCommandEnvPrefix(mq.TestCommand)

	// Default: the gate runs the rig's whole hermetic test_command
	// (gt-btw1), and the changed-package list is only recorded on the MR
	// bead. A rig that sets merge_queue.test_verify_command with the
	// {packages} token opts into running just that list instead, and the
	// label below is corrected where that substitution happens.
	scope := "full"
	var pkgs []string
	// changed is filled for a Go rig only; its zero value reports "no changed
	// .go files", which is what a non-Go rig has to say.
	var changed changedGoResolution
	if isGoRig {
		resolved, pkgErr := changedGoPackages(g, worktree, verifiedBase)
		if pkgErr != nil {
			return testVerifyResult{}, fmt.Errorf("gt done: could not compute changed packages for the default test-verify gate: %w", pkgErr)
		}
		changed = resolved
		if !changed.changedGoFiles {
			return testVerifyResult{skipReason: fmt.Sprintf("no changed .go files since %s — nothing to verify", shortSHA(verifiedBase))}, nil
		}
		if len(changed.unresolvable) > 0 {
			// Changed .go files that are still in the worktree but that
			// `go list` will not make a package of: a file that no longer
			// compiles, a file whose build constraints exclude it, a stray
			// .go file outside any package. They are not deletions, and a
			// whole-module build cannot stand in for testing them — for the
			// all-build-tag-excluded shape `go build ./...` SUCCEEDS, having
			// nothing to build — so there is nothing safe to scope a suite to
			// either. Refuse, and quote what `go list` said so the polecat
			// can act on it.
			details := make([]string, 0, len(changed.unresolvable))
			for _, u := range changed.unresolvable {
				details = append(details, fmt.Sprintf("%s: %v", u.dir, u.listErr))
			}
			return testVerifyResult{}, fmt.Errorf("gt done: the branch's changed .go file(s) are still present but `go list` resolves no package for %s — that is not a package deletion, so no whole-module build can verify it; fix the file so it compiles and resolves (or use --skip-verify with justification if this is genuinely not testable)", strings.Join(details, "; "))
		}
		// A single diff can carry both shapes at once (a deleted package here,
		// a nested-module edit there), so these run independently rather than
		// as a switch's mutually-exclusive cases: a switch that only ran the
		// first matching case silently skipped the other build while the log
		// below still claimed every nested module got one.
		if len(changed.deletedDirs) > 0 {
			// A whole-package deletion: every changed .go file in these
			// directories is gone, so go list no longer resolves them to a
			// package and nothing is left to scope. That is not a broken
			// build — a clean deletion of every .go file in a package is
			// legitimate work — but the rest of the module either still
			// compiles (the deletion is verified) or it doesn't (the
			// refusal below stays). The build runs whenever the diff deletes
			// a package, even when other changed packages did resolve,
			// because the packages that import the deleted one are not in
			// the changed set: `go list` resolves a package whose imports are
			// broken, so only the build sees them.
			if buildErr := goBuildWholeModule(worktree); buildErr != nil {
				return testVerifyResult{}, fmt.Errorf("gt done: deleting every .go file in a package since %s left the rest of the module unbuildable — the deletion broke something that imports it; fix the build (or undo the deletion) before submitting, or use --skip-verify with justification if this is genuinely not testable: %w", shortSHA(verifiedBase), buildErr)
			}
		}
		if len(changed.nestedModules) > 0 {
			// The changed files belong to nested modules, so no package of
			// this module names them and there is nothing here to scope: the
			// suite runs over the module root, and the header below records
			// which directories this module's gate cannot reach. Each of
			// those modules is built in its own directory — the same
			// stand-in the deletion path uses, for the same reason. It is
			// not a formality: `go list` does not typecheck, so a file that
			// resolves in its own module can still be one this build is the
			// only check of.
			for _, n := range changed.nestedModules {
				if buildErr := goBuildWholeModule(filepath.Join(worktree, n.moduleRoot)); buildErr != nil {
					return testVerifyResult{}, fmt.Errorf("gt done: the branch's changed .go file(s) are in the nested module %s, and building that module failed — fix it before submitting, or use --skip-verify with justification if this is genuinely not testable: %w", n.moduleRoot, buildErr)
				}
			}
		}
		switch {
		case len(changed.deletedDirs) > 0, len(changed.nestedModules) > 0:
			pkgs = []string{"."}
			if len(changed.packages) > 0 {
				pkgs = changed.packages
			}
		default:
			pkgs = changed.packages
		}
	}

	// The full suite may spin Dolt/testcontainers containers (internal/cmd,
	// internal/refinery, ...), and a container must never start outside the
	// container-gate slot. gt-wx53: it must not start *inside* it either when
	// nothing in this run needs one — a gate that always took the slot made
	// every `gt done` in the town queue for a town-wide, one-suite-wide
	// resource, sometimes for longer than the suite it was queueing behind.
	// So the gate either runs the rig's whole suite with the container opt-in
	// written off (container-backed tests skip, no slot needed) or, when the
	// rig's own command asks for containers, runs it inside a slot.
	cswitch := resolveContainerSwitch(isGoRig, mq.TestCommand, mq.TestVerifyCommand)
	needsSlot := cswitch.slot
	containersOptedOut := cswitch.optedOut()

	budgets := resolveTestVerifyBudgets(mq)

	// merge_queue.test_verify_command, when set, is the rig's own scoped
	// variant of the suite (e.g. "make test-changed PKGS='{packages}'") and
	// replaces the full test_command; the {packages} token receives the
	// resolved changed-package list.
	testCmd := mq.TestCommand
	if override := strings.TrimSpace(mq.TestVerifyCommand); override != "" {
		if isGoRig {
			// Only a command that actually consumed the package list ran a
			// scoped suite; an override without the placeholder is simply a
			// different whole-suite command. The label is the sole record of
			// which one a polecat verified with, so it has to distinguish
			// them rather than report "full" for both.
			if strings.Contains(override, testVerifyPackagesPlaceholder) {
				scope = "changed"
			}
			testCmd = strings.ReplaceAll(override, testVerifyPackagesPlaceholder, strings.Join(pkgs, " "))
		} else if strings.Contains(override, testVerifyPackagesPlaceholder) {
			return testVerifyResult{}, fmt.Errorf("gt done: merge_queue.test_verify_command uses %s, but this rig is not a Go module so no changed-package list can be resolved for it — either drop the placeholder or point the gate at the whole suite", testVerifyPackagesPlaceholder)
		}
	}

	logDir := filepath.Join(worktree, constants.DirRuntime)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return testVerifyResult{}, fmt.Errorf("creating test-verify log dir %s: %w", logDir, err)
	}
	logPath := testVerifyLogPath(worktree)
	logFile, err := os.Create(logPath)
	if err != nil {
		return testVerifyResult{}, fmt.Errorf("creating test-verify log %s: %w", logPath, err)
	}
	defer logFile.Close()

	// Resolved budget + environment first, command second, so the header of
	// the log is self-explanatory even if the gate never gets further than
	// this (gt-pnkd's victims had a 144-byte header and no way to tell a slot
	// cap from a test failure).
	fmt.Fprintf(logFile, "=== gt done default test-verify (scope=%s) ===\n", scope)
	fmt.Fprintf(logFile, "command: %s\n", testCmd)
	if lint := strings.TrimSpace(mq.LintCommand); lint != "" {
		fmt.Fprintf(logFile, "lint: %s (budget %s, no container slot)\n", lint, humanDuration(lintVerifyTimeout))
	}
	fmt.Fprintf(logFile, "run budget: %s (%s)\n", humanDuration(budgets.runTimeout), budgets.runSource)
	fmt.Fprintf(logFile, "slot cap: %s (%s)\n", humanDuration(budgets.slotTimeout), budgets.slotSource)
	if len(envPrefix) > 0 {
		fmt.Fprintf(logFile, "env (inherited from test_command): %s\n", strings.Join(envPrefix, " "))
	}
	// A directory this gate could not scope has to be visible in the artifact
	// that outlives the run: a reader who sees the changed files and no line
	// for them cannot tell a file that was skipped from one that was never
	// noticed.
	for _, n := range changed.nestedModules {
		fmt.Fprintf(logFile, "not scoped: %s belongs to the nested module %s (%s); this module's suite does not run it, so the gate built that module in its own directory instead\n", n.dir, n.moduleRoot, n.importPath)
	}
	// Whether this gate queues for the town's container-gate slot, and why, is
	// the first thing a reader of a slow or refused gate needs (gt-wx53: the
	// answer used to be an unconditional yes, stated nowhere).
	if containersOptedOut {
		fmt.Fprintf(logFile, "containers: opted out (%s=0) — container-backed tests skip here and run once per submission in the refinery's gate, so this gate takes no container-gate slot; a container that starts anyway fails this run (gt-0ss4)\n", dockerTestsEnv)
	} else if cswitch.value == "" {
		fmt.Fprintf(logFile, "containers: enabled — a non-Go rig's command does not say whether it starts Docker, so this gate takes a container-gate slot\n")
	} else {
		fmt.Fprintf(logFile, "containers: enabled (the rig's command turns %s=1 on) — this run may start container-backed suites, so it takes a container-gate slot\n", dockerTestsEnv)
	}
	reportVerifyProgress(logFile, fmt.Sprintf("gate starting (command: %s; run budget %s; slot cap %s)",
		testCmd, humanDuration(budgets.runTimeout), humanDuration(budgets.slotTimeout)))

	env := verifyGateEnv(envPrefix, cswitch.value)

	// Lint first: cheap, slot-free, and a lint failure should not cost a
	// suite run. The rig's lint_command is the same one the batch gate runs.
	result := testVerifyResult{
		scope:              scope,
		packages:           pkgs,
		slotUsed:           needsSlot,
		containersOptedOut: containersOptedOut,
		logPath:            logPath,
		runBudget:          budgets.runTimeout,
		slotTimeout:        budgets.slotTimeout,
	}
	if lint := strings.TrimSpace(mq.LintCommand); lint != "" {
		result.lintCommand = lint
		reportVerifyProgress(logFile, fmt.Sprintf("lint starting (command: %s)", lint))
		// One context spans every attempt: the lint budget bounds the lint,
		// retries and waits included, rather than being renewed per try
		// (gt-xsty).
		lintCtx, lintCancel := context.WithTimeout(context.Background(), lintVerifyTimeout)
		lintStart := time.Now()
		outcome := lintlock.Retry(lintCtx, func() lintlock.Attempt {
			from := fileSize(logFile)
			err := runVerifySuite(lintCtx, worktree, lint, env, logFile)
			return lintlock.Attempt{Err: err, Output: readLogFrom(logPath, from)}
		}, func(attempt, attempts int, wait time.Duration) {
			reportVerifyProgress(logFile, fmt.Sprintf("lint lock held by another golangci-lint (attempt %d/%d); retrying in %s", attempt, attempts, wait.Round(time.Second)))
		})
		// Read the deadline out before canceling: afterwards the context is
		// canceled either way, and "the budget ran out while this lint was
		// still going" is the whole difference between a finding and nothing
		// having been linted at all (gt-taoz).
		budgetExpired := errors.Is(lintCtx.Err(), context.DeadlineExceeded)
		lintCancel()
		result.lintElapsed = time.Since(lintStart)
		if outcome.Err != nil {
			exitCode := -1
			var exitErr *exec.ExitError
			if errors.As(outcome.Err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			fmt.Fprintf(logFile, "=== lint failed (exit %d) ===\n", exitCode)
			tail := readLogTail(logPath, 4000)
			detail := lintFailureDetail(outcome, budgetExpired, lintVerifyTimeout)
			return testVerifyResult{}, fmt.Errorf("gt done: default lint-verify failed (exit %d) running %q — %s (no tests were run; full log: %s):\n%s", exitCode, lint, detail, logPath, tail)
		}
		result.lintRan = true
		reportVerifyProgress(logFile, fmt.Sprintf("lint passed in %s", result.lintElapsed.Round(time.Second)))
	}

	var slotWait time.Duration
	if needsSlot {
		release, waited, acquireErr := acquireVerifySlotWithProgress(townRoot, role, budgets.slotTimeout, logFile)
		if acquireErr != nil {
			// One invocation, then a bead comment and an escalation to the
			// mayor: the escalation is the sanctioned move at the cap, not a
			// retry, so the message must not be the thing that suggests one
			// (gt-7dxw).
			return testVerifyResult{}, fmt.Errorf(
				"gt done: could not acquire the container-gate slot for the default test-verify gate after %s (cap %s): %w — this is slot contention, NOT a test failure, and nothing in your diff was tested. Do not retry or loop on it: add a bead comment with this error and the verify log at %s, then run `gt escalate -s medium` asking the mayor for a one-shot --skip-verify ruling, and wait. Raising merge_queue.test_verify_slot_timeout is the rig-level alternative; --skip-verify with justification is the last resort (gt-7dxw)",
				waited.Round(time.Second), humanDuration(budgets.slotTimeout), acquireErr, logPath)
		}
		defer release()
		slotWait = waited
		reportVerifyProgress(logFile, fmt.Sprintf("container-gate slot acquired after %s", slotWait.Round(time.Second)))
	} else {
		reportVerifyProgress(logFile, fmt.Sprintf("running without a container-gate slot (containers opted out with %s=0: container-backed tests skip here and run in the refinery's gate)", dockerTestsEnv))
	}

	ctx, cancel := context.WithTimeout(context.Background(), budgets.runTimeout)
	defer cancel()
	// The run gets its own cancellable context so the container watch can end
	// it the moment it sees a container this run started (gt-0ss4), without
	// touching the budget: ctx still distinguishes a timeout from a kill, and
	// the watch's finding is reported as itself rather than as either.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// The gate ran slot-free because the rig's command does not ask for
	// containers. That text is a proxy for "this suite starts no container",
	// so watch the thing itself: a container that appears while no slot is
	// held is one this run started outside the slot (gt-0ss4).
	var watch *containerWatch
	if !needsSlot {
		watch = startGateContainerWatch(runCtx, cancelRun, townRoot, logFile)
	}

	runStart := time.Now()
	_, runErr := runWithProgress(testVerifyProgressInterval, func() error {
		return runVerifySuite(runCtx, worktree, testCmd, env, logFile)
	}, func(elapsed time.Duration) {
		logBytes := int64(-1)
		if fi, statErr := logFile.Stat(); statErr == nil {
			logBytes = fi.Size()
		}
		reportVerifyProgress(logFile, fmt.Sprintf("test-verify still running (%s elapsed of a %s run budget, log %d bytes)",
			elapsed.Round(time.Second), humanDuration(budgets.runTimeout), logBytes))
	})
	runElapsed := time.Since(runStart)
	timedOut := ctx.Err() == context.DeadlineExceeded
	if watch != nil {
		watch.stop()
	}

	result.slotWait = slotWait
	result.runElapsed = runElapsed

	// The watch's finding outranks the run's own outcome: the watch killed the
	// run, so its exit status is not a test result (gt-0ss4). A watch that
	// found nothing writes the observation that says the slot-free decision
	// held for this run — or why it was not checked.
	if watch != nil {
		if stray := watch.strayContainers(); len(stray) > 0 {
			return testVerifyResult{}, fmt.Errorf(
				"gt done: the default test-verify gate ran this rig's suite WITHOUT a container-gate slot (its command asks for no containers), but a container appeared during the run and no slot holder owns it: %s. The Docker VM is shared by the whole town and only a slot holder may use it (gt-wx53, gt-0ss4), so the gate killed the run — its exit status is not a test result. Either this rig's command starts containers without declaring the opt-in — declare it (merge_queue.test_command \"%s=1 make test\") so the gate takes a slot before it runs, or make the container-backed tests honor %s=0, which a recipe reading $${%s:-1} does — or another suite is running unwrapped, which `gt slot status` reports for the town. Full log: %s",
				strings.Join(stray, ", "), dockerTestsEnv, dockerTestsEnv, dockerTestsEnv, logPath)
		}
		reportVerifyProgress(logFile, watch.summary())
	}

	if timedOut {
		fmt.Fprintf(logFile, "=== test-verify timed out after %s ===\n", humanDuration(budgets.runTimeout))
		slotNote := "no container-gate slot was taken"
		if needsSlot {
			slotNote = fmt.Sprintf("the %s slot wait is not counted against it", slotWait.Round(time.Second))
		}
		return testVerifyResult{}, fmt.Errorf(
			"gt done: default test-verify timed out after %s running %q (run budget from %s; %s) — see %s. Raise merge_queue.test_verify_run_timeout for this rig if the suite legitimately needs longer",
			humanDuration(budgets.runTimeout), testCmd, budgets.runSource,
			slotNote, logPath)
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

	result.ran = true
	result.success = true
	if logBytes, readErr := os.ReadFile(logPath); readErr == nil {
		sum := sha256.Sum256(logBytes)
		result.logSHA256 = hex.EncodeToString(sum[:])
	}
	return result, nil
}
