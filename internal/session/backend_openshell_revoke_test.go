package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/seattoken"
)

// seatTokenValue is the token a seeded seat file holds. It is never sent
// anywhere: the revoker this file substitutes answers with no tracker.
const seatTokenValue = "trk_seat-token-value"

// fixedIssuer hands out one token, for the file a test seeds.
type fixedIssuer struct{}

func (fixedIssuer) Issue(context.Context, string) (string, error) { return seatTokenValue, nil }

// revokeFake records the actors it was asked to revoke, and fails as set.
type revokeFake struct {
	err    error
	actors []string
}

func (f *revokeFake) Revoke(_ context.Context, actor string) (int, error) {
	f.actors = append(f.actors, actor)
	if f.err != nil {
		return 0, f.err
	}
	return 1, nil
}

// writeTrackerRoute puts rig on the town tracker in this town's route table.
func writeTrackerRoute(t *testing.T, townRoot, rig string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"prefix":"gt-","path":"%s/mayor/rig","backend":"tracker"}`+"\n", rig)
	if err := os.WriteFile(filepath.Join(dir, "routes.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedSeatToken writes the seat's token file the way a launch does, and
// returns its path.
func seedSeatToken(t *testing.T, townRoot, rig, polecat string) string {
	t.Helper()
	seat := seattoken.PolecatSeat(rig, polecat)
	path, err := seattoken.New(townRoot).Ensure(context.Background(), seat, fixedIssuer{})
	if err != nil {
		t.Fatalf("seeding the seat's token file: %v", err)
	}
	return path
}

// trackerSeatToken seeds the seat's token file and points the backend at it the
// way a launch's seat-token ask does, so a tracker-routed Prepare gets past
// staging the token and reaches the step under test.
func trackerSeatToken(t *testing.T, b *openshellBackend, town string) {
	t.Helper()
	path := seedSeatToken(t, town, openshellTestRig, openshellTestPolecat)
	b.seatToken = func(context.Context, intent.Seat) (string, error) { return path, nil }
}

func wantActors(rig, polecat string) []string {
	return []string{seattoken.PolecatSeat(rig, polecat).String()}
}

// Delete ends the tracker token of the seat the sandbox ran: the file goes and
// its actor is revoked once.
func TestOpenShellOpsDeleteRevokesTheSeatToken(t *testing.T) {
	t.Parallel()
	b, _ := opsBackend(t, &sandbox.Fake{}, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	writeTrackerRoute(t, b.townRoot, openshellTestRig)
	tokenPath := seedSeatToken(t, b.townRoot, openshellTestRig, openshellTestPolecat)
	revoker := &revokeFake{}
	b.revoker = revoker

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if want := wantActors(openshellTestRig, openshellTestPolecat); !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v", revoker.actors, want)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file stat = %v, want it gone", err)
	}
}

// A sandbox that is already gone counts as deleted, so its seat's token still
// ends.
func TestOpenShellOpsDeleteOfAGoneSandboxRevokesTheSeatToken(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"delete": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "Error: sandbox not found: gt-gastown-sandbox-ace"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	writeTrackerRoute(t, b.townRoot, openshellTestRig)
	tokenPath := seedSeatToken(t, b.townRoot, openshellTestRig, openshellTestPolecat)
	revoker := &revokeFake{}
	b.revoker = revoker

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete of a gone sandbox = %v, want nil", err)
	}
	if want := wantActors(openshellTestRig, openshellTestPolecat); !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v", revoker.actors, want)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file stat = %v, want it gone", err)
	}
}

// A rig on Dolt has no tracker token: Delete revokes nothing.
func TestOpenShellOpsDeleteOfADoltRigRevokesNothing(t *testing.T) {
	t.Parallel()
	b, _ := opsBackend(t, &sandbox.Fake{}, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	revoker := &revokeFake{}
	b.revoker = revoker

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(revoker.actors) != 0 {
		t.Errorf("revoked %v on a Dolt rig, want nothing", revoker.actors)
	}
}

// A failed revoke warns naming the seat and never fails the delete: the sandbox
// is gone, so the delete has happened.
func TestOpenShellOpsDeleteRevokeFailureWarnsAndDeletes(t *testing.T) {
	t.Parallel()
	b, _ := opsBackend(t, &sandbox.Fake{}, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	writeTrackerRoute(t, b.townRoot, openshellTestRig)
	tokenPath := seedSeatToken(t, b.townRoot, openshellTestRig, openshellTestPolecat)
	b.revoker = &revokeFake{err: errors.New("the tracker is down")}
	var warnings []string
	b.warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the warning to leave the delete a success", err)
	}
	if _, ok, _ := ReadSandboxRecord(b.townRoot, openshellTestSession); ok {
		t.Error("record survived a delete whose revoke failed")
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file stat = %v, want it gone even though the revoke failed", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], seattoken.PolecatSeat(openshellTestRig, openshellTestPolecat).String()) {
		t.Errorf("warnings = %q, want one naming the seat", warnings)
	}
}

// A failed populate deletes its sandbox through Delete, so the seat's token
// ends with it.
func TestOpenShellPrepareRollbackRevokesTheSeatToken(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Reply: map[string]string{
		"get":  openshellReadyReply,
		"exec": "gt version 1.2.1 (dev: main@" + openshellTestDriftedGT + ")\n",
	}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	writeTrackerRoute(t, town, openshellTestRig)
	trackerSeatToken(t, b, town)
	revoker := &revokeFake{}
	b.revoker = revoker

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
		t.Fatal("Prepare = nil, want the drifted image refused")
	}
	if want := wantActors(openshellTestRig, openshellTestPolecat); !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v after the rollback", revoker.actors, want)
	}
}

// A rollback whose sandbox delete fails reports the failure with its cause and
// revokes nothing: the sandbox may still exist.
func TestOpenShellPrepareRollbackReportsAFailedDelete(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{
		Reply:  map[string]string{"get": openshellReadyReply, "exec": "gt version 1.2.1 (dev: main@" + openshellTestDriftedGT + ")\n"},
		Err:    map[string]error{"delete": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "connection refused"},
	}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	writeTrackerRoute(t, town, openshellTestRig)
	trackerSeatToken(t, b, town)
	revoker := &revokeFake{}
	b.revoker = revoker

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "rolling back sandbox") {
		t.Fatalf("Prepare = %v, want the failed rollback reported with its cause", err)
	}
	if len(revoker.actors) != 0 {
		t.Errorf("revoked %v although the sandbox may still exist", revoker.actors)
	}
}

// A delete that failed leaves the sandbox in place, so its seat's token stays
// live for the retry.
func TestOpenShellOpsDeleteFailureRevokesNothing(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"delete": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "connection refused"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	writeTrackerRoute(t, b.townRoot, openshellTestRig)
	revoker := &revokeFake{}
	b.revoker = revoker

	if err := b.Delete(openshellTestSession); err == nil {
		t.Fatal("Delete = nil, want the failure")
	}
	if len(revoker.actors) != 0 {
		t.Errorf("revoked %v although the sandbox may still exist", revoker.actors)
	}
}
