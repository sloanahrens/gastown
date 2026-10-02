package land

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

// Gate runs the checks a tree must pass. It is the one seam both sides of a
// landing use: gt done runs it on the author's rebased branch before pushing
// (the local pre-submit), and Land runs it on the merged tree before pushing
// the target. D9 replaces the step list with `make gate`; callers do not
// change.
//
// A Gate never retries. Land's flake policy (flake.go) reads per-package
// results from GateResult and decides reruns itself.
type Gate interface {
	Run(ctx context.Context, dir string) GateResult
}

// GateResult is the outcome of one gate run. Passed is true only when every
// step ran and exited zero. Err is set when a step could not be run at all;
// that is an infrastructure failure and says nothing about the tree.
type GateResult struct {
	Passed bool
	Steps  []StepResult
	Err    error
}

// StepResult is one step's outcome.
type StepResult struct {
	Name     string
	Command  string
	ExitCode int
	Elapsed  time.Duration
	// Tail is the last gateTailLines lines of the step's combined output.
	Tail string
	// Packages are the Go packages the step reported as ok or FAIL, in order.
	Packages []PackageResult
	// FailedTests are the top-level tests go test reported as --- FAIL, each
	// under the package whose FAIL line followed it.
	FailedTests []FailedTest
	// BudgetOverruns are the packages the testpolicy budget runner failed on
	// a "BUDGET:" line (over the user-CPU budget, or CPU time unrecorded).
	// A "BUDGET (reported only ...)" line fails nothing and is not one.
	BudgetOverruns []BudgetOverrun
	// Warnings are the step's "gate: WARNING" lines, in order. They name a
	// host condition that slowed the gate without failing it (the exec-tax
	// preflight, gt-2ycne.1); the landing record keeps them, so a slow
	// landing says why where it is recorded.
	Warnings []string
	// ShellFailures are the scripts the shell tier reported red, in the order
	// its summary line named them (scripts/tier-sweep.sh). Empty for every
	// other step, and for a shell failure that named no script.
	ShellFailures []string
	// TimedOut is true when the step's own Timeout killed it.
	TimedOut bool
	// Timeout is the step's own bound, for the rejection that names it.
	Timeout time.Duration
}

// FailedTest is one failing top-level test in `go test` output.
type FailedTest struct {
	Package string
	Test    string
}

// BudgetOverrun is one failing "BUDGET:" line of the budget runner.
type BudgetOverrun struct {
	Package string
	Line    string
}

// PackageResult is one package's line in `go test` output.
type PackageResult struct {
	Package string
	Passed  bool
}

// gateTailLines bounds the output a rejection note carries.
const gateTailLines = 40

// Summary is one line naming each step that ran, its exit code and wall time.
func (r GateResult) Summary() string {
	if len(r.Steps) == 0 && r.Err != nil {
		return "gate did not run: " + r.Err.Error()
	}
	parts := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		parts = append(parts, fmt.Sprintf("%s exit %d %s", s.Name, s.ExitCode, s.Elapsed.Round(time.Second)))
	}
	out := strings.Join(parts, ", ")
	switch {
	case r.Err != nil:
		out += "; infrastructure error: " + r.Err.Error()
	case r.Passed:
		out = "pass (" + out + ")"
	default:
		out = "fail (" + out + ")"
	}
	return out
}

// Warnings are every step's "gate: WARNING" lines, in step order. A warning
// is the gate's own account of the host it ran on and fails nothing.
func (r GateResult) Warnings() []string {
	var out []string
	for _, s := range r.Steps {
		out = append(out, s.Warnings...)
	}
	return out
}

// TimedOutStep is the step whose own timeout stopped the gate, if one did.
func (r GateResult) TimedOutStep() (StepResult, bool) {
	if len(r.Steps) == 0 {
		return StepResult{}, false
	}
	last := r.Steps[len(r.Steps)-1]
	return last, last.TimedOut
}

// FailureTail is the output tail of the step that stopped the gate, or "".
func (r GateResult) FailureTail() string {
	if r.Passed || len(r.Steps) == 0 {
		return ""
	}
	return r.Steps[len(r.Steps)-1].Tail
}

// ShellTierFailures is the scripts the shell step reported red, so a rejection
// names what failed rather than quoting the tier's output. Empty when the
// shell step did not run or did not fail.
func (r GateResult) ShellTierFailures() []string {
	for _, s := range r.Steps {
		if s.Name == ShellStepName {
			return s.ShellFailures
		}
	}
	return nil
}

// Step is one gate command, run as `sh -c Command` in the gated tree.
type Step struct {
	Name    string
	Command string
	// Env is added to the inherited environment.
	Env []string
	// LockRetry re-runs the step while its output says golangci-lint never
	// ran because another instance held the module lock (the lintlock policy
	// gt done and the refinery already share). Retries need ctx to carry a
	// deadline; without one the first attempt is final.
	LockRetry bool
	// SlotRole, when set, runs this step with the town's container-gate slot
	// held in process for that role (see CommandGate.TownRoot): the hold the
	// step's argv used to be wrapped in a second `gt` process for. See
	// WithSlot, which marks the step that needs it.
	SlotRole string
	// Timeout bounds this step alone (its lint-lock retries included); 0
	// means only ctx bounds it. A step that outlives it is killed with its
	// whole process group and reported TimedOut, a verdict rather than an
	// infrastructure error (gt-b5ugw).
	Timeout time.Duration
}

// runFunc runs argv in dir with env added, writing combined output to out.
// It returns the exit code, or an error when the process could not be run.
type runFunc func(ctx context.Context, dir string, env, argv []string, out io.Writer) (int, error)

// CommandGate runs Steps in order and stops at the first red one.
type CommandGate struct {
	Steps []Step
	// LogDir, when set, receives <step>.log for every step (dir 0700, files
	// 0600), so a full log survives beside the tail a note carries.
	LogDir string
	// Out, when set, streams every step's output as it runs.
	Out io.Writer
	// TownRoot is the town whose container-gate slot a step with SlotRole
	// holds. It is read only for such a step, and a step with a SlotRole and
	// no TownRoot is an error rather than a silent unguarded run.
	TownRoot string
	// ShellTier appends the shell tier (ShellStepName, ShellStepCommand) after
	// every other step, and only when the gated tree changes one of the tier's
	// inputs against its base (shellTierNeeded). LandGate sets it; RigGate,
	// gt done's pre-submit, never does.
	ShellTier bool

	// shellTimeout bounds the appended shell step; see WithTimeouts.
	shellTimeout time.Duration

	run        runFunc         // nil means realRun
	lockDelays []time.Duration // nil means lintlock.RetryDelay
	// holdSlot takes the container-gate slot a step with SlotRole runs under
	// and returns the release to call when the step has ended, with its exit
	// status (nil when the step never ran). nil means the town's real slot.
	holdSlot func(ctx context.Context, townRoot, role string) (release func(exitCode *int), err error)
}

// GoGate is the Go rigs' default gate: `make lint`, `go build ./...`,
// `make test`. unitOnly runs the unit tier (GT_TEST_DOCKER=0), which needs no
// container slot; gt done uses it. Land runs the full tier on the merged tree
// and must hold the container slot for it (WithSlot).
func GoGate(unitOnly bool) CommandGate {
	return goGate("make lint", "make test", unitOnly)
}

func goGate(lint, test string, unitOnly bool) CommandGate {
	testStep := Step{Name: "test", Command: test}
	if unitOnly {
		testStep.Env = []string{"GT_TEST_DOCKER=0"}
	}
	return CommandGate{Steps: []Step{
		{Name: "lint", Command: lint, LockRetry: true},
		{Name: "build", Command: "go build ./..."},
		testStep,
	}}
}

// RigGate is the gate for the tree in dir. A Go module always builds with
// `go build ./...` and lints and tests with the rig's lint_command and
// test_command, defaulting to `make lint` and `make test`. Any other tree runs
// the rig's lint, build and test commands. A rig with none of them is refused:
// "no gate configured" must stop a landing, never wave it through (G2-11).
//
// In the unit tier (gt done's pre-submit) the merge_queue.presubmit_command
// wins when set, else `make presubmit` when a Go repo's Makefile has that
// target, else `make gate`, else the steps above (gt-ssyxd). Land never calls
// this; it uses LandGate, whose full gate is not weakened by any of it.
func RigGate(dir string, mq *config.MergeQueueConfig, unitOnly bool) (CommandGate, error) {
	if unitOnly && mq != nil {
		// An explicit presubmit_command replaces the steps on any tree.
		if c := strings.TrimSpace(mq.PresubmitCommand); c != "" {
			if optsIntoContainers(c) {
				return CommandGate{}, fmt.Errorf("the rig's presubmit_command %q opts into the container suite, which the unit tier cannot run: it holds no container-gate slot (gt-0ss4)", c)
			}
			return CommandGate{Steps: []Step{{Name: "presubmit", Command: c, Env: []string{"GT_TEST_DOCKER=0"}, LockRetry: true}}}, nil
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		if unitOnly && hasMakeTarget(dir, "presubmit") {
			// gt-ssyxd: the landing worker gates the merged tree with the
			// full `make gate`; the author's pre-submit is lint, build and
			// the tests of the changed packages only.
			return CommandGate{Steps: []Step{{Name: "presubmit", Command: "make presubmit", LockRetry: true}}}, nil
		}
		if unitOnly && hasMakeTarget(dir, "gate") {
			// D9's `make gate` is lint, build and the unit tier in one
			// target, with containers forced off.
			return CommandGate{Steps: []Step{{Name: "gate", Command: "make gate", LockRetry: true}}}, nil
		}
		lint, test := "make lint", "make test"
		if mq != nil {
			if c := strings.TrimSpace(mq.LintCommand); c != "" {
				lint = c
			}
			if c := strings.TrimSpace(mq.TestCommand); c != "" {
				test = c
			}
		}
		if unitOnly && optsIntoContainers(test) {
			return CommandGate{}, fmt.Errorf("the rig's test_command %q opts into the container suite, which the unit tier cannot run: it holds no container-gate slot (gt-0ss4)", test)
		}
		return goGate(lint, test, unitOnly), nil
	}
	var steps []Step
	if mq != nil {
		for _, s := range []Step{
			{Name: "lint", Command: mq.LintCommand},
			{Name: "build", Command: mq.BuildCommand},
			{Name: "test", Command: mq.TestCommand},
		} {
			if strings.TrimSpace(s.Command) != "" {
				steps = append(steps, s)
			}
		}
	}
	if len(steps) == 0 {
		return CommandGate{}, fmt.Errorf("no gate configured for %s: it is not a Go module and the rig sets no lint_command, build_command or test_command", dir)
	}
	return CommandGate{Steps: steps}, nil
}

// ShellStepName names the gate's shell-tier step. It must not be "test":
// WithSlot holds the container-gate slot around that name, and the shell tier
// starts no container.
const ShellStepName = "shell"

// ShellStepCommand is the shell tier alone, what scripts/post-land-shell.sh
// execs after a landing; the landing gate runs it before the push instead when
// the submission can move its verdict (gt-vsct7.8).
const ShellStepCommand = "bash scripts/tier-sweep.sh shell"

// ShellTierInputs is the ERE of the paths whose change can move the shell
// tier's verdict. It is the INPUTS line of scripts/post-land-shell.sh
// (gt-er6jn), the post-land run's own rule for the same decision;
// TestShellTierInputsMatchPostLandScript fails when the two drift.
const ShellTierInputs = `^(scripts/|plugins/|\.githooks/|Makefile$|internal/testpolicy/docker\.txt$)`

// shellTierScript is the tier's entry point in the gated tree. A tree that
// does not ship it has no shell tier, whatever it changed: another rig's
// Makefile change must not run a script its tree does not have.
const shellTierScript = "scripts/tier-sweep.sh"

// shellTierDiffCommand asks git what the merged tree changed against its base.
// The landing worktree holds exactly one commit on top of the base - mergeWork
// makes a --no-ff merge or a squash - so its first parent is that base.
const shellTierDiffCommand = "git diff --name-only HEAD^ HEAD"

var (
	shellTierInputsRE = regexp.MustCompile(ShellTierInputs)
	// shellTierSummaryRE matches the shell tier's one summary line, e.g.
	// "tier-sweep: shell RED passed=8 failed=1 skipped=0 failed: scripts/x.sh",
	// and captures the scripts it names.
	shellTierSummaryRE = regexp.MustCompile(`^tier-sweep: shell RED passed=\d+ failed=\d+ skipped=\d+ failed:(.*)$`)
)

// LandGate is the gate Land runs on the merged tree, one command from the
// rig's settings: merge_queue.gate when set, else `make gate` when the
// Makefile has that target, else `make test`. Only the `make test` fallback
// needs the container slot; its step is named "test", so WithSlot puts that
// step under the slot and nothing else. A configured gate that needs a slot
// holds it itself.
//
// Every shape may append the shell tier (ShellTier): the tree's own
// scripts/tier-sweep.sh, run for the submissions that can move its verdict,
// in place of the post-land run that reaches that verdict too late
// (gt-vsct7.8).
func LandGate(dir string, mq *config.MergeQueueConfig) CommandGate {
	cmd := ""
	if mq != nil {
		cmd = strings.TrimSpace(mq.Gate)
	}
	if cmd == "" && hasMakeTarget(dir, "gate") {
		cmd = "make gate"
	}
	if cmd == "make gate" && hasMakeTarget(dir, "gate-lint") && hasMakeTarget(dir, "gate-test") {
		// make gate's two stages, run one at a time so each has its own
		// timeout and a lint failure stops the landing before the tests
		// (gt-b5ugw).
		return CommandGate{ShellTier: true, Steps: []Step{
			{Name: "lint", Command: "make gate-lint", LockRetry: true},
			{Name: "gate", Command: "make gate-test"},
		}}
	}
	if cmd != "" {
		return CommandGate{ShellTier: true, Steps: []Step{{Name: "gate", Command: cmd, LockRetry: true}}}
	}
	return CommandGate{ShellTier: true, Steps: []Step{{Name: "test", Command: "make test", LockRetry: true}}}
}

// UnitTier reports whether g is make gate's unit tier (containers off), in
// one step or in its two stages, rather than a gate that may run containers.
func (g CommandGate) UnitTier() bool {
	if len(g.Steps) == 0 {
		return false
	}
	c := g.Steps[len(g.Steps)-1].Command
	return c == "make gate" || c == "make gate-test"
}

// WithTimeouts returns g with the step named "lint" bounded by lint and every
// other step by test, and with the shell step Run may append bounded by shell.
// A zero leaves that step bounded only by its context.
func WithTimeouts(g CommandGate, lint, test, shell time.Duration) CommandGate {
	steps := make([]Step, len(g.Steps))
	copy(steps, g.Steps)
	for i := range steps {
		if steps[i].Name == "lint" {
			steps[i].Timeout = lint
		} else {
			steps[i].Timeout = test
		}
	}
	g.Steps = steps
	g.shellTimeout = shell
	return g
}

// hasMakeTarget reports whether dir's Makefile defines target.
func hasMakeTarget(dir, target string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "Makefile")) //nolint:gosec // G304: the gated tree's own Makefile
	if err != nil {
		return false
	}
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `\s*:([^=]|$)`).Match(data)
}

// containerOptInRE matches a command that sets GT_TEST_DOCKER to anything but
// an explicit off, which re-enables the container suite over the unit tier's
// GT_TEST_DOCKER=0.
var containerOptInRE = regexp.MustCompile(`GT_TEST_DOCKER=([^0\s;&|]|0\S)`)

func optsIntoContainers(cmd string) bool {
	return containerOptInRE.MatchString(cmd)
}

// WithSlot returns g with its test step run under the town's container-gate
// slot. Land's caller must use it for the full tier until the suite is
// Docker-free: the container tests start only under a held slot.
//
// The hold is taken here, in this process (holdTownSlot), rather than by
// wrapping the step's argv in `gt slot run`: the gate spawns the step itself,
// with this process's writers and its own kill-on-deadline, so the slot and
// the command it guards stay in one place (gt-638go.12).
func WithSlot(g CommandGate, townRoot, role string) CommandGate {
	steps := make([]Step, len(g.Steps))
	copy(steps, g.Steps)
	for i := range steps {
		if steps[i].Name == "test" {
			steps[i].SlotRole = role
		}
	}
	g.Steps = steps
	g.TownRoot = townRoot
	return g
}

// takeSlot holds the container-gate slot a step runs under, returning the
// release to call when the step has ended. A step with no SlotRole gets a
// no-op release, so callers call what they are handed without asking.
func (g CommandGate) takeSlot(ctx context.Context, role string) (func(exitCode *int), error) {
	if role == "" {
		return func(*int) {}, nil
	}
	if g.TownRoot == "" {
		return nil, fmt.Errorf("gate step holds the container-gate slot as %q with no TownRoot to hold it in", role)
	}
	hold := g.holdSlot
	if hold == nil {
		hold = holdTownSlot
	}
	return hold(ctx, g.TownRoot, role)
}

// holdTownSlot is the real container-gate hold: the town's pool, and a wait
// capped by the gate's own deadline so a canceled landing does not sit on a
// slot for the slot's full default first.
//
// It always locks for real (slot.AcquirePoolReal) instead of taking the
// reentrant fast path: its caller is the daemon's landing worker, which
// outlives every hold it takes, so a marker naming that very role may be
// inherited from a predecessor process whose flock is long gone (gt-off9).
func holdTownSlot(ctx context.Context, townRoot, role string) (func(exitCode *int), error) {
	timeout, err := slotWaitTimeout(ctx)
	if err != nil {
		return nil, err
	}
	h, err := slot.AcquirePoolReal(townRoot, role, timeout, slot.PoolForTown(townRoot))
	if err != nil {
		return nil, err
	}
	return func(exitCode *int) {
		if exitCode == nil {
			_ = h.Release()
			return
		}
		_ = h.ReleaseWithExit(*exitCode)
	}, nil
}

// slotWaitTimeout is how long a gate step waits for the container-gate slot:
// the slot's own default, capped by ctx's remaining time when it has a
// deadline. The result is always positive — slot.Acquire reads a timeout <= 0
// as "wait forever", which an expired context must not turn into.
//
// The wait itself does not observe cancellation between polls; a caller that
// must stop waiting is bounded by this cap and by Acquire's own polling
// against a pool whose holder has died.
func slotWaitTimeout(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	timeout := slot.DefaultRunTimeout
	deadline, ok := ctx.Deadline()
	if !ok {
		return timeout, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	if remaining < timeout {
		timeout = remaining
	}
	return timeout, nil
}

// Run runs each step in dir and stops at the first that does not exit zero.
func (g CommandGate) Run(ctx context.Context, dir string) GateResult {
	run := g.run
	if run == nil {
		run = realRun
	}
	if len(g.Steps) == 0 {
		return GateResult{Err: errors.New("gate has no steps")}
	}
	steps := g.Steps
	if g.ShellTier {
		needed, err := shellTierNeeded(ctx, dir, run)
		if err != nil {
			return GateResult{Err: fmt.Errorf("the shell tier's change check: %w", err)}
		}
		if needed {
			steps = append(slices.Clone(g.Steps), Step{Name: ShellStepName, Command: ShellStepCommand, Timeout: g.shellTimeout})
		}
	}
	if g.LogDir != "" {
		if err := os.MkdirAll(g.LogDir, 0o700); err != nil {
			return GateResult{Err: fmt.Errorf("creating gate log dir: %w", err)}
		}
		if err := os.Chmod(g.LogDir, 0o700); err != nil {
			return GateResult{Err: fmt.Errorf("securing gate log dir: %w", err)}
		}
	}
	var res GateResult
	for _, s := range steps {
		if !g.runStep(ctx, dir, run, s, &res) {
			return res
		}
	}
	res.Passed = true
	return res
}

// shellTierNeeded reports whether dir's merged tree changes one of the shell
// tier's inputs against its base, and whether it ships the tier at all.
func shellTierNeeded(ctx context.Context, dir string, run runFunc) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, shellTierScript)); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		// A tree the check cannot read is not a tree without the tier: an
		// unreadable path is infrastructure, not a skip.
		return false, fmt.Errorf("looking for %s in %s: %w", shellTierScript, dir, err)
	}
	var buf bytes.Buffer
	code, err := run(ctx, dir, nil, []string{"sh", "-c", shellTierDiffCommand}, &buf)
	if err != nil {
		return false, fmt.Errorf("asking git what %s changed: %w", dir, err)
	}
	if code != 0 {
		return false, fmt.Errorf("%s in %s exited %d: %s", shellTierDiffCommand, dir, code, strings.TrimSpace(buf.String()))
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if shellTierInputsRE.MatchString(strings.TrimSpace(line)) {
			return true, nil
		}
	}
	return false, nil
}

// runStep runs one step under its own timeout, appends its result to res, and
// reports whether the gate goes on to the next step.
func (g CommandGate) runStep(parent context.Context, dir string, run runFunc, s Step, res *GateResult) bool {
	ctx := parent
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, s.Timeout)
		defer cancel()
	}
	release, err := g.takeSlot(ctx, s.SlotRole)
	if err != nil {
		res.Err = fmt.Errorf("gate step %s (%s) could not take the container-gate slot: %w", s.Name, s.Command, err)
		return false
	}
	argv := []string{"sh", "-c", s.Command}
	var buf bytes.Buffer
	start := time.Now()
	attempt := func() (int, error) {
		buf.Reset()
		writers := []io.Writer{&buf}
		if g.Out != nil {
			writers = append(writers, g.Out)
		}
		if g.LogDir != "" {
			f, err := os.OpenFile(filepath.Join(g.LogDir, s.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return -1, fmt.Errorf("opening %s log: %w", s.Name, err)
			}
			defer func() { _ = f.Close() }()
			writers = append(writers, f)
		}
		return run(ctx, dir, s.Env, argv, io.MultiWriter(writers...))
	}
	var code int
	if s.LockRetry {
		lintlock.RetryWithDelays(ctx, g.lockDelays, func() lintlock.Attempt {
			code, err = attempt()
			switch {
			case err != nil:
				return lintlock.Attempt{Err: err, Output: buf.String()}
			case code != 0:
				return lintlock.Attempt{Err: fmt.Errorf("exit %d", code), Output: buf.String()}
			}
			return lintlock.Attempt{Output: buf.String()}
		}, func(n, of int, wait time.Duration) {
			if g.Out != nil {
				_, _ = fmt.Fprintf(g.Out, "%s: golangci-lint held by another run; retry %d/%d in %s\n", s.Name, n, of, wait)
			}
		})
	} else {
		code, err = attempt()
	}
	out := buf.String()
	pkgs, tests := parseGoTestOutput(out)
	var shells []string
	if s.Name == ShellStepName {
		shells = parseShellTierFailures(out)
	}
	res.Steps = append(res.Steps, StepResult{
		Name:           s.Name,
		Command:        s.Command,
		ExitCode:       code,
		Elapsed:        time.Since(start),
		Tail:           lastLines(out, gateTailLines),
		Packages:       pkgs,
		FailedTests:    tests,
		BudgetOverruns: parseBudgetOverruns(out),
		Warnings:       parseWarnings(out),
		ShellFailures:  shells,
	})
	if s.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		// Its own timeout, not the landing's: the step was killed, and that
		// is the verdict on this stage (gt-b5ugw).
		last := &res.Steps[len(res.Steps)-1]
		last.TimedOut, last.Timeout, last.ExitCode = true, s.Timeout, -1
		killed := -1
		release(&killed)
		return false
	}
	if err != nil {
		// The step never ran, so there is no exit status for the hold to
		// carry: release it open-ended rather than inventing one.
		release(nil)
		res.Err = fmt.Errorf("gate step %s (%s) did not run: %w", s.Name, s.Command, err)
		return false
	}
	release(&code)
	if code != 0 && s.LockRetry && lintlock.Unfinished(out) {
		// Still contended (or stopped at its own timeout) after every
		// retry: nothing was linted, so this is not a verdict on the tree.
		res.Err = fmt.Errorf("gate step %s never finished: golangci-lint's module lock stayed held or it hit its own timeout; nothing was linted", s.Name)
		return false
	}
	if code != 0 {
		return false
	}
	return true
}

// packageLineRE matches go test's per-package summary lines:
// "ok  \t<pkg>\t0.1s" and "FAIL\t<pkg>\t0.2s". A bare "FAIL" carries no
// package and is skipped.
var packageLineRE = regexp.MustCompile(`^(ok|FAIL)\s+(\S+)(\s|$)`)

// failedTestRE matches a "--- FAIL: TestName (0.01s)" line, subtests
// (indented, "TestName/sub") included.
var failedTestRE = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)

// parseGoTestOutput reads go test's text output: each package's summary line
// and the top-level tests that failed in it. go test (and the budget runner)
// print a package's output in one block ending in its summary line, so a
// --- FAIL line belongs to the next FAIL line's package.
func parseGoTestOutput(out string) ([]PackageResult, []FailedTest) {
	var (
		pkgs    []PackageResult
		tests   []FailedTest
		pending []string
	)
	for _, line := range strings.Split(out, "\n") {
		if m := failedTestRE.FindStringSubmatch(line); m != nil {
			name, _, _ := strings.Cut(m[1], "/")
			if !slices.Contains(pending, name) {
				pending = append(pending, name)
			}
			continue
		}
		m := packageLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		passed := m[1] == "ok"
		pkgs = append(pkgs, PackageResult{Package: m[2], Passed: passed})
		if !passed {
			for _, name := range pending {
				tests = append(tests, FailedTest{Package: m[2], Test: name})
			}
		}
		pending = nil
	}
	return pkgs, tests
}

// parseShellTierFailures is the scripts the shell tier's summary line named,
// from the last line that carried one.
func parseShellTierFailures(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if m := shellTierSummaryRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			names = strings.Fields(m[1])
		}
	}
	return names
}

// budgetLineRE matches the budget runner's failing lines, "BUDGET: <pkg>
// used ..." and "BUDGET: <pkg> passed but its CPU time was not recorded".
var budgetLineRE = regexp.MustCompile(`^BUDGET: (\S+) `)

// warningLineRE matches a step's own warnings, "gate: WARNING <text>". The
// text after the marker is kept whole; a step that warns in another shape is
// not a warning this reads.
var warningLineRE = regexp.MustCompile(`^gate: WARNING (\S.*)$`)

func parseWarnings(out string) []string {
	var warns []string
	for _, line := range strings.Split(out, "\n") {
		if m := warningLineRE.FindStringSubmatch(line); m != nil {
			warns = append(warns, strings.TrimSpace(m[1]))
		}
	}
	return warns
}

func parseBudgetOverruns(out string) []BudgetOverrun {
	var over []BudgetOverrun
	for _, line := range strings.Split(out, "\n") {
		if m := budgetLineRE.FindStringSubmatch(line); m != nil {
			over = append(over, BudgetOverrun{Package: m[1], Line: strings.TrimSpace(line)})
		}
	}
	return over
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func realRun(ctx context.Context, dir string, env, argv []string, out io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: gate commands come from the rig's own config
	cmd.Dir = dir
	// Its own process group, so a step's timeout kills make and the test
	// binaries under it, not only the shell (gt-b5ugw).
	util.SetProcessGroup(cmd)
	cmd.Env = mergeEnv(os.Environ(), env)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Start()
	if err == nil {
		// The slow-landing alarm names the processes a stage is running.
		untrack := trackPID(ctx, cmd.Process.Pid)
		err = cmd.Wait()
		untrack()
	}
	if err == nil {
		return 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// mergeEnv is base with every key in extra replaced, never duplicated. An
// entry in extra with no "=" unsets that key instead.
// Readers resolve duplicate keys differently (Go takes the last, some libc
// getenv the first), so a gate's GT_TEST_DOCKER=0 beside an inherited =1 could
// start containers with no slot held (gt-0hbm).
func mergeEnv(base, extra []string) []string {
	override := make(map[string]bool, len(extra))
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		override[k] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !override[k] {
			out = append(out, kv)
		}
	}
	for _, kv := range extra {
		if strings.Contains(kv, "=") {
			out = append(out, kv)
		}
	}
	return out
}
