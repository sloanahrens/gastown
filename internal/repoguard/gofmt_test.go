package repoguard

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// fakeExec records each command and answers it from run. A nil run prints
// nothing and succeeds.
type fakeExec struct {
	calls []execCall
	run   func(args []string) (string, error)
}

type execCall struct {
	dir  string
	name string
	args []string
}

func (f *fakeExec) Run(dir string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, execCall{dir: dir, name: name, args: args})
	if f.run == nil {
		return "", nil
	}
	return f.run(args)
}

// fakeGofmt stands in for gofmt -s -l. It lists each argument in needsFormat,
// and each argument in needsSimplify only when -s is among its flags, which is
// how the simplify pass differs from a plain gofmt -l.
func fakeGofmt(needsFormat, needsSimplify []string) func(args []string) (string, error) {
	return func(args []string) (string, error) {
		simplify := slices.Contains(args, "-s")
		var out strings.Builder
		for _, a := range args {
			if slices.Contains(needsFormat, a) || (simplify && slices.Contains(needsSimplify, a)) {
				out.WriteString(a + "\n")
			}
		}
		return out.String(), nil
	}
}

const testGofmtPath = "/usr/bin/gofmt"

func lookPathFound(string) (string, error) { return testGofmtPath, nil }

func lookPathMissing(string) (string, error) { return "", errors.New("not found") }

// TestCheckGofmtTable runs the guard over fake git and gofmt. Every case also
// checks the invocation: gofmt runs in root with -s -l first, and no file in
// unchecked is ever handed to it. The listing of tracked .go files is checked
// too: one NUL-separated call for *.go, in root.
func TestCheckGofmtTable(t *testing.T) {
	t.Parallel()
	const header = "repo-guards: these files are not gofmt -s clean; run 'gofmt -s -w' on them:"
	tests := []struct {
		name          string
		files         []string
		deleted       []string
		needsFormat   []string
		needsSimplify []string
		unchecked     []string
		want          []string
	}{
		{
			name:      "tracked file deleted from the worktree is not checked",
			files:     []string{"main.go", "gone.go"},
			deleted:   []string{"gone.go"},
			unchecked: []string{"gone.go"},
		},
		{
			name:  "clean files pass",
			files: []string{"main.go", "pkg/a.go"},
		},
		{
			name:        "unformatted tracked file is reported",
			files:       []string{"main.go", "bad.go"},
			needsFormat: []string{"bad.go"},
			want:        []string{"bad.go"},
		},
		{
			name:          "file only -s would simplify is reported",
			files:         []string{"simple.go"},
			needsSimplify: []string{"simple.go"},
			want:          []string{"simple.go"},
		},
		{
			name:        "testdata fixtures are never passed to gofmt",
			files:       []string{"main.go", "testdata/fixture.go", "a/testdata/b.go"},
			needsFormat: []string{"testdata/fixture.go", "a/testdata/b.go"},
			unchecked:   []string{"testdata/fixture.go", "a/testdata/b.go"},
		},
		{
			name:        "only testdata files means nothing to check",
			files:       []string{"testdata/fixture.go"},
			needsFormat: []string{"testdata/fixture.go"},
			unchecked:   []string{"testdata/fixture.go"},
		},
		{
			name:        "untracked unformatted file is never listed",
			files:       []string{"main.go"},
			needsFormat: []string{"untracked.go"},
			unchecked:   []string{"untracked.go"},
		},
		{
			name: "no tracked go files is clean",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const root = "/repo"
			g := &fakeGit{files: tc.files}
			g.deleted = tc.deleted
			e := &fakeExec{run: fakeGofmt(tc.needsFormat, tc.needsSimplify)}
			got, problem := CheckGofmt(g, e, lookPathFound, root)
			if !slices.Equal(got, tc.want) {
				t.Errorf("unformatted = %q, want %q", got, tc.want)
			}
			if len(g.lsCalls) != 1 || g.lsCalls[0].root != root || !g.lsCalls[0].nul || !slices.Equal(g.lsCalls[0].pathspec, []string{"*.go"}) {
				t.Errorf("ls-files calls = %+v, want one NUL-separated listing of *.go in %s", g.lsCalls, root)
			}
			wantProblem := ""
			if len(tc.want) > 0 {
				wantProblem = header
			}
			if problem != wantProblem {
				t.Errorf("problem = %q, want %q", problem, wantProblem)
			}
			for _, c := range e.calls {
				if c.dir != root || c.name != testGofmtPath {
					t.Errorf("gofmt ran as %q in %q, want %q in %q", c.name, c.dir, testGofmtPath, root)
				}
				if len(c.args) < 2 || !slices.Equal(c.args[:2], []string{"-s", "-l"}) {
					t.Errorf("gofmt flags = %q, want them to start with -s -l", c.args)
				}
				for _, u := range tc.unchecked {
					if slices.Contains(c.args, u) {
						t.Errorf("gofmt was given %q, which must never be checked", u)
					}
				}
			}
		})
	}
}

// TestCheckGofmtProblems pins the guard's own failures to their exact messages,
// so a broken toolchain or a failed listing cannot pass as clean.
func TestCheckGofmtProblems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		lookPath    func(string) (string, error)
		files       []string
		lsErr       error
		deletedErr  error
		run         func(args []string) (string, error)
		wantProblem string
	}{
		{
			name:        "listing deleted go files fails",
			lookPath:    lookPathFound,
			files:       []string{"main.go"},
			deletedErr:  errors.New("not a git repository"),
			wantProblem: "repo-guards: cannot list the tracked .go files",
		},
		{
			name:        "gofmt missing from PATH",
			lookPath:    lookPathMissing,
			files:       []string{"main.go"},
			wantProblem: "repo-guards: gofmt is not on PATH; make lint needs the Go toolchain",
		},
		{
			name:        "gofmt missing from PATH even with no go files",
			lookPath:    lookPathMissing,
			wantProblem: "repo-guards: gofmt is not on PATH; make lint needs the Go toolchain",
		},
		{
			name:        "gofmt exits non-zero",
			lookPath:    lookPathFound,
			files:       []string{"main.go"},
			run:         func([]string) (string, error) { return "", errors.New("exit status 2") },
			wantProblem: "repo-guards: gofmt -s failed",
		},
		{
			name:        "listing tracked go files fails",
			lookPath:    lookPathFound,
			lsErr:       errors.New("not a git repository"),
			wantProblem: "repo-guards: cannot list the tracked .go files",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{files: tc.files, lsErr: tc.lsErr}
			g.delErr = tc.deletedErr
			e := &fakeExec{run: tc.run}
			got, problem := CheckGofmt(g, e, tc.lookPath, "/repo")
			if got != nil {
				t.Errorf("unformatted = %q, want none", got)
			}
			if problem != tc.wantProblem {
				t.Errorf("problem = %q, want %q", problem, tc.wantProblem)
			}
		})
	}
}

// TestCLIExecRunReturnsStdoutAndError drives the production runner with start
// replaced, so no process starts. The command must run in dir with its name and
// arguments, its stdout comes back, and a failure from start is returned.
func TestCLIExecRunReturnsStdoutAndError(t *testing.T) {
	t.Parallel()
	failure := errors.New("exit status 2")
	tests := []struct {
		name     string
		startErr error
	}{
		{name: "success returns stdout", startErr: nil},
		{name: "failure returns stdout and the error", startErr: failure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got *exec.Cmd
			e := CLIExec{start: func(cmd *exec.Cmd) error {
				got = cmd
				_, _ = cmd.Stdout.Write([]byte("main.go\n"))
				return tc.startErr
			}}
			out, err := e.Run("/repo", "/fake/gofmt", "-s", "-l", "main.go")
			if !errors.Is(err, tc.startErr) {
				t.Errorf("err = %v, want %v", err, tc.startErr)
			}
			if out != "main.go\n" {
				t.Errorf("stdout = %q, want %q", out, "main.go\n")
			}
			if got.Dir != "/repo" {
				t.Errorf("dir = %q, want %q", got.Dir, "/repo")
			}
			if want := []string{"/fake/gofmt", "-s", "-l", "main.go"}; !slices.Equal(got.Args, want) {
				t.Errorf("argv = %q, want %q", got.Args, want)
			}
		})
	}
}

// TestGuardEnvironDropsHookVariables gives the builder an environment a git hook
// would leave. The three repository variables go, matched by exact name; a near
// miss such as GIT_DIRECTORY stays, and so does everything else, PATH included.
func TestGuardEnvironDropsHookVariables(t *testing.T) {
	t.Parallel()
	environ := []string{
		"PATH=/usr/bin",
		"HOME=/home/slit",
		"GIT_DIR=/hook/.git",
		"GIT_WORK_TREE=/hook",
		"GIT_INDEX_FILE=/hook/index",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_DIRECTORY=/keep",
	}
	want := []string{"PATH=/usr/bin", "HOME=/home/slit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_DIRECTORY=/keep"}
	if got := GuardEnviron(environ); !slices.Equal(got, want) {
		t.Errorf("GuardEnviron() = %q, want %q", got, want)
	}
}

// TestCLIExecRunDropsHookVariables holds the production runner to GuardEnviron:
// a command the guards start gets an environment of its own without the hook
// variables, even when the process itself carries them. It sets the process
// environment, so it is not parallel.
func TestCLIExecRunDropsHookVariables(t *testing.T) {
	t.Setenv("GIT_DIR", "/hook/.git")
	t.Setenv("GIT_INDEX_FILE", "/hook/index")
	var got *exec.Cmd
	e := CLIExec{start: func(cmd *exec.Cmd) error {
		got = cmd
		return nil
	}}
	if _, err := e.Run("/repo", "git", "ls-files"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got.Env == nil {
		t.Fatal("Env = nil, so the command inherits the hook variables")
	}
	for _, kv := range got.Env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(hookVariables, name) {
			t.Errorf("command environment carries %s", name)
		}
	}
}

// TestCheckGofmtBatchesLongLists lists 5000 tracked files, enough that one
// call would overrun argv. Every call must stay under 100000 bytes, and the
// reported files must come back in order.
func TestCheckGofmtBatchesLongLists(t *testing.T) {
	t.Parallel()
	const n = 5000
	const argvLimit = 100000
	files := make([]string, n)
	var flagged []string
	for i := range files {
		files[i] = fmt.Sprintf("internal/pkg%04d/file%04d.go", i/50, i)
		if i%500 == 0 {
			flagged = append(flagged, files[i])
		}
	}
	g := &fakeGit{files: files}
	e := &fakeExec{run: fakeGofmt(flagged, nil)}
	got, problem := CheckGofmt(g, e, lookPathFound, "/repo")
	if problem == "" {
		t.Fatalf("problem = empty, want the unformatted header")
	}
	if len(e.calls) < 2 {
		t.Fatalf("gofmt calls = %d, want the list split into several", len(e.calls))
	}
	var passed []string
	for _, c := range e.calls {
		size := len(c.name) + 1
		for _, a := range c.args {
			size += len(a) + 1
		}
		if size >= argvLimit {
			t.Errorf("one gofmt call has %d argv bytes, want under %d", size, argvLimit)
		}
		passed = append(passed, c.args[2:]...)
	}
	if !slices.Equal(passed, files) {
		t.Errorf("files passed to gofmt across calls differ from the tracked list (got %d, want %d)", len(passed), len(files))
	}
	if !slices.Equal(got, flagged) {
		t.Errorf("unformatted = %q, want %q", got, flagged)
	}
}
