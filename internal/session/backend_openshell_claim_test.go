package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

// TestOpenshellHalfMadeClaim: a written claim names its session and gateway, a
// second writer is refused until Unlock, the record listing sees neither file,
// and Remove deletes the claim and frees the lock. A handle on the lock opened
// before Remove still makes the next write refused (gt-5dw70j).
func TestOpenshellHalfMadeClaim(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const session = "gt-box"
	c, err := writeSandboxClaim(town, session, "gw-one")
	must(t, err)
	data, err := os.ReadFile(sandboxClaimPath(town, session))
	must(t, err)
	var got sandboxClaimFile
	must(t, json.Unmarshal(data, &got))
	if want := (sandboxClaimFile{Session: session, Gateway: "gw-one"}); got != want {
		t.Errorf("claim = %+v, want %+v", got, want)
	}
	if names := ListSandboxSessions(town); len(names) != 0 {
		t.Errorf("ListSandboxSessions = %v, want the claim ignored", names)
	}
	if _, err := writeSandboxClaim(town, session, "gw-two"); err == nil {
		t.Fatal("a second write while the claim is held succeeded, want it refused")
	}
	must(t, c.Unlock())
	next, err := writeSandboxClaim(town, session, "gw-two")
	must(t, err)
	lockFile, err := os.Open(sandboxClaimLockPath(town, session))
	must(t, err)
	defer lockFile.Close()
	must(t, next.Remove())
	if _, err := os.Stat(sandboxClaimPath(town, session)); !os.IsNotExist(err) {
		t.Errorf("stat of the claim after Remove = %v, want gone", err)
	}
	stale := flock.New(sandboxClaimLockPath(town, session))
	if ok, err := stale.TryLock(); !ok || err != nil {
		t.Fatalf("TryLock after Remove = %v, %v, want taken", ok, err)
	}
	defer stale.Unlock()
	lockInfo, err := lockFile.Stat()
	must(t, err)
	heldInfo, err := stale.Stat()
	must(t, err)
	if !os.SameFile(lockInfo, heldInfo) {
		t.Fatal("the lock taken after Remove is not the file opened before it")
	}
	if _, err := writeSandboxClaim(town, session, "gw-three"); err == nil {
		t.Fatal("a write while the stale handle holds the lock succeeded, want it refused")
	}
}

// TestOpenshellHalfMadeClaimRefuses: a bad name, a registry that cannot be made,
// and a lock that cannot be opened are errors wrapping errClaimNotTaken with no
// claim. A claim that cannot be written under the lock returns the claim still
// holding it, with an error that does not wrap errClaimNotTaken; Unlock frees
// the lock (gt-5dw70j).
func TestOpenshellHalfMadeClaimRefuses(t *testing.T) {
	t.Parallel()
	const session = "gt-box"
	cases := []struct {
		name, session, wantErr string
		block                  func(t *testing.T, town string)
		notTaken               bool
	}{
		{"bad session name", "../gt-box", "bad session name", nil, true},
		{"registry cannot be made", session, "creating the sandbox registry", func(t *testing.T, town string) {
			must(t, os.WriteFile(filepath.Join(town, ".runtime"), nil, 0o600))
		}, true},
		{"lock cannot be opened", session, "locking the sandbox claim", func(t *testing.T, town string) {
			must(t, os.MkdirAll(sandboxClaimLockPath(town, session), 0o755))
		}, true},
		{"claim cannot be written", session, "writing the sandbox claim", func(t *testing.T, town string) {
			must(t, os.MkdirAll(sandboxClaimPath(town, session), 0o755))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.block != nil {
				tc.block(t, town)
			}
			c, err := writeSandboxClaim(town, tc.session, "gw")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("writeSandboxClaim = %v, want an error naming %q", err, tc.wantErr)
			}
			if got := errors.Is(err, errClaimNotTaken); got != tc.notTaken {
				t.Fatalf("errors.Is(err, errClaimNotTaken) = %v, want %v: %v", got, tc.notTaken, err)
			}
			if info, err := os.Stat(sandboxClaimPath(town, tc.session)); err == nil && info.Mode().IsRegular() {
				t.Error("a failed write left a claim file")
			}
			if tc.notTaken {
				if c != nil {
					t.Fatal("a failure before the lock returned a claim")
				}
				return
			}
			if c == nil {
				t.Fatal("a failed write under the lock returned no claim, so nothing could release the lock")
			}
			requireSandboxLockHeld(t, town, tc.session)
			must(t, c.Unlock())
			requireSandboxLockFree(t, town, tc.session)
		})
	}
}

// TestOpenshellHalfMadeClaimRefusesHeldLock: a write while another holder has
// the lock is refused, wraps errClaimNotTaken, names the lock and leaves no
// claim (gt-5dw70j).
func TestOpenshellHalfMadeClaimRefusesHeldLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const session = "gt-box"
	must(t, os.MkdirAll(SandboxRegistryDir(town), 0o755))
	holder := flock.New(sandboxClaimLockPath(town, session))
	if ok, err := holder.TryLock(); !ok || err != nil {
		t.Fatalf("holder TryLock = %v, %v", ok, err)
	}
	c, err := writeSandboxClaim(town, session, "gw")
	if err == nil || !strings.Contains(err.Error(), sandboxClaimLockPath(town, session)) {
		t.Fatalf("writeSandboxClaim = %v, want an error naming the lock", err)
	}
	if !errors.Is(err, errClaimNotTaken) {
		t.Errorf("a write refused by another holder does not wrap errClaimNotTaken: %v", err)
	}
	if c != nil {
		t.Fatal("a refused write returned a claim")
	}
	if _, err := os.Stat(sandboxClaimPath(town, session)); !os.IsNotExist(err) {
		t.Errorf("a refused write left a claim: stat = %v", err)
	}
	must(t, holder.Unlock())
	requireSandboxLockFree(t, town, session)
}

// TestOpenshellHalfMadeClaimRefusesFailedRemove: a Remove whose delete fails
// returns that error and still frees the lock (gt-5dw70j).
func TestOpenshellHalfMadeClaimRefusesFailedRemove(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const session = "gt-box"
	c, err := writeSandboxClaim(town, session, "gw")
	must(t, err)
	path := sandboxClaimPath(town, session)
	must(t, os.Remove(path))
	// A directory with an entry in the claim's place: os.Remove cannot delete it.
	must(t, os.MkdirAll(filepath.Join(path, "entry"), 0o755))
	if err := c.Remove(); err == nil {
		t.Fatal("Remove of a non-empty directory returned nil, want its error")
	}
	requireSandboxLockFree(t, town, session)
}

// TestOpenshellHalfMadeClaimRemoveGone: a Remove of a claim already deleted is
// not an error, and it frees the lock (gt-5dw70j).
func TestOpenshellHalfMadeClaimRemoveGone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const session = "gt-box"
	c, err := writeSandboxClaim(town, session, "gw")
	must(t, err)
	must(t, os.Remove(sandboxClaimPath(town, session)))
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove of a gone claim = %v, want nil", err)
	}
	requireSandboxLockFree(t, town, session)
}

// requireSandboxLockFree fails the test unless session's lock can be taken, and
// releases it again.
func requireSandboxLockFree(t *testing.T, town, session string) {
	t.Helper()
	probe := flock.New(sandboxClaimLockPath(town, session))
	if ok, err := probe.TryLock(); !ok || err != nil {
		t.Fatalf("sandbox claim lock not free: TryLock = %v, %v", ok, err)
	}
	must(t, probe.Unlock())
}

// requireSandboxLockHeld fails the test unless session's lock is held by
// another open handle.
func requireSandboxLockHeld(t *testing.T, town, session string) {
	t.Helper()
	probe := flock.New(sandboxClaimLockPath(town, session))
	if ok, err := probe.TryLock(); ok || err != nil {
		t.Fatalf("sandbox claim lock not held: TryLock = %v, %v", ok, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
