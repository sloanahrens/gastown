package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/events"
)

// The dispatch clock names the spec dispatcher by its actor string to tell its
// slings from a handoff's; the two spellings must stay one string, or the clock
// silently stops restarting a run that moved to another seat (gt-3ga5l).
func TestSpecDispatchActorMatchesTheDispatchClock(t *testing.T) {
	t.Parallel()
	if specDispatchActor != events.SpecDispatchActor {
		t.Errorf("specDispatchActor = %q, events.SpecDispatchActor = %q: they must be equal", specDispatchActor, events.SpecDispatchActor)
	}
}
