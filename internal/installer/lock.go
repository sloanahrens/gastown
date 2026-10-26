package installer

import (
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/installlock"
)

// DefaultLockWait is how long a run waits for the install lock when it sets no
// wait: 300 seconds (gt-tpg0p.13).
const DefaultLockWait = 300 * time.Second

// InstallLock is the exclusive lock a run takes before it swaps the binary.
//
// The image build runs before the lock is taken and holds only its own
// polecat-image lock, so an install queued behind a build waits this wait, not
// the build (gt-tpg0p.13).
type InstallLock struct {
	// Path is the lock file, <daemon>/install-gt.lock.
	Path string
	// Wait is how long to wait for the lock before the run is refused as busy;
	// zero means DefaultLockWait.
	Wait time.Duration
	// Acquire takes the lock and returns its release, returning
	// installlock.ErrBusy when the wait ran out. Nil takes the real flock; a
	// unit test replaces it to observe the order without a lock file.
	Acquire func(path string, wait time.Duration) (release func(), err error)
}

// wait is the configured wait, or the default.
func (l InstallLock) wait() time.Duration {
	if l.Wait > 0 {
		return l.Wait
	}
	return DefaultLockWait
}

// take takes the lock through the seam.
func (l InstallLock) take() (func(), error) {
	acquire := l.Acquire
	if acquire == nil {
		acquire = installlock.Acquire
	}
	return acquire(l.Path, l.wait())
}

// Swap is the install's lock boundary: image builds the sandbox image first
// with no install lock held, then the install lock is taken and guarded — the
// binary swap and everything the lock covers after it — runs under it
// (gt-tpg0p.13). A failed image build returns before any lock is taken.
func (c *Contract) Swap(l InstallLock, image, guarded func() Exit) Exit {
	if code := image(); code != ExitOK {
		return code
	}
	release, err := l.take()
	if errors.Is(err, installlock.ErrBusy) {
		c.Log("Another install held the lock for " + l.wait().String() + "; not installing " + c.Expected + ".")
		c.Receipt("refused", c.Expected, "", "lock-busy")
		return c.Finish(Result{Event: "refused", Commit: c.Expected, Reason: "lock-busy"}, ExitBusy)
	}
	if err != nil {
		// The run named no result, so OnExit reports failed/unexpected: the
		// lock could not be taken at all, which is not a busy install.
		c.Log("Cannot take the install lock: " + err.Error())
		return ExitFailed
	}
	defer release()
	return guarded()
}
