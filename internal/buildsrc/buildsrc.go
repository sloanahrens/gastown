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
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// dirRel is the directory under the town root that holds one build checkout
// per rig.
const dirRel = ".runtime/build-src"

// Each git call is bounded by its own timeout. The per-rig lock is held only
// across those calls, so a caller waiting on it waits for the holder's calls to
// finish, not for anything unbounded. The lock wait watches ctx: a caller whose
// ctx ends before it gets the lock returns ctx's error.
const (
	cloneTimeout    = 5 * time.Minute
	fetchTimeout    = 2 * time.Minute
	checkoutTimeout = 2 * time.Minute
	// lockPollInterval is how often a caller waiting on the rig's lock tries
	// again. A waiter stops waiting as soon as its ctx ends, not at the next try.
	lockPollInterval = 50 * time.Millisecond
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
	// The flock is taken before the checkout is looked at, so two callers cannot
	// both find none and clone. It is released on every return.
	unlock, err := lockRig(ctx, lock.FlockTryAcquire, filepath.Join(base, rig+".lock"), rig)
	if err != nil {
		return "", err
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

// lockRig takes the exclusive flock at path for rig through try, which is
// lock.FlockTryAcquire. It polls try every lockPollInterval rather than blocking
// in FlockAcquire, so a ctx that ends while another holder has the lock returns
// ctx's error instead of waiting for the holder.
func lockRig(ctx context.Context, try func(string) (func(), bool, error), path, rig string) (func(), error) {
	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()
	for {
		unlock, ok, err := try(path)
		if err != nil {
			return nil, fmt.Errorf("locking the build checkout of rig %s: %w", rig, err)
		}
		if ok {
			return unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the build checkout lock of rig %s: %w", rig, ctx.Err())
		case <-ticker.C:
		}
	}
}

// cloneMarkerSuffix names the marker beside a checkout directory. ensureClone
// writes it once a clone returns, and clears it before any clone starts.
const cloneMarkerSuffix = ".cloned"

// ensureClone clones bare into dir unless dir already holds a finished clone. A
// clone is finished when dir has a .git and the marker beside it. A .git alone
// does not say so: a clone killed after git made the .git leaves one behind. Any
// other directory is a clone that died part-way, so it is removed first, which
// would otherwise make the next clone fail on a non-empty destination. A clone
// that fails is removed too, and the next call clones again.
func ensureClone(ctx context.Context, run gitFunc, bare, dir string) error {
	marker := dir + cloneMarkerSuffix
	finished, err := cloneFinished(dir, marker)
	if err != nil {
		return fmt.Errorf("the build checkout at %s: %w", dir, err)
	}
	if finished {
		return nil
	}
	// The marker goes first. If it cannot be cleared the call refuses, because a
	// stale marker over a killed clone would make that clone look finished.
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing the clone marker of %s: %w", dir, err)
	}
	// The clone is attempted even if clearing failed: git refuses a non-empty
	// destination, and the clearing error is reported with the clone's.
	clearErr := os.RemoveAll(dir)
	cloneErr := run(ctx, cloneTimeout, "", "clone", "--quiet", "--no-checkout", "--", bare, dir)
	if err := errors.Join(clearErr, cloneErr); err != nil {
		// A clone that failed is removed, so the next call clones again.
		return fmt.Errorf("cloning %s into %s: %w", bare, dir, errors.Join(err, os.RemoveAll(dir)))
	}
	// The marker is written last, so a clone killed before this point has none.
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return fmt.Errorf("marking the clone of %s in %s finished: %w", bare, dir, err)
	}
	return nil
}

// cloneFinished reports whether dir has a .git and marker is present: the state
// ensureClone leaves only when a clone has returned. A stat error other than
// not-exist is returned, not read as unfinished.
func cloneFinished(dir, marker string) (bool, error) {
	for _, p := range []string{filepath.Join(dir, ".git"), marker} {
		_, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
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

// repoEnvVars are the variables git reads to find a repository, its objects and
// its config: the names `git rev-parse --local-env-vars` prints. A caller's value
// for one would point a build call at another repo, so gitEnv drops them.
var repoEnvVars = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_CONFIG",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_PARAMETERS",
	"GIT_DIR",
	"GIT_GRAFT_FILE",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_OBJECT_DIRECTORY",
	"GIT_PREFIX",
	"GIT_REPLACE_REF_BASE",
	"GIT_SHALLOW_FILE",
	"GIT_WORK_TREE",
}

// gitEnv returns environ without the repoEnvVars, plus GIT_TERMINAL_PROMPT=0.
// A background call must never sit on a credential prompt.
func gitEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(repoEnvVars, name) {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// execGit runs git in dir, bounded by timeout, with the caller's repo variables
// dropped (see gitEnv). Its error names the failed call by verb and exit status
// only: captured output can carry a credential (a url with a token, say), so it
// is never put in an error.
func execGit(ctx context.Context, timeout time.Duration, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	if _, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
