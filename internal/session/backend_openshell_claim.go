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

// writeSandboxClaim takes session's claim lock without waiting and writes the
// claim naming gateway, returning the claim with the lock held. A held lock is
// an error naming the lock path. A claim that cannot be written is an error too,
// and the failed write unlocks the lock it took before returning it.
func writeSandboxClaim(townRoot, session, gateway string) (*sandboxClaim, error) {
	if !validSessionName(session) {
		return nil, fmt.Errorf("sandbox claim: bad session name %q", session)
	}
	if err := os.MkdirAll(SandboxRegistryDir(townRoot), 0o755); err != nil {
		return nil, fmt.Errorf("creating the sandbox registry: %w", err)
	}
	lockPath := sandboxClaimLockPath(townRoot, session)
	lock := flock.New(lockPath)
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking the sandbox claim %s: %w", lockPath, err)
	}
	if !ok {
		return nil, fmt.Errorf("sandbox claim %s is held by another writer", lockPath)
	}
	path := sandboxClaimPath(townRoot, session)
	// The claim holds only strings, so its JSON cannot fail to encode.
	data, _ := json.Marshal(sandboxClaimFile{Session: session, Gateway: gateway})
	if err := atomicfile.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("writing the sandbox claim %s: %w", path, err), lock.Unlock())
	}
	return &sandboxClaim{path: path, lock: lock}, nil
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
