package git

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// configReadFatal is the error git printed in the gt-n3slrv bursts.
const configReadFatal = "fatal: unknown error occurred while reading the configuration files"

// configReadAttempt is one git call's answer in a configReadRunner.
type configReadAttempt struct {
	stdout string
	stderr string
	err    error
}

// configReadRunner answers successive git calls with its attempts, the last one
// repeating, and counts the calls it was sent.
type configReadRunner struct {
	attempts []configReadAttempt
	calls    int
}

func (r *configReadRunner) run(gitCall) (string, string, error) {
	i := r.calls
	if i >= len(r.attempts) {
		i = len(r.attempts) - 1
	}
	r.calls++
	a := r.attempts[i]
	return a.stdout, a.stderr, a.err
}

// configReadGit is a retrying Git over r whose waits are recorded in waits. Its
// environment has an empty HOME of its own and unsets the variables the report
// reads from the process, so no test reads the host's configuration.
func configReadGit(t *testing.T, r *configReadRunner, waits *[]time.Duration) *Git {
	t.Helper()
	g := (&Git{workDir: t.TempDir(), exec: r.run}).
		WithEnv([]string{"HOME=" + t.TempDir()}).
		WithoutEnv("XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM").
		WithConfigReadRetry()
	g.retryWait = func(d time.Duration) { *waits = append(*waits, d) }
	return g
}

// failingConfigRead answers every call with the configuration-files error.
func failingConfigRead() *configReadRunner {
	return &configReadRunner{attempts: []configReadAttempt{{stderr: configReadFatal, err: gitExit(128)}}}
}

func TestGitConfigReadRetriesOnceThenSucceeds(t *testing.T) {
	t.Parallel()
	r := &configReadRunner{attempts: []configReadAttempt{
		{stderr: configReadFatal, err: gitExit(128)},
		{stdout: "abc1234\n"},
	}}
	var waits []time.Duration
	g := configReadGit(t, r, &waits)

	out, err := g.run("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("run after one retry: %v", err)
	}
	if out != "abc1234" {
		t.Errorf("stdout = %q, want %q", out, "abc1234")
	}
	if r.calls != 2 {
		t.Errorf("git calls = %d, want 2", r.calls)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Errorf("waits = %v, want one wait of 1s before the retry", waits)
	}
}

func TestGitConfigReadReturnsBothReportsAfterSecondFailure(t *testing.T) {
	t.Parallel()
	r := failingConfigRead()
	var waits []time.Duration
	g := configReadGit(t, r, &waits)

	_, err := g.run("push", "origin", "main")
	if err == nil {
		t.Fatal("run failing twice: want an error")
	}
	if got := exitCode(err); got != 128 {
		t.Errorf("exit code = %d, want 128: the error must keep git's exit status", got)
	}
	if r.calls != 2 {
		t.Errorf("git calls = %d, want 2", r.calls)
	}
	if len(waits) != 1 {
		t.Errorf("waits = %d, want 1", len(waits))
	}
	if n := strings.Count(err.Error(), "git configuration read failed"); n != 2 {
		t.Errorf("reports in error = %d, want 2 (one per attempt): %s", n, err.Error())
	}
}

func TestGitConfigReadLeavesOtherErrorsAlone(t *testing.T) {
	t.Parallel()
	const other = "fatal: not a git repository"
	r := &configReadRunner{attempts: []configReadAttempt{{stderr: other, err: gitExit(128)}}}
	var waits []time.Duration
	g := configReadGit(t, r, &waits)

	_, err := g.run("status")
	if err == nil {
		t.Fatal("want an error")
	}
	if r.calls != 1 {
		t.Errorf("git calls = %d, want 1: an error without the marker is never retried", r.calls)
	}
	if len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
	var gitErr *GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("error %T is not a *GitError", err)
	}
	if gitErr.Stderr != other {
		t.Errorf("stderr = %q, want %q unchanged", gitErr.Stderr, other)
	}
}

func TestGitConfigReadReportNamesFiles(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	xdg := t.TempDir()
	repo := t.TempDir()
	// The contents must never reach the report, so each file holds this text.
	const secret = "SECRET-BODY-DO-NOT-PRINT"
	homeConfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(homeConfig, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(xdg, "git", "config")
	if err := os.MkdirAll(filepath.Dir(unreadable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unreadable, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(unreadable); err == nil {
		t.Fatalf("precondition: a chmod 000 file opened (close: %v); run this test as a non-root user", f.Close())
	}
	repoConfig := filepath.Join(repo, "config")
	if err := os.WriteFile(repoConfig, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	r := failingConfigRead()
	g := (&Git{gitDir: repo, workDir: t.TempDir(), exec: r.run}).
		WithEnv([]string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg}).
		WithoutEnv("GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM").
		WithConfigReadRetry()
	g.retryWait = func(time.Duration) {}

	_, err := g.run("ls-remote", "origin")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	size := len(secret)
	want := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + xdg,
		"GIT_CONFIG_GLOBAL=(unset)",
		"GIT_CONFIG_SYSTEM=(unset)",
		"repository=" + repo,
		homeConfig + ": exists, size " + strconv.Itoa(size) + ", open ok",
		unreadable + ": exists, size " + strconv.Itoa(size) + ", open error: ",
		repoConfig + ": exists, size " + strconv.Itoa(size) + ", open ok",
		filepath.Join(repo, "config.worktree") + ": absent",
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("report lacks %q:\n%s", w, msg)
		}
	}
	if strings.Contains(msg, secret) {
		t.Errorf("report carries file contents:\n%s", msg)
	}
}

func TestGitConfigReadReportSkipsFilesWithoutBase(t *testing.T) {
	t.Parallel()
	r := failingConfigRead()
	g := (&Git{workDir: t.TempDir(), exec: r.run}).
		WithoutEnv("HOME", "XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM").
		WithConfigReadRetry()
	g.retryWait = func(time.Duration) {}

	_, err := g.run("fetch", "origin")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, w := range []string{
		"HOME=(unset)",
		"$HOME/.gitconfig: not checked, its base directory is unset",
		"$XDG_CONFIG_HOME/git/config: not checked, its base directory is unset",
	} {
		if !strings.Contains(msg, w) {
			t.Errorf("report lacks %q:\n%s", w, msg)
		}
	}
}

func TestGitConfigReadReportDefaultsXDGUnderHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	r := failingConfigRead()
	g := (&Git{workDir: t.TempDir(), exec: r.run}).
		WithEnv([]string{"HOME=" + home}).
		WithoutEnv("XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM").
		WithConfigReadRetry()
	g.retryWait = func(time.Duration) {}

	_, err := g.run("fetch", "origin")
	if err == nil {
		t.Fatal("want an error")
	}
	want := filepath.Join(home, ".config", "git", "config") + ": absent"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("report lacks %q:\n%s", want, err.Error())
	}
}

func TestGitConfigReadOffByDefault(t *testing.T) {
	t.Parallel()
	r := failingConfigRead()
	g := &Git{workDir: t.TempDir(), exec: r.run}

	_, err := g.run("ls-remote", "origin")
	if err == nil {
		t.Fatal("want an error")
	}
	if r.calls != 1 {
		t.Errorf("git calls = %d, want 1: a Git without WithConfigReadRetry is never retried", r.calls)
	}
	if strings.Contains(err.Error(), "git configuration read failed") {
		t.Errorf("error carries a report without the option: %s", err.Error())
	}
}
