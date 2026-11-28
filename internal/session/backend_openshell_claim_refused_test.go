package session

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// refuseUnclaimedSetup is a town with a record and a seat token for the test
// session, and another writer's claim on it: the refused spawn holds neither.
func refuseUnclaimedSetup(t *testing.T, fake *sandbox.Fake) (*openshellBackend, *revokeFake, *sandboxClaim, string, SpawnSpec) {
	t.Helper()
	town, work := openshellTown(t)
	profile := openshellTestProfile
	profile.Gateway = "gw-own"
	b, _ := openshellTestBackend(town, profile, fake)
	writeOpsRecord(t, b)
	writeTrackerRoute(t, town, openshellTestRig)
	tokenPath := seedSeatToken(t, town, openshellTestRig, openshellTestPolecat)
	other, err := writeSandboxClaim(town, openshellTestSession, "another-writer")
	must(t, err)
	revoker := &revokeFake{}
	b.revoker = revoker
	return b, revoker, other, tokenPath, openshellSpawn(town, work)
}

// TestOpenshellHalfMadeRefuseUnclaimed: a spawn whose claim was refused deletes
// only the sandbox it created on its own gateway. Another writer's claim file
// and held lock, the record and the seat's token stay, and no token is revoked.
// A failed delete is named beside the cause (gt-5j8bcy).
func TestOpenshellHalfMadeRefuseUnclaimed(t *testing.T) {
	t.Parallel()
	cause := errors.New("claim refused")
	cases := []struct {
		name     string
		fake     *sandbox.Fake
		wantText string // text the error must name beside the cause; "" wants cause alone
	}{
		{name: "delete succeeds", fake: &sandbox.Fake{}},
		{
			name: "delete fails",
			fake: &sandbox.Fake{
				Err:    map[string]error{"delete": errors.New("exit status 1")},
				Stderr: map[string]string{"delete": "connection refused"},
			},
			wantText: "connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, revoker, other, tokenPath, spec := refuseUnclaimedSetup(t, tc.fake)
			claimPath := sandboxClaimPath(b.townRoot, openshellTestSession)
			claimBefore, err := os.ReadFile(claimPath)
			must(t, err)

			err = b.refuseUnclaimed(spec, cause, nil)
			if !errors.Is(err, cause) {
				t.Fatalf("refuseUnclaimed = %v, want the cause", err)
			}
			if tc.wantText == "" && err != cause {
				t.Fatalf("refuseUnclaimed = %v, want the cause alone", err)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("refuseUnclaimed = %v, want the failed delete named (%q)", err, tc.wantText)
			}

			calls := tc.fake.Calls()
			if len(calls) != 1 {
				t.Fatalf("sandbox CLI calls = %v, want the one delete", calls)
			}
			if del := calls[0]; del[len(del)-1] != openshellTestSession || !slices.Contains(del, "delete") || !slices.Contains(del, "gw-own") {
				t.Errorf("CLI call = %v, want the delete of %s on this gateway gw-own", del, openshellTestSession)
			}
			if len(revoker.actors) != 0 {
				t.Errorf("revoked %v, want nothing: another writer owns the seat", revoker.actors)
			}
			if _, err := os.Stat(tokenPath); err != nil {
				t.Errorf("seat token stat = %v, want it untouched", err)
			}
			want := SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}
			if rec, ok, err := ReadSandboxRecord(b.townRoot, openshellTestSession); err != nil || !ok || rec != want {
				t.Errorf("record after refuseUnclaimed = %+v (ok=%v, err=%v), want it unchanged %+v", rec, ok, err, want)
			}
			claimAfter, err := os.ReadFile(claimPath)
			must(t, err)
			if string(claimAfter) != string(claimBefore) {
				t.Errorf("claim file = %q, want it unchanged %q", claimAfter, claimBefore)
			}
			if present, locked := claimState(t, b.townRoot); !present || !locked {
				t.Errorf("claim present=%v locked=%v, want both: another writer still holds it", present, locked)
			}
			must(t, other.Unlock())
		})
	}
}
