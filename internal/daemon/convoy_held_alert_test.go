package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// heldAlerts records what trackBlockedHolds raises and clears.
type heldAlerts struct {
	raised  []string // alert keys, in order
	msgs    []string
	cleared []string
}

func newHeldTestManager() (*ConvoyManager, *heldAlerts) {
	a := &heldAlerts{}
	m := &ConvoyManager{}
	m.SetAlertHooks(func(key, source, message string) {
		a.raised = append(a.raised, key)
		a.msgs = append(a.msgs, message)
	}, func(reason string, keys ...string) error {
		a.cleared = append(a.cleared, keys...)
		return nil
	})
	return m, a
}

func heldConvoy(convoyID string, holds ...strandedHold) []strandedConvoyInfo {
	return []strandedConvoyInfo{{ID: convoyID, TrackedCount: 2, Held: holds}}
}

var danglingHold = strandedHold{
	Issue: "gt-work", Blocker: "oag-x", Cause: "unresolved",
	Reason: "blocks blocker oag-x unresolved in any rig",
}

// TestTrackBlockedHolds_PersistentHoldEscalatesOncePerWindow (gt-gg7w9): a
// bead held on every scan for the window produces one escalation, however many
// scans follow, and the message carries the blocker and the cause.
func TestTrackBlockedHolds_PersistentHoldEscalatesOncePerWindow(t *testing.T) {
	t.Parallel()
	m, a := newHeldTestManager()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	scans := []time.Duration{
		0, 30 * time.Second, 5 * time.Minute, // inside the window
		blockedHoldEscalationAfter - time.Second, // the last moment before it
		blockedHoldEscalationAfter,               // the window elapses: escalate
		blockedHoldEscalationAfter + 30*time.Second,
		time.Hour, 24 * time.Hour, // still held: nothing more
	}
	for i, d := range scans {
		m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0.Add(d))
		want := 0
		if d >= blockedHoldEscalationAfter {
			want = 1
		}
		if len(a.raised) != want {
			t.Fatalf("scan %d (+%s): %d escalations, want %d", i, d, len(a.raised), want)
		}
	}
	if a.raised[0] != blockedHoldAlertKey("gt-work") {
		t.Errorf("alert key = %q, want %q", a.raised[0], blockedHoldAlertKey("gt-work"))
	}
	for _, want := range []string{"gt-work", "oag-x", "unresolved", "hq-cv1"} {
		if !strings.Contains(a.msgs[0], want) {
			t.Errorf("message %q does not name %q", a.msgs[0], want)
		}
	}
	if len(a.cleared) != 0 {
		t.Errorf("a hold that never cleared closed an escalation: %v", a.cleared)
	}
}

// TestTrackBlockedHolds_TransientHoldNeverEscalates: a store that is down for
// a few scans and comes back produces nothing, and the next outage starts a
// fresh window rather than inheriting the first one's age.
func TestTrackBlockedHolds_TransientHoldNeverEscalates(t *testing.T) {
	t.Parallel()
	m, a := newHeldTestManager()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	unreadable := strandedHold{Issue: "gt-work", Blocker: "oag-x", Cause: "unreadable", Reason: "blocks blocker oag-x unreadable (connection refused)"}

	m.trackBlockedHolds(heldConvoy("hq-cv1", unreadable), t0)
	m.trackBlockedHolds(heldConvoy("hq-cv1", unreadable), t0.Add(6*time.Minute))
	m.trackBlockedHolds(heldConvoy("hq-cv1"), t0.Add(7*time.Minute)) // the store is back

	// Down again; 12 minutes after the first sighting but 5 into this outage.
	m.trackBlockedHolds(heldConvoy("hq-cv1", unreadable), t0.Add(12*time.Minute))
	m.trackBlockedHolds(heldConvoy("hq-cv1", unreadable), t0.Add(17*time.Minute))

	if len(a.raised) != 0 {
		t.Errorf("transient holds escalated: %v", a.msgs)
	}
	if len(a.cleared) != 0 {
		t.Errorf("nothing was raised, so nothing should be cleared: %v", a.cleared)
	}
}

// TestTrackBlockedHolds_ClearingClosesTheEscalationAndResetsTheWindow: a hold
// that was escalated and then lifts closes its escalation, and a later hold on
// the same bead is a new streak that escalates again after its own window.
func TestTrackBlockedHolds_ClearingClosesTheEscalationAndResetsTheWindow(t *testing.T) {
	t.Parallel()
	m, a := newHeldTestManager()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0)
	m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0.Add(blockedHoldEscalationAfter))
	if len(a.raised) != 1 {
		t.Fatalf("want 1 escalation after the window, got %d", len(a.raised))
	}

	m.trackBlockedHolds(heldConvoy("hq-cv1"), t0.Add(time.Hour)) // edge dropped
	if len(a.cleared) != 1 || a.cleared[0] != blockedHoldAlertKey("gt-work") {
		t.Errorf("cleared = %v, want the gt-work key once", a.cleared)
	}
	m.trackBlockedHolds(heldConvoy("hq-cv1"), t0.Add(time.Hour+time.Minute))
	if len(a.cleared) != 1 {
		t.Errorf("a bead that stays clear must not clear again, got %v", a.cleared)
	}

	m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0.Add(2*time.Hour))
	if len(a.raised) != 1 {
		t.Fatalf("a new hold escalated before its own window: %d escalations", len(a.raised))
	}
	m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0.Add(2*time.Hour+blockedHoldEscalationAfter))
	if len(a.raised) != 2 {
		t.Errorf("the new streak should escalate once its window elapses, got %d", len(a.raised))
	}
}

// TestTrackBlockedHolds_OneEscalationPerBead: a bead tracked by two convoys is
// one hold, and two beads are two.
func TestTrackBlockedHolds_OneEscalationPerBead(t *testing.T) {
	t.Parallel()
	m, a := newHeldTestManager()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	other := strandedHold{Issue: "gt-other", Blocker: "oag-y", Cause: "unresolved", Reason: "blocks blocker oag-y unresolved in any rig"}

	scan := append(heldConvoy("hq-cv1", danglingHold, other), heldConvoy("hq-cv2", danglingHold)...)
	m.trackBlockedHolds(scan, t0)
	m.trackBlockedHolds(scan, t0.Add(blockedHoldEscalationAfter))

	if len(a.raised) != 2 {
		t.Fatalf("want one escalation per bead (2), got %v", a.raised)
	}
	if a.raised[0] == a.raised[1] {
		t.Errorf("both escalations share key %q", a.raised[0])
	}
}

// TestTrackBlockedHolds_NoAlertHooksStillTracks: a manager with no sink (the
// log-only default) must not panic on a hold that outlasts the window.
func TestTrackBlockedHolds_NoAlertHooksStillTracks(t *testing.T) {
	t.Parallel()
	m := &ConvoyManager{}
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0)
	m.trackBlockedHolds(heldConvoy("hq-cv1", danglingHold), t0.Add(time.Hour))
	m.trackBlockedHolds(heldConvoy("hq-cv1"), t0.Add(2*time.Hour))
}

// TestStrandedConvoyInfo_HeldJSONParsing pins the wire shape shared with `gt
// convoy stranded --json`: the daemon escalates only what it can decode.
func TestStrandedConvoyInfo_HeldJSONParsing(t *testing.T) {
	t.Parallel()
	jsonStr := `[{"id":"hq-cv1","title":"C","tracked_count":2,"ready_count":1,"ready_issues":["gt-sib"],` +
		`"held":[{"issue":"gt-work","blocker":"oag-x","cause":"unresolved","reason":"blocks blocker oag-x unresolved in any rig"}]}]`
	var got []strandedConvoyInfo
	if err := json.Unmarshal([]byte(jsonStr), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || len(got[0].Held) != 1 || got[0].Held[0] != danglingHold {
		t.Errorf("decoded %+v, want one hold %+v", got, danglingHold)
	}
}
