package main

import (
	"bytes"
	"errors"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/repoguard"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// The stderr lines are copied from the previous shell version. TestRunMessages
// holds the guards to them byte for byte, so a reworded message fails here.
const (
	replaceLine    = "replace example.com/y => ../y"
	replaceStderr  = "repo-guards: go.mod contains replace directives; they break go install ...@latest (upstream #2230)\n"
	issuesStderr   = "repo-guards: .beads/issues.jsonl is tracked; the Dolt server is the only beads backend (git rm --cached it)\n"
	gofmtStderr    = "repo-guards: these files are not gofmt -s clean; run 'gofmt -s -w' on them:\n"
	gofmtMissing   = "repo-guards: gofmt is not on PATH; make lint needs the Go toolchain\n"
	gofmtFailed    = "repo-guards: gofmt -s failed\n"
	gomodReadError = "repo-guards: cannot read go.mod; the replace guard did not run\n"
)

// fakeGit answers the Git seam from fixed data and records the root each
// question was asked about.
type fakeGit struct {
	tracked map[string]bool
	files   []string
	roots   []string
}

func (f *fakeGit) LsFiles(root string, nul bool, pathspec ...string) ([]string, error) {
	f.roots = append(f.roots, root)
	return f.files, nil
}

func (f *fakeGit) LsDeleted(root string, pathspec ...string) ([]string, error) {
	f.roots = append(f.roots, root)
	return nil, nil
}

func (f *fakeGit) IsTracked(root, path string) bool {
	f.roots = append(f.roots, root)
	return f.tracked[path]
}

// fakeExec answers the commands the guards run. toplevel is what git rev-parse
// --show-toplevel prints, prefix what --show-prefix prints, or revErr is what
// rev-parse fails with. gofmt lists the files in unformatted and fails with
// gofmtErr when set. calls records every command, so a test can see what ran.
type fakeExec struct {
	toplevel    string
	prefix      string
	prefixErr   error
	revErr      error
	unformatted []string
	gofmtErr    error
	calls       []string
}

func (f *fakeExec) Run(dir string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "git" && slices.Contains(args, "--show-prefix") {
		return f.prefix, f.prefixErr
	}
	if name == "git" && slices.Contains(args, "rev-parse") {
		return f.toplevel, f.revErr
	}
	// The guard runs gofmt by the path LookPath found, so match the base name.
	if path.Base(name) == "gofmt" {
		if f.gofmtErr != nil {
			return "", f.gofmtErr
		}
		return strings.Join(f.unformatted, "\n") + "\n", nil
	}
	return "", errors.New("unexpected command")
}

// fakeEnv is a repository with a go.mod that holds goMod, the given tracked
// files and gofmt answers, and a gofmt on PATH unless noGofmt.
func fakeEnv(goMod string, g *fakeGit, e *fakeExec, noGofmt bool) repoguard.Env {
	return repoguard.Env{
		Git:  g,
		Exec: e,
		LookPath: func(file string) (string, error) {
			if noGofmt {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + file, nil
		},
		ReadFile: func(name string) ([]byte, error) {
			if goMod == "" {
				return nil, errors.New("missing")
			}
			return []byte(goMod), nil
		},
	}
}

const cleanGoMod = "module example.com/x\n\ngo 1.26\n"

// TestRunRunsEveryGuard holds the run to all three guards. The failing case
// makes every guard fail; stopping after the first failure would drop the
// other two messages.
func TestRunRunsEveryGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		goMod       string
		tracked     map[string]bool
		unformatted []string
		wantExit    int
		wantStdout  string
		wantStderr  string
	}{
		{
			name:       "all clean exits 0 and prints nothing",
			goMod:      cleanGoMod,
			wantExit:   0,
			wantStdout: "",
			wantStderr: "",
		},
		{
			name:        "all three fail exit 1 with every message",
			goMod:       cleanGoMod + replaceLine + "\n",
			tracked:     map[string]bool{".beads/issues.jsonl": true},
			unformatted: []string{"a.go"},
			wantExit:    1,
			wantStdout:  replaceLine + "\n",
			wantStderr:  replaceStderr + issuesStderr + gofmtStderr + "a.go\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{tracked: tc.tracked, files: []string{"a.go"}}
			e := &fakeExec{toplevel: "/repo\n", unformatted: tc.unformatted}
			var stdout, stderr bytes.Buffer
			got := run([]string{"/repo"}, fakeEnv(tc.goMod, g, e, false), &stdout, &stderr)
			if got != tc.wantExit {
				t.Errorf("exit = %d, want %d", got, tc.wantExit)
			}
			if stdout.String() != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
			if stderr.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestRunMessages pins each guard's failing text byte for byte, copied from the
// previous shell version, and the offending go.mod lines on stdout.
func TestRunMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		goMod       string
		tracked     map[string]bool
		unformatted []string
		gofmtErr    error
		noGofmt     bool
		wantStdout  string
		wantStderr  string
	}{
		{
			name:       "replace directive",
			goMod:      cleanGoMod + replaceLine + "\n",
			wantStdout: replaceLine + "\n",
			wantStderr: replaceStderr,
		},
		{
			name:       "tracked issues.jsonl",
			goMod:      cleanGoMod,
			tracked:    map[string]bool{".beads/issues.jsonl": true},
			wantStderr: issuesStderr,
		},
		{
			name:        "unformatted tracked go file",
			goMod:       cleanGoMod,
			unformatted: []string{"b.go"},
			wantStderr:  gofmtStderr + "b.go\n",
		},
		{
			name:       "gofmt not on PATH",
			goMod:      cleanGoMod,
			noGofmt:    true,
			wantStderr: gofmtMissing,
		},
		{
			name:       "gofmt -s fails and counts as failed",
			goMod:      cleanGoMod,
			gofmtErr:   errors.New("exit 2"),
			wantStderr: gofmtFailed,
		},
		{
			name:       "go.mod cannot be read and counts as failed",
			goMod:      "",
			wantStderr: gomodReadError,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{tracked: tc.tracked, files: []string{"b.go"}}
			e := &fakeExec{toplevel: "/repo\n", unformatted: tc.unformatted, gofmtErr: tc.gofmtErr}
			var stdout, stderr bytes.Buffer
			if got := run([]string{"/repo"}, fakeEnv(tc.goMod, g, e, tc.noGofmt), &stdout, &stderr); got != 1 {
				t.Errorf("exit = %d, want 1", got)
			}
			if stdout.String() != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
			if stderr.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestMainDefaultRoot checks the root the guards run in: with no argument, the
// git top level of the working directory; with an argument that is not a git
// repository, exit 1 with a message.
func TestMainDefaultRoot(t *testing.T) {
	t.Parallel()
	t.Run("no argument uses the git top level", func(t *testing.T) {
		t.Parallel()
		g := &fakeGit{}
		e := &fakeExec{toplevel: "/sandbox/repo\n"}
		var stdout, stderr bytes.Buffer
		if got := run(nil, fakeEnv(cleanGoMod, g, e, false), &stdout, &stderr); got != 0 {
			t.Fatalf("exit = %d, want 0; stderr %q", got, stderr.String())
		}
		if len(g.roots) == 0 {
			t.Fatal("no guard ran")
		}
		for _, root := range g.roots {
			if root != "/sandbox/repo" {
				t.Errorf("guard ran in %q, want the git top level /sandbox/repo", root)
			}
		}
	})
	t.Run("bad root exits 1 with a message", func(t *testing.T) {
		t.Parallel()
		g := &fakeGit{}
		e := &fakeExec{revErr: errors.New("exit 128")}
		var stdout, stderr bytes.Buffer
		if got := run([]string{"/nope"}, fakeEnv(cleanGoMod, g, e, false), &stdout, &stderr); got != 1 {
			t.Fatalf("exit = %d, want 1", got)
		}
		want := "repo-guards: /nope is not a git repository\n"
		if stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
		if len(g.roots) != 0 {
			t.Errorf("guards ran in a bad root: %q", g.roots)
		}
	})
}

// TestRunRootMustBeTopLevel holds an explicit root to the repository's top
// level. From a subdirectory the guards would read that subtree, where a tracked
// root file looks untracked, so the refusal must exit 2 before any guard runs.
// Git answers the question; the spellings of the top level are checked against
// real git in TestIntegrationRepoGuardsSpellings.
func TestRunRootMustBeTopLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		arg        string
		prefix     string
		prefixErr  error
		wantExit   int
		wantRoot   string
		wantStderr string
	}{
		{
			name:       "a prefix git cannot report is refused with exit 1",
			arg:        "/repo",
			prefixErr:  errors.New("exit 128"),
			wantExit:   1,
			wantStderr: "repo-guards: /repo is not a git repository\n",
		},
		{
			name:     "the top level is accepted and returned",
			arg:      "/repo",
			wantExit: 0,
			wantRoot: "/repo",
		},
		{
			name:       "a subdirectory is refused and no guard runs",
			arg:        "/repo/sub",
			prefix:     "sub/\n",
			wantExit:   2,
			wantStderr: "repo-guards: /repo/sub is not the top level of its repository (/repo)\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &fakeGit{}
			e := &fakeExec{toplevel: "/repo\n", prefix: tc.prefix, prefixErr: tc.prefixErr}
			var stdout, stderr bytes.Buffer
			if got := run([]string{tc.arg}, fakeEnv(cleanGoMod, g, e, false), &stdout, &stderr); got != tc.wantExit {
				t.Errorf("exit = %d, want %d; stderr %q", got, tc.wantExit, stderr.String())
			}
			if stderr.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantExit != 0 {
				if len(g.roots) != 0 {
					t.Errorf("guards ran in a refused root: %q", g.roots)
				}
				for _, call := range e.calls {
					if !strings.Contains(call, "rev-parse") {
						t.Errorf("command ran after the refusal: %q", call)
					}
				}
				return
			}
			if len(g.roots) == 0 {
				t.Fatal("no guard ran")
			}
			for _, root := range g.roots {
				if root != tc.wantRoot {
					t.Errorf("guard ran in %q, want %q", root, tc.wantRoot)
				}
			}
		})
	}
}
