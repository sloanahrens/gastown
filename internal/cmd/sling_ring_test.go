package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// slingRingKey is the key the sling path checks the Ring under in these tests.
var slingRingKey = []byte("sling-ring-key")

// TestSlingRing: the sling path refuses a rig under a Waned Ring and proceeds
// under a Valid one or under none. The check reads real ring files in a
// temporary town through ring.Hold, the same call production makes.
func TestSlingRing(t *testing.T) {
	t.Parallel()
	key := ring.KeyFunc(func(string) ([]byte, error) { return slingRingKey, nil })
	cases := []struct {
		name string
		// mint writes the town's ring; nil writes none, so the town has no
		// ring section. Absent = off lands with slice 9, after the proving step
		// (slice 8), so an unforged town still dispatches.
		mint    func(t *testing.T, town string)
		refused bool
	}{
		{name: "waned Ring refuses", refused: true,
			mint: func(t *testing.T, town string) { ringtest.Waned(t, town, slingRingKey, time.Now()) }},
		{name: "valid Ring proceeds",
			mint: func(t *testing.T, town string) { ringtest.Valid(t, town, slingRingKey, time.Now()) }},
		{name: "absent Ring proceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.mint != nil {
				tc.mint(t, town)
			}
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{})
			h.run.townRoot = town
			h.run.ringHold = func(townRoot string) string { return ring.Hold(townRoot, key, time.Now()) }

			p := executeParams()
			p.TownRoot = town
			res, err := h.run.executeSling(p)
			if !tc.refused {
				if err != nil {
					t.Fatalf("executeSling under %s: %v", tc.name, err)
				}
				h.wantCalls("spawn", "spawn gastown")
				return
			}
			wantSlingErr(t, err, "the Ring waned at")
			if res == nil || res.ErrMsg != "e-stop" {
				t.Fatalf("result = %+v, want the e-stop label", res)
			}
			h.wantNo("spawn")
			h.wantNo("hook")
		})
	}
}

// The rig target is refused under a Waned Ring before any polecat spawns, and
// names the renewal the operator runs.
func TestSlingRingRefusesRigTargetBeforeSpawn(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	ringtest.Waned(t, town, slingRingKey, time.Now())
	h := newSlingHarness(t)
	h.run.ringHold = func(townRoot string) string {
		return ring.Hold(townRoot, ring.KeyFunc(func(string) ([]byte, error) { return slingRingKey, nil }), time.Now())
	}
	_, err := h.run.resolveSlingTarget("gastown", ResolveTargetOptions{TownRoot: town, NoBoot: true})
	wantSlingErr(t, err, "the Ring waned at")
	if !strings.Contains(err.Error(), "gt ring renew") {
		t.Errorf("refusal %q does not name the renewal", err)
	}
	h.wantNo("spawn")
	h.wantNo("admit")
}
