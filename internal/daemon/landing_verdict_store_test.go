package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// verdictStoreLander builds a Forgejo rig's lander (review off when off is set) beside its alert recorder.
func verdictStoreLander(t *testing.T, off *bool) (*land.Lander, *notifyfake.Recorder) {
	t.Helper()
	townRoot := t.TempDir()
	const rigName = "testrig"
	forgejoRigConfig(t, townRoot, rigName)
	writeOverseerIdentity(t, townRoot, overseerTestName)
	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	rec := notifyfake.New()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: rec,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
			Forgejo: &config.ForgejoWorkerConfig{TokenDir: tokenDir},
			OMPath:  filepath.Join(t.TempDir(), "om"), Review: off}}}}
	forgejoLandingRemote(t, useGitfake(t, d), townRoot, rigName)
	w, err := d.newRigLandingWorker(rigName)
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	lander, ok := w.Lander.(*land.Lander)
	if !ok {
		t.Fatalf("Lander = %T, want *land.Lander", w.Lander)
	}
	return lander, rec
}

// TestVerdictUnstored: an unstored verdict reaches the overseer as one
// low-severity escalation naming rig, bead, head and error; review off files nothing.
func TestVerdictUnstored(t *testing.T) {
	t.Parallel()
	const bead, head = "gt-c789n9", "abc1234"
	writeErr := errors.New("verdict store: disk full")
	for _, tc := range []struct {
		name   string
		review *bool
	}{{"reviewing rig", nil}, {"review off", new(bool)}} {
		lander, rec := verdictStoreLander(t, tc.review)
		_, skip := lander.Reviewer.(land.SkipReviewer)
		if skip != (tc.review != nil) || skip && len(rec.Escalations()) != 0 {
			t.Fatalf("%s: Reviewer = %T is not the skip reviewer review off names, filing nothing", tc.name, lander.Reviewer)
		} else if skip {
			continue
		}
		rev, ok := lander.Reviewer.(land.OMReviewer)
		if !ok || rev.StoreFailed == nil {
			t.Fatalf("%s: Reviewer = %T, want the om reviewer with a store hook", tc.name, lander.Reviewer)
		}
		rev.StoreFailed(bead, head, writeErr)
		got := rec.Escalations()
		if len(got) != 1 {
			t.Fatalf("%s: escalations = %+v, want one", tc.name, got)
		}
		esc := got[0].Escalation
		if esc.Severity != "low" || esc.Fingerprint != "landing-verdict-unstored:"+bead || !strings.HasPrefix(esc.Description, "landing_worker: ") {
			t.Errorf("%s: escalation = %+v, want a low landing_worker alert keyed landing-verdict-unstored:%s", tc.name, esc, bead)
		}
		if !strings.Contains(esc.Reason, "testrig") || !strings.Contains(esc.Reason, bead) || !strings.Contains(esc.Reason, head) || !strings.Contains(esc.Reason, writeErr.Error()) {
			t.Errorf("%s: reason %q does not name the rig, bead, head and error", tc.name, esc.Reason)
		}
	}
}
