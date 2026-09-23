package beads

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestCommandSetsDirAndDetachedProcessGroup(t *testing.T) {
	cmd := Command("/tmp", "", MutationRouting, "show", "gt-1")
	if cmd.Dir != "/tmp" {
		t.Fatalf("Dir = %q, want /tmp", cmd.Dir)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set; expected a detached process group")
	}
	if len(cmd.Args) < 1 || cmd.Args[0] != "bd" {
		t.Fatalf("Args[0] = %v, want bd", cmd.Args)
	}
}

func TestCommandContextAppliesSameConfiguration(t *testing.T) {
	cmd := CommandContext(context.Background(), "/tmp", "", ReadOnlyRouting, "list")
	if cmd.Dir != "/tmp" {
		t.Fatalf("Dir = %q, want /tmp", cmd.Dir)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set; expected a detached process group")
	}
}

func TestCommandWithEnvPreservesCallerEnv(t *testing.T) {
	env := append(append([]string{}, os.Environ()...), "GT_TEST_MARKER=1")
	cmd := CommandWithEnv("/tmp", env, "show", "gt-1")
	if cmd.Dir != "/tmp" {
		t.Fatalf("Dir = %q, want /tmp", cmd.Dir)
	}
	if len(cmd.Env) != len(env) {
		t.Fatalf("Env len = %d, want %d (env must pass through unmodified)", len(cmd.Env), len(env))
	}
	found := false
	for _, e := range cmd.Env {
		if e == "GT_TEST_MARKER=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("caller-supplied env var not present on the built command")
	}
	if cmd.SysProcAttr != nil {
		t.Fatal("CommandWithEnv must not impose a process-group policy; callers set their own")
	}
}

func TestCommandContextWithEnvPreservesCallerEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "GT_TEST_MARKER=2"}
	cmd := CommandContextWithEnv(context.Background(), "/work", env, "list", "--json")
	if cmd.Dir != "/work" {
		t.Fatalf("Dir = %q, want /work", cmd.Dir)
	}
	if len(cmd.Env) != len(env) {
		t.Fatalf("Env len = %d, want %d", len(cmd.Env), len(env))
	}
	if cmd.SysProcAttr != nil {
		t.Fatal("CommandContextWithEnv must not impose a process-group policy; callers set their own")
	}
}

func TestCommandWithPathUsesGivenArgv0(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	cmd := CommandWithPath("/opt/bin/bd", "/work", env, "show", "gt-1")
	if len(cmd.Args) == 0 || cmd.Args[0] != "/opt/bin/bd" {
		t.Fatalf("Args[0] = %v, want /opt/bin/bd", cmd.Args)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("Dir = %q, want /work", cmd.Dir)
	}
	if len(cmd.Env) != len(env) {
		t.Fatalf("Env len = %d, want %d", len(cmd.Env), len(env))
	}
}

func TestCommandContextWithBinUsesGivenArgv0AndAppliesPolicy(t *testing.T) {
	cmd := CommandContextWithBin(context.Background(), "/opt/bin/bd", "/work", "/work/.beads", MutationRouting, "list")
	if len(cmd.Args) == 0 || cmd.Args[0] != "/opt/bin/bd" {
		t.Fatalf("Args[0] = %v, want /opt/bin/bd", cmd.Args)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("Dir = %q, want /work", cmd.Dir)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set; expected a detached process group")
	}
}

// TestNilEnvCarriesPWD pins the contract call sites rely on: bd locates its
// database through PWD, and os/exec supplies it only while Env is nil. (gt-sz0s)
func TestNilEnvCarriesPWD(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *exec.Cmd
	}{
		{"CommandWithEnv", CommandWithEnv("/work", nil, "list")},
		{"CommandContextWithEnv", CommandContextWithEnv(context.Background(), "/work", nil, "list")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !hasEnvEntry(tc.cmd.Environ(), "PWD=/work") {
				t.Fatalf("Environ() = %v, want a PWD=/work entry", tc.cmd.Environ())
			}
		})
	}
}

// TestExplicitEnvLeavesPWDtoCaller states the other half of the contract, so a
// call site passing os.Environ() can see it is handing bd the parent's PWD.
func TestExplicitEnvLeavesPWDtoCaller(t *testing.T) {
	cmd := CommandWithEnv("/work", []string{"PATH=/usr/bin"}, "list")
	if hasEnvEntry(cmd.Environ(), "PWD=/work") {
		t.Fatalf("Environ() = %v, want no injected PWD", cmd.Environ())
	}
}

func hasEnvEntry(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestContextConstructorsAreContextBound pins which constructors apply a
// caller's context. The policy test cannot see this, and losing it silently
// removes a call site's timeout: a bd subprocess that used to be killed by
// cmdTimeout then runs unbounded. (gt-sz0s)
func TestContextConstructorsAreContextBound(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		cmd  *exec.Cmd
		want bool
	}{
		{"Command", Command("/work", "", MutationRouting, "list"), false},
		{"CommandContext", CommandContext(ctx, "/work", "", MutationRouting, "list"), true},
		{"CommandContextWithBin", CommandContextWithBin(ctx, "/opt/bin/bd", "/work", "", MutationRouting, "list"), true},
		{"CommandWithEnv", CommandWithEnv("/work", nil, "list"), false},
		{"CommandContextWithEnv", CommandContextWithEnv(ctx, "/work", nil, "list"), true},
		{"CommandWithPath", CommandWithPath("/opt/bin/bd", "/work", nil, "list"), false},
		{"CommandContextWithPath", CommandContextWithPath(ctx, "/opt/bin/bd", "/work", nil, "list"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cmd.Cancel != nil; got != tt.want {
				t.Fatalf("context-bound = %v, want %v", got, tt.want)
			}
		})
	}
}
