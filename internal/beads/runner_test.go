package beads

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDefaultRunnerIsRealBD is the wiring guard for the bd seam: a Beads
// built through the exported constructors, with no runner injected, must run
// the real bd. Everything else in this file injects a recorder, so without
// this test a constructor that dropped the default would go unnoticed until
// a production command silently did nothing.
func TestDefaultRunnerIsRealBD(t *testing.T) {
	t.Parallel()
	real := reflect.ValueOf(runBDProcess).Pointer()
	for name, b := range map[string]*Beads{
		"New":                 New(t.TempDir()),
		"NewIsolated":         NewIsolated(t.TempDir()),
		"NewIsolatedWithPort": NewIsolatedWithPort(t.TempDir(), 1),
		"NewWithBeadsDir":     NewWithBeadsDir(t.TempDir(), t.TempDir()),
		"NewRigLocal":         NewRigLocal(t.TempDir()),
		"NewPlain":            NewPlain(t.TempDir(), nil),
	} {
		if got := reflect.ValueOf(b.runner()).Pointer(); got != real {
			t.Errorf("%s: default runner is not runBDProcess", name)
		}
	}

	// runBDProcess's policy branch builds its command with newBDCmd; pin
	// what that command is without starting it.
	var stdout, stderr bytes.Buffer
	cmd := newBDCmd(context.Background(), "/work", []string{"A=1"}, []byte("in"), []string{"show", "x"}, &stdout, &stderr)
	if filepath.Base(cmd.Path) != "bd" && filepath.Base(cmd.Args[0]) != "bd" {
		t.Errorf("newBDCmd runs %q, want bd", cmd.Path)
	}
	if !reflect.DeepEqual(cmd.Args[1:], []string{"show", "x"}) || cmd.Dir != "/work" {
		t.Errorf("newBDCmd args/dir = %v %q", cmd.Args, cmd.Dir)
	}
	if len(cmd.Env) == 0 || cmd.Env[0] != "A=1" || cmd.Stdin == nil || cmd.Stdout != &stdout || cmd.Stderr != &stderr {
		t.Errorf("newBDCmd env/stdio not wired: env=%v", cmd.Env)
	}
}

// TestDerivedWrappersKeepRunner checks that the wrappers a Beads hands out
// for routing (per-ID targets, agent scope, pinned databases) keep its bd
// seam, so no call escapes to the real bd.
func TestDerivedWrappersKeepRunner(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newBeads(beadsFields{workDir: t.TempDir(), isolated: true, exec: r.exec})
	want := reflect.ValueOf(r.exec).Pointer()
	for name, d := range map[string]*Beads{
		"ForAgentBead":     b.ForAgentBead(),
		"pinnedToBeadsDir": b.pinnedToBeadsDir(t.TempDir()),
	} {
		if d.exec == nil || reflect.ValueOf(d.exec).Pointer() != want {
			t.Errorf("%s dropped the runner", name)
		}
	}
	p := NewPlain(t.TempDir(), nil)
	p.exec = r.exec
	if reflect.ValueOf(p.WithTimeout(1).exec).Pointer() != want {
		t.Error("WithTimeout dropped the runner")
	}
}

func TestShowSendsPinnedShow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newRecorder(func([]string) reply { return reply{stdout: `[{"id":"gt-1","title":"t","status":"open"}]`} })
	b := newRecordedBeads(dir, r)
	issue, err := b.Show("gt-1")
	if err != nil || issue.ID != "gt-1" {
		t.Fatalf("Show = %+v, %v", issue, err)
	}
	calls := r.calls()
	if len(calls) != 1 || strings.Join(calls[0].args, " ") != "show gt-1 --json" {
		t.Fatalf("calls = %v", r.argvs())
	}
	if got, _ := lastEnvValue(calls[0].env, "BEADS_DIR"); got != filepath.Join(dir, ".beads") {
		t.Errorf("BEADS_DIR = %q, want %q", got, filepath.Join(dir, ".beads"))
	}
	if calls[0].dir != dir || calls[0].plain {
		t.Errorf("dir/plain = %q/%v", calls[0].dir, calls[0].plain)
	}
}

func TestAllowStaleProbeGoesThroughRunner(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{true, false} {
		r := newRecorder(func([]string) reply { return reply{stdout: "[]"} })
		r.allowStale = supported
		b := newRecordedBeads(t.TempDir(), r)
		if _, err := b.List(ListOptions{Priority: -1}); err != nil {
			t.Fatal(err)
		}
		got := r.argvs()
		want := "list --json --limit=0 --flat"
		if supported {
			want = "--allow-stale " + want
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("supported=%v: calls = %q, want %q", supported, got, want)
		}
		if r.probes != 1 {
			t.Errorf("supported=%v: probes = %d, want 1", supported, r.probes)
		}
	}
}

func TestUpdateDescriptionGoesOnStdin(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)
	desc := "line one\nline two"
	status := "in_progress"
	if err := b.Update("gt-1", UpdateOptions{Description: &desc, Status: &status, AddLabels: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v", r.argvs())
	}
	if got := strings.Join(calls[0].args, " "); got != "update gt-1 --status=in_progress --body-file=- --add-label=a" {
		t.Errorf("argv = %q", got)
	}
	if string(calls[0].stdin) != desc {
		t.Errorf("stdin = %q, want %q", calls[0].stdin, desc)
	}
}

func TestWrapErrorMapsNotFoundAndGuard(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if args[0] == "show" {
			return reply{stderr: "Error: no issue found matching \"gt-x\"", err: exitError{1}}
		}
		return reply{stderr: "guard not held", err: exitError{bdGuardNotHeldExit}}
	})
	b := newRecordedBeads(t.TempDir(), r)
	if _, err := b.Show("gt-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Show missing = %v, want ErrNotFound", err)
	}
	status := "open"
	if err := b.Update("gt-x", UpdateOptions{Status: &status}); !errors.Is(err, ErrGuardNotHeld) {
		t.Errorf("guarded update = %v, want ErrGuardNotHeld", err)
	}
}

func TestPlainRunsExactlyWhatItIsGiven(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply { return reply{stdout: "value\n"} })
	r.allowStale = true
	dir := t.TempDir()
	env := []string{"ONLY=this"}
	b := NewPlain(dir, env)
	b.exec = r.exec
	if _, err := b.run("config", "get", "k"); err != nil {
		t.Fatal(err)
	}
	calls := r.calls()
	if len(calls) != 1 || !calls[0].plain || calls[0].dir != dir || !reflect.DeepEqual(calls[0].env, env) {
		t.Fatalf("plain call = %+v", calls)
	}
	if got := strings.Join(calls[0].args, " "); got != "config get k" {
		t.Errorf("argv = %q, want no --allow-stale and nothing added", got)
	}
	if r.probes != 0 {
		t.Errorf("plain wrapper probed --allow-stale %d times", r.probes)
	}
}

func TestPlainErrorKeepsOutput(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stdout: "partial\n", stderr: "Error: issue gt-9 not found\n", err: exitError{1}}
	})
	b := NewPlain(t.TempDir(), nil)
	b.exec = r.exec
	_, err := b.run("show", "gt-9", "--json")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %T %v, want *CLIError", err, err)
	}
	if cliErr.Output() != "partial\nError: issue gt-9 not found" {
		t.Errorf("Output = %q", cliErr.Output())
	}
	if !errors.Is(err, ErrNotFound) {
		t.Error("not-found stderr does not unwrap to ErrNotFound")
	}
	var code interface{ ExitCode() int }
	if !errors.As(err, &code) || code.ExitCode() != 1 {
		t.Error("exit status lost")
	}
}

// TestReleaseOverridesTheDeadHoldersClaim pins that Release passes --force.
// Release exists to recover a step whose worker died, so the claim it clears
// is by definition another actor's, and bd 1.2 refuses to reassign another
// actor's in_progress claim without --force ("cannot reassign X: held by
// ..."): without it `gt release` failed on exactly the issues it is for.
// TestIntegrationClientContract/release pins the same against real bd.
func TestReleaseOverridesTheDeadHoldersClaim(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)
	if err := b.ReleaseWithReason("gt-1", "worker died"); err != nil {
		t.Fatal(err)
	}
	if err := b.Release("gt-2"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"update gt-1 --status=open --assignee= --force --notes=Released: worker died",
		"update gt-2 --status=open --assignee= --force",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q\nwant    %q", got, want)
	}
}

// TestNewPlainBDCmdWiring pins the plain call's process wiring, which every
// NewPlain wrapper (doctor's bd calls, and the CommandWithEnv sites moving
// onto it) depends on: bd, the call's argv, its directory, its environment
// exactly (nil stays nil, so the child inherits), and its stdin.
func TestNewPlainBDCmdWiring(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := []string{"BEADS_DIR=/rig/.beads", "ONLY=this"}
	cmd := newPlainBDCmd(context.Background(), bdCall{dir: "/rig", env: env, args: []string{"config", "get", "k"}, stdin: []byte("in"), plain: true}, &stdout, &stderr)
	if filepath.Base(cmd.Args[0]) != "bd" || !reflect.DeepEqual(cmd.Args[1:], []string{"config", "get", "k"}) {
		t.Errorf("args = %q, want bd config get k", cmd.Args)
	}
	if cmd.Dir != "/rig" {
		t.Errorf("Dir = %q, want /rig", cmd.Dir)
	}
	if !reflect.DeepEqual(cmd.Env, env) {
		t.Errorf("Env = %q, want exactly %q", cmd.Env, env)
	}
	if cmd.Stdin == nil || cmd.Stdout != &stdout || cmd.Stderr != &stderr {
		t.Error("stdio not wired")
	}
	if cmd.SysProcAttr != nil {
		t.Error("plain calls stay in the caller's process group, as CommandWithEnv's did")
	}

	inherit := newPlainBDCmd(context.Background(), bdCall{dir: "/rig", args: []string{"stats"}, plain: true}, &stdout, &stderr)
	if inherit.Env != nil {
		t.Errorf("nil env became %q; it must stay nil so bd inherits", inherit.Env)
	}
	if inherit.Stdin != nil {
		t.Error("no stdin was given, but one is wired")
	}
}

// TestBDProcessChoosesPlainOnlyForPlainCalls pins the branch runBDProcess
// takes: a plain call (NewPlain, CommandWithEnv's replacement) gets exactly
// the caller's environment and process group, and every other call gets the
// routed build with its detached process group and OTEL variables. Swapping
// or collapsing the branch fails here.
func TestBDProcessChoosesPlainOnlyForPlainCalls(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := []string{"BEADS_DIR=/rig/.beads"}

	plain := newBDProcess(context.Background(), bdCall{dir: "/rig", env: env, args: []string{"stats"}, plain: true}, &stdout, &stderr)
	if !reflect.DeepEqual(plain.Env, env) || plain.SysProcAttr != nil {
		t.Errorf("plain call: Env = %q, SysProcAttr = %+v; want exactly %q in the caller's process group", plain.Env, plain.SysProcAttr, env)
	}

	routed := newBDProcess(context.Background(), bdCall{dir: "/rig", env: env, args: []string{"stats"}}, &stdout, &stderr)
	if routed.SysProcAttr == nil {
		t.Error("routed call: not in a detached process group, so it was built as a plain call")
	}
	if routed.Dir != "/rig" || len(routed.Env) < len(env) || !reflect.DeepEqual(routed.Env[:len(env)], env) {
		t.Errorf("routed call: Dir = %q, Env = %q; want /rig and the call's env first", routed.Dir, routed.Env)
	}
}

// TestReleaseFallsBackWithoutForce: deps.MinBeadsVersion still admits bd
// builds older than the claim fence, which may not know `update --force`.
// On such a bd, Release retries without it, as --flat already falls back,
// instead of failing every release.
func TestReleaseFallsBackWithoutForce(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		for _, a := range args {
			if a == "--force" {
				return reply{stderr: "Error: unknown flag: --force\n", err: exitError{1}}
			}
		}
		return reply{}
	})
	b := newRecordedBeads(t.TempDir(), r)
	if err := b.ReleaseWithReason("gt-1", "worker died"); err != nil {
		t.Fatalf("ReleaseWithReason on a bd without --force: %v", err)
	}
	want := []string{
		"update gt-1 --status=open --assignee= --force --notes=Released: worker died",
		"update gt-1 --status=open --assignee= --notes=Released: worker died",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q\nwant    %q", got, want)
	}

	// Any other failure is not retried.
	other := newRecorder(func([]string) reply { return reply{stderr: "Error: issue not found", err: exitError{1}} })
	if err := newRecordedBeads(t.TempDir(), other).Release("gt-2"); err == nil || len(other.calls()) != 1 {
		t.Errorf("Release on a missing issue = %v after %d calls, want one failing call", err, len(other.calls()))
	}
}

// TestNewWithBeadsDirAndRunner checks the exported seam: every bd call of the
// wrapper and of the wrappers derived from it reaches the injected runner
// with the argv, dir and env the policy path built, and a nil runner is the
// real bd.
func TestNewWithBeadsDirAndRunner(t *testing.T) {
	t.Parallel()
	if got := reflect.ValueOf(NewWithBeadsDirAndRunner(t.TempDir(), t.TempDir(), nil).runner()).Pointer(); got != reflect.ValueOf(runBDProcess).Pointer() {
		t.Error("a nil runner is not the real bd")
	}

	work, dir := t.TempDir(), t.TempDir()
	var calls []BDCall
	b := NewWithBeadsDirAndRunner(work, dir, func(_ context.Context, c BDCall) ([]byte, []byte, error) {
		calls = append(calls, c)
		if len(c.Args) > 0 && c.Args[0] == "show" {
			return []byte(`[{"id":"gt-x","title":"t","status":"open"}]`), nil, nil
		}
		return nil, nil, nil
	})
	is, err := b.ForAgentBead().Show("gt-x")
	if err != nil || is == nil || is.ID != "gt-x" {
		t.Fatalf("Show through the runner = %v, %v", is, err)
	}
	var show *BDCall
	for i := range calls {
		if calls[i].Args[0] == "show" {
			show = &calls[i]
		}
	}
	if show == nil {
		t.Fatalf("runner never saw the show call: %v", calls)
	}
	if show.Dir == "" || !containsEnvPrefix(show.Env, "BEADS_DIR=") {
		t.Errorf("show call lost its dir or BEADS_DIR: dir=%q env=%v", show.Dir, show.Env)
	}
}
