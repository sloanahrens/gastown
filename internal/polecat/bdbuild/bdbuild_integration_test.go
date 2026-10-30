//go:build integration

package bdbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestIntegrationExecGitReturnsGitsLastStderrLine pins a failure of the host's
// git to its own last stderr line. It runs real git, so it lives in the
// integration tier (gt-6uid9).
func TestIntegrationExecGitReturnsGitsLastStderrLine(t *testing.T) {
	isolateGitConfig(t)
	_, err := ExecGit(t.TempDir(), "rev-parse", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("ExecGit in a directory that is not a checkout: err = %v, want git's own \"not a git repository\"", err)
	}
}

// TestIntegrationRunGitStopsACommandItsContextEnds runs a live command through
// the kill path behind ExecGit's ceiling and watches the context stop it,
// whether the deadline passed or the run was canceled (gt-6uid9, gt-qivx4).
// ExecGit owns a five-minute ceiling no test can wait out, so the test hands
// runGit, which is the code that applies it, a deadline it can wait for. The
// stand-in writes a marker before it blocks, so an error naming the context is
// one a run of it produced, not a command that never started.
func TestIntegrationRunGitStopsACommandItsContextEnds(t *testing.T) {
	standIn := t.TempDir()
	started := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\n: > \"" + started + "\"\nexec /bin/sleep " + strconv.Itoa(int(standInBlocks/time.Second)) + "\n"
	if err := os.WriteFile(filepath.Join(standIn, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the blocking stand-in: %v", err)
	}
	t.Setenv("PATH", standIn)

	ceiling := 200 * time.Millisecond
	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want string
	}{
		{
			name: "a deadline passed",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), ceiling)
			},
			want: "git timed out after the " + gitCeiling.String() + " ceiling",
		},
		{
			name: "a canceled run",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(2*time.Second, cancel)
				return ctx, cancel
			},
			want: "git was canceled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			start := time.Now()
			_, err := runGit(ctx, t.TempDir(), "--version")
			elapsed := time.Since(start)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("runGit under %s: err = %v, want one naming %q", tc.name, err, tc.want)
			}
			if _, statErr := os.Stat(started); statErr != nil {
				t.Errorf("the stand-in never started (%v), so the run never reached a live command", statErr)
			}
			if elapsed >= standInBlocks/2 {
				t.Errorf("runGit returned after %s, so it did not stop a command that blocks for %s", elapsed, standInBlocks)
			}
		})
	}
}

// standInBlocks is how long the stand-in blocks: past the deadline and the
// cancellation the test sets, and short enough that a runner which fails to
// stop it is seen returning late.
const standInBlocks = 10 * time.Second

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
	home, err := os.MkdirTemp("", "git-home-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), gitConfigOff(home)...)
	return cmd.Output()
}

// isolateGitConfig gives the test process's git the empty configuration
// isolatedGit gives a fixture, for a caller whose git reads the process's own
// environment instead of one it passes in (ExecGit).
func isolateGitConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, kv := range gitConfigOff(home) {
		key, value, _ := strings.Cut(kv, "=")
		t.Setenv(key, value)
	}
}

// gitConfigOff reports the environment that puts the host's git configuration
// out of a fixture's way: HOME and XDG_CONFIG_HOME name an empty directory the
// caller owns, and GIT_CONFIG_NOSYSTEM drops /etc/gitconfig. Naming the global
// file instead takes GIT_CONFIG_GLOBAL, which only git 2.32 reads (gt-qivx4).
func gitConfigOff(home string) []string {
	return []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}
}
