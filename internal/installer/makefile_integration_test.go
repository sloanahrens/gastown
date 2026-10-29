//go:build integration

package installer

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeDryRun runs `make -n` for the repository's real Makefile and returns what
// make would execute. -n runs no recipe, so nothing is built or installed and
// the daemon is untouched: the test reads the recipe as make expands it. It is
// an integration test because it starts make, which a unit test must not do.
func makeDryRun(t *testing.T, args ...string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", append([]string{"-n", "--no-print-directory"}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestIntegrationMakeInstallBuildsThenRunsTheInstaller pins the recipe the
// bead's acceptance names: make install builds ./gt-installer, the path the
// daemon's rebuild_gt job runs, with the cgo-free flags, and only then runs it
// (gt-tpg0p.14).
func TestIntegrationMakeInstallBuildsThenRunsTheInstaller(t *testing.T) {
	out := makeDryRun(t, "install")
	build := "CGO_ENABLED=0 go build -buildvcs=false -o ./gt-installer ./cmd/gt-installer"
	b := strings.Index(out, build)
	if b < 0 {
		t.Fatalf("make install does not build the installer:\n%s", out)
	}
	ran := false
	for _, line := range strings.Split(out[b+len(build):], "\n") {
		// With SHA unset the recipe's $(if ...) leaves a trailing space.
		if strings.TrimSpace(line) == "./gt-installer" {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("make install does not run ./gt-installer after building it:\n%s", out)
	}
	if strings.Contains(out, "--sha") {
		t.Errorf("make install passes --sha without SHA set:\n%s", out)
	}
}

// TestIntegrationMakeInstallPassesTheShaThrough pins SHA=<rev> reaching the
// installer, the way rebuild_gt and the operator pin a build.
func TestIntegrationMakeInstallPassesTheShaThrough(t *testing.T) {
	out := makeDryRun(t, "install", "SHA=abc1234")
	if !strings.Contains(out, "./gt-installer --sha abc1234") {
		t.Fatalf("make install SHA=abc1234 does not pass --sha to the installer:\n%s", out)
	}
}

// TestIntegrationMakeInstallerTargetIsPhony pins that an installer binary left
// by an earlier checkout is never what installs: make rebuilds it every time. A
// target that names a file is skipped when that file exists, and -n shows no
// build line then, so the rule is read from make's database.
func TestIntegrationMakeInstallerTargetIsPhony(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "-pqn", "--no-print-directory")
	cmd.Dir = root
	// -q exits 1 when a target is out of date: only the database text matters.
	out, _ := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, ".PHONY:"); ok {
			for _, name := range strings.Fields(rest) {
				if name == "gt-installer" {
					return
				}
			}
		}
	}
	t.Fatal("gt-installer is not a .PHONY target in make's database")
}
