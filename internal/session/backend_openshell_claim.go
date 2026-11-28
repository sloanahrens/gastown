package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// sandboxClaimPath is the claim file of a sandbox being created: beside the
// record with a suffix the record listing does not read, so a spawn killed
// between creating its sandbox and writing the record leaves a file naming it
// (gt-5dw70j).
func sandboxClaimPath(townRoot, session string) string {
	return filepath.Join(SandboxRegistryDir(townRoot), session+".creating")
}

// sandboxClaimFile is the claim's content: the session it names and the
// gateway holding its sandbox.
type sandboxClaimFile struct {
	Session string `json:"session"`
	Gateway string `json:"gateway"`
}

// sandboxClaim is a claim on a sandbox being created, holding the claim file's
// exclusive lock until it is removed or unlocked.
type sandboxClaim struct {
	path string
	lock *flock.Flock
}

// writeSandboxClaim writes session's claim naming gateway and returns it with
// the file's exclusive lock held, so another writer waits until it is removed
// or unlocked. A failed write removes the claim file and returns its error.
func writeSandboxClaim(townRoot, session, gateway string) (*sandboxClaim, error) {
	if !validSessionName(session) {
		return nil, fmt.Errorf("sandbox claim: bad session name %q", session)
	}
	if err := os.MkdirAll(SandboxRegistryDir(townRoot), 0o755); err != nil {
		return nil, fmt.Errorf("creating the sandbox registry: %w", err)
	}
	path := sandboxClaimPath(townRoot, session)
	lock := flock.New(path)
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("locking the sandbox claim %s: %w", path, err)
	}
	c := &sandboxClaim{path: path, lock: lock}
	// The claim holds only strings, so its JSON cannot fail to encode.
	data, _ := json.Marshal(sandboxClaimFile{Session: session, Gateway: gateway})
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("writing the sandbox claim %s: %w", path, err), c.Remove())
	}
	return c, nil
}

// Unlock releases the claim's lock and leaves its file for the next writer.
func (c *sandboxClaim) Unlock() error { return c.lock.Unlock() }

// Remove deletes the claim file and releases its lock. A file already gone is
// not an error, and a failed removal still unlocks, so a name is never held by
// a claim that cannot be deleted; the removal error is returned either way.
func (c *sandboxClaim) Remove() error {
	err := os.Remove(c.path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return errors.Join(err, c.lock.Unlock())
}
