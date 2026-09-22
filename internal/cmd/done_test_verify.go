package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// containersOptedOut is true when the gate forced the container opt-in off
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

// goBuildWholeModule builds the entire module in the worktree. It is the
// check that distinguishes a whole-package deletion that still compiles
// (legitimate, and verifiable — the deletion is either consistent or the
// build breaks, and the build is the authority) from one whose importers
// are now broken (gt-ytjh). A var so tests can stub it: the real one shells
// out to `go build`, which no unit test may do.
var goBuildWholeModule = func(worktree string) error {
	//nolint:gosec // G204: fixed argument, run in the polecat's own worktree.
	out, err := exec.Command("go", "build", "./...").CombinedOutput()
	_ = out
	return err
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

// gateNeedsSlot reports whether the default test-verify gate must hold a
// container-gate slot for the run it is about to make (gt-wx53).
//
// The slot is town-wide and one suite wide, so taking it for a run that cannot
// start a container makes every `gt done` in the town queue behind the daemon's
// main-branch patrol and the refinery's batch gate — mica's gt done on gt-jqif
// waited out the full 20m cap for a gate that never ran a test (gt-pnkd). The
// gate therefore takes the slot only when its run can actually start a
// container-backed suite: a non-Go rig (whose test_command is opaque), or a Go
// rig whose command turns the opt-in on. Everything else runs the rig's whole
// suite with the opt-in off: the container-backed tests skip, and the
// refinery's gate (which does hold a slot) runs them once per submission
// (gt-yihz, operator decision 14:44 2026-09-17).
//
// The *ambient* opt-in is deliberately not consulted, even though the
// container-suite guard reads it that way. The guard can only inspect the
// command it is shown; this gate builds its child's environment, so the same
// input would otherwise decide differently depending on who invoked `gt done`
// (a polecat spawned by a suite inherits GT_TEST_DOCKER=1 and would take the
// town-wide slot back; the integration harness sets it too, so the same gate
// would behave two ways in one test binary). Reading the rig's own command
// keeps the answer a property of the rig's configuration, which is what an
// operator can reason about — and overriding an inherited opt-in is safe in
// the only direction that matters: the run cannot start a container it did not
// take a slot for.
func gateNeedsSlot(isGoRig bool, commands ...string) bool {
	if !isGoRig {
		return true
	}
	return containerOptInRequested(commands...)
}

// verifyGateEnv builds the child environment for the gate's lint and suite
// runs: this process's environment, then the rig's test_command env prefix
// (gt-fa3s), then — when the run must not start containers — an explicit
// opt-out.
//
// The opt-out is *appended after filtering every inherited and rig-supplied
// value of the same name*, because a duplicate entry in a child's environment
// is resolved differently by different readers (Go's os.Getenv takes the
// first, a shell's assignment takes the last), and this value is what decides
// whether a container starts outside the container-gate slot.
//
// A rig whose command turns the opt-in on is never opted out here: the rig
// wins (see gateNeedsSlot), and the gate takes a slot for it.
func verifyGateEnv(envPrefix []string, optOutContainers bool) []string {
	env := make([]string, 0, len(os.Environ())+len(envPrefix)+1)
	keep := func(kv string) bool {
		return !optOutContainers || !strings.HasPrefix(kv, dockerTestsEnv+"=")
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
	if optOutContainers {
		env = append(env, dockerTestsEnv+"=0")
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
// takes no slot at all. Any failure — the run itself failing, timing out, or
// changed .go files that resolve to no buildable package and a whole-module
// build that confirms the deletion broke the build — returns an error and the
// caller must not create the MR bead: that is the refusal gt-h9kf asks for.
// The one exception is a whole-package deletion that still compiles (gt-ytjh):
// it is verified by `go build ./...` before the slot and then runs the full
// suite like any other Go change.
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
	if isGoRig {
		resolvedPkgs, changedGo, pkgErr := changedGoPackages(g, worktree, verifiedBase)
		if pkgErr != nil {
			return testVerifyResult{}, fmt.Errorf("gt done: could not compute changed packages for the default test-verify gate: %w", pkgErr)
		}
		if !changedGo {
			return testVerifyResult{skipReason: fmt.Sprintf("no changed .go files since %s — nothing to verify", shortSHA(verifiedBase))}, nil
		}
		if len(resolvedPkgs) == 0 {
			// The only diff shape that reaches here is a whole-package
			// deletion: changedGoFiles is true, but go list no longer
			// resolves the deleted directory to a package, so nothing is
			// left to scope. That is not a broken build — a clean deletion
			// of every .go file in a package is legitimate work, and the
			// whole module either still compiles (it is verified) or it
			// doesn't (the refusal below stays). Refusing every deletion of
			// a Go package was the false refusal gt-ytjh opened. (A
			// deletion whose importers are broken but which still
			// resolves to a named package never reaches here: go list
			// resolves a package regardless of whether its imports
			// build, so the gate's suite-run refusal covers it.)
			pkgs = []string{"."}
			if buildErr := goBuildWholeModule(worktree); buildErr != nil {
				return testVerifyResult{}, fmt.Errorf("gt done: deleting every .go file in a package since %s left the rest of the module unbuildable — the deletion broke something that imports it; fix the build (or undo the deletion) before submitting, or use --skip-verify with justification if this is genuinely not testable", shortSHA(verifiedBase))
			}
		} else {
			pkgs = resolvedPkgs
		}
	}

	// The full suite may spin Dolt/testcontainers containers (internal/cmd,
	// internal/refinery, ...), and a container must never start outside the
	// container-gate slot. gt-wx53: it must not start *inside* it either when
	// nothing in this run needs one — a gate that always took the slot made
	// every `gt done` in the town queue for a town-wide, one-suite-wide
	// resource, sometimes for longer than the suite it was queueing behind.
	// So the gate either runs the rig's whole suite with the container opt-in
	// forced off (container-backed tests skip, no slot needed) or, when the
	// rig's own command asks for containers, runs it unchanged inside a slot.
	needsSlot := gateNeedsSlot(isGoRig, mq.TestCommand, mq.TestVerifyCommand)
	containersOptedOut := !needsSlot

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
	logPath := filepath.Join(logDir, "gt-done-verify.log")
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
	// Whether this gate queues for the town's container-gate slot, and why, is
	// the first thing a reader of a slow or refused gate needs (gt-wx53: the
	// answer used to be an unconditional yes, stated nowhere).
	if containersOptedOut {
		fmt.Fprintf(logFile, "containers: opted out (%s=0) — container-backed tests skip here and run once per submission in the refinery's gate, so this gate takes no container-gate slot\n", dockerTestsEnv)
	} else {
		fmt.Fprintf(logFile, "containers: enabled — this run may start container-backed suites, so it takes a container-gate slot\n")
	}
	reportVerifyProgress(logFile, fmt.Sprintf("gate starting (command: %s; run budget %s; slot cap %s)",
		testCmd, humanDuration(budgets.runTimeout), humanDuration(budgets.slotTimeout)))

	env := verifyGateEnv(envPrefix, containersOptedOut)

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
			return testVerifyResult{}, fmt.Errorf(
				"gt done: could not acquire the container-gate slot for the default test-verify gate after %s (cap %s): %w — this is slot contention, NOT a test failure, and nothing in your diff was tested. Re-run gt done once the queue drains, or raise merge_queue.test_verify_slot_timeout for this rig; --skip-verify with justification is the last resort",
				waited.Round(time.Second), humanDuration(budgets.slotTimeout), acquireErr)
		}
		defer release()
		slotWait = waited
		reportVerifyProgress(logFile, fmt.Sprintf("container-gate slot acquired after %s", slotWait.Round(time.Second)))
	} else {
		reportVerifyProgress(logFile, fmt.Sprintf("running without a container-gate slot (containers opted out with %s=0: container-backed tests skip here and run in the refinery's gate)", dockerTestsEnv))
	}

	ctx, cancel := context.WithTimeout(context.Background(), budgets.runTimeout)
	defer cancel()

	runStart := time.Now()
	_, runErr := runWithProgress(testVerifyProgressInterval, func() error {
		return runVerifySuite(ctx, worktree, testCmd, env, logFile)
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

	result.slotWait = slotWait
	result.runElapsed = runElapsed

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
