package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// sandboxClaimPath is the claim file of a sandbox being created: beside the
// record with a suffix the record listing does not read, so a spawn killed
// between creating its sandbox and writing the record leaves a file naming it
// (gt-5dw70j).
func sandboxClaimPath(townRoot, session string) string {
	return filepath.Join(SandboxRegistryDir(townRoot), session+".creating")
}

// sandboxClaimLockPath is the lock every writer of a session's claim takes. It
// is never removed, so every holder locks the same inode (gt-5dw70j).
func sandboxClaimLockPath(townRoot, session string) string {
	return filepath.Join(SandboxRegistryDir(townRoot), session+".creating.lock")
}

// sandboxClaimFile is the claim's content: the session it names and the
// gateway holding its sandbox.
type sandboxClaimFile struct {
	Session string `json:"session"`
	Gateway string `json:"gateway"`
}

// sandboxClaim is a claim on a sandbox being created, holding the session's
// lock until it is removed or unlocked.
type sandboxClaim struct {
	path string
	lock *flock.Flock
}

// errClaimNotTaken marks a claim refused before its lock was taken, when another
// writer may own the seat name.
var errClaimNotTaken = errors.New("sandbox claim lock not taken")

// writeSandboxClaim takes session's claim lock without waiting and writes the
// claim naming gateway, returning the claim with the lock held. A failure before
// the lock is taken wraps errClaimNotTaken and returns no claim. A claim that
// cannot be written under the lock returns the claim still holding the lock,
// with an error that does not wrap errClaimNotTaken: the caller releases the
// claim with Unlock or Remove.
func writeSandboxClaim(townRoot, session, gateway string) (*sandboxClaim, error) {
	if !validSessionName(session) {
		return nil, fmt.Errorf("%w: bad session name %q", errClaimNotTaken, session)
	}
	if err := os.MkdirAll(SandboxRegistryDir(townRoot), 0o755); err != nil {
		return nil, fmt.Errorf("%w: creating the sandbox registry: %w", errClaimNotTaken, err)
	}
	lockPath := sandboxClaimLockPath(townRoot, session)
	lock := flock.New(lockPath)
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("%w: locking the sandbox claim %s: %w", errClaimNotTaken, lockPath, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: sandbox claim %s is held by another writer", errClaimNotTaken, lockPath)
	}
	path := sandboxClaimPath(townRoot, session)
	claim := &sandboxClaim{path: path, lock: lock}
	// The claim holds only strings, so its JSON cannot fail to encode.
	data, _ := json.Marshal(sandboxClaimFile{Session: session, Gateway: gateway})
	if err := atomicfile.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return claim, fmt.Errorf("writing the sandbox claim %s: %w", path, err)
	}
	return claim, nil
}

// Unlock releases the claim's lock and leaves its file in place.
func (c *sandboxClaim) Unlock() error { return c.lock.Unlock() }

// Remove deletes the claim file while the lock is held, then releases the lock.
// A file already gone is not an error. A failed delete still unlocks, and its
// error is returned.
func (c *sandboxClaim) Remove() error {
	err := os.Remove(c.path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return errors.Join(err, c.lock.Unlock())
}
