//go:build integration

package bdbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationCompileRefusesACommitTheCheckoutDoesNotHold pins the refusal
// to real git, which alone decides which commits an object store holds.
func TestIntegrationCompileRefusesACommitTheCheckoutDoesNotHold(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	srcCommit := initScratchRepo(t, src, "src")
	other := t.TempDir()
	otherCommit := initScratchRepo(t, other, "other")
	if srcCommit == otherCommit {
		t.Fatalf("both scratch repositories made commit %s; the test needs a commit %s lacks", srcCommit, src)
	}
	buildDir := t.TempDir()
	build := &fakeGo{}
	_, err := Compile(Options{Src: src, Out: filepath.Join(src, "bd"), Commit: otherCommit, BuildDir: buildDir}, isolatedGit, build.run)
	if err == nil {
		t.Fatalf("Compile built %s, which %s does not hold", otherCommit, src)
	}
	for _, want := range []string{otherCommit, src, "does not hold"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Compile: error lacks %q: %v", want, err)
		}
	}
	if build.dir != "" {
		t.Errorf("Compile built in %q after the commit was refused", build.dir)
	}
	if entries, statErr := os.ReadDir(buildDir); statErr != nil {
		t.Fatalf("reading the build directory: %v", statErr)
	} else if len(entries) != 0 {
		t.Errorf("Compile left %d entries in %s after the commit was refused", len(entries), buildDir)
	}
}

// TestIntegrationCompileRefusesACommitStillMissingAfterTheFetch pins the
// refusal that follows a real fetch which ran and did not bring the commit in:
// the operator is told to stop fetching, because fetching is what just failed.
func TestIntegrationCompileRefusesACommitStillMissingAfterTheFetch(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	initScratchRepo(t, src, "src")
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := isolatedGit(t.TempDir(), "init", "-q", "--bare", origin); err != nil {
		t.Fatalf("making the bare origin: %v: %s", err, out)
	}
	scratchGit(t, src, "remote", "add", "origin", origin)
	other := t.TempDir()
	otherCommit := initScratchRepo(t, other, "other")

	build := &fakeGo{}
	_, err := Compile(Options{Src: src, Out: filepath.Join(src, "bd"), Commit: otherCommit, BuildDir: t.TempDir()}, isolatedGit, build.run)
	if err == nil {
		t.Fatalf("Compile built %s, which neither %s nor its origin holds", otherCommit, src)
	}
	for _, want := range []string{otherCommit, src, "still does not hold", "after fetching all of origin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Compile: error lacks %q: %v", want, err)
		}
	}
	if fix := fixLine(err.Error()); fix == "" || strings.Contains(fix, "fetch") {
		t.Errorf("Compile: the fix line %q still tells the operator to fetch after the fetch already ran", fix)
	}
	if build.dir != "" {
		t.Errorf("Compile built in %q for a commit that is still missing", build.dir)
	}
}

// initScratchRepo makes a git repository at dir with one commit naming marker
// in its go.mod, and returns that commit's full sha.
func initScratchRepo(t *testing.T, dir, marker string) string {
	t.Helper()
	scratchGit(t, dir, "init", "-q")
	gomod := "module example.invalid/" + marker + "\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("writing the scratch go.mod: %v", err)
	}
	scratchGit(t, dir, "add", "-A")
	scratchGit(t, dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "init")
	out, err := isolatedGit(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("reading the scratch commit: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func scratchGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := isolatedGit(dir, args...); err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
}

// isolatedGit runs git with the host's global and system configuration out of
// the picture, so a machine's commit.gpgsign, hooks or default branch cannot
// change the fixture.
func isolatedGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	return cmd.Output()
}
