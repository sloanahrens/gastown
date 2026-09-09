package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestDetectActorOutputsToleratedByTripwire cross-checks this package's Role
// enum (the enum backing detectActor(), which most agent-originated events
// pass to events.Log as the actor) against the hermetic test tripwire's
// known-actor set in internal/testutil.
//
// gt-9pn: that known-actor set used to be maintained purely by hand and went
// stale twice (gt-ro0 "unknown", gt-kvc "dog"), each time producing a false
// positive that could have blocked a legitimate merge. This test makes the
// two sides self-checking: if AllRoles() ever grows a Role whose
// RoleInfo.ActorString() the tripwire doesn't tolerate, this test fails
// here — at the point the actor is constructed — instead of surfacing later
// as an intermittent tripwire false positive on an unrelated branch.
func TestDetectActorOutputsToleratedByTripwire(t *testing.T) {
	known := make(map[string]bool)
	for _, p := range testutil.BuiltinActorPrefixes() {
		known[p] = true
	}

	for _, role := range AllRoles() {
		actor := RoleInfo{Role: role}.ActorString()
		if !known[actor] {
			t.Errorf("Role %q produces actor %q via RoleInfo.ActorString(), which "+
				"internal/testutil's BuiltinActorPrefixes does not tolerate — "+
				"add %q to builtinActorPrefixes in internal/testutil/hermetic.go",
				role, actor, actor)
		}
	}
}
