// Package buildsrc owns the job-owned build checkout: the clone that builds and
// gate commands run in once a rig's mayor/rig working tree is gone. Each rig
// gets one under the town's .runtime directory, cloned from the rig's bare
// repo and moved to the bare repo's main (or to a named commit) on every call.
// Nothing here writes to the bare repo or to the rig's own checkout.
package buildsrc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// dirRel is the directory under the town root that holds one build checkout
// per rig.
const dirRel = ".runtime/build-src"

// Each git call is bounded by its own timeout. The per-rig lock is held only
// across those calls, so a caller waiting on it waits for the holder's calls to
// finish, not for anything unbounded. The lock wait itself does not watch ctx.
const (
	cloneTimeout    = 5 * time.Minute
	fetchTimeout    = 2 * time.Minute
	checkoutTimeout = 2 * time.Minute
)

// gitFunc runs git in dir and returns an error that names nothing from the
// call's output. Ensure sends every git call through execGit; tests send them
// through a runner that records the call and then runs the real git.
type gitFunc func(ctx context.Context, timeout time.Duration, dir string, args ...string) error

// Ensure returns the build checkout of rig, detached at rev: "" for the bare
// repo's main, or a full commit sha the bare repo has. Calls for one rig are
// serialized, and each one fetches from <townRoot>/<rig>/.repo.git, so a commit
// that landed since the last call is there.
func Ensure(ctx context.Context, townRoot, rig, rev string) (string, error) {
	return ensure(ctx, execGit, townRoot, rig, rev)
}

// ensure is Ensure with the git runner injected.
func ensure(ctx context.Context, run gitFunc, townRoot, rig, rev string) (string, error) {
	if !rigNameOK(rig) {
		return "", fmt.Errorf("rig name %q is not a single directory name", rig)
	}
	if rev != "" && !fullSHA(rev) {
		return "", fmt.Errorf("rev %q is not a full commit sha", rev)
	}
	bare := filepath.Join(townRoot, rig, ".repo.git")
	if _, err := os.Stat(bare); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("rig %s has no bare repo at %s: %w", rig, bare, err)
		}
		return "", fmt.Errorf("the bare repo of rig %s at %s: %w", rig, bare, err)
	}

	base := filepath.Join(townRoot, filepath.FromSlash(dirRel))
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", base, err)
	}
	// internal/lock's FlockAcquire takes the flock. It is taken before the
	// checkout is looked at, so two callers cannot both find none and clone.
	// It is released on every return.
	unlock, err := lock.FlockAcquire(filepath.Join(base, rig+".lock"))
	if err != nil {
		return "", fmt.Errorf("locking the build checkout of rig %s: %w", rig, err)
	}
	defer unlock()

	dir := filepath.Join(base, rig)
	if err := ensureClone(ctx, run, bare, dir); err != nil {
		return "", err
	}

	target := rev
	if rev == "" {
		if err := run(ctx, fetchTimeout, dir, "fetch", "--quiet", "--no-tags", "--", bare, "main"); err != nil {
			return "", fmt.Errorf("fetching main of %s into %s: %w", bare, dir, err)
		}
		target = "FETCH_HEAD"
	} else {
		if err := run(ctx, fetchTimeout, dir, "fetch", "--quiet", "--no-tags", "--", bare, rev); err != nil {
			return "", fmt.Errorf("fetching commit %s from %s: %w", rev, bare, err)
		}
	}
	if err := run(ctx, checkoutTimeout, dir, "checkout", "--quiet", "--detach", target, "--"); err != nil {
		return "", fmt.Errorf("checking out %s in %s: %w", target, dir, err)
	}
	return dir, nil
}

// ensureClone clones bare into dir when dir is not a checkout yet. A directory
// without a .git is a clone that died part-way, which would make the next clone
// fail on a non-empty destination, so it is removed first. A clone that fails is
// removed too, and the next call clones again.
func ensureClone(ctx context.Context, run gitFunc, bare, dir string) error {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("the build checkout at %s: %w", dir, err)
	}
	// The clone is attempted even if clearing failed: git refuses a non-empty
	// destination, and the clearing error is reported with the clone's.
	clearErr := os.RemoveAll(dir)
	cloneErr := run(ctx, cloneTimeout, "", "clone", "--quiet", "--no-checkout", "--", bare, dir)
	if err := errors.Join(clearErr, cloneErr); err != nil {
		// A clone that failed is removed, so the next call clones again.
		return fmt.Errorf("cloning %s into %s: %w", bare, dir, errors.Join(err, os.RemoveAll(dir)))
	}
	return nil
}

// rigNameOK reports whether rig can be joined under the town root as one
// directory name. An empty name, "." or ".." would name the town root or its
// parent, and a separator would name a directory that is not the rig.
func rigNameOK(rig string) bool {
	if rig == "" || rig == "." || rig == ".." {
		return false
	}
	for _, r := range rig {
		if r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

// fullSHA reports whether s is a full lowercase object name, sha-1 or sha-256.
// A rev of any other shape is refused, so nothing a caller passes can read as a
// git option.
func fullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// execGit runs git in dir, bounded by timeout. Its error names the failed call
// by verb and exit status only: captured output can carry a credential (a url
// with a token, say), so it is never put in an error.
func execGit(ctx context.Context, timeout time.Duration, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// A background call must never sit on a credential prompt.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if _, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
