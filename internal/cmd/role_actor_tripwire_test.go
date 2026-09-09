package cmd

import (
	"strings"
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

// TestGetAgentIdentityOutputsToleratedByTripwire cross-checks this package's
// getAgentIdentity() — the SECOND, independent actor-construction path used
// by emitSessionEvent (internal/cmd/prime_session.go) to set the actor field
// on every session_start event — against the tripwire's known-actor set.
//
// gt-jna (CRITICAL regression in gt-9pn): TestDetectActorOutputsToleratedByTripwire
// above only ever cross-checked RoleInfo.ActorString(). gt-9pn wrongly assumed
// that was the only place agent-originated actor strings come from, and its
// fix replaced builtinActorPrefixes' literal "boot" entry with "deacon-boot"
// (ActorString()'s value for RoleBoot) on the theory that "boot" never
// matched anything real. It does: getAgentIdentity() returns bare "boot" for
// RoleBoot, and emitSessionEvent writes it to .events.jsonl on every `gt
// prime` Boot runs — 97 times in one night, at the daemon's boot-spawn
// cooldown cadence — which the "fixed" tripwire then flagged as leaks.
//
// This test only checks roles whose getAgentIdentity() output is a bare (no
// "/") string. Rig-qualified roles (polecat, crew, witness, refinery) embed
// a rig name that the tripwire validates against mayor/rigs.json instead of
// builtinActorPrefixes — asserting those here would require faking a
// registered rig and wouldn't exercise the mechanism this test guards.
func TestGetAgentIdentityOutputsToleratedByTripwire(t *testing.T) {
	known := make(map[string]bool)
	for _, p := range testutil.BuiltinActorPrefixes() {
		known[p] = true
	}

	for _, role := range AllRoles() {
		actor := getAgentIdentity(RoleContext{Role: role})
		if actor == "" || strings.Contains(actor, "/") {
			// Empty (RoleDog, RoleUnknown: never written as an event actor —
			// emitSessionEvent guards on actor == "") or rig-qualified: out
			// of scope for this test, see doc comment.
			continue
		}
		if !known[actor] {
			t.Errorf("Role %q produces actor %q via getAgentIdentity(), which "+
				"internal/testutil's BuiltinActorPrefixes does not tolerate — "+
				"add %q to builtinActorPrefixes in internal/testutil/hermetic.go",
				role, actor, actor)
		}
	}
}

// TestTripwireStillFlagsNovelActor guards against a fix to this tripwire
// silently widening its tolerance instead of narrowly correcting it: a
// completely unrecognized actor prefix must still fail the known-actor
// check that both TestDetectActorOutputsToleratedByTripwire and
// TestGetAgentIdentityOutputsToleratedByTripwire rely on.
func TestTripwireStillFlagsNovelActor(t *testing.T) {
	known := make(map[string]bool)
	for _, p := range testutil.BuiltinActorPrefixes() {
		known[p] = true
	}
	if known["totally-unknown-actor-gt-jna"] {
		t.Fatal("tripwire's known-actor set unexpectedly tolerates a novel, made-up actor prefix")
	}
}
