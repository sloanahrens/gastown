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

	run runFunc // nil means realRun
}

// GoGate is the Go rigs' gate: `make lint`, `go build ./...`, `make test`.
// unitOnly runs the unit tier (GT_TEST_DOCKER=0), which needs no container
// slot; gt done uses it. Land runs the full tier on the merged tree.
func GoGate(unitOnly bool) CommandGate {
	test := Step{Name: "test", Command: "make test"}
	if unitOnly {
		test.Env = []string{"GT_TEST_DOCKER=0"}
	}
	return CommandGate{Steps: []Step{
		{Name: "lint", Command: "make lint"},
		{Name: "build", Command: "go build ./..."},
		test,
	}}
}

// RigGate is the gate for the tree in dir: GoGate when it is a Go module,
// otherwise the rig's merge_queue lint, build and test commands. A rig with
// none of them is refused: "no gate configured" must stop a landing, never
// wave it through (G2-11).
func RigGate(dir string, mq *config.MergeQueueConfig, unitOnly bool) (CommandGate, error) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return GoGate(unitOnly), nil
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
		writers := []io.Writer{&buf}
		if g.Out != nil {
			writers = append(writers, g.Out)
		}
		var logFile *os.File
		if g.LogDir != "" {
			f, err := os.OpenFile(filepath.Join(g.LogDir, s.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				res.Err = fmt.Errorf("opening %s log: %w", s.Name, err)
				return res
			}
			logFile = f
			writers = append(writers, f)
		}
		start := time.Now()
		code, err := run(ctx, dir, s.Env, argv, io.MultiWriter(writers...))
		if logFile != nil {
			_ = logFile.Close()
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
	cmd.Env = append(os.Environ(), env...)
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
