//go:build integration

package land

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// RiskPaths over real git: diff --name-only prints internal/daemon/café.go
// C-quoted, and DiffNameOnly has to unquote it before the glob sees it.
func TestIntegrationRiskPathsNonASCIIPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	write(RiskPathsFile, "internal/daemon/**\n")
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	run("checkout", "-q", "-b", "work")
	write("internal/daemon/café.go", "package daemon\n")
	run("add", ".")
	run("commit", "-q", "-m", "work")

	got, err := RiskPaths(git.NewGit(dir), "main", "work")
	if err != nil {
		t.Fatalf("RiskPaths: %v", err)
	}
	if !slices.Equal(got, []string{"internal/daemon/café.go"}) {
		t.Errorf("RiskPaths = %q, want internal/daemon/café.go", got)
	}
}

// RiskPaths over real git: git's default rename detection pairs a moved file
// and --name-only then prints the destination alone, whose path matches no
// glob in internal/daemon/**. The move out of the glob is exactly what the
// landing has to see, so the diff asks for both names (gt-j8m6g).
func TestIntegrationRiskPathsRenameOutOfRiskDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	write(RiskPathsFile, "internal/daemon/**\n")
	write("internal/daemon/x.go", "package daemon\n")
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	run("checkout", "-q", "-b", "work")
	if err := os.MkdirAll(filepath.Join(dir, "internal/misc"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("mv", "internal/daemon/x.go", "internal/misc/x.go")
	run("commit", "-q", "-m", "work")

	got, err := RiskPaths(git.NewGit(dir), "main", "work")
	if err != nil {
		t.Fatalf("RiskPaths: %v", err)
	}
	if !slices.Equal(got, []string{"internal/daemon/x.go"}) {
		t.Errorf("RiskPaths = %q, want the moved file's source internal/daemon/x.go", got)
	}
}
