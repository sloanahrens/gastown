package land

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

// Gate runs the checks a tree must pass. It is the one seam both sides of a
// landing use: gt done runs it on the author's rebased branch before pushing
// (the local pre-submit), and a cut-over rig's candidate gate runs the rig's
// gate command on the merged tree in Forgejo CI (candidate.go). D9 replaces
// the step list with `make gate`; callers do not change.
//
// A Gate never retries: a red gate is the verdict.
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
	// Warnings are the step's "gate: WARNING" lines, in order. They name a
	// host condition that slowed the gate without failing it (the exec-tax
	// preflight, gt-2ycne.1); the landing record keeps them, so a slow
	// landing says why where it is recorded.
	Warnings []string
	// ShellFailures are the scripts this step's own tier sweep reported red,
	// in the order its summary line named them (scripts/tier-sweep.sh). A
	// sweep names them whatever the step is called, so the post-land run's
	// "test" step reads like the gate's "shell" step (gt-40so9). Empty when
	// the step ran no sweep, or the sweep named no script.
	ShellFailures []string
	// TimedOut is true when the step's own Timeout killed it.
	TimedOut bool
	// Timeout is the step's own bound, for the rejection that names it.
	Timeout time.Duration
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
// target, else `make gate`, else the steps above (gt-ssyxd). The rig's own
// gate on the candidate branch is the Forgejo workflow's, not this one.
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

// ShellTierInputs is the ERE of the paths whose change can move the shell
// tier's verdict: the INPUTS line of scripts/post-land-shell.sh (gt-er6jn),
// the post-land run's own rule and the one the post-land revert reads
// (landworker/revert.go). TestShellTierInputsMatchPostLandScript fails when
// this constant and the script drift.
const ShellTierInputs = `^(scripts/|plugins/|\.githooks/|Makefile$|internal/testpolicy/docker\.txt$)`

// shellTierSummaryRE matches the shell tier's one summary line and captures
// the scripts it names. The log directory trails the names, and the tier's
// elapsed time trails that (gt-iqzr0); scripts/post-land-shell.sh execs the
// same sweep, so this is the post-land run's own line too (gt-40so9):
//
//	tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.aB12) in 2m14s
//
// A script path holds no parenthesis, so the capture stops at the marker; a
// line without the marker or the duration (an older sweep) still matches.
var shellTierSummaryRE = regexp.MustCompile(`^tier-sweep: shell RED passed=\d+ failed=\d+ skipped=\d+ failed:([^()]*?)(?: \(logs [^)]*\))?(?: in \S+)?$`)

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
	if g.LogDir != "" {
		if err := os.MkdirAll(g.LogDir, 0o700); err != nil {
			return GateResult{Err: fmt.Errorf("creating gate log dir: %w", err)}
		}
		if err := os.Chmod(g.LogDir, 0o700); err != nil {
			return GateResult{Err: fmt.Errorf("securing gate log dir: %w", err)}
		}
	}
	var res GateResult
	for _, s := range g.Steps {
		if !g.runStep(ctx, dir, run, s, &res) {
			return res
		}
	}
	res.Passed = true
	return res
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
	pkgs := parseGoTestOutput(out)
	shells := parseShellTierFailures(out)
	res.Steps = append(res.Steps, StepResult{
		Name:          s.Name,
		Command:       s.Command,
		ExitCode:      code,
		Elapsed:       time.Since(start),
		Tail:          lastLines(out, gateTailLines),
		Packages:      pkgs,
		Warnings:      parseWarnings(out),
		ShellFailures: shells,
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
// (indented, "TestName/sub") included. The candidate gate uses it to find a CI
// log's failure blocks (candidate.go); the duration it captures is not read.
var failedTestRE = regexp.MustCompile(`^\s*--- FAIL: (\S+)(?: \(([0-9.]+)s\))?`)

// parseGoTestOutput reads go test's text output: each package's summary line,
// ok or FAIL.
func parseGoTestOutput(out string) []PackageResult {
	var pkgs []PackageResult
	for _, line := range strings.Split(out, "\n") {
		m := packageLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pkgs = append(pkgs, PackageResult{Package: m[2], Passed: m[1] == "ok"})
	}
	return pkgs
}

// parseShellTierFailures is the scripts the tier sweep's summary line named,
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
