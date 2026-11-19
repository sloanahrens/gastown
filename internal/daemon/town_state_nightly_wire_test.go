package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/townbackup"
)

// wireTestDaemon is a daemon over a town the town-state export can read, with
// the patrol config the case needs and an alert recorder for the step's alert.
func wireTestDaemon(t *testing.T, patrol *DaemonPatrolConfig, logs *strings.Builder) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	rec := notifyfake.New()
	d := nightlyTestDaemon(nightlyTestTown(t), logs)
	d.patrolConfig = patrol
	d.notifier = rec
	return d, rec
}

// TestTownStateNightlyWireAbsentConfigAlerts is the first acceptance box's
// absent-config half: a config that is nil, or holds no scheduled_maintenance
// section, is an error that alerts under town_state_backup:nightly and names
// the setting, not the deliberate off state. A second run the same night leaves
// the one escalation open rather than filing another.
func TestTownStateNightlyWireAbsentConfigAlerts(t *testing.T) {
	t.Parallel()
	cases := map[string]*DaemonPatrolConfig{
		"no patrol config":                 nil,
		"no scheduled_maintenance section": {Patrols: &PatrolsConfig{}},
	}
	for name, patrol := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logs strings.Builder
			d, rec := wireTestDaemon(t, patrol, &logs)
			now := nightlyTestNow()

			if got := d.runTownStateBackupStep(now); got != townStateNightlyFailed {
				t.Fatalf("first run outcome = %v, want failed", got)
			}
			if got := d.runTownStateBackupStep(now); got != townStateNightlyFailed {
				t.Fatalf("second run outcome = %v, want failed", got)
			}

			open := rec.OpenEscalations(alertKeyTownStateNightly)
			if len(open) != 1 {
				t.Fatalf("open escalations under %s = %+v, want one after two runs the same night",
					alertKeyTownStateNightly, open)
			}
			if !strings.Contains(open[0].Reason, config.MaintenanceTownStateBackupRootKey) {
				t.Errorf("alert reason = %q, want it to name %s", open[0].Reason, config.MaintenanceTownStateBackupRootKey)
			}
			if !strings.Contains(open[0].Reason, "absent") {
				t.Errorf("alert reason = %q, want it to say the config is absent", open[0].Reason)
			}
			if !strings.Contains(logs.String(), "scheduled_maintenance patrol config is absent") {
				t.Errorf("log missing the absent-config line:\n%s", logs.String())
			}
		})
	}
}

// TestTownStateNightlyWireRootEmptyIsOffOncePerDay is the second acceptance
// box's off half, observed at the step rather than inferred from a silent
// alert: a failing night raises the step's alert, and the operator's next run
// with the root empty in the same present config reports the off outcome,
// clears that alert, files no new one, and says why once per day.
func TestTownStateNightlyWireRootEmptyIsOffOncePerDay(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "town-state")
	// A file where the root's directory belongs makes the night fail, which
	// raises the alert the off branch has to settle.
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	section := &ScheduledMaintenanceConfig{TownStateBackupRoot: root}
	var logs strings.Builder
	d, rec := wireTestDaemon(t, &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		ScheduledMaintenance: section,
	}}, &logs)
	day := nightlyTestNow()

	if got := d.runTownStateBackupStep(day); got != townStateNightlyFailed {
		t.Fatalf("failing run outcome = %v, want failed", got)
	}
	if open := rec.OpenEscalations(alertKeyTownStateNightly); len(open) != 1 {
		t.Fatalf("open escalations after a failing night = %+v, want one", open)
	}

	// The operator turns the step off in the same, present config.
	section.TownStateBackupRoot = ""
	for run := 0; run < 2; run++ {
		if got := d.runTownStateBackupStep(day); got != townStateNightlyOff {
			t.Fatalf("off run %d outcome = %v, want off", run+1, got)
		}
	}
	if open := rec.OpenEscalations(alertKeyTownStateNightly); len(open) != 0 {
		t.Errorf("open escalations after the step was turned off = %+v, want the earlier alert cleared", open)
	}
	if got := strings.Count(logs.String(), "scheduled_maintenance: town-state backup is off"); got != 1 {
		t.Errorf("off lines after two runs on one day = %d, want 1:\n%s", got, logs.String())
	}
	if got := len(rec.Escalations()); got != 1 {
		t.Errorf("escalations filed = %d, want only the failing night's one:\n%+v", got, rec.Escalations())
	}

	if got := d.runTownStateBackupStep(day.AddDate(0, 0, 1)); got != townStateNightlyOff {
		t.Fatalf("next-day outcome = %v, want off", got)
	}
	if got := strings.Count(logs.String(), "scheduled_maintenance: town-state backup is off"); got != 2 {
		t.Errorf("off lines after a second day = %d, want 2:\n%s", got, logs.String())
	}
	if open := rec.OpenEscalations(alertKeyTownStateNightly); len(open) != 0 {
		t.Errorf("the off step raised an escalation: %+v", open)
	}
}

// TestTownStateNightlyWireFailureAlertsThenSuccessClears is the third
// acceptance box: a failing step raises the alert under the new key naming the
// root, and a later success clears it.
func TestTownStateNightlyWireFailureAlertsThenSuccessClears(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "town-state")
	// A file where the root's directory belongs makes the night's MkdirAll
	// fail, which is the step's own failure path, not a config refusal.
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	d, rec := wireTestDaemon(t, &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		ScheduledMaintenance: &ScheduledMaintenanceConfig{TownStateBackupRoot: root},
	}}, &logs)
	now := nightlyTestNow()

	if got := d.runTownStateBackupStep(now); got != townStateNightlyFailed {
		t.Fatalf("failing run outcome = %v, want failed", got)
	}
	open := rec.OpenEscalations(alertKeyTownStateNightly)
	if len(open) != 1 {
		t.Fatalf("open escalations under %s = %+v, want one", alertKeyTownStateNightly, open)
	}
	if !strings.Contains(open[0].Reason, root) {
		t.Errorf("alert reason = %q, want it to name the root %s", open[0].Reason, root)
	}

	// The operator's fix: the path is free for the night's directory.
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if got := d.runTownStateBackupStep(now); got != townStateNightlyDone {
		t.Fatalf("repaired run outcome = %v, want done", got)
	}
	if open := rec.OpenEscalations(alertKeyTownStateNightly); len(open) != 0 {
		t.Errorf("open escalations after a success = %+v, want none", open)
	}
	if !townbackup.Taken(root, now) {
		t.Errorf("no town-state night under %s after a successful run", root)
	}
}

// TestTownStateNightlyWireRootComesFromDaemonJSON pins the setting's name
// through the loader the daemon reads: the step must take the root from
// patrols.scheduled_maintenance.town_state_backup_root, so a config carrying it
// loads (the decoder is strict) and the night lands under it.
func TestTownStateNightlyWireRootComesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := nightlyTestTown(t)
	root := t.TempDir()
	writeTownFile(t, town, "settings/config.json",
		`{"theme":"dark","daemon":{"patrols":{"scheduled_maintenance":{"town_state_backup_root":"`+root+`"}}}}`)
	var logs strings.Builder
	d := nightlyTestDaemon(town, &logs)
	d.notifier = notifyfake.New()
	d.patrolConfig = LoadPatrolConfig(town)
	if d.patrolConfig == nil {
		t.Fatalf("LoadPatrolConfig read no config; the daemon log:\n%s", logs.String())
	}

	if got := d.runTownStateBackupStep(nightlyTestNow()); got != townStateNightlyDone {
		t.Fatalf("outcome = %v, want done", got)
	}
	if !townbackup.Taken(root, nightlyTestNow()) {
		t.Errorf("no night under the configured root %s", root)
	}
}

// TestTownStateNightlyWireRefusedRoots takes the root values the step will not
// use: a relative path is refused with an alert (it would write under whatever
// directory the daemon was started in), and a blank value reads as the off
// state rather than as a root.
func TestTownStateNightlyWireRefusedRoots(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		root string
		want townStateNightlyOutcome
	}{
		"relative root is refused": {root: "backups/town-state", want: townStateNightlyFailed},
		"blank root is off":        {root: "   ", want: townStateNightlyOff},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logs strings.Builder
			d, rec := wireTestDaemon(t, &DaemonPatrolConfig{Patrols: &PatrolsConfig{
				ScheduledMaintenance: &ScheduledMaintenanceConfig{TownStateBackupRoot: tc.root},
			}}, &logs)

			if got := d.runTownStateBackupStep(nightlyTestNow()); got != tc.want {
				t.Fatalf("outcome = %v, want %v", got, tc.want)
			}
			open := rec.OpenEscalations(alertKeyTownStateNightly)
			switch tc.want {
			case townStateNightlyFailed:
				if len(open) != 1 || !strings.Contains(open[0].Reason, "absolute path") {
					t.Errorf("open escalations = %+v, want one naming the absolute-path refusal", open)
				}
			case townStateNightlyOff:
				if len(open) != 0 {
					t.Errorf("blank root escalated: %+v", open)
				}
			}
		})
	}
}

// TestTownStateNightlyWireDoltBackupFailureStillTakesTheNight is the fourth
// acceptance box: the Dolt backup's failure does not stop the town-state step,
// which runs straight after it in the maintenance cycle.
func TestTownStateNightlyWireDoltBackupFailureStillTakesTheNight(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.config = &Config{TownRoot: nightlyTestTown(t)}
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {mib, 0}}
	f.backupErr = map[string]error{"gt": errors.New("injected: backup")}
	root := t.TempDir()
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		ScheduledMaintenance: &ScheduledMaintenanceConfig{TownStateBackupRoot: root},
	}}

	d.startMaintenanceGC([]string{"gt"}, time.Time{}, nil, true)

	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "injected: backup") {
		t.Fatalf("Dolt backup escalations = %v, want the injected failure", f.escalations)
	}
	if !townbackup.Taken(root, gcTestNow) {
		t.Fatalf("no town-state night under %s after the Dolt backup failed", root)
	}
	if _, err := townbackup.Verify(filepath.Join(townbackup.Dir(root, gcTestNow), townbackup.ArchiveName)); err != nil {
		t.Errorf("the night's archive does not verify: %v", err)
	}
}
