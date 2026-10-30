//go:build integration

package tiersweep

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// gitIn runs git in dir for a test's setup.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// realGitRun runs Run with NO Src injected and --root at root. git runs for real
// (the listing and the commit under test); every other command is recorded and
// succeeds. It returns the code, stdout, stderr, the directory each go test ran
// in, and every name ReadFile was asked for.
func realGitRun(t *testing.T, root string, tiers ...string) (code int, stdout, stderr string, tierDirs, reads []string) {
	t.Helper()
	logDir := t.TempDir()
	var out, errw bytes.Buffer
	code = Run(append([]string{"--root", root}, tiers...), &out, &errw, Deps{
		Host: Host{
			Env: func(name string) string {
				if name == EnvLogDir {
					return logDir
				}
				return ""
			},
			MkdirTemp: func(string, string) (string, error) { return logDir, nil },
			MkdirAll:  func(string, fs.FileMode) error { return nil },
		},
		Exec: func(dir string, env []string, argv []string, w io.Writer) int {
			if argv[0] == "git" {
				return ProcessExec(dir, env, argv, w)
			}
			if argv[0] == "gt" {
				tierDirs = append(tierDirs, dir)
			}
			return 0
		},
		ReadFile: func(name string) ([]byte, error) {
			reads = append(reads, name)
			return os.ReadFile(name)
		},
		Environ: os.Environ(),
		Now:     func() time.Time { return time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC) },
	})
	return code, out.String(), errw.String(), tierDirs, reads
}

// TestIntegrationSweepListsFromTheRootOfARealRepo: a real git repository holding
// one integration-tagged package and a docker.txt, named only by --root and not
// the working directory. The run lists it with real `git ls-files` in that
// directory, reads its files under that root, and runs the package from it.
func TestIntegrationSweepListsFromTheRootOfARealRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, text string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("r/r_test.go", taggedTest)
	write("untagged/u_test.go", plainTest)
	write(dockerList, noDocker)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "fixture")

	code, stdout, stderr, tierDirs, reads := realGitRun(t, root, "integration")
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Fatalf("code %d, stdout %q, stderr %q, want GREEN passed=1 (only ./r is tagged)", code, stdout, stderr)
	}
	if !reflect.DeepEqual(tierDirs, []string{root}) {
		t.Errorf("go test ran in %v, want only %q", tierDirs, root)
	}
	if len(reads) == 0 {
		t.Fatal("no file was read")
	}
	for _, name := range reads {
		if !strings.HasPrefix(name, root+string(filepath.Separator)) {
			t.Errorf("ReadFile(%q) is outside the root %q", name, root)
		}
	}
	if !strings.Contains(strings.Join(reads, "\n"), filepath.Join(root, "r", "r_test.go")) {
		t.Errorf("the listed test file was not read under the root: %v", reads)
	}
}

// TestIntegrationSweepRootThatIsNotARepoIsRed: a directory that is no git
// repository makes the real `git ls-files` fail, and the tier is RED with exit 1
// rather than a clean empty list.
func TestIntegrationSweepRootThatIsNotARepoIsRed(t *testing.T) {
	// A fresh temp dir sits outside any repository only when no parent is one, so
	// the ceiling stops git's search above it. Setenv forbids t.Parallel.
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	code, stdout, stderr, tierDirs, _ := realGitRun(t, root, "integration")
	if code != 1 || !strings.Contains(stdout, "integration RED passed=0 failed=0 skipped=0") {
		t.Errorf("code %d, stdout %q, stderr %q, want exit 1 and a RED line", code, stdout, stderr)
	}
	if len(tierDirs) != 0 {
		t.Errorf("go test ran in %v, want nothing run", tierDirs)
	}
}
