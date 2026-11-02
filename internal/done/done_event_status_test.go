package done

import (
	"testing"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/role"
)

// recordDoneTestRun is a run with just what reportDone reads before it writes
// the agent bead: the event write is what the tests here read.
func recordDoneTestRun(seen *seenDoneEvent) *doneRun {
	return &doneRun{
		issueID: "gt-work",
		branch:  "polecat/dag/gt-work+abc",
		sender:  "gastown/polecats/dag",
		agent:   Agent{Role: role.Crew},
		env:     func(string) string { return "" },
		logEvent: func(eventType, actor string, payload map[string]interface{}) error {
			seen.eventType, seen.actor, seen.payload = eventType, actor, payload
			return nil
		},
	}
}

// seenDoneEvent is what a test's logEvent seam captured.
type seenDoneEvent struct {
	eventType string
	actor     string
	payload   map[string]interface{}
}

// gt done records the exit type in its done event, because the event is the
// only record of a run's end for an escalation, which submits nothing for the
// host to repair a lost event from (gt-1m9gr). The dispatch clock reads the
// status to end the bead's run (events.DispatchStart), so the next seat starts
// a fresh clock instead of being stopped on the previous seat's (gt-ntvac).
func TestReportDoneRecordsTheExitStatus(t *testing.T) {
	t.Parallel()
	for _, exitType := range []string{ExitCompleted, ExitEscalated, ExitDeferred} {
		t.Run(exitType, func(t *testing.T) {
			t.Parallel()
			var seen seenDoneEvent
			r := recordDoneTestRun(&seen)
			if err := reportDone(r, exitType); err != nil {
				t.Fatalf("reportDone(%s): %v", exitType, err)
			}
			if seen.eventType != events.TypeDone {
				t.Errorf("event type = %q, want %q", seen.eventType, events.TypeDone)
			}
			if seen.actor != r.sender {
				t.Errorf("actor = %q, want %q", seen.actor, r.sender)
			}
			if got := seen.payload["bead"]; got != r.issueID {
				t.Errorf("bead = %v, want %q", got, r.issueID)
			}
			if got := seen.payload["branch"]; got != r.branch {
				t.Errorf("branch = %v, want %q", got, r.branch)
			}
			if got := seen.payload["status"]; got != exitType {
				t.Errorf("status = %v, want %q: the event does not say how the turn ended", got, exitType)
			}
		})
	}
}

// The dispatch clock recognizes the exit statuses the payload writes, and the
// values are the ones gt done validates --status against, so the two packages
// cannot drift apart.
func TestDoneStatusesMatchTheExitTypes(t *testing.T) {
	t.Parallel()
	for exitType, status := range map[string]string{
		ExitCompleted: events.DoneStatusCompleted,
		ExitEscalated: events.DoneStatusEscalated,
		ExitDeferred:  events.DoneStatusDeferred,
	} {
		if exitType != status {
			t.Errorf("exit type %q does not match the event status %q", exitType, status)
		}
	}
}
