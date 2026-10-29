package polecat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/gofrs/flock"
)

// SandboxSeatLockTimeout bounds the wait for a polecat's directory lock, so a
// wedged holder costs one bounded step rather than a stuck goroutine.
const SandboxSeatLockTimeout = 30 * time.Second

// ErrSandboxSeatLockBusy is the answer to a seat lock that was not free in
// time: nothing was read and nothing was removed.
var ErrSandboxSeatLockBusy = errors.New("polecat directory lock is busy")

// WithSandboxSeatLock runs fn while holding the lock every writer of a polecat's
// directory takes before it touches it: Manager.Add builds the worktree under
// it, Manager.Remove deletes it under it, and the reuse paths run under it too.
// A caller that decides from a seat's facts and then deletes must hold it
// across both, or a sling can build the very directory the caller removes
// (gt-sefzt). A lock that is not free in time returns ErrSandboxSeatLockBusy
// and runs nothing.
func WithSandboxSeatLock(rigPath, name string, fn func() error) error {
	return withSandboxSeatLockTimeout(rigPath, name, SandboxSeatLockTimeout, fn)
}

// polecatLockPath is the file a polecat's directory lock is taken on. Manager's
// lockPolecat takes the same file, so the two exclude each other.
func polecatLockPath(rigPath, name string) string {
	return filepath.Join(rigPath, ".runtime", "locks", "polecat-"+name+".lock")
}

// withSandboxSeatLockTimeout is WithSandboxSeatLock over a wait the caller
// chooses, so a test can prove the busy answer without waiting out the bound.
func withSandboxSeatLockTimeout(rigPath, name string, timeout time.Duration, fn func() error) error {
	if !filepath.IsAbs(rigPath) {
		return fmt.Errorf("sandbox seat lock: rig path %q is not absolute", rigPath)
	}
	if err := requireSandboxSeatSegment("polecat", name); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(polecatLockPath(rigPath, name)), 0o755); err != nil {
		return fmt.Errorf("creating lock dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fl := flock.New(polecatLockPath(rigPath, name))
	locked, err := fl.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("acquiring the polecat lock for %s: %w", name, err)
	}
	if !locked {
		return fmt.Errorf("%w: %s", ErrSandboxSeatLockBusy, name)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}

// sandboxSeatSegmentRe matches a rig or polecat name that is safe as one path
// component: never empty, hidden, ".", "..", or carrying a separator. Both
// names are joined into the directories this file deletes, so both are checked
// before anything is removed.
var sandboxSeatSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func requireSandboxSeatSegment(what, name string) error {
	if !sandboxSeatSegmentRe.MatchString(name) {
		return fmt.Errorf("sandbox seat directories: %s %q is not a single path segment", what, name)
	}
	return nil
}

// CheckSandboxSeatNames reports whether rig and name are each one safe path
// segment. A caller checks them before it reads anything under the seat's
// names, so an unsafe name is refused before any fact is measured.
func CheckSandboxSeatNames(rig, name string) error {
	if err := requireSandboxSeatSegment("rig", rig); err != nil {
		return err
	}
	return requireSandboxSeatSegment("polecat", name)
}

// SandboxSeatHostDirs is the host-side state a sandbox seat leaves behind: its
// polecat directory, the clone inside it, and the system prompt the host
// rendered for its sandbox.
type SandboxSeatHostDirs struct {
	// PolecatDir is <rig>/polecats/<name>, the polecat's home directory.
	PolecatDir string
	// ClonePath is <rig>/polecats/<name>/<rig>, the git worktree a sling made.
	ClonePath string
	// PromptFile is <rig>/polecats/.claude/sandbox/system-prompt-<name>.md,
	// the prompt buildRuntimeArchiveLogging rendered for the sandbox. It sits
	// in the shared polecats/.claude directory, so only the file goes.
	PromptFile string
}

// SandboxSeatHostDirsFor names the host paths of the sandbox seat rig/name.
func SandboxSeatHostDirsFor(rigPath, rigName, name string) SandboxSeatHostDirs {
	polecatDir := filepath.Join(rigPath, "polecats", name)
	return SandboxSeatHostDirs{
		PolecatDir: polecatDir,
		ClonePath:  filepath.Join(polecatDir, rigName),
		PromptFile: filepath.Join(rigPath, "polecats", ".claude", "sandbox", "system-prompt-"+name+".md"),
	}
}

// SandboxSeatWorktreeRemover detaches a seat's clone from the rig repository.
// A nil remover detaches through the rig's own git; a test passes one that
// starts no process.
type SandboxSeatWorktreeRemover func(rigPath, clonePath string) error

// RemoveSandboxSeatHostDirs removes the host directories a sandbox seat left.
// deleted reports whether it removed any of them, so a caller can tell a
// removal that failed before it touched anything from one that failed halfway
// and left the seat partly gone; err names the steps that could not be
// finished. It is idempotent: a seat whose directories are already gone
// reports deleted=false and no error.
//
// The remover detaches the clone from the rig repository first, so the
// worktree entry goes with it; a remover that fails does not stop the removal,
// which then leaves the entry to the prune below. A rig with no repository at
// all has no entry to detach and nothing is reported for it.
func RemoveSandboxSeatHostDirs(rigPath, rigName, name string, remover SandboxSeatWorktreeRemover) (bool, error) {
	return removeSandboxSeatHostDirs(gitOpener{}, rigPath, rigName, name, remover)
}

// removeSandboxSeatHostDirs is RemoveSandboxSeatHostDirs with git opened by
// gits, so a test drives the prune's failure through a fake world.
func removeSandboxSeatHostDirs(gits gitOpener, rigPath, rigName, name string, remover SandboxSeatWorktreeRemover) (bool, error) {
	if !filepath.IsAbs(rigPath) {
		return false, fmt.Errorf("sandbox seat directories: rig path %q is not absolute", rigPath)
	}
	if err := requireSandboxSeatSegment("rig", rigName); err != nil {
		return false, err
	}
	if err := requireSandboxSeatSegment("polecat", name); err != nil {
		return false, err
	}
	dirs := SandboxSeatHostDirsFor(rigPath, rigName, name)
	if remover == nil {
		remover = func(rigPath, clonePath string) error { return detachRigWorktree(gits, rigPath, clonePath) }
	}
	var (
		deleted bool
		errs    []error
	)
	clonePresent, err := seatPathPresent(dirs.ClonePath)
	if err != nil {
		errs = append(errs, err)
	}
	if clonePresent {
		if err := remover(rigPath, dirs.ClonePath); err != nil {
			errs = append(errs, fmt.Errorf("detaching worktree %s: %w", dirs.ClonePath, err))
		}
		// The clone goes even after a failed detach: it is the stale copy, and
		// the prune below forgets an entry whose worktree is gone.
		if err := os.RemoveAll(dirs.ClonePath); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", dirs.ClonePath, err))
		} else {
			deleted = true
		}
	}
	if present, err := seatPathPresent(dirs.PolecatDir); err != nil {
		errs = append(errs, err)
	} else if present {
		if err := os.RemoveAll(dirs.PolecatDir); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", dirs.PolecatDir, err))
		} else {
			deleted = true
		}
	}
	if present, err := seatPathPresent(dirs.PromptFile); err != nil {
		errs = append(errs, err)
	} else if present {
		if err := os.Remove(dirs.PromptFile); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", dirs.PromptFile, err))
		} else {
			deleted = true
		}
	}
	if err := pruneRigWorktrees(gits, rigPath); err != nil {
		errs = append(errs, err)
	}
	return deleted, errors.Join(errs...)
}

// seatPathPresent reports whether path is there. Only a missing path answers
// "not there"; any other read failure is returned, so a caller that cannot see
// a path never reports it gone.
func seatPathPresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking %s: %w", path, err)
}

// detachRigWorktree removes clonePath from the rig repository's worktree list.
// A rig with no repository has no entry, so there is nothing to do.
func detachRigWorktree(gits gitOpener, rigPath, clonePath string) error {
	root := rigGitRepo(rigPath)
	if root == "" {
		return nil
	}
	return openRigRepo(gits, root).WorktreeRemove(clonePath, true)
}

// pruneRigWorktrees forgets the rig repository's worktree entries whose
// directories are gone, the cleanup a plain directory delete needs. A rig with
// no repository has nothing to prune.
func pruneRigWorktrees(gits gitOpener, rigPath string) error {
	root := rigGitRepo(rigPath)
	if root == "" {
		return nil
	}
	if err := openRigRepo(gits, root).WorktreePrune(); err != nil {
		return fmt.Errorf("pruning the worktrees of %s: %w", root, err)
	}
	return nil
}
