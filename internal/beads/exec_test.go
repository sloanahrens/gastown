package beads

import (
	"context"
	"os"
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
	if cmd.Path != "/opt/bin/bd" && (len(cmd.Args) == 0 || cmd.Args[0] != "/opt/bin/bd") {
		t.Fatalf("argv0 = %q/%v, want /opt/bin/bd", cmd.Path, cmd.Args)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("Dir = %q, want /work", cmd.Dir)
	}
	if len(cmd.Env) != len(env) {
		t.Fatalf("Env len = %d, want %d", len(cmd.Env), len(env))
	}
}

func TestCommandContextWithPathUsesGivenArgv0(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	cmd := CommandContextWithPath(context.Background(), "/opt/bin/bd", "/work", env, "list")
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
