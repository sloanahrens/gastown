package battery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/lock"
)

// ErrBusy means another pass holds the run lock.
var ErrBusy = errors.New("another pass holds the run lock")

// Lock takes the run lock at path without waiting, and returns the release.
// Every pass that locks the same path contends on it, in this process and in
// others. A held lock is ErrBusy.
func Lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	release, ok, err := lock.FlockTryAcquire(path)
	if err != nil {
		return nil, fmt.Errorf("locking the pass: %w", err)
	}
	if !ok {
		return nil, ErrBusy
	}
	return release, nil
}
