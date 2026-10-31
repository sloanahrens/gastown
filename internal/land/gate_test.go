package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	if len(pk) != 2 || pk[0] != (PackageResult{Package: "github.com/x/a", Passed: true, Elapsed: 100 * time.Millisecond, Timed: true}) {
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
// a gate's steps: the gate is the command the cap exists to leave cores for
// (gt-v4r0x). A step that named GOFLAGS would cap it.
func TestRigGateStepsCarryNoGOFLAGSEnv(t *testing.T) {
	t.Parallel()
	dir := writeMakefile(t, "gate:\n\tmake gate-lint gate-test\ngate-lint:\n\t@golangci-lint run\ngate-test:\n\t@go test ./...\n")
	g, err := RigGate(dir, &config.MergeQueueConfig{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Steps) == 0 {
		t.Fatal("RigGate returned no steps")
	}
	for _, s := range g.Steps {
		for _, e := range s.Env {
			if e == "GOFLAGS" || strings.HasPrefix(e, "GOFLAGS=") {
				t.Fatalf("gate step %q carries %q; the seat cap must not reach the gate", s.Name, e)
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

// fakeClock is the gate's clock in a test: advancing it costs no wall time, so
// a wait is measured without waiting (gt-njgu2).
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// A step's slot wait is what its wall time hid: a sweep that spends 22 minutes
// waiting and 4 testing recorded only the 26. Elapsed keeps the step's own
// time, so the two numbers stay separable (gt-njgu2).
func TestStepRecordsSlotWait(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Unix(1000, 0)}
	run := func(hold time.Duration) GateResult {
		s := &scriptedRun{answers: map[string]scriptedAnswer{}}
		g := WithSlot(GoGate(false), "/town", "gastown/landing")
		g.run = s.run
		g.now = clock.now
		g.holdSlot = func(context.Context, string, string) (func(*int), error) {
			clock.advance(hold)
			return func(*int) {}, nil
		}
		return g.Run(context.Background(), "/w")
	}

	// testStepOf finds the test step by name, so the test does not depend on
	// where GoGate places it.
	testStepOf := func(res GateResult) StepResult {
		t.Helper()
		for _, st := range res.Steps {
			if st.Name == "test" {
				return st
			}
		}
		t.Fatalf("gate ran no test step: %+v", res)
		return StepResult{}
	}

	waited := testStepOf(run(90 * time.Second))
	if got := waited.SlotWait; got != 90*time.Second {
		t.Errorf("test step SlotWait = %s, want the 90s it waited", got)
	}
	unwaited := testStepOf(run(0))
	if got := unwaited.SlotWait; got != 0 {
		t.Errorf("test step SlotWait = %s for a hold taken at once, want zero", got)
	}
	// The wait must not reach Elapsed: on the gate's clock the waited step runs
	// for exactly as long as the run that took the slot at once.
	if waited.Elapsed != unwaited.Elapsed {
		t.Errorf("test step Elapsed = %s beside a %s slot wait, want the unwaited run's %s", waited.Elapsed, waited.SlotWait, unwaited.Elapsed)
	}
}

// Only a step that asks for the slot reads the clock: a gate with no slot step
// measures nothing (gt-njgu2).
func TestStepWithoutSlotRoleHasNoSlotWait(t *testing.T) {
	t.Parallel()
	s := &scriptedRun{answers: map[string]scriptedAnswer{}}
	g := GoGate(false)
	g.run = s.run
	g.now = func() time.Time {
		t.Error("the gate read its clock for a gate with no slot step")
		return time.Time{}
	}
	res := g.Run(context.Background(), "/w")
	if !res.Passed {
		t.Fatalf("gate = %+v, want passed", res)
	}
	for _, st := range res.Steps {
		if st.SlotWait != 0 {
			t.Errorf("%s SlotWait = %s, want zero", st.Name, st.SlotWait)
		}
	}
}

// A refused hold still costs its wait, and the step it refused never ran: the
// wait rides on the result, the step gets no StepResult (gt-njgu2).
func TestRefusedSlotReportsTheWait(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Unix(1000, 0)}
	s := &scriptedRun{answers: map[string]scriptedAnswer{}}
	g := CommandGate{TownRoot: "/town", Steps: []Step{{Name: "test", Command: "make test", SlotRole: "gastown/landing"}}}
	g.run = s.run
	g.now = clock.now
	g.holdSlot = func(context.Context, string, string) (func(*int), error) {
		clock.advance(30 * time.Second)
		return nil, errors.New("pool has no slot available")
	}
	res := g.Run(context.Background(), "/w")
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no slot available") {
		t.Fatalf("gate = %+v, want the refusal as an infrastructure error", res)
	}
	if got := res.SlotWait; got != 30*time.Second {
		t.Errorf("SlotWait = %s, want the 30s spent being refused", got)
	}
	if len(res.Steps) != 0 || len(s.calls) != 0 {
		t.Errorf("gate reported %d step(s) and ran %d after a refusal, want none of either", len(res.Steps), len(s.calls))
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

// A post-land command that declares GT_TEST_DOCKER=0 runs no containers, so
// it takes no slot: a held gate-class slot blocks installs for nothing
// (gt-7k0hz).
func TestWithSlotForCommandSkipsADeclaredContainerFreeCommand(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		"GT_TEST_DOCKER=0 bash scripts/post-land-shell.sh",
		"GT_TEST_DOCKER=0",
		"  GT_TEST_DOCKER=0 make test  ",
		"GT_TEST_DOCKER=0 make test && GT_TEST_DOCKER=0 make lint",
		"GT_TEST_DOCKER=0 make test; GT_TEST_DOCKER=0 make lint",
		"GT_TEST_DOCKER=0 make test\nGT_TEST_DOCKER=0 make lint",
		"GT_TEST_DOCKER=0 bash scripts/post-land-shell.sh 2>&1",
		"GT_TEST_DOCKER=0 make test 2>&1; GT_TEST_DOCKER=0 make lint",
	} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			s := &scriptedRun{answers: map[string]scriptedAnswer{cmd: {output: "tier-sweep: shell GREEN\n"}}}
			holds := 0
			g := WithSlotForCommand(CommandGate{Steps: []Step{{Name: "test", Command: cmd}}}, "/town", "gastown/post-land", cmd)
			g.run = s.run
			g.holdSlot = func(context.Context, string, string) (func(*int), error) {
				holds++
				return func(*int) {}, nil
			}
			if res := g.Run(context.Background(), "/w"); !res.Passed {
				t.Fatalf("gate = %+v", res)
			}
			if holds != 0 {
				t.Errorf("holds taken = %d, want none for a container-free command", holds)
			}
			for _, st := range g.Steps {
				if st.SlotRole != "" {
					t.Errorf("step %q carries slot role %q", st.Name, st.SlotRole)
				}
			}
			if len(s.calls) != 1 {
				t.Errorf("steps run = %d, want the command once", len(s.calls))
			}
		})
	}
}

// A command whose text does not declare itself container-free keeps the slot
// exactly as WithSlot gives it: its tests may start containers, and that is
// not visible in the command (gt-7k0hz). A line where only one segment
// declares the assignment keeps the slot too: the other segment may start
// containers and inherits the ambient value (gt-7k0hz).
func TestWithSlotForCommandKeepsTheSlotOtherwise(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		"make test",
		"GOFLAGS=-p=6 make test",
		"bash scripts/post-land-shell.sh",
		"GT_TEST_DOCKER=1 make test",
		"GT_TEST_DOCKER=01 make test",
		"MY_GT_TEST_DOCKER=0 make test",
		`GT_TEST_DOCKER="0" make test`,
		"echo GT_TEST_DOCKER=0; make test",
		"GT_TEST_DOCKER=0 bash scripts/post-land-shell.sh; make test",
		"cd /w && GT_TEST_DOCKER=0 make test",
		"make lint; GT_TEST_DOCKER=0 make test",
		"true | GT_TEST_DOCKER=0 make test",
		"GT_TEST_DOCKER=0 make test | tee log",
		"GT_TEST_DOCKER=0 make test &> log",
		"",
		"   ",
		"GT_TEST_DOCKER=0 FOO=1 GT_TEST_DOCKER=1 bash scripts/post-land-shell.sh",
		"GT_TEST_DOCKER=0 GT_TEST_DOCKER=1 make test",
		"GT_TEST_DOCKER=0 env -u GT_TEST_DOCKER bash scripts/post-land-shell.sh",
		"GT_TEST_DOCKER=0 env GT_TEST_DOCKER=1 bash scripts/post-land-shell.sh",
	} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			s := &scriptedRun{answers: map[string]scriptedAnswer{cmd: {}}}
			var held []string
			g := WithSlotForCommand(CommandGate{Steps: []Step{{Name: "test", Command: cmd}}}, "/town", "gastown/post-land", cmd)
			g.run = s.run
			g.holdSlot = func(_ context.Context, townRoot, role string) (func(*int), error) {
				held = append(held, townRoot+" "+role)
				return func(*int) {}, nil
			}
			if res := g.Run(context.Background(), "/w"); !res.Passed {
				t.Fatalf("gate = %+v", res)
			}
			if len(held) != 1 || held[0] != "/town gastown/post-land" {
				t.Errorf("holds taken = %v, want the post-land slot once", held)
			}
			want := WithSlot(CommandGate{Steps: []Step{{Name: "test", Command: cmd}}}, "/town", "gastown/post-land")
			if !reflect.DeepEqual(g.Steps, want.Steps) || g.TownRoot != want.TownRoot {
				t.Errorf("gate = %+v, want the steps and town WithSlot gives", g)
			}
		})
	}
}

// Taking no slot changes no part of the run's verdict: for a declared and an
// undeclared command alike, the step result and the full log read as the
// always-slotted gate's own (gt-7k0hz).
func TestWithSlotForCommandLeavesTheResultAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		cmd           string
		output        string
		code          int
		shellFailures []string
	}{
		{
			name:          "declared",
			cmd:           "GT_TEST_DOCKER=0 bash scripts/post-land-shell.sh",
			output:        "tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.aB12) in 2m14s\n",
			code:          1,
			shellFailures: []string{"scripts/x.sh"},
		},
		{
			name:   "undeclared",
			cmd:    "make test",
			output: "--- FAIL: TestX\nFAIL\tgithub.com/x/a\t0.2s\n",
			code:   2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runOnce := func(g CommandGate) (StepResult, string) {
				s := &scriptedRun{answers: map[string]scriptedAnswer{tc.cmd: {output: tc.output, code: tc.code}}}
				g.run = s.run
				g.holdSlot = func(context.Context, string, string) (func(*int), error) { return func(*int) {}, nil }
				g.LogDir = t.TempDir()
				res := g.Run(context.Background(), "/w")
				if res.Err != nil || len(res.Steps) != 1 {
					t.Fatalf("gate = %+v, want one step result", res)
				}
				return res.Steps[0], filepath.Join(g.LogDir, "test.log")
			}
			base := CommandGate{Steps: []Step{{Name: "test", Command: tc.cmd}}}
			slotted, slottedLog := runOnce(WithSlot(base, "/town", "gastown/post-land"))
			free, freeLog := runOnce(WithSlotForCommand(base, "/town", "gastown/post-land", tc.cmd))
			if !reflect.DeepEqual(free.ShellFailures, slotted.ShellFailures) || free.ExitCode != slotted.ExitCode ||
				free.Tail != slotted.Tail || !reflect.DeepEqual(free.Packages, slotted.Packages) {
				t.Errorf("step = %+v, want the slotted one %+v", free, slotted)
			}
			if free.ExitCode != tc.code || !reflect.DeepEqual(free.ShellFailures, tc.shellFailures) || free.Tail == "" {
				t.Errorf("step = %+v, want the command's own exit %d, shell failures %v and a tail", free, tc.code, tc.shellFailures)
			}
			for _, p := range []string{slottedLog, freeLog} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("full log %s: %v", p, err)
				}
			}
		})
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
// presubmit, then make gate.
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

// TestShellTierInputsMatchPostLandScript: the shell-tier rule the post-land
// run and its revert read is the script's own INPUTS set, and the Go constant
// is checked against the script so the two cannot drift (gt-er6jn).
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

// postLandShellRedLine is a post-land run's own red summary, verbatim from
// the gt-40so9 incident's post-land log: the run that blamed a Go-only
// landing for a scripts/test-makefile.sh failure and opened the revert. The
// "(logs DIR)" suffix is part of what cmd/tier-sweep prints, so a
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
// accepts, pinned to the one cmd/tier-sweep actually prints — with the
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
		{name: "the elapsed time trails the names", out: "tier-sweep: shell RED passed=8 failed=2 skipped=0 failed: scripts/a_test.sh plugins/b_test.sh (logs /tmp/tier-sweep.aB12) in 2m14s",
			want: []string{"scripts/a_test.sh", "plugins/b_test.sh"}},
		{name: "a green run names none", out: "tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /tmp/tier-sweep.aB12) in 2m14s", want: nil},
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

// TestRealRunExitZeroWithWaitDelayIsSuccess: a step that exits 0 while a
// background child holds the output pipe past WaitDelay passed. Wait reports
// exec.ErrWaitDelay for that, which is not an ExitError; reading it as an
// infrastructure error told the gate a passing step "did not run". The real
// process is TestIntegrationRunStepWaitDelay.
func TestRealRunExitZeroWithWaitDelayIsSuccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		exitedZero bool
		want       bool
	}{
		{name: "exit 0, pipe held past WaitDelay", err: exec.ErrWaitDelay, exitedZero: true, want: true},
		{name: "wrapped WaitDelay error", err: fmt.Errorf("wait: %w", exec.ErrWaitDelay), exitedZero: true, want: true},
		{name: "non-zero exit stays a failure", err: exec.ErrWaitDelay, exitedZero: false, want: false},
		{name: "exit 0 with another error stays an error", err: errors.New("boom"), exitedZero: true, want: false},
		{name: "no error needs no rescue", err: nil, exitedZero: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := passedPastWaitDelay(tt.err, tt.exitedZero); got != tt.want {
				t.Fatalf("passedPastWaitDelay(%v, %v) = %v, want %v", tt.err, tt.exitedZero, got, tt.want)
			}
		})
	}
}

// TestStepOutcomeMapsWaitResults covers what runStep does with Wait's result,
// not only the predicate it calls: an exit-0 step held past WaitDelay passes, a
// cancelled context wins over the error, and any other error is an
// infrastructure error with no exit code. A real child holding the pipe, and
// the exit codes it returns, are TestIntegrationRunStepWaitDelay's.
func TestStepOutcomeMapsWaitResults(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	boom := errors.New("boom")
	signaled := &exec.ExitError{} // no exit code: the step died on a signal
	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		exitedZero bool
		wantCode   int
		wantErr    error
	}{
		{name: "clean exit", ctx: context.Background(), exitedZero: true},
		{name: "exit 0, pipe held past WaitDelay", ctx: context.Background(), err: exec.ErrWaitDelay, exitedZero: true},
		{name: "wrapped WaitDelay error", ctx: context.Background(), err: fmt.Errorf("wait: %w", exec.ErrWaitDelay), exitedZero: true},
		{name: "WaitDelay without an exit 0 stays an error", ctx: context.Background(), err: exec.ErrWaitDelay, wantCode: -1, wantErr: exec.ErrWaitDelay},
		{name: "another error is infrastructure", ctx: context.Background(), err: boom, exitedZero: true, wantCode: -1, wantErr: boom},
		{name: "an exit with no code is infrastructure", ctx: context.Background(), err: signaled, wantCode: -1, wantErr: signaled},
		{name: "a cancelled context is its error", ctx: cancelled, err: boom, wantCode: -1, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, err := stepOutcome(tt.ctx, tt.err, tt.exitedZero)
			if code != tt.wantCode || !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("stepOutcome = (%d, %v), want (%d, %v)", code, err, tt.wantCode, tt.wantErr)
			}
		})
	}
}

// TestRunStepCannotStart: a command that cannot start is an error with no exit
// code. No process runs, so this stays in the default suite; the exit codes of
// a real child are TestIntegrationRunStepWaitDelay's.
func TestRunStepCannotStart(t *testing.T) {
	t.Parallel()
	code, err := runStep(context.Background(), t.TempDir(), nil, []string{filepath.Join(t.TempDir(), "missing")}, io.Discard, time.Second)
	if err == nil || code != -1 {
		t.Fatalf("runStep = (%d, %v), want (-1, an error)", code, err)
	}
}
