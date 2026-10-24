//go:build integration

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// GNU Make 4.3 cannot spawn recipes in an OpenShell sandbox (its recipe child's
// setresuid/setresgid is denied by the seccomp filter). The image builds 4.4.1
// into /usr/local/bin and must also make /usr/bin/make that binary, or a process
// with a restricted PATH (devops' host-rehearsal fixture builds with root's
// directories only) still finds 4.3 and fails with "cp: Operation not permitted"
// (gt-r1rd5). The build fails unless both paths report 4.4.1.
//
// The test runs the Dockerfile's own RUN instruction against a scratch root that
// holds a fake 4.3 in /usr/bin and a fake make in /usr/local/bin, so it checks
// what the instruction does, not how it is spelled. That needs a real sh and
// executable fakes, so it is an integration test.

// makeInstruction returns the shell command of the Dockerfile's one RUN
// instruction that touches /usr/bin/make.
func makeInstruction(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	// Continuation lines joined, so each instruction is one line.
	var found []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\\\n", " "), "\n") {
		if cmd, isRun := strings.CutPrefix(line, "RUN "); isRun && strings.Contains(cmd, "/usr/bin/make") {
			found = append(found, cmd)
		}
	}
	if len(found) != 1 {
		t.Fatalf("deploy/polecat/Dockerfile has %d RUN instructions that touch /usr/bin/make, want 1: %q", len(found), found)
	}
	return found[0]
}

// fakeMake writes a make that reports the given version line.
func fakeMake(t *testing.T, path, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\necho 'GNU Make %s'\necho 'Built for aarch64-unknown-linux-gnu'\n", version)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runInRoot runs the instruction with its /usr and /bin paths moved under a
// scratch root laid out like Ubuntu 24.04 (/bin is a link to /usr/bin), where
// /usr/bin/make is Ubuntu's 4.3 and /usr/local/bin/make is localMake. It
// returns the root, the command's combined output and its error.
func runInRoot(t *testing.T, localMake string) (root, out string, err error) {
	t.Helper()
	root = t.TempDir()
	fakeMake(t, filepath.Join(root, "usr", "bin", "make"), "4.3")
	fakeMake(t, filepath.Join(root, "usr", "local", "bin", "make"), localMake)
	if err := os.Symlink("usr/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	cmd := regexp.MustCompile(`(^|\s)/(usr|bin)/`).ReplaceAllString(makeInstruction(t), "${1}"+root+"/${2}/")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	return root, string(b), err
}

func TestIntegrationPolecatImageReplacesTheDistroMake(t *testing.T) {
	t.Parallel()
	root, out, err := runInRoot(t, "4.4.1")
	if err != nil {
		t.Fatalf("the Dockerfile's make instruction failed with 4.4.1 in /usr/local/bin: %v\n%s", err, out)
	}
	for _, p := range []string{"usr/bin/make", "bin/make", "usr/local/bin/make"} {
		got, err := exec.Command(filepath.Join(root, p), "--version").Output()
		if err != nil {
			t.Fatalf("/%s --version: %v", p, err)
		}
		if first, _, _ := strings.Cut(string(got), "\n"); first != "GNU Make 4.4.1" {
			t.Errorf("/%s reports %q after the instruction, want GNU Make 4.4.1: /usr/bin/make would stay Ubuntu's 4.3", p, first)
		}
	}
}

func TestIntegrationPolecatImageBuildFailsWithoutMake441(t *testing.T) {
	t.Parallel()
	_, out, err := runInRoot(t, "4.3")
	if err == nil {
		t.Fatalf("the Dockerfile's make instruction passed with 4.3 in /usr/local/bin; the build must fail unless /usr/bin/make and /bin/make report 4.4.1\n%s", out)
	}
}
