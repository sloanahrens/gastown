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
)

// Gate runs the checks a tree must pass. It is the one seam both sides of a
// landing use: gt done runs it on the author's rebased branch before pushing
// (the local pre-submit), and Land runs it on the merged tree before pushing
// the target. D9 replaces the step list with `make gate`; callers do not
// change.
//
// A Gate never retries. The flake policy (gt-v4ssj.5) reads per-package
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
	// Wrap, when set, rewrites the argv before it runs. Land's caller uses
	// it to hold the container slot around the test step
	// (`gt slot run --role <r> --`) until the suite is Docker-free.
	Wrap func(argv []string) []string
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

	run        runFunc         // nil means realRun
	lockDelays []time.Duration // nil means lintlock.RetryDelay
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
func RigGate(dir string, mq *config.MergeQueueConfig, unitOnly bool) (CommandGate, error) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
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

// containerOptInRE matches a command that sets GT_TEST_DOCKER to anything but
// an explicit off, which re-enables the container suite over the unit tier's
// GT_TEST_DOCKER=0.
var containerOptInRE = regexp.MustCompile(`GT_TEST_DOCKER=([^0\s;&|]|0\S)`)

func optsIntoContainers(cmd string) bool {
	return containerOptInRE.MatchString(cmd)
}

// WithSlot returns g with its test step run under the town's container-gate
// slot (`gt slot run --role <role> -- ...`). Land's caller must use it for the
// full tier until the suite is Docker-free: the container tests start only
// under a held slot.
func WithSlot(g CommandGate, gtPath, role string) CommandGate {
	steps := make([]Step, len(g.Steps))
	copy(steps, g.Steps)
	for i := range steps {
		if steps[i].Name == "test" {
			steps[i].Wrap = func(argv []string) []string {
				return append([]string{gtPath, "slot", "run", "--role", role, "--"}, argv...)
			}
		}
	}
	g.Steps = steps
	return g
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
		argv := []string{"sh", "-c", s.Command}
		if s.Wrap != nil {
			argv = s.Wrap(argv)
		}
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
		var err error
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
		res.Steps = append(res.Steps, StepResult{
			Name:     s.Name,
			Command:  s.Command,
			ExitCode: code,
			Elapsed:  time.Since(start),
			Tail:     lastLines(out, gateTailLines),
			Packages: parsePackageResults(out),
		})
		if err != nil {
			res.Err = fmt.Errorf("gate step %s (%s) did not run: %w", s.Name, s.Command, err)
			return res
		}
		if code != 0 && s.LockRetry && lintlock.Unfinished(out) {
			// Still contended (or stopped at its own timeout) after every
			// retry: nothing was linted, so this is not a verdict on the tree.
			res.Err = fmt.Errorf("gate step %s never finished: golangci-lint's module lock stayed held or it hit its own timeout; nothing was linted", s.Name)
			return res
		}
		if code != 0 {
			return res
		}
	}
	res.Passed = true
	return res
}

// packageLineRE matches go test's per-package summary lines:
// "ok  \t<pkg>\t0.1s" and "FAIL\t<pkg>\t0.2s". A bare "FAIL" carries no
// package and is skipped.
var packageLineRE = regexp.MustCompile(`^(ok|FAIL)\s+(\S+)(\s|$)`)

func parsePackageResults(out string) []PackageResult {
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
	cmd.Env = mergeEnv(os.Environ(), env)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
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

// mergeEnv is base with every key in extra replaced, never duplicated.
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
	return append(out, extra...)
}
