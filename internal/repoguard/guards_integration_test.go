//go:build integration

package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The 14 cases of the previous shell test, run against real git and real gofmt
// in a throwaway repository under t.TempDir(). Each case is a subtest here, and
// each asserts whether the guards fail, which is the exit status repo-guards
// returns. Cases that stage a file with git add exercise the index, which is
// what the guards read.

// guardRepo is a throwaway repository and the environment its git and gofmt
// run in. The HOME is made once per test, with the repository, so every command
// the test runs shares it.
type guardRepo struct {
	root string
	env  []string
}

// guardEnviron is the environment git and gofmt run in here. It carries no
// ambient git config and no repository variables from a hook, so the machine
// the test runs on cannot change its answer. HOME is home, the test's own.
func guardEnviron(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}
}

// liveEnv is the production wiring of Check: the git binary, gofmt from PATH,
// and the real filesystem for go.mod. The commands run in r.env.
func liveEnv(r guardRepo) Env {
	ex := CLIExec{start: func(cmd *exec.Cmd) error {
		cmd.Env = r.env
		return cmd.Run()
	}}
	return Env{
		Git:      CLIGit{ex: ex},
		Exec:     ex,
		LookPath: exec.LookPath,
		ReadFile: os.ReadFile,
	}
}

// newGuardRepo makes a git repository with a .beads directory and the go.mod
// every case starts from.
func newGuardRepo(t *testing.T) guardRepo {
	t.Helper()
	r := guardRepo{root: t.TempDir(), env: guardEnviron(t.TempDir())}
	gitIn(t, r, "init", "-q")
	if err := os.Mkdir(filepath.Join(r.root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGuardFile(t, r.root, "go.mod", "module example.com/x\n\ngo 1.26\n")
	return r
}

// gitIn runs git in r.root in r.env. Its output is not shown on failure: a git
// error there can echo file contents.
func gitIn(t *testing.T, r guardRepo, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.root}, args...)...)
	cmd.Env = r.env
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v", args[0], err)
	}
}

// writeGuardFile writes rel under root, creating its directory.
func writeGuardFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendGuardFile adds content to the end of rel under root.
func appendGuardFile(t *testing.T, root, rel, content string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationRepoGuardsCases mirrors the 14 cases of the previous shell test.
func TestIntegrationRepoGuardsCases(t *testing.T) {
	t.Parallel()
	const trackerReplaceLine = "replace github.com/steveyegge/gastown/tracker => ./tracker\n"
	tests := []struct {
		name  string
		setup func(t *testing.T, r guardRepo)
		fail  bool
	}{
		{
			name:  "clean repo",
			setup: func(t *testing.T, r guardRepo) {},
			fail:  false,
		},
		{
			name: "replace directive",
			setup: func(t *testing.T, r guardRepo) {
				appendGuardFile(t, r.root, "go.mod", "replace example.com/y => ../y\n")
			},
			fail: true,
		},
		{
			name: "replace block",
			setup: func(t *testing.T, r guardRepo) {
				appendGuardFile(t, r.root, "go.mod", "replace (\n\texample.com/y => ../y\n)\n")
			},
			fail: true,
		},
		{
			name: "the tracker module replace",
			setup: func(t *testing.T, r guardRepo) {
				appendGuardFile(t, r.root, "go.mod", trackerReplaceLine)
			},
			fail: false,
		},
		{
			name: "the tracker module replaced by another path",
			setup: func(t *testing.T, r guardRepo) {
				appendGuardFile(t, r.root, "go.mod", "replace github.com/steveyegge/gastown/tracker => ../tracker\n")
			},
			fail: true,
		},
		{
			name: "the tracker replace beside another",
			setup: func(t *testing.T, r guardRepo) {
				appendGuardFile(t, r.root, "go.mod", trackerReplaceLine+"replace example.com/y => ../y\n")
			},
			fail: true,
		},
		{
			name: "the tracker replace in a block",
			setup: func(t *testing.T, r guardRepo) {
				appendGuardFile(t, r.root, "go.mod", "replace (\n\tgithub.com/steveyegge/gastown/tracker => ./tracker\n)\n")
			},
			fail: true,
		},
		{
			name: "untracked issues.jsonl",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, ".beads/issues.jsonl", "{}\n")
			},
			fail: false,
		},
		{
			name: "tracked issues.jsonl",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, ".beads/issues.jsonl", "{}\n")
				gitIn(t, r, "add", ".beads/issues.jsonl")
			},
			fail: true,
		},
		{
			name: "gofmt-clean go file",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, "a.go", "package x\n")
				gitIn(t, r, "add", "a.go")
			},
			fail: false,
		},
		{
			name: "unformatted tracked go file",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, "a.go", "package x\n\nfunc  f( ) {}\n")
				gitIn(t, r, "add", "a.go")
			},
			fail: true,
		},
		{
			name: "drift only gofmt -s sees",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, "a.go", "package x\n\nfunc f(s []int) []int { return s[0:len(s)] }\n")
				gitIn(t, r, "add", "a.go")
			},
			fail: true,
		},
		{
			name: "unformatted testdata fixture",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, "testdata/fixture.go", "package x\n\nfunc  f( ) {}\n")
				gitIn(t, r, "add", "testdata/fixture.go")
			},
			fail: false,
		},
		{
			name: "unformatted untracked go file",
			setup: func(t *testing.T, r guardRepo) {
				writeGuardFile(t, r.root, "a.go", "package x\n\nfunc  f( ) {}\n")
			},
			fail: false,
		},
	}
	if len(tests) != 14 {
		t.Fatalf("got %d cases, want the 14 of the previous shell test", len(tests))
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newGuardRepo(t)
			tc.setup(t, r)
			if got := Check(r.root, liveEnv(r)).Failed(); got != tc.fail {
				t.Errorf("guards failed = %v, want %v", got, tc.fail)
			}
		})
	}
}

// TestIntegrationRepoGuardsMessages asserts the failing text of each guard in a
// real repository, byte for byte as the previous shell version printed it.
func TestIntegrationRepoGuardsMessages(t *testing.T) {
	t.Parallel()
	t.Run("replace directive", func(t *testing.T) {
		t.Parallel()
		r := newGuardRepo(t)
		appendGuardFile(t, r.root, "go.mod", "replace example.com/y => ../y\n")
		rep := Check(r.root, liveEnv(r))
		wantLines := []string{"repo-guards: go.mod contains replace directives; they break go install ...@latest (upstream #2230)"}
		assertProblems(t, rep, wantLines)
		assertReplaces(t, rep, []string{"replace example.com/y => ../y"})
	})
	t.Run("tracked issues.jsonl", func(t *testing.T) {
		t.Parallel()
		r := newGuardRepo(t)
		writeGuardFile(t, r.root, ".beads/issues.jsonl", "{}\n")
		gitIn(t, r, "add", ".beads/issues.jsonl")
		assertProblems(t, Check(r.root, liveEnv(r)), []string{
			"repo-guards: .beads/issues.jsonl is tracked; the Dolt server is the only beads backend (git rm --cached it)",
		})
	})
	t.Run("unformatted tracked go file", func(t *testing.T) {
		t.Parallel()
		r := newGuardRepo(t)
		writeGuardFile(t, r.root, "a.go", "package x\n\nfunc  f( ) {}\n")
		gitIn(t, r, "add", "a.go")
		rep := Check(r.root, liveEnv(r))
		assertProblems(t, rep, []string{
			"repo-guards: these files are not gofmt -s clean; run 'gofmt -s -w' on them:",
			"a.go",
		})
	})
}

// assertProblems checks the stderr lines of rep, in order.
func assertProblems(t *testing.T, rep Report, want []string) {
	t.Helper()
	if len(rep.Problems) != len(want) {
		t.Fatalf("problems = %q, want %q", rep.Problems, want)
	}
	for i := range want {
		if rep.Problems[i] != want[i] {
			t.Errorf("problem %d = %q, want %q", i, rep.Problems[i], want[i])
		}
	}
}

// assertReplaces checks the go.mod lines the replace guard named.
func assertReplaces(t *testing.T, rep Report, want []string) {
	t.Helper()
	if len(rep.Replaces) != len(want) {
		t.Fatalf("replaces = %q, want %q", rep.Replaces, want)
	}
	for i := range want {
		if rep.Replaces[i] != want[i] {
			t.Errorf("replace %d = %q, want %q", i, rep.Replaces[i], want[i])
		}
	}
}
