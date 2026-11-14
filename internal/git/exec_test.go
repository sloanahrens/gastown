package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestNewGitWithExecRunsMethodsThroughItsRunner: a Git built by NewGitWithExec
// sends its git calls through the caller's ExecFunc, which sees each call's
// arguments and working directory.
func TestNewGitWithExecRunsMethodsThroughItsRunner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var calls []ExecCall
	exec := ExecFunc(func(c ExecCall) (string, string, error) {
		calls = append(calls, c)
		switch strings.Join(c.args, " ") {
		case "rev-parse HEAD":
			return "abc123\n", "", nil
		case "status --porcelain -uall":
			return "", "", nil
		}
		return "", "unscripted git call", gitExit(128)
	})

	g := NewGitWithExec(dir, exec)

	rev, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}
	if rev != "abc123" {
		t.Errorf("Rev = %q, want %q", rev, "abc123")
	}
	status, err := g.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Clean {
		t.Errorf("Status reports %+v, want a clean tree", status)
	}

	want := []string{"rev-parse HEAD", "status --porcelain -uall"}
	if len(calls) != len(want) {
		t.Fatalf("runner saw %d calls, want %d: %v", len(calls), len(want), calls)
	}
	for i, args := range want {
		if got := strings.Join(calls[i].args, " "); got != args {
			t.Errorf("call %d args = %q, want %q", i, got, args)
		}
		if calls[i].dir != dir {
			t.Errorf("call %d dir = %q, want %q", i, calls[i].dir, dir)
		}
	}
}

// TestNewGitWithExecKeepsExitCode: an error the runner returns comes back from
// the Git method with its exit code and stderr intact.
func TestNewGitWithExecKeepsExitCode(t *testing.T) {
	t.Parallel()
	g := NewGitWithExec(t.TempDir(), func(ExecCall) (string, string, error) {
		return "", "fatal: bad revision 'missing'", gitExit(5)
	})

	_, err := g.Rev("missing")
	if err == nil {
		t.Fatal("Rev succeeded with a runner that failed; want the runner's error")
	}
	if got := exitCode(err); got != 5 {
		t.Errorf("exitCode(err) = %d, want 5", got)
	}
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("Rev error %v is not a *GitError", err)
	}
	if !strings.Contains(ge.Stderr, "fatal: bad revision") {
		t.Errorf("GitError.Stderr = %q, want the runner's stderr", ge.Stderr)
	}
}

// TestExecTypesAreAliasesOfTheUnexportedTypes: ExecFunc and ExecCall are
// aliases, not separate types, so either name is the same type the package's
// own code uses and needs no conversion.
func TestExecTypesAreAliasesOfTheUnexportedTypes(t *testing.T) {
	t.Parallel()
	// These assignments compile only while the exported names are aliases: a
	// defined type with the same signature would need a conversion.
	var (
		_ runFunc  = ExecFunc(nil)
		_ ExecFunc = runFunc(nil)
		_ gitCall  = ExecCall{}
		_ ExecCall = gitCall{}
	)

	var exec ExecFunc = func(c ExecCall) (string, string, error) {
		return c.args[0] + " answered\n", "", nil
	}
	var g Git
	g.exec = exec // no conversion: an ExecFunc is a runFunc
	if g.exec == nil {
		t.Fatal("assigning an ExecFunc to the exec field left it nil")
	}

	out, err := g.run("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "rev-parse answered" {
		t.Errorf("run = %q, want %q", out, "rev-parse answered")
	}
}

// TestNewGitLeavesExecNil: the existing constructors leave the exec function
// nil, so they still select the real git on PATH. No git process runs here.
func TestNewGitLeavesExecNil(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := map[string]*Git{
		//testpolicy:allow no-git — this test reads the constructor's exec field and runs no git; WithoutGit fails the run if it starts one
		"NewGit": NewGit(dir),
		//testpolicy:allow no-git — same, for the explicit git-dir constructor
		"NewGitWithDir":                    NewGitWithDir("", dir),
		"NewGitWithExec with a nil runner": NewGitWithExec(dir, nil),
	}
	for name, g := range cases {
		if g.exec != nil {
			t.Errorf("%s set an exec function; a caller's runner must be opt-in", name)
		}
		if g.runner() == nil {
			t.Fatalf("%s selects no runner", name)
		}
		if got, want := reflect.ValueOf(g.runner()).Pointer(), reflect.ValueOf(realRun).Pointer(); got != want {
			t.Errorf("%s does not select the real runner for a nil exec function", name)
		}
	}
}
