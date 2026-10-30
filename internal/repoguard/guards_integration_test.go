//go:build integration

package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The cases of scripts/repo-guards_test.sh, run against real git and real gofmt
// in a throwaway repository under t.TempDir(). The script test counts 14 cases;
// each one is a subtest here, and each asserts whether the guards fail, which is
// the exit status repo-guards returns. Cases that stage a file with git add
// exercise the index, which is what the guards read.

// liveEnv is the production wiring of Check: the git binary, gofmt from PATH,
// and the real filesystem for go.mod.
var liveEnv = Env{
	Git:      CLIGit{},
	Exec:     CLIExec{},
	LookPath: exec.LookPath,
	ReadFile: os.ReadFile,
}

// newGuardRepo makes a git repository with a .beads directory and the go.mod
// every case starts from.
func newGuardRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	if err := os.Mkdir(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGuardFile(t, root, "go.mod", "module example.com/x\n\ngo 1.26\n")
	return root
}

// gitIn runs git in root. Its output is not shown on failure: a git error
// there can echo file contents.
func gitIn(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
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

// TestIntegrationRepoGuardsCases mirrors the 14 cases of the script test.
func TestIntegrationRepoGuardsCases(t *testing.T) {
	t.Parallel()
	const trackerReplaceLine = "replace github.com/steveyegge/gastown/tracker => ./tracker\n"
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		fail  bool
	}{
		{
			name:  "clean repo",
			setup: func(t *testing.T, root string) {},
			fail:  false,
		},
		{
			name: "replace directive",
			setup: func(t *testing.T, root string) {
				appendGuardFile(t, root, "go.mod", "replace example.com/y => ../y\n")
			},
			fail: true,
		},
		{
			name: "replace block",
			setup: func(t *testing.T, root string) {
				appendGuardFile(t, root, "go.mod", "replace (\n\texample.com/y => ../y\n)\n")
			},
			fail: true,
		},
		{
			name: "the tracker module replace",
			setup: func(t *testing.T, root string) {
				appendGuardFile(t, root, "go.mod", trackerReplaceLine)
			},
			fail: false,
		},
		{
			name: "the tracker module replaced by another path",
			setup: func(t *testing.T, root string) {
				appendGuardFile(t, root, "go.mod", "replace github.com/steveyegge/gastown/tracker => ../tracker\n")
			},
			fail: true,
		},
		{
			name: "the tracker replace beside another",
			setup: func(t *testing.T, root string) {
				appendGuardFile(t, root, "go.mod", trackerReplaceLine+"replace example.com/y => ../y\n")
			},
			fail: true,
		},
		{
			name: "the tracker replace in a block",
			setup: func(t *testing.T, root string) {
				appendGuardFile(t, root, "go.mod", "replace (\n\tgithub.com/steveyegge/gastown/tracker => ./tracker\n)\n")
			},
			fail: true,
		},
		{
			name: "untracked issues.jsonl",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, ".beads/issues.jsonl", "{}\n")
			},
			fail: false,
		},
		{
			name: "tracked issues.jsonl",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, ".beads/issues.jsonl", "{}\n")
				gitIn(t, root, "add", ".beads/issues.jsonl")
			},
			fail: true,
		},
		{
			name: "gofmt-clean go file",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, "a.go", "package x\n")
				gitIn(t, root, "add", "a.go")
			},
			fail: false,
		},
		{
			name: "unformatted tracked go file",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, "a.go", "package x\n\nfunc  f( ) {}\n")
				gitIn(t, root, "add", "a.go")
			},
			fail: true,
		},
		{
			name: "drift only gofmt -s sees",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, "a.go", "package x\n\nfunc f(s []int) []int { return s[0:len(s)] }\n")
				gitIn(t, root, "add", "a.go")
			},
			fail: true,
		},
		{
			name: "unformatted testdata fixture",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, "testdata/fixture.go", "package x\n\nfunc  f( ) {}\n")
				gitIn(t, root, "add", "testdata/fixture.go")
			},
			fail: false,
		},
		{
			name: "unformatted untracked go file",
			setup: func(t *testing.T, root string) {
				writeGuardFile(t, root, "a.go", "package x\n\nfunc  f( ) {}\n")
			},
			fail: false,
		},
	}
	if len(tests) != 14 {
		t.Fatalf("got %d cases, want the 14 of scripts/repo-guards_test.sh", len(tests))
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newGuardRepo(t)
			tc.setup(t, root)
			if got := Check(root, liveEnv).Failed(); got != tc.fail {
				t.Errorf("guards failed = %v, want %v", got, tc.fail)
			}
		})
	}
}

// TestIntegrationRepoGuardsMessages asserts the failing text of each guard in a
// real repository, byte for byte as the script prints it.
func TestIntegrationRepoGuardsMessages(t *testing.T) {
	t.Parallel()
	t.Run("replace directive", func(t *testing.T) {
		t.Parallel()
		root := newGuardRepo(t)
		appendGuardFile(t, root, "go.mod", "replace example.com/y => ../y\n")
		rep := Check(root, liveEnv)
		wantLines := []string{"repo-guards: go.mod contains replace directives; they break go install ...@latest (upstream #2230)"}
		assertProblems(t, rep, wantLines)
		assertReplaces(t, rep, []string{"replace example.com/y => ../y"})
	})
	t.Run("tracked issues.jsonl", func(t *testing.T) {
		t.Parallel()
		root := newGuardRepo(t)
		writeGuardFile(t, root, ".beads/issues.jsonl", "{}\n")
		gitIn(t, root, "add", ".beads/issues.jsonl")
		assertProblems(t, Check(root, liveEnv), []string{
			"repo-guards: .beads/issues.jsonl is tracked; the Dolt server is the only beads backend (git rm --cached it)",
		})
	})
	t.Run("unformatted tracked go file", func(t *testing.T) {
		t.Parallel()
		root := newGuardRepo(t)
		writeGuardFile(t, root, "a.go", "package x\n\nfunc  f( ) {}\n")
		gitIn(t, root, "add", "a.go")
		rep := Check(root, liveEnv)
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
