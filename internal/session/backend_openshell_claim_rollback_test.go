package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// claimState reports whether the spawn's claim file exists and whether its lock
// is held by another writer. The probe takes the lock only to test it.
func claimState(t *testing.T, town string) (present, locked bool) {
	t.Helper()
	_, statErr := os.Stat(sandboxClaimPath(town, openshellTestSession))
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	lock := flock.New(sandboxClaimLockPath(town, openshellTestSession))
	ok, err := lock.TryLock()
	must(t, err)
	if ok {
		must(t, lock.Unlock())
	}
	return statErr == nil, !ok
}

// claimProbeRevoker records the claim's state at each revoke, so a test sees
// whether the claim still held the lock while the seat's token was live.
type claimProbeRevoker struct {
	t             *testing.T
	town          string
	revokes       int
	present, lock bool
}

func (r *claimProbeRevoker) Revoke(_ context.Context, _ string) error {
	r.revokes++
	r.present, r.lock = claimState(r.t, r.town)
	return nil
}

// rollbackClaimSetup is a town with a tracker route and a seat token for the
// test polecat, and a claim on the test session that the spawn holds.
func rollbackClaimSetup(t *testing.T, fake *sandbox.Fake) (*openshellBackend, *claimProbeRevoker, *sandboxClaim, SpawnSpec) {
	t.Helper()
	town, work := openshellTown(t)
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	writeTrackerRoute(t, town, openshellTestRig)
	seedSeatToken(t, town, openshellTestRig, openshellTestPolecat)
	claim, err := writeSandboxClaim(town, openshellTestSession, openshellTestProfile.Gateway)
	must(t, err)
	probe := &claimProbeRevoker{t: t, town: town}
	b.revoker = probe
	return b, probe, claim, openshellSpawn(town, work)
}

// TestOpenshellHalfMadeRollbackClaimDeleteSucceeds: after a successful delete the
// seat's token is revoked while the claim lock is still held, then the claim is
// removed and its lock is free (gt-5j8bcy).
func TestOpenshellHalfMadeRollbackClaimDeleteSucceeds(t *testing.T) {
	t.Parallel()
	b, probe, claim, spec := rollbackClaimSetup(t, &sandbox.Fake{})
	cause := errors.New("populate failed")

	if err := b.rollback(spec, cause, nil, claim); err != cause {
		t.Fatalf("rollback = %v, want the populate failure alone", err)
	}
	if probe.revokes != 1 {
		t.Fatalf("revokes = %d, want 1", probe.revokes)
	}
	if !probe.present || !probe.lock {
		t.Errorf("during the revoke: claim present=%v locked=%v, want both: the name must stay taken until the token is revoked", probe.present, probe.lock)
	}
	if present, locked := claimState(t, probe.town); present || locked {
		t.Errorf("after the rollback: claim present=%v locked=%v, want neither", present, locked)
	}
}

// TestOpenshellHalfMadeRollbackClaimDeleteFails: a failed delete leaves the
// claim file in place for the sandbox that may still exist, frees its lock, and
// revokes nothing (gt-5j8bcy).
func TestOpenshellHalfMadeRollbackClaimDeleteFails(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"delete": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "connection refused"},
	}
	b, probe, claim, spec := rollbackClaimSetup(t, fake)
	cause := errors.New("populate failed")

	err := b.rollback(spec, cause, nil, claim)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "rolling back sandbox") {
		t.Fatalf("rollback = %v, want the populate failure with the rollback named", err)
	}
	if probe.revokes != 0 {
		t.Errorf("revokes = %d, want 0: the sandbox may still exist", probe.revokes)
	}
	if present, locked := claimState(t, probe.town); !present || locked {
		t.Errorf("after the rollback: claim present=%v locked=%v, want present and unlocked", present, locked)
	}
}

// TestOpenshellHalfMadeRollbackClaimRemovalFailureIsNamed: a claim that cannot
// be removed is named in the rollback's error, and its lock is free all the same.
// A directory at the claim path makes the removal fail.
func TestOpenshellHalfMadeRollbackClaimRemovalFailureIsNamed(t *testing.T) {
	t.Parallel()
	b, probe, claim, spec := rollbackClaimSetup(t, &sandbox.Fake{})
	claimPath := sandboxClaimPath(probe.town, openshellTestSession)
	must(t, os.Remove(claimPath))
	must(t, os.MkdirAll(filepath.Join(claimPath, "held"), 0o755))
	cause := errors.New("populate failed")

	err := b.rollback(spec, cause, nil, claim)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "rolling back sandbox") {
		t.Fatalf("rollback = %v, want the populate failure with the rollback named", err)
	}
	if !strings.Contains(err.Error(), claimPath) {
		t.Errorf("rollback = %v, want the unremovable claim %s named", err, claimPath)
	}
	if probe.revokes != 1 || !probe.lock {
		t.Errorf("revokes = %d, lock during revoke = %v; want one revoke while the claim was held", probe.revokes, probe.lock)
	}
	if _, locked := claimState(t, probe.town); locked {
		t.Error("after the rollback: claim lock held, want it free")
	}
}
