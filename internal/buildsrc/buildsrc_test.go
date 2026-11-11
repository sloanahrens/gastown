package buildsrc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// newTown returns a town root under a fresh temp dir. The town itself is not
// created: each test seeds only what it needs.
func newTown(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "town")
}

// fakeGit answers the git calls Ensure makes without running git. clone is what
// a clone does on disk, and fail is a verb whose calls fail; every other call
// succeeds. calls records the verb of each call, in order.
type fakeGit struct {
	calls []string
	clone func(dir string) error
	fail  string
}

func (f *fakeGit) run(_ context.Context, _ time.Duration, _ string, args ...string) error {
	f.calls = append(f.calls, args[0])
	if args[0] == "clone" {
		return f.clone(args[len(args)-1])
	}
	if args[0] == f.fail {
		return errors.New("git " + args[0] + " failed")
	}
	return nil
}

// cloneWithGit is what a clone that succeeded leaves behind: a checkout directory
// with a .git in it.
func cloneWithGit(dir string) error {
	return os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
}

func TestEnsureRefusesBadInputBeforeGit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		rig    string
		rev    string
		noBare bool
	}{
		{name: "missing bare repo", rig: "gastown", noBare: true},
		{name: "empty rig name", rig: ""},
		{name: "dot rig name", rig: "."},
		{name: "dot-dot rig name", rig: ".."},
		{name: "slash in rig name", rig: "a/b"},
		{name: "backslash in rig name", rig: `a\b`},
		{name: "branch name as rev", rig: "gastown", rev: "main"},
		{name: "option as rev", rig: "gastown", rev: "--upload-pack=touch ran"},
		{name: "short sha as rev", rig: "gastown", rev: "4b825dc"},
		{name: "upper-case sha as rev", rig: "gastown", rev: strings.ToUpper("0123456789abcdef0123456789abcdef01234567")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := newTown(t)
			bare := filepath.Join(town, tc.rig, ".repo.git")
			// A directory is enough: a refusal must come before git looks at it.
			if !tc.noBare {
				if err := os.MkdirAll(bare, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The lock file's directory exists, so a missing check would reach git.
			if err := os.MkdirAll(filepath.Join(town, dirRel, filepath.Dir(tc.rig)), 0o755); err != nil {
				t.Fatal(err)
			}
			git := &fakeGit{clone: cloneWithGit}

			dir, err := ensure(context.Background(), git.run, town, tc.rig, tc.rev)
			if err == nil {
				t.Fatalf("ensure(%q, %q) returned %s and no error", tc.rig, tc.rev, dir)
			}
			if len(git.calls) != 0 {
				t.Fatalf("git was called %v before the refusal", git.calls)
			}
			if tc.noBare && (!strings.Contains(err.Error(), bare) || !errors.Is(err, fs.ErrNotExist)) {
				t.Fatalf("error %q is not the missing-bare refusal naming %s", err, bare)
			}
		})
	}
}

func TestEnsureReportsFailedGitStepByName(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		rev  string
		fail string
		// named is what the error must say, if anything.
		named string
	}{
		{name: "fetch of main fails", fail: "fetch"},
		{name: "fetch of a rev fails and names the sha", rev: sha, fail: "fetch", named: sha},
		{name: "checkout fails and names the target", rev: sha, fail: "checkout", named: sha},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := newTown(t)
			if err := os.MkdirAll(filepath.Join(town, "gastown", ".repo.git"), 0o755); err != nil {
				t.Fatal(err)
			}
			git := &fakeGit{clone: cloneWithGit, fail: tc.fail}

			_, err := ensure(context.Background(), git.run, town, "gastown", tc.rev)
			if err == nil {
				t.Fatalf("ensure returned no error with %s failing", tc.fail)
			}
			if tc.named != "" && !strings.Contains(err.Error(), tc.named) {
				t.Fatalf("error %q does not name %s", err, tc.named)
			}
		})
	}
}

func TestEnsureFailsWhenItsPathsCannotBeUsed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// bare is whether the rig has a bare repo directory to find.
		bare bool
		// setup makes one of the town's paths unusable, given the town root.
		setup func(t *testing.T, town string)
	}{
		{name: "the rig path is a file", setup: func(t *testing.T, town string) {
			writeFile(t, filepath.Join(town, "gastown"))
		}},
		{name: "the runtime directory is a file", bare: true, setup: func(t *testing.T, town string) {
			writeFile(t, filepath.Join(town, ".runtime"))
		}},
		{name: "the lock path is a directory", bare: true, setup: func(t *testing.T, town string) {
			mkdir(t, filepath.Join(town, dirRel, "gastown.lock"))
		}},
		{name: "the checkout path is a file", bare: true, setup: func(t *testing.T, town string) {
			mkdir(t, filepath.Join(town, dirRel))
			writeFile(t, filepath.Join(town, dirRel, "gastown"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := newTown(t)
			mkdir(t, town)
			if tc.bare {
				mkdir(t, filepath.Join(town, "gastown", ".repo.git"))
			}
			tc.setup(t, town)
			git := &fakeGit{clone: cloneWithGit}

			if _, err := ensure(context.Background(), git.run, town, "gastown", ""); err == nil {
				t.Fatalf("ensure returned no error")
			}
			if len(git.calls) != 0 {
				t.Fatalf("git was called %v on an unusable path", git.calls)
			}
		})
	}
}

// TestExecGitReportsCallThatCannotStart runs execGit in a directory that does
// not exist, so git never starts: the call fails in the process that would run
// it. The error names the verb and nothing else.
func TestExecGitReportsCallThatCannotStart(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(newTown(t), "absent")

	err := execGit(context.Background(), time.Minute, missing, "status")
	if err == nil {
		t.Fatalf("execGit in a missing directory returned no error")
	}
	if !strings.Contains(err.Error(), "git status") {
		t.Fatalf("error %q does not name the call", err)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFailedFirstCloneLeavesNoDirectory(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	if err := os.MkdirAll(filepath.Join(town, "gastown", ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(town, dirRel, "gastown")
	git := &fakeGit{clone: func(dir string) error {
		if err := cloneWithGit(dir); err != nil {
			return err
		}
		// The clone got as far as making its directory, then died.
		return errors.New("clone cut short")
	}}

	_, err := ensure(context.Background(), git.run, town, "gastown", "")
	if err == nil {
		t.Fatalf("ensure with a failing clone returned no error")
	}
	if _, statErr := os.Stat(checkout); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("partial checkout at %s left behind (stat: %v)", checkout, statErr)
	}
}

func TestEnsureReplacesDirectoryLeftByDeadClone(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	if err := os.MkdirAll(filepath.Join(town, "gastown", ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(town, dirRel, "gastown")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(checkout, "partial")
	if err := os.WriteFile(junk, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := &fakeGit{clone: cloneWithGit}

	if _, err := ensure(context.Background(), git.run, town, "gastown", ""); err != nil {
		t.Fatalf("ensure over a dead clone: %v", err)
	}
	if _, err := os.Stat(junk); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the dead clone's file %s survived (stat: %v)", junk, err)
	}
	if len(git.calls) == 0 || git.calls[0] != "clone" {
		t.Fatalf("git calls = %v, want a clone first", git.calls)
	}
}

// A caller whose ctx has ended while another holder has the rig's lock gets
// ctx's error back and runs no git: the lock wait watches ctx. The ctx is
// already past its deadline, so the wait is over as soon as it is tried. The
// failsafe fires only if Ensure goes on waiting for the holder.
func TestEnsureReturnsWhenItsCtxEndsWhileTheRigLockIsHeld(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	if err := os.MkdirAll(filepath.Join(town, "gastown", ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(town, dirRel)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := lock.FlockAcquire(filepath.Join(base, "gastown.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	git := &fakeGit{clone: cloneWithGit}
	done := make(chan error, 1)
	go func() {
		_, err := ensure(ctx, git.run, town, "gastown", "")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ensure error = %v, want one wrapping context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "gastown") {
			t.Errorf("ensure error %q does not name the rig", err)
		}
		if len(git.calls) != 0 {
			t.Errorf("git calls = %v, want none while the lock is held", git.calls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ensure still waits for the rig lock after its ctx ended")
	}
}
