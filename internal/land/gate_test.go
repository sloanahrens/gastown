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

func TestCommandGateLogDir(t *testing.T) {
	t.Parallel()
	logDir := filepath.Join(t.TempDir(), "logs")
	s := &scriptedRun{answers: map[string]scriptedAnswer{"make test": {output: "hello\n"}}}
	g := CommandGate{
		Steps:  []Step{{Name: "test", Command: "make test"}},
		LogDir: logDir,
	}
	g.run = s.run
	if res := g.Run(context.Background(), "/w"); !res.Passed {
		t.Fatalf("gate = %+v", res)
	}
	if got := strings.Join(s.calls[0].argv, " "); got != "sh -c make test" {
		t.Fatalf("argv = %q", got)
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

// The -p cap gt-5lyns puts on a polecat seat's go commands must never reach
// the landing worker's merged-tree gate: the gate is the command the cap
// exists to leave cores for (gt-v4r0x), and it runs from the daemon's own
// environment. A step that named GOFLAGS would cap it.
func TestLandGateStepsCarryNoGOFLAGSEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	makefile := "gate:\n\tmake gate-lint gate-test\ngate-lint:\n\t@golangci-lint run\ngate-test:\n\t@go test ./...\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	g := LandGate(dir, &config.MergeQueueConfig{})
	if len(g.Steps) == 0 {
		t.Fatal("LandGate returned no steps")
	}
	for _, s := range g.Steps {
		for _, e := range s.Env {
			if e == "GOFLAGS" || strings.HasPrefix(e, "GOFLAGS=") {
				t.Fatalf("landing gate step %q carries %q; the seat cap must not reach the gate", s.Name, e)
			}
		}
	}
}

func TestMergeEnvReplacesInheritedKeys(t *testing.T) {
	t.Parallel()
	got := mergeEnv([]string{"A=1", "GT_TEST_DOCKER=1", "B=2"}, []string{"GT_TEST_DOCKER=0"})
	if strings.Join(got, " ") != "A=1 B=2 GT_TEST_DOCKER=0" {
		t.Fatalf("mergeEnv = %q, want the inherited GT_TEST_DOCKER dropped", got)
	}
}

func TestWithSlotHoldsOnlyForTheTestStep(t *testing.T) {
	t.Parallel()
	s := &scriptedRun{answers: map[string]scriptedAnswer{}}
	g := WithSlot(GoGate(false), "/town", "gastown/landing")
	g.run = s.run

	var held []string
	var released []*int
	g.holdSlot = func(_ context.Context, townRoot, role string) (func(*int), error) {
		held = append(held, townRoot+" "+role)
		return func(code *int) { released = append(released, code) }, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if res := g.Run(ctx, "/w"); !res.Passed {
		t.Fatalf("gate = %+v", res)
	}
	if len(held) != 1 || held[0] != "/town gastown/landing" {
		t.Errorf("holds taken = %v, want the test step's alone", held)
	}
	if len(released) != 1 || released[0] == nil || *released[0] != 0 {
		t.Errorf("releases = %v, want one carrying the step's exit 0", released)
	}
	// The hold is in process: the step's own argv is unchanged, with no `gt
	// slot run` in front of it.
	if got := strings.Join(s.calls[2].argv, " "); got != "sh -c make test" {
		t.Errorf("test argv = %q, want it unwrapped", got)
	}
	if got := strings.Join(s.calls[0].argv, " "); got != "sh -c make lint" {
		t.Errorf("lint argv = %q, want it unwrapped", got)
	}
	if GoGate(false).Steps[2].SlotRole != "" {
		t.Error("WithSlot mutated the gate it was given")
	}
}

// A step whose command never ran has no exit status, and a gate that could not
// take the slot at all must not run the step: both are the hold's own
// outcomes, and both must leave the slot behind (gt-638go.12).
func TestWithSlotReleasesOnInfraFailureAndRefusesWithoutTheSlot(t *testing.T) {
	t.Parallel()

	t.Run("a step that never ran releases without an exit code", func(t *testing.T) {
		t.Parallel()
		s := &scriptedRun{answers: map[string]scriptedAnswer{"make test": {code: -1, err: errors.New("exec: sh: not found")}}}
		g := CommandGate{TownRoot: "/town", Steps: []Step{{Name: "test", Command: "make test", SlotRole: "gastown/landing"}}}
		g.run = s.run
		var released []*int
		g.holdSlot = func(context.Context, string, string) (func(*int), error) {
			return func(code *int) { released = append(released, code) }, nil
		}
		if res := g.Run(context.Background(), "/w"); res.Err == nil {
			t.Fatalf("gate = %+v, want an infrastructure error", res)
		}
		if len(released) != 1 || released[0] != nil {
			t.Errorf("releases = %v, want one with no exit status", released)
		}
	})

	t.Run("a slot the gate cannot take stops the step", func(t *testing.T) {
		t.Parallel()
		s := &scriptedRun{answers: map[string]scriptedAnswer{"make test": {}}}
		g := CommandGate{TownRoot: "/town", Steps: []Step{{Name: "test", Command: "make test", SlotRole: "gastown/landing"}}}
		g.run = s.run
		g.holdSlot = func(context.Context, string, string) (func(*int), error) {
			return nil, errors.New("pool has no slot available")
		}
		res := g.Run(context.Background(), "/w")
		if res.Err == nil || len(s.calls) != 0 {
			t.Fatalf("gate = %+v with %d step(s) run, want the failure before the step", res, len(s.calls))
		}
	})

	t.Run("a slot step with no town is refused", func(t *testing.T) {
		t.Parallel()
		s := &scriptedRun{answers: map[string]scriptedAnswer{"make test": {}}}
		g := CommandGate{Steps: []Step{{Name: "test", Command: "make test", SlotRole: "gastown/landing"}}}
		g.run = s.run
		res := g.Run(context.Background(), "/w")
		if res.Err == nil || len(s.calls) != 0 {
			t.Fatalf("gate = %+v with %d step(s) run, want the failure before the step", res, len(s.calls))
		}
	})
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
// target exists, else make test (the only step WithSlot puts under the slot).
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
		underSlot := WithSlot(g, "/town", "r").Steps[0].SlotRole != ""
		if underSlot != (tc.step == "test") {
			t.Errorf("%s: WithSlot holds the slot=%v", tc.name, underSlot)
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

// TestCommandGateStepTimeoutIsAVerdict: a step that outlives its own timeout
// is killed and reported TimedOut, the steps after it never start, and that
// is a verdict on the tree rather than an infrastructure error (gt-b5ugw).
func TestCommandGateStepTimeoutIsAVerdict(t *testing.T) {
	t.Parallel()
	var ran []string
	g := CommandGate{Steps: []Step{
		{Name: "lint", Command: "make gate-lint", Timeout: 20 * time.Millisecond},
		{Name: "gate", Command: "make gate-test", Timeout: time.Hour},
	}}
	g.run = func(ctx context.Context, _ string, _, argv []string, _ io.Writer) (int, error) {
		ran = append(ran, argv[len(argv)-1])
		<-ctx.Done()
		return -1, ctx.Err()
	}
	res := g.Run(context.Background(), "/w")
	step, timedOut := res.TimedOutStep()
	if res.Passed || res.Err != nil || !timedOut || step.Name != "lint" || step.Timeout != 20*time.Millisecond {
		t.Fatalf("gate = %+v, want the lint step timed out with no infrastructure error", res)
	}
	if !reflect.DeepEqual(ran, []string{"make gate-lint"}) {
		t.Errorf("ran %v, want only the lint stage", ran)
	}
}

// TestCommandGateStepTimeoutLeavesFastStepsAlone: a step under its timeout
// passes as before.
func TestCommandGateStepTimeoutLeavesFastStepsAlone(t *testing.T) {
	t.Parallel()
	s := &scriptedRun{answers: map[string]scriptedAnswer{}}
	g := CommandGate{Steps: []Step{
		{Name: "lint", Command: "make gate-lint", Timeout: time.Minute},
		{Name: "gate", Command: "make gate-test", Timeout: time.Minute},
	}}
	g.run = s.run
	res := g.Run(context.Background(), "/w")
	if _, timedOut := res.TimedOutStep(); !res.Passed || timedOut || len(s.calls) != 2 {
		t.Fatalf("gate = %+v after %d call(s), want both stages passed", res, len(s.calls))
	}
}

// TestLandGateSplitsMakeGateIntoStages: a tree whose Makefile has gate-lint
// and gate-test gates in those two stages, lint first; WithTimeouts bounds
// each; the unit tier is still recognized for the flake rerun (gt-b5ugw).
func TestLandGateSplitsMakeGateIntoStages(t *testing.T) {
	t.Parallel()
	dir := writeMakefile(t, "lint:\n\ttrue\ngate-lint: lint\ngate-test:\n\ttrue\ngate: gate-lint gate-test\n")
	for _, mq := range []*config.MergeQueueConfig{nil, {Gate: "make gate"}} {
		g := WithTimeouts(LandGate(dir, mq), time.Minute, 5*time.Minute, 3*time.Minute)
		if len(g.Steps) != 2 || g.Steps[0].Command != "make gate-lint" || g.Steps[1].Command != "make gate-test" {
			t.Fatalf("mq %+v: steps = %+v, want gate-lint then gate-test", mq, g.Steps)
		}
		if g.Steps[0].Timeout != time.Minute || g.Steps[1].Timeout != 5*time.Minute || !g.Steps[0].LockRetry {
			t.Errorf("mq %+v: steps = %+v, want lint 1m with lock retry, tests 5m", mq, g.Steps)
		}
		if !g.UnitTier() {
			t.Errorf("mq %+v: the split make gate is not read as the unit tier", mq)
		}
		if WithSlot(g, "/town", "r").Steps[1].SlotRole != "" {
			t.Errorf("mq %+v: the unit tier took the container slot", mq)
		}
	}
	// A rig's own gate command is one step, whatever the Makefile offers.
	if g := LandGate(dir, &config.MergeQueueConfig{Gate: "make test"}); len(g.Steps) != 1 || g.UnitTier() {
		t.Errorf("custom gate: steps = %+v unit=%v, want one step, not the unit tier", g.Steps, g.UnitTier())
	}
}

// writeShellTierTree is a tree with make gate's two stages and the shell
// tier's entry point. The script only has to exist for the gate's own check;
// the fake runner answers the step.
func writeShellTierTree(t *testing.T, makefile string) string {
	t.Helper()
	dir := writeMakefile(t, makefile)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, shellTierScript), []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const gateStagesMakefile = "gate-lint:\n\ttrue\ngate-test:\n\ttrue\ngate: gate-lint gate-test\n"

// TestShellTierInputsMatchPostLandScript: the gate's shell-tier rule is the
// post-land run's own INPUTS set, and the Go constant is checked against the
// script so the two cannot drift (gt-vsct7.8, gt-er6jn).
func TestShellTierInputsMatchPostLandScript(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "post-land-shell.sh"))
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "INPUTS='"); ok {
			want = strings.TrimSuffix(v, "'")
			break
		}
	}
	if want == "" {
		t.Fatal("scripts/post-land-shell.sh has no INPUTS='...' line")
	}
	if want != ShellTierInputs {
		t.Fatalf("ShellTierInputs = %q, want scripts/post-land-shell.sh's %q", ShellTierInputs, want)
	}
}

// TestLandGateRunsTheShellTierForItsInputs: a merged tree that changed a
// shell-tier input runs the tier as a third step, after the gate stages and
// under its own timeout, and a red summary line names the scripts (gt-vsct7.8).
func TestLandGateRunsTheShellTierForItsInputs(t *testing.T) {
	t.Parallel()
	dir := writeShellTierTree(t, gateStagesMakefile)
	s := &scriptedRun{answers: map[string]scriptedAnswer{
		shellTierDiffCommand: {output: "plugins/rebuild-gt\ninternal/cmd/x.go\n"},
		ShellStepCommand:     {code: 1, output: "tier-sweep: shell RED passed=8 failed=2 skipped=0 failed: scripts/a_test.sh plugins/b_test.sh (logs /tmp/tier-sweep.aB12)\n"},
	}}
	g := WithTimeouts(LandGate(dir, nil), time.Minute, 5*time.Minute, 3*time.Minute)
	g.run = s.run
	res := g.Run(context.Background(), dir)
	if res.Passed || res.Err != nil {
		t.Fatalf("gate = %+v, want the shell step's failure", res)
	}
	if len(res.Steps) != 3 || res.Steps[2].Name != ShellStepName || res.Steps[2].Command != ShellStepCommand {
		t.Fatalf("steps = %+v, want the two gate stages then %s", res.Steps, ShellStepCommand)
	}
	if res.Steps[2].ExitCode != 1 {
		t.Errorf("shell step = %+v, want exit 1", res.Steps[2])
	}
	if got := res.ShellTierFailures(); !reflect.DeepEqual(got, []string{"scripts/a_test.sh", "plugins/b_test.sh"}) {
		t.Errorf("shell failures = %v, want the scripts the summary named", got)
	}
	if !strings.Contains(res.Summary(), "shell exit 1") {
		t.Errorf("summary = %q, want the shell step", res.Summary())
	}
	if got := strings.Join(s.calls[0].argv, " "); got != "sh -c "+shellTierDiffCommand {
		t.Errorf("the change check ran %q, want %q", got, shellTierDiffCommand)
	}
}

// postLandShellRedLine is a post-land run's own red summary, verbatim from
// the gt-40so9 incident's post-land log: the run that blamed a Go-only
// landing for a scripts/test-makefile.sh failure and opened the revert. The
// "(logs DIR)" suffix is part of what scripts/tier-sweep.sh prints, so a
// parser that reads past it names the log directory as a failing script
// (gt-40so9).
const postLandShellRedLine = "tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/test-makefile.sh (logs /var/folders/dx/ccj87p8d14l8cs64cnp691pm0000gn/T//tier-sweep.PpcO4T)"

// TestCommandGateReadsShellFailuresFromAnyStep: the post-land command runs
// the tier sweep inside its "test" step (the name that holds the container
// slot), so the scripts its summary named are that step's shell failures too
// (gt-40so9).
func TestCommandGateReadsShellFailuresFromAnyStep(t *testing.T) {
	t.Parallel()
	const postLand = "bash scripts/post-land-shell.sh"
	s := &scriptedRun{answers: map[string]scriptedAnswer{
		postLand: {code: 1, output: postLandShellRedLine + "\n"},
	}}
	g := CommandGate{Steps: []Step{{Name: "test", Command: postLand}}}
	g.run = s.run
	res := g.Run(context.Background(), t.TempDir())
	if res.Passed || res.Err != nil || len(res.Steps) != 1 {
		t.Fatalf("gate = %+v, want the test step's failure", res)
	}
	if got := res.Steps[0].ShellFailures; !reflect.DeepEqual(got, []string{"scripts/test-makefile.sh"}) {
		t.Fatalf("shell failures = %v, want just the script the summary named, not the log directory", got)
	}
}

// TestParseShellTierFailuresReadsTheRealSummaryLine: the formats the parse
// accepts, pinned to the one scripts/tier-sweep.sh actually prints — with the
// "(logs DIR)" suffix, with several names, and with none.
func TestParseShellTierFailuresReadsTheRealSummaryLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{name: "the post-land run's line", out: postLandShellRedLine, want: []string{"scripts/test-makefile.sh"}},
		{name: "several scripts", out: "tier-sweep: shell RED passed=8 failed=2 skipped=0 failed: scripts/a_test.sh plugins/b_test.sh (logs /tmp/tier-sweep.aB12)",
			want: []string{"scripts/a_test.sh", "plugins/b_test.sh"}},
		{name: "a green run names none", out: "tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /tmp/tier-sweep.aB12)", want: nil},
		{name: "a red run that named no script", out: "tier-sweep: shell RED passed=6 failed=1 skipped=0 (logs /tmp/tier-sweep.aB12)", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseShellTierFailures(tc.out + "\n"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseShellTierFailures(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestLandGateSkipsTheShellTierWithoutItsInputs: a tree that changed none of
// the tier's inputs runs no shell step and pays no time for it, and a tree
// that does not ship the tier never gets one, whatever it changed
// (gt-vsct7.8).
func TestLandGateSkipsTheShellTierWithoutItsInputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, dir, diff string
	}{
		{"a changed Go file is not a shell input", writeShellTierTree(t, gateStagesMakefile), "internal/cmd/x.go\nREADME.md\n"},
		{"a tree without the tier gets no step", writeMakefile(t, gateStagesMakefile), "Makefile\nscripts/a_test.sh\n"},
	} {
		s := &scriptedRun{answers: map[string]scriptedAnswer{shellTierDiffCommand: {output: tc.diff}}}
		g := WithTimeouts(LandGate(tc.dir, nil), time.Minute, time.Minute, time.Minute)
		g.run = s.run
		res := g.Run(context.Background(), tc.dir)
		if !res.Passed || res.Err != nil || len(res.Steps) != 2 {
			t.Fatalf("%s: gate = %+v, want the two stages and no shell step", tc.name, res)
		}
		for _, c := range s.calls {
			if c.argv[len(c.argv)-1] == ShellStepCommand {
				t.Errorf("%s: the shell tier ran", tc.name)
			}
		}
		if res.ShellTierFailures() != nil {
			t.Errorf("%s: shell failures = %v", tc.name, res.ShellTierFailures())
		}
	}
}

// TestLandGateShellStepTimeoutIsItsOwn: the shell step carries the bound
// WithTimeouts gave the gate, so its hang is killed at that bound and
// reported as the step's own timeout (gt-vsct7.8).
func TestLandGateShellStepTimeoutIsItsOwn(t *testing.T) {
	t.Parallel()
	dir := writeShellTierTree(t, gateStagesMakefile)
	s := &scriptedRun{answers: map[string]scriptedAnswer{shellTierDiffCommand: {output: "Makefile\n"}}}
	g := WithTimeouts(LandGate(dir, nil), time.Hour, time.Hour, 20*time.Millisecond)
	step := s.run
	g.run = func(ctx context.Context, d string, env, argv []string, out io.Writer) (int, error) {
		if argv[len(argv)-1] == ShellStepCommand {
			<-ctx.Done()
			return -1, ctx.Err()
		}
		return step(ctx, d, env, argv, out)
	}
	res := g.Run(context.Background(), dir)
	last, timedOut := res.TimedOutStep()
	if res.Passed || res.Err != nil || !timedOut || last.Name != ShellStepName || last.Timeout != 20*time.Millisecond {
		t.Fatalf("gate = %+v, want the shell step killed by its own timeout", res)
	}
}

// TestLandGateShellTierDiffFailureIsInfra: a change check that cannot be read
// says nothing about the tree, so it is an infrastructure error rather than a
// silent skip or a verdict (gt-vsct7.8).
func TestLandGateShellTierDiffFailureIsInfra(t *testing.T) {
	t.Parallel()
	dir := writeShellTierTree(t, gateStagesMakefile)
	s := &scriptedRun{answers: map[string]scriptedAnswer{shellTierDiffCommand: {code: 128, output: "fatal: bad revision\n"}}}
	g := LandGate(dir, nil)
	g.run = s.run
	res := g.Run(context.Background(), dir)
	if res.Err == nil || len(res.Steps) != 0 {
		t.Fatalf("gate = %+v, want an infrastructure error before any step", res)
	}
}

// TestLandGateShellTierUnreadableTreeIsInfra: a tree the change check cannot
// read is not a tree without the tier, so it stops the landing as
// infrastructure rather than skipping the step (gt-vsct7.8).
func TestLandGateShellTierUnreadableTreeIsInfra(t *testing.T) {
	t.Parallel()
	dir := writeMakefile(t, gateStagesMakefile)
	if err := os.WriteFile(filepath.Join(dir, "scripts"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &scriptedRun{}
	g := LandGate(dir, nil)
	g.run = s.run
	res := g.Run(context.Background(), dir)
	if res.Err == nil || len(res.Steps) != 0 {
		t.Fatalf("gate = %+v, want an infrastructure error for an unreadable tree", res)
	}
}
