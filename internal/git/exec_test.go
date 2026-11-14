package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
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

// TestExecCallExposesCallThroughItsReadMethods: a runner handed a call by a
// Git built with NewGitWithExec reads the directory, arguments, environment,
// unset keys, stdin and timeout the call holds, through the methods rather
// than the unexported fields.
func TestExecCallExposesCallThroughItsReadMethods(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const diff = "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+new\n"

	type seen struct {
		dir     string
		args    []string
		env     []string
		unset   []string
		stdin   string
		timeout time.Duration
	}
	var calls []seen
	exec := ExecFunc(func(c ExecCall) (string, string, error) {
		calls = append(calls, seen{
			dir:     c.Dir(),
			args:    c.Args(),
			env:     c.Env(),
			unset:   c.Unset(),
			stdin:   c.Stdin(),
			timeout: c.Timeout(),
		})
		switch strings.Join(c.Args(), " ") {
		case "diff base..head":
			return diff, "", nil
		case "patch-id --stable":
			return "abc123 1\n", "", nil
		}
		return "", "unscripted git call", gitExit(128)
	})

	g := NewGitWithExec(dir, exec).
		WithEnv([]string{"GT_EXEC_TEST=1"}).
		WithoutEnv("GIT_DIR").
		WithTimeout(3 * time.Second)
	id, err := g.PatchID("base", "head")
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if id != "abc123" {
		t.Errorf("PatchID = %q, want %q", id, "abc123")
	}

	want := []seen{
		{dir: dir, args: []string{"diff", "base..head"}, env: []string{"GT_EXEC_TEST=1"}, unset: []string{"GIT_DIR"}, stdin: "", timeout: 3 * time.Second},
		{dir: dir, args: []string{"patch-id", "--stable"}, env: []string{"GT_EXEC_TEST=1"}, unset: []string{"GIT_DIR"}, stdin: diff, timeout: 3 * time.Second},
	}
	if len(calls) != len(want) {
		t.Fatalf("runner saw %d calls, want %d: %+v", len(calls), len(want), calls)
	}
	for i, w := range want {
		got := calls[i]
		if got.dir != w.dir {
			t.Errorf("call %d Dir = %q, want %q", i, got.dir, w.dir)
		}
		if !reflect.DeepEqual(got.args, w.args) {
			t.Errorf("call %d Args = %q, want %q", i, got.args, w.args)
		}
		if !reflect.DeepEqual(got.env, w.env) {
			t.Errorf("call %d Env = %q, want %q", i, got.env, w.env)
		}
		if !reflect.DeepEqual(got.unset, w.unset) {
			t.Errorf("call %d Unset = %q, want %q", i, got.unset, w.unset)
		}
		if got.stdin != w.stdin {
			t.Errorf("call %d Stdin = %q, want %q", i, got.stdin, w.stdin)
		}
		if got.timeout != w.timeout {
			t.Errorf("call %d Timeout = %v, want %v", i, got.timeout, w.timeout)
		}
	}
}

// TestExecCallReadMethodsCopyTheirSlices: writing to the slice Args, Env or
// Unset returns changes only that slice, never the call it was read from, so
// a runner can adjust what it holds without altering the invocation.
func TestExecCallReadMethodsCopyTheirSlices(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		read  func(ExecCall) []string
		field func(ExecCall) []string
	}{
		"Args":  {func(c ExecCall) []string { return c.Args() }, func(c ExecCall) []string { return c.args }},
		"Env":   {func(c ExecCall) []string { return c.Env() }, func(c ExecCall) []string { return c.env }},
		"Unset": {func(c ExecCall) []string { return c.Unset() }, func(c ExecCall) []string { return c.unset }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := ExecCall{
				args:  []string{"rev-parse", "HEAD"},
				env:   []string{"A=1", "B=2"},
				unset: []string{"GIT_DIR"},
			}
			read := tc.read(c)
			read[0] = "changed"
			if got := tc.field(c)[0]; got == "changed" {
				t.Errorf("%s returned the call's own slice: writing to it changed the call's first entry", name)
			}
		})
	}
}

// TestExecCallReadMethodsOnEmptyCall: a call with no directory, arguments,
// environment, unset keys, stdin or timeout reads as the zero value from every
// method.
func TestExecCallReadMethodsOnEmptyCall(t *testing.T) {
	t.Parallel()
	var c ExecCall
	if got := c.Dir(); got != "" {
		t.Errorf("Dir = %q, want %q", got, "")
	}
	if got := c.Args(); len(got) != 0 {
		t.Errorf("Args = %q, want none", got)
	}
	if got := c.Env(); len(got) != 0 {
		t.Errorf("Env = %q, want none", got)
	}
	if got := c.Unset(); len(got) != 0 {
		t.Errorf("Unset = %q, want none", got)
	}
	if got := c.Stdin(); got != "" {
		t.Errorf("Stdin = %q, want %q", got, "")
	}
	if got := c.Timeout(); got != 0 {
		t.Errorf("Timeout = %v, want 0", got)
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
