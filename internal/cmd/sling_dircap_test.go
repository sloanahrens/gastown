package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

// fullRig is a town whose rig has n polecat directories, named p00, p01, ...,
// and the pool's seat for each.
func fullRig(t *testing.T, n int) (townRoot string, seats []*polecat.Polecat) {
	t.Helper()
	townRoot = t.TempDir()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("p%02d", i)
		if err := os.MkdirAll(filepath.Join(townRoot, "rig", "polecats", name), 0o755); err != nil {
			t.Fatal(err)
		}
		seats = append(seats, &polecat.Polecat{Name: name, ClonePath: "/town/rig/polecats/" + name + "/rig"})
	}
	return townRoot, seats
}

// respawnCountOf is the count a bead has now. RecordBeadRespawn adds one and
// returns the new count, so the count before it is one less.
func respawnCountOf(t *testing.T, townRoot, bead string) int {
	t.Helper()
	n, err := polecat.RecordBeadRespawn(townRoot, bead)
	if err != nil {
		t.Fatalf("RecordBeadRespawn: %v", err)
	}
	return n - 1
}

// recordRespawnFn is the recordRespawn prepareSlingPolecat hands the spawn
// paths: one count on the bead.
func recordRespawnFn(t *testing.T, townRoot, bead string) func() polecat.RespawnMark {
	return func() polecat.RespawnMark {
		mark, err := polecat.RecordBeadRespawnMark(townRoot, bead)
		if err != nil {
			t.Errorf("RecordBeadRespawnMark: %v", err)
			return polecat.RespawnMark{}
		}
		return mark
	}
}

// refuseSeats refuses every seat the way the reuse gate does once a concurrent
// sling has hooked it.
func refuseSeats(seats []*polecat.Polecat) map[string]error {
	refused := map[string]error{}
	for _, s := range seats {
		refused[s.Name] = fmt.Errorf("%w: hook_bead set", polecat.ErrPolecatNeedsRecovery)
	}
	return refused
}

// TestSlingReusesNextSeatWhenFirstIsTaken guards gt-pwn2s. A rig at the
// 30-directory cap has reusable seats, but parallel slings read the same first
// one and all but one lose it at the reuse gate (ErrPolecatNeedsRecovery once
// the winner has hooked it). The losers gave up on the pool and fell to the
// directory-cap refusal; they now take the next seat from the same read, and
// the sling that wins one is counted once.
func TestSlingReusesNextSeatWhenFirstIsTaken(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"
	townRoot, seats := fullRig(t, 30)
	fake := &idlePolecatReuseFake{seats: seats, reuseErrFor: refuseSeats(seats[:24])}

	info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
		SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

	if err != nil {
		t.Fatalf("sling refused with reusable seats in the rig: %v", err)
	}
	if info == nil || info.PolecatName != "p24" {
		t.Fatalf("got %+v; want the first seat that was not taken, p24", info)
	}
	if len(fake.reuseNames) != 25 {
		t.Errorf("reuse tried %d seats, want 25 (24 taken, then p24)", len(fake.reuseNames))
	}
	if got := respawnCountOf(t, townRoot, bead); got != 1 {
		t.Errorf("respawn count after the reuse = %d, want 1", got)
	}
}

// TestSlingRefusedAtDirCapRecordsNoRespawn: when every seat refuses and the rig
// is at the cap, the sling is refused with the cap error. The polecat never
// started, so the bead's respawn count is untouched.
func TestSlingRefusedAtDirCapRecordsNoRespawn(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"
	townRoot, seats := fullRig(t, 30)
	fake := &idlePolecatReuseFake{seats: seats, reuseErrFor: refuseSeats(seats)}
	if _, err := polecat.RecordBeadRespawn(townRoot, bead); err != nil {
		t.Fatal(err)
	}

	info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
		SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

	if err == nil || !strings.Contains(err.Error(), "30 polecat directories (max 30)") {
		t.Fatalf("err = %v; want the directory-cap refusal", err)
	}
	if info != nil {
		t.Errorf("sling returned %+v alongside the refusal", info)
	}
	if len(fake.reuseNames) != 30 {
		t.Errorf("reuse tried %d seats, want all 30", len(fake.reuseNames))
	}
	if got := respawnCountOf(t, townRoot, bead); got != 1 {
		t.Errorf("respawn count = %d after a cap refusal, want the 1 it had before", got)
	}
}

// TestSlingBelowDirCapLeavesAllocationToCaller: with room under the cap and no
// seat to reuse, the path hands back to the caller to allocate. Nothing has
// started, so nothing is counted.
func TestSlingBelowDirCapLeavesAllocationToCaller(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"
	townRoot, _ := fullRig(t, 5)

	info, err := reuseOrRefuseAtDirCap(&idlePolecatReuseFake{}, fakeIdleReuseEnv(), townRoot, "rig", 30,
		SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

	if err != nil || info != nil {
		t.Fatalf("got (%+v, %v); want (nil, nil) so the caller allocates", info, err)
	}
	if got := respawnCountOf(t, townRoot, bead); got != 0 {
		t.Errorf("respawn count = %d before any polecat started, want 0", got)
	}
}

// TestSlingReuseCountsTowardRespawnLimit: each reuse that starts a polecat
// records one count when it succeeds, and the bead is blocked once those reach
// the limit. This path cannot see whether a started polecat later dies, so the
// count does not depend on that. The cap refusal above adds nothing to the total.
func TestSlingReuseCountsTowardRespawnLimit(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"
	townRoot, seats := fullRig(t, 30)
	limit := config.DefaultRecoveryMaxBeadRespawns

	for i := 0; i < limit; i++ {
		if polecat.ShouldBlockRespawn(townRoot, bead) {
			t.Fatalf("blocked after %d started polecats, before the limit of %d", i, limit)
		}
		fake := &idlePolecatReuseFake{seats: seats[:1]}
		info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
			SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))
		if err != nil || info == nil {
			t.Fatalf("round %d: got (%+v, %v); want a reused polecat", i, info, err)
		}
	}
	if !polecat.ShouldBlockRespawn(townRoot, bead) {
		t.Errorf("not blocked after %d reused polecats", limit)
	}
}

// TestSlingTakesNextSeatWhenFirstIsNotFound: a seat that vanished between the
// read and the reuse (ErrPolecatNotFound) is passed over for the next reusable
// seat, the same as one the reuse gate refuses.
func TestSlingTakesNextSeatWhenFirstIsNotFound(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"
	townRoot, seats := fullRig(t, 30)
	fake := &idlePolecatReuseFake{seats: seats, reuseErrFor: map[string]error{
		"p00": polecat.ErrPolecatNotFound,
	}}

	info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
		SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

	if err != nil {
		t.Fatalf("sling refused after a vanished seat: %v", err)
	}
	if info == nil || info.PolecatName != "p01" {
		t.Fatalf("got %+v; want the next seat, p01", info)
	}
	if got := fake.reuseNames[:2]; got[0] != "p00" || got[1] != "p01" {
		t.Errorf("reuse order = %v, want p00 then p01", got)
	}
}

// TestSlingExhaustedSeatsRefusedOnlyAtCap: when every seat is gone or refused,
// the sling allocates when the rig has room and is refused with the cap error
// when it does not.
func TestSlingExhaustedSeatsRefusedOnlyAtCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		polecats  int
		wantCapOK bool
	}{
		{"below cap allocates", 5, true},
		{"at cap refuses", 30, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const bead = "gt-next"
			townRoot, seats := fullRig(t, tt.polecats)
			gone := map[string]error{}
			for _, s := range seats {
				gone[s.Name] = polecat.ErrPolecatNotFound
			}
			fake := &idlePolecatReuseFake{seats: seats, reuseErrFor: gone}

			info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
				SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

			if len(fake.reuseNames) != tt.polecats {
				t.Errorf("reuse tried %d seats, want all %d", len(fake.reuseNames), tt.polecats)
			}
			if info != nil {
				t.Errorf("sling returned %+v with no seat reused", info)
			}
			if tt.wantCapOK {
				if err != nil {
					t.Errorf("err = %v; want nil so the caller allocates", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "30 polecat directories (max 30)") {
				t.Errorf("err = %v; want the directory-cap refusal", err)
			}
		})
	}
}

// TestSlingRefusesWhenCapCannotBeRead: a polecats directory that exists but
// cannot be listed leaves the cap unknown, so the sling is refused rather than
// allocated past it. A rig with no polecats directory yet has no directories to
// count and is allocated normally.
func TestSlingRefusesWhenCapCannotBeRead(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"

	t.Run("unreadable polecats path refuses", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		rigDir := filepath.Join(townRoot, "rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A file where the directory should be: listing it fails, and the
		// failure is not "does not exist".
		if err := os.WriteFile(filepath.Join(rigDir, "polecats"), nil, 0o644); err != nil {
			t.Fatal(err)
		}

		info, err := reuseOrRefuseAtDirCap(&idlePolecatReuseFake{}, fakeIdleReuseEnv(), townRoot, "rig", 30,
			SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

		if err == nil || !strings.Contains(err.Error(), "cannot read polecat directories") {
			t.Fatalf("err = %v; want the unreadable-cap refusal", err)
		}
		if info != nil {
			t.Errorf("sling returned %+v alongside the refusal", info)
		}
	})

	t.Run("missing polecats directory allocates", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		info, err := reuseOrRefuseAtDirCap(&idlePolecatReuseFake{}, fakeIdleReuseEnv(), townRoot, "rig", 30,
			SlingSpawnOptions{HookBead: bead}, recordRespawnFn(t, townRoot, bead))

		if err != nil || info != nil {
			t.Fatalf("got (%+v, %v); want (nil, nil) so the caller allocates", info, err)
		}
	})
}

// TestSlingRetriedSeatGetsSameSlingOptions: when the first seat refuses and the
// sling moves to the next, the next seat's reuse carries the sling's own options
// (resume branch included), not anything left over from the first attempt.
func TestSlingRetriedSeatGetsSameSlingOptions(t *testing.T) {
	t.Parallel()
	const bead = "gt-next"
	const resume = "polecat/old/gt-next+abc123"
	townRoot, seats := fullRig(t, 30)
	fake := &idlePolecatReuseFake{seats: seats, reuseErrFor: refuseSeats(seats[:1])}

	info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
		SlingSpawnOptions{HookBead: bead, ResumeBranch: resume}, recordRespawnFn(t, townRoot, bead))

	if err != nil || info == nil || info.PolecatName != "p01" {
		t.Fatalf("got (%+v, %v); want p01 reused", info, err)
	}
	if len(fake.reuseOpts) != 2 {
		t.Fatalf("reuse called %d times, want 2", len(fake.reuseOpts))
	}
	first, second := fake.reuseOpts[0], fake.reuseOpts[1]
	if first != second {
		t.Errorf("options changed between seats: first %+v, retry %+v", first, second)
	}
	if second.HookBead != bead || second.ResumeBranch != resume {
		t.Errorf("retry options = %+v; want hook %q and resume %q", second, bead, resume)
	}
}
