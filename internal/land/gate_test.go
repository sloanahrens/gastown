package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lintlock"
)

// scriptedRun answers each step by its command string.
type scriptedRun struct {
	answers map[string]scriptedAnswer
	calls   []runCall
}

type scriptedAnswer struct {
	output string
	code   int
	err    error
}

type runCall struct {
	dir  string
	env  []string
	argv []string
}

func (s *scriptedRun) run(_ context.Context, dir string, env, argv []string, out io.Writer) (int, error) {
	s.calls = append(s.calls, runCall{dir: dir, env: env, argv: argv})
	a := s.answers[argv[len(argv)-1]]
	_, _ = io.WriteString(out, a.output)
	return a.code, a.err
}

func TestCommandGateRunsStepsInOrderInDir(t *testing.T) {
	t.Parallel()
	s := &scriptedRun{answers: map[string]scriptedAnswer{
		"make lint":      {output: "lint ok\n"},
		"go build ./...": {},
		"make test":      {output: "ok  \tgithub.com/x/a\t0.1s\nok  \tgithub.com/x/b\t(cached)\n"},
	}}
	g := GoGate(false)
	g.run = s.run
	res := g.Run(context.Background(), "/work/tree")
	if !res.Passed || res.Err != nil {
		t.Fatalf("gate = %+v, want passed", res)
	}
	var got []string
	for _, c := range s.calls {
		if c.dir != "/work/tree" {
			t.Errorf("step ran in %q", c.dir)
		}
		got = append(got, strings.Join(c.argv, " "))
	}
	want := []string{"sh -c make lint", "sh -c go build ./...", "sh -c make test"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	pk := res.Steps[2].Packages
	if len(pk) != 2 || pk[0] != (PackageResult{Package: "github.com/x/a", Passed: true}) {
		t.Fatalf("packages = %+v", pk)
	}
}

func TestCommandGateStopsAtFirstRedStepWithTail(t *testing.T) {
	t.Parallel()
	var lines strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	lines.WriteString("--- FAIL: TestX (0.00s)\nFAIL\tgithub.com/x/c\t0.2s\nok  \tgithub.com/x/d\t0.1s\n")
	s := &scriptedRun{answers: map[string]scriptedAnswer{
		"make test": {output: lines.String(), code: 2},
	}}
	g := GoGate(false)
	g.run = s.run
	res := g.Run(context.Background(), "/w")
	if res.Passed || res.Err != nil {
		t.Fatalf("gate = %+v, want red", res)
	}
	last := res.Steps[len(res.Steps)-1]
	if last.Name != "test" || last.ExitCode != 2 {
		t.Fatalf("last step = %+v", last)
	}
	if n := strings.Count(res.FailureTail(), "\n"); n > gateTailLines+1 {
		t.Errorf("tail has %d lines", n)
	}
	if !strings.Contains(res.FailureTail(), "FAIL\tgithub.com/x/c") || strings.Contains(res.FailureTail(), "line 5\n") {
		t.Errorf("tail = %q", res.FailureTail())
	}
	if pk := last.Packages; len(pk) != 2 || pk[0].Passed || pk[0].Package != "github.com/x/c" || !pk[1].Passed {
		t.Errorf("packages = %+v", pk)
	}
	if !strings.Contains(res.Summary(), "test exit 2") {
		t.Errorf("summary = %q", res.Summary())
	}

	s2 := &scriptedRun{answers: map[string]scriptedAnswer{"make lint": {code: 1}}}
	g2 := GoGate(false)
	g2.run = s2.run
	if res := g2.Run(context.Background(), "/w"); res.Passed || len(s2.calls) != 1 {
		t.Fatalf("red lint ran %d steps, passed=%v", len(s2.calls), res.Passed)
	}
}

func TestCommandGateStartFailureIsInfraNotRed(t *testing.T) {
	t.Parallel()
	s := &scriptedRun{answers: map[string]scriptedAnswer{"make lint": {code: -1, err: errors.New("exec: sh: not found")}}}
	g := GoGate(false)
	g.run = s.run
	res := g.Run(context.Background(), "/w")
	if res.Passed || res.Err == nil {
		t.Fatalf("gate = %+v, want an infrastructure error", res)
	}
}

func TestCommandGateWrapAndLogDir(t *testing.T) {
	t.Parallel()
	logDir := filepath.Join(t.TempDir(), "logs")
	s := &scriptedRun{answers: map[string]scriptedAnswer{"make test": {output: "hello\n"}}}
	g := CommandGate{
		Steps:  []Step{{Name: "test", Command: "make test", Wrap: func(argv []string) []string { return append([]string{"gt", "slot", "run", "--"}, argv...) }}},
		LogDir: logDir,
	}
	g.run = s.run
	if res := g.Run(context.Background(), "/w"); !res.Passed {
		t.Fatalf("gate = %+v", res)
	}
	if got := strings.Join(s.calls[0].argv, " "); got != "gt slot run -- sh -c make test" {
		t.Fatalf("wrapped argv = %q", got)
	}
	info, err := os.Stat(filepath.Join(logDir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", info.Mode().Perm())
	}
	dinfo, _ := os.Stat(logDir)
	if dinfo.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode = %v, want 0700", dinfo.Mode().Perm())
	}
}

func TestGoGateUnitOnlyTurnsContainersOff(t *testing.T) {
	t.Parallel()
	for _, unit := range []bool{true, false} {
		g := GoGate(unit)
		test := g.Steps[len(g.Steps)-1]
		has := false
		for _, e := range test.Env {
			has = has || e == "GT_TEST_DOCKER=0"
		}
		if has != unit {
			t.Errorf("GoGate(%v) test env = %v", unit, test.Env)
		}
	}
}

func TestRigGatePicksGoOrRigCommandsOrRefuses(t *testing.T) {
	t.Parallel()
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := RigGate(goDir, &config.MergeQueueConfig{TestCommand: "pytest"}, true)
	if err != nil || g.Steps[0].Command != "make lint" {
		t.Fatalf("go rig gate = %+v, %v", g.Steps, err)
	}
	plain := t.TempDir()
	g, err = RigGate(plain, &config.MergeQueueConfig{LintCommand: "ruff .", TestCommand: "pytest"}, true)
	if err != nil || len(g.Steps) != 2 || g.Steps[0].Command != "ruff ." || g.Steps[1].Command != "pytest" {
		t.Fatalf("rig gate = %+v, %v", g.Steps, err)
	}
	if _, err := RigGate(plain, &config.MergeQueueConfig{}, true); err == nil {
		t.Fatal("a rig with no gate commands must refuse, not pass (G2-11)")
	}
	if _, err := RigGate(plain, nil, true); err == nil {
		t.Fatal("nil config must refuse")
	}
}

// A contended golangci-lint linted nothing; the lint step waits it out on the
// shared lintlock schedule instead of reporting findings that do not exist.
func TestCommandGateRetriesAContendedLint(t *testing.T) {
	t.Parallel()
	attempts := 0
	g := GoGate(true)
	g.lockDelays = []time.Duration{0, 0}
	g.run = func(_ context.Context, _ string, _, argv []string, out io.Writer) (int, error) {
		if argv[len(argv)-1] == "make lint" {
			attempts++
			if attempts == 1 {
				_, _ = io.WriteString(out, "level=error msg=\""+lintlock.Marker+"\"\n")
				return 3, nil
			}
		}
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	res := g.Run(ctx, "/w")
	if !res.Passed || attempts != 2 {
		t.Fatalf("gate = %+v after %d lint attempts, want a pass on the second", res, attempts)
	}

	// A lint still contended after every retry linted nothing: an
	// infrastructure error, never a red verdict on the tree.
	g3 := GoGate(true)
	g3.lockDelays = []time.Duration{0}
	g3.run = func(_ context.Context, _ string, _, _ []string, out io.Writer) (int, error) {
		_, _ = io.WriteString(out, lintlock.Marker+"\n")
		return 3, nil
	}
	if res := g3.Run(ctx, "/w"); res.Passed || res.Err == nil {
		t.Fatalf("a lint that never ran = %+v, want an infrastructure error", res)
	}

	// A lint that found something is not retried.
	found := 0
	g2 := GoGate(true)
	g2.lockDelays = []time.Duration{0, 0}
	g2.run = func(_ context.Context, _ string, _, argv []string, out io.Writer) (int, error) {
		found++
		_, _ = io.WriteString(out, "a.go:1: unused variable\n")
		return 1, nil
	}
	if res := g2.Run(ctx, "/w"); res.Passed || found != 1 {
		t.Fatalf("a lint with findings ran %d times, passed=%v", found, res.Passed)
	}
}
