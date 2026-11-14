package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

var specStaleSeatsNow = time.Date(2026, 11, 14, 19, 40, 0, 0, time.UTC)

// TestSpecStaleSeatsFailsOpenWhenTheListingDoesNotAnswer is the bead's rule for
// a gateway read that fails: the sling goes ahead as before, and the tick logs
// one line saying the check was skipped (gt-zapy97).
func TestSpecStaleSeatsFailsOpenWhenTheListingDoesNotAnswer(t *testing.T) {
	t.Parallel()
	traces := &specSlingTraces{}
	m := newSpecStaleSeats(traces, func() time.Time { return specStaleSeatsNow }, true)
	m.list = func(context.Context) ([]sandbox.Entry, error) {
		return nil, errors.New("the gateway did not answer")
	}

	if g := m.get(); g != nil {
		t.Fatalf("guard = %+v, want none: a failed listing must not hold a sling", g)
	}
	lines := traces.notices()
	if len(lines) != 1 || !strings.Contains(lines[0], "the gateway did not answer") {
		t.Errorf("notices = %q, want one line naming the failed listing", lines)
	}
}

// TestSpecStaleSeatsLogsTheListingOncePerTick: the listing is read and the
// stale sandboxes named once, however many slings and retries read it
// (gt-zapy97).
func TestSpecStaleSeatsLogsTheListingOncePerTick(t *testing.T) {
	t.Parallel()
	traces := &specSlingTraces{}
	m := newSpecStaleSeats(traces, func() time.Time { return specStaleSeatsNow }, true)
	reads := 0
	m.list = func(context.Context) ([]sandbox.Entry, error) {
		reads++
		return []sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, nil
	}

	if g := m.get(); g == nil {
		t.Fatal("guard is nil for a listing that answered")
	}
	m.get()

	if reads != 1 {
		t.Errorf("the gateway was read %d times, want 1", reads)
	}
	lines := traces.notices()
	if len(lines) != 1 || !strings.Contains(lines[0], "gt-dag") || !strings.Contains(lines[0], "Error") {
		t.Errorf("notices = %q, want one line naming the stale sandbox", lines)
	}
}

// TestSpecStaleSeatsLeavesAHostOnlyTownAlone: a town that names no sandbox
// profile runs no gateway, so the check asks it nothing and adds no line
// (gt-zapy97).
func TestSpecStaleSeatsLeavesAHostOnlyTownAlone(t *testing.T) {
	t.Parallel()
	traces := &specSlingTraces{}
	m := newSpecStaleSeats(traces, func() time.Time { return specStaleSeatsNow }, false)
	asked := false
	m.list = func(context.Context) ([]sandbox.Entry, error) {
		asked = true
		return nil, nil
	}

	if g := m.get(); g != nil {
		t.Errorf("guard = %+v, want none in a town with no sandbox profile", g)
	}
	if asked {
		t.Error("the gateway was listed in a town with no sandbox profile")
	}
	if lines := traces.notices(); len(lines) != 0 {
		t.Errorf("notices = %q, want none", lines)
	}
}

// TestSpecSlingSpawnCarriesTheStaleNames: the names the guard blocks reach the
// spawn's options, which is where the pool reads them (gt-zapy97).
func TestSpecSlingSpawnCarriesTheStaleNames(t *testing.T) {
	t.Parallel()
	var got SlingSpawnOptions
	spawn := func(_ string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		got = opts
		return &SpawnedPolecatInfo{}, nil
	}

	if _, err := specSlingSpawn(spawn, nil, nil, nil, []string{"dag"})("gastown", SlingSpawnOptions{}); err != nil {
		t.Fatalf("spawn error = %v", err)
	}
	if !slices.Equal(got.staleSeatNames, []string{"dag"}) {
		t.Errorf("spawn's stale seat names = %v, want [dag]", got.staleSeatNames)
	}
}

// TestSlingPassesOverAnIdleSeatWhoseSandboxIsStale: the dispatcher draws the
// next idle seat when the pool's first one is a name whose gateway sandbox is
// stale (gt-zapy97).
func TestSlingPassesOverAnIdleSeatWhoseSandboxIsStale(t *testing.T) {
	t.Parallel()
	const bead = "gt-stale"
	townRoot, seats := fullRig(t, 30)
	fake := &idlePolecatReuseFake{seats: seats}

	info, err := reuseOrRefuseAtDirCap(fake, fakeIdleReuseEnv(), townRoot, "rig", 30,
		SlingSpawnOptions{HookBead: bead, staleSeatNames: []string{seats[0].Name}},
		recordRespawnFn(t, townRoot, bead))

	if err != nil || info == nil {
		t.Fatalf("got (%+v, %v); want the next idle seat", info, err)
	}
	if info.PolecatName != seats[1].Name {
		t.Errorf("reused %q, want %q: the stale name must be passed over", info.PolecatName, seats[1].Name)
	}
	if len(fake.reuseNames) != 1 || fake.reuseNames[0] != seats[1].Name {
		t.Errorf("reuse was asked for %v, want only %q", fake.reuseNames, seats[1].Name)
	}
}
