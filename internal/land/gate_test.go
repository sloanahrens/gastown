package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	g, err := RigGate(goDir, &config.MergeQueueConfig{}, true)
	if err != nil || g.Steps[0].Command != "make lint" || g.Steps[1].Command != "go build ./..." || g.Steps[2].Command != "make test" {
		t.Fatalf("go rig default gate = %+v, %v", g.Steps, err)
	}
	g, err = RigGate(goDir, &config.MergeQueueConfig{LintCommand: "golangci-lint run", BuildCommand: "make build", TestCommand: "GOFLAGS=-p=8 make test"}, true)
	if err != nil || g.Steps[0].Command != "golangci-lint run" || g.Steps[1].Command != "go build ./..." || g.Steps[2].Command != "GOFLAGS=-p=8 make test" {
		t.Fatalf("go rig gate must use the rig's lint and test commands and always go build ./...: %+v, %v", g.Steps, err)
	}
	if !g.Steps[0].LockRetry || len(g.Steps[2].Env) != 1 || g.Steps[2].Env[0] != "GT_TEST_DOCKER=0" {
		t.Fatalf("go rig gate lost the lint lock retry or the unit-tier switch: %+v", g.Steps)
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

func TestMergeEnvReplacesInheritedKeys(t *testing.T) {
	t.Parallel()
	got := mergeEnv([]string{"A=1", "GT_TEST_DOCKER=1", "B=2"}, []string{"GT_TEST_DOCKER=0"})
	if strings.Join(got, " ") != "A=1 B=2 GT_TEST_DOCKER=0" {
		t.Fatalf("mergeEnv = %q, want the inherited GT_TEST_DOCKER dropped", got)
	}
}

func TestWithSlotWrapsOnlyTheTestStep(t *testing.T) {
	t.Parallel()
	s := &scriptedRun{answers: map[string]scriptedAnswer{}}
	g := WithSlot(GoGate(false), "/bin/gt", "gastown/landing")
	g.run = s.run
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if res := g.Run(ctx, "/w"); !res.Passed {
		t.Fatalf("gate = %+v", res)
	}
	if got := strings.Join(s.calls[2].argv, " "); got != "/bin/gt slot run --role gastown/landing -- sh -c make test" {
		t.Errorf("test argv = %q", got)
	}
	if got := strings.Join(s.calls[0].argv, " "); got != "sh -c make lint" {
		t.Errorf("lint argv = %q, want it unwrapped", got)
	}
	if GoGate(false).Steps[2].Wrap != nil {
		t.Error("WithSlot mutated the gate it was given")
	}
}

// Without a deadline the lint lock is not waited on: a contended first
// attempt is an infrastructure error, never a red verdict.
func TestCommandGateContendedLintWithoutDeadlineIsInfra(t *testing.T) {
	t.Parallel()
	g := GoGate(true)
	g.lockDelays = []time.Duration{0}
	g.run = func(_ context.Context, _ string, _, _ []string, out io.Writer) (int, error) {
		_, _ = io.WriteString(out, lintlock.Marker+"\n")
		return 3, nil
	}
	if res := g.Run(context.Background(), "/w"); res.Passed || res.Err == nil {
		t.Fatalf("gate = %+v, want an infrastructure error", res)
	}
}

// A lint killed by the gate's own deadline linted nothing: an
// infrastructure error, never a red verdict.
func TestCommandGateLintKilledByDeadlineIsInfra(t *testing.T) {
	t.Parallel()
	g := GoGate(true)
	g.run = func(ctx context.Context, _ string, _, _ []string, _ io.Writer) (int, error) {
		return -1, context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	res := g.Run(ctx, "/w")
	if res.Passed || !errors.Is(res.Err, context.DeadlineExceeded) || len(res.Steps) != 1 {
		t.Fatalf("gate = %+v, want an infrastructure error from the lint step alone", res)
	}
}

// The unit tier holds no container slot, so a rig whose test command opts
// itself into the container suite cannot run there: RigGate refuses rather
// than start containers beside the rest of the town (gt-0ss4).
func TestRigGateUnitTierRefusesAContainerOptIn(t *testing.T) {
	t.Parallel()
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"GT_TEST_DOCKER=1 make test", "export GT_TEST_DOCKER=1; make test", "env GT_TEST_DOCKER=true go test ./..."} {
		if _, err := RigGate(goDir, &config.MergeQueueConfig{TestCommand: cmd}, true); err == nil {
			t.Errorf("unit tier accepted %q", cmd)
		}
		if _, err := RigGate(goDir, &config.MergeQueueConfig{TestCommand: cmd}, false); err != nil {
			t.Errorf("full tier refused %q: %v", cmd, err)
		}
	}
	if _, err := RigGate(goDir, &config.MergeQueueConfig{TestCommand: "GT_TEST_DOCKER=0 make test"}, true); err != nil {
		t.Errorf("an explicit opt-out was refused: %v", err)
	}
}

func writeMakefile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"go.mod": "module x\n", "Makefile": body} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Land's gate comes from the rig: merge_queue.gate, else make gate when the
// target exists, else make test (the only one WithSlot wraps).
func TestLandGateComesFromTheRig(t *testing.T) {
	t.Parallel()
	withGate := writeMakefile(t, "lint:\n\ttrue\ngate: lint\n\ttrue\n")
	noGate := writeMakefile(t, "GATE := x\ngate-docs:\n\ttrue\ntest:\n\ttrue\n")
	hm := "gt slot run --role hm/crew/sloan -- make test"
	for _, tc := range []struct {
		name, dir string
		mq        *config.MergeQueueConfig
		want      string
		step      string
	}{
		{"rig setting wins", withGate, &config.MergeQueueConfig{Gate: hm}, hm, "gate"},
		{"make gate target", withGate, &config.MergeQueueConfig{}, "make gate", "gate"},
		{"make test fallback", noGate, nil, "make test", "test"},
	} {
		g := LandGate(tc.dir, tc.mq)
		if len(g.Steps) != 1 || g.Steps[0].Command != tc.want || g.Steps[0].Name != tc.step {
			t.Errorf("%s: steps = %+v, want one %q step %q", tc.name, g.Steps, tc.step, tc.want)
		}
		wrapped := WithSlot(g, "gt", "r").Steps[0].Wrap != nil
		if wrapped != (tc.step == "test") {
			t.Errorf("%s: WithSlot wrapped=%v", tc.name, wrapped)
		}
	}
}

// With D9's make gate present, gt done's unit tier is that one target.
func TestRigGateUsesMakeGateWhenPresent(t *testing.T) {
	t.Parallel()
	g, err := RigGate(writeMakefile(t, "gate:\n\ttrue\n"), &config.MergeQueueConfig{TestCommand: "make test"}, true)
	if err != nil || len(g.Steps) != 1 || g.Steps[0].Command != "make gate" {
		t.Fatalf("RigGate = %+v, %v; want make gate", g.Steps, err)
	}
	g, err = RigGate(writeMakefile(t, "test:\n\ttrue\n"), nil, true)
	if err != nil || len(g.Steps) != 3 {
		t.Fatalf("RigGate without a gate target = %+v, %v", g.Steps, err)
	}
}

// gt-ssyxd: gt done's pre-submit prefers presubmit_command, then make
// presubmit, then make gate; Land's gate is untouched by any of them.
func TestRigGatePresubmitPrecedence(t *testing.T) {
	t.Parallel()
	both := writeMakefile(t, "presubmit:\n\ttrue\ngate:\n\ttrue\n")

	g, err := RigGate(both, &config.MergeQueueConfig{PresubmitCommand: "make my-check"}, true)
	if err != nil || len(g.Steps) != 1 || g.Steps[0].Command != "make my-check" || g.Steps[0].Name != "presubmit" {
		t.Fatalf("explicit presubmit_command = %+v, %v", g.Steps, err)
	}
	if !reflect.DeepEqual(g.Steps[0].Env, []string{"GT_TEST_DOCKER=0"}) {
		t.Errorf("explicit presubmit_command env = %v, want containers forced off", g.Steps[0].Env)
	}

	g, err = RigGate(both, &config.MergeQueueConfig{TestCommand: "make test"}, true)
	if err != nil || len(g.Steps) != 1 || g.Steps[0].Command != "make presubmit" {
		t.Fatalf("Makefile with presubmit = %+v, %v; want make presubmit", g.Steps, err)
	}

	g, err = RigGate(writeMakefile(t, "gate:\n\ttrue\n"), nil, true)
	if err != nil || len(g.Steps) != 1 || g.Steps[0].Command != "make gate" {
		t.Fatalf("Makefile without presubmit = %+v, %v; want make gate", g.Steps, err)
	}

	// A non-Go tree takes an explicit presubmit_command too.
	g, err = RigGate(t.TempDir(), &config.MergeQueueConfig{PresubmitCommand: "pytest -x"}, true)
	if err != nil || len(g.Steps) != 1 || g.Steps[0].Command != "pytest -x" {
		t.Fatalf("non-Go presubmit_command = %+v, %v", g.Steps, err)
	}

	// The container opt-in is refused, as it is for test_command.
	if _, err := RigGate(both, &config.MergeQueueConfig{PresubmitCommand: "GT_TEST_DOCKER=1 make x"}, true); err == nil {
		t.Error("a presubmit_command that opts into containers was accepted")
	}

	// Only the unit tier reads it: the full tier keeps test_command.
	g, err = RigGate(both, &config.MergeQueueConfig{PresubmitCommand: "make my-check"}, false)
	if err != nil || len(g.Steps) != 3 {
		t.Fatalf("full tier = %+v, %v; want lint, build, test", g.Steps, err)
	}

	// Land's gate is make gate, whatever presubmit says.
	lg := LandGate(both, &config.MergeQueueConfig{PresubmitCommand: "make my-check"})
	if len(lg.Steps) != 1 || lg.Steps[0].Command != "make gate" {
		t.Fatalf("LandGate = %+v; want make gate", lg.Steps)
	}
}
