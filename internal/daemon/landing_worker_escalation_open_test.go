package daemon

import (
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// failedList is a store whose listing fails, as a store that is down does.
type failedList struct{ beads.Client }

func (failedList) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("store down")
}

// TestLandingEscalationOpen: the escalation check the daemon builds asks
// under the bead's key with the lane appended, so only that lane's open
// escalation reads open. A check that cannot read answers not open, so the
// worker raises it again and says so at every check (gt-il3out).
func TestLandingEscalationOpen(t *testing.T) {
	t.Parallel()
	const laneKey = "landing-needs-overseer:gt-1#polecat"
	townRoot := t.TempDir()
	promoteRigConfig(t, townRoot, "gastown", filepath.Join(t.TempDir(), "promote-gastown.key"))
	writeRigsJSON(t, townRoot, []string{"gastown"})
	d := newForgejoPromoteDaemon(t, townRoot, "gastown")
	rig := beadsfake.New()
	rig.Seed(beads.Issue{ID: "gt-1", Labels: []string{land.LabelNeedsOverseer}})
	d.landingBeads = rig
	for _, tc := range []struct {
		name, lane, filed, status string
		fail, want                bool
	}{
		{"closed for the lane reads closed", "polecat", laneKey, "closed", false, false},
		{"another lane's open reads closed", "polecat", "landing-needs-overseer:gt-1#refinery", "open", false, false},
		// Third: dropping the lane from the key reads this open one closed.
		{"open for the lane reads open", "polecat", laneKey, "open", false, true},
		{"a failed read answers not open", "polecat", "", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			esc := beadsfake.New()
			if tc.filed != "" {
				esc.Seed(beads.Issue{ID: "esc-1", Status: tc.status, Labels: []string{"gt:escalation", notify.FingerprintLabel(tc.filed)}})
			}
			d.landingEscalations = esc
			if tc.fail {
				d.landingEscalations = failedList{esc}
			}
			w, err := d.newRigLandingWorker("gastown")
			if err != nil {
				t.Fatal(err)
			}
			if got, err := w.EscalationOpen("gt-1", tc.lane); err != nil || got != tc.want {
				t.Errorf("EscalationOpen(gt-1, %s) = %v, %v; want %v, nil", tc.lane, got, err, tc.want)
			}
		})
	}
	// The failed read says so at every check, and the raise files under the
	// key the check reads.
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	d.landingEscalations = failedList{beadsfake.New()}
	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatal(err)
	}
	w.EscalationOpen("gt-1", "polecat")
	w.EscalationOpen("gt-1", "polecat")
	if n := strings.Count(logs.String(), "store down"); n != 2 {
		t.Errorf("the failed read was logged %d times; want 2", n)
	}
	w.Escalate("gt-1", "polecat", "rejected")
	got := d.notifier.(*notifyfake.Recorder).Escalations()
	if len(got) != 1 || got[0].Escalation.Fingerprint != laneKey {
		t.Errorf("escalations = %+v; want one under the lane's key", got)
	}
}
