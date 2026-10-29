package daemon

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// diskMaintHealthyDF is df -P output for a disk with room to spare.
const diskMaintHealthyDF = "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
	"/dev/vdb 1000000 100000 900000 10% /mnt/data\n"

// diskMaintNextDay moves the harness clock a day on, so a second pass is due.
func diskMaintNextDay(d *Daemon, days int) {
	day := diskMaintNow
	d.diskMaint.now = func() time.Time { return day.Add(time.Duration(days) * 24 * time.Hour) }
}

// diskMaintAlertIsOpen reports whether the recorder holds an open alert under
// the key, as gt escalate would after the firing.
func diskMaintAlertIsOpen(rec *notifyfake.Recorder, key string) bool {
	return len(rec.OpenEscalations(key)) == 1
}

// TestDiskMaintenanceRaisesAlertAtEighty: a disk at the raise threshold files
// an alert keyed diskmaint:<disk> from the patrol, and a disk just under it
// files nothing.
func TestDiskMaintenanceRaisesAlertAtEighty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pct  float64
		want bool
	}{
		{"at the threshold", 80, true},
		{"over it", 91, true},
		{"just below it", 79.9, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintTestDaemon(t)
			f.readings = []disks.Reading{{Name: "colima", UsedPct: tt.pct}}

			d.runDiskMaintenance()

			got := f.notify.Escalations()
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("escalations = %+v at %.1f%%, want none", got, tt.pct)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("escalations = %+v, want one", got)
			}
			e := got[0].Escalation
			if e.Fingerprint != "diskmaint:colima" {
				t.Errorf("escalation = %+v, want fingerprint diskmaint:colima", e)
			}
			// The sent alert names the patrol as its source in the title.
			if !strings.HasPrefix(e.Description, "disk_maintenance: ") || !strings.Contains(e.Description, "colima") {
				t.Errorf("description = %q, want a disk_maintenance title naming the disk", e.Description)
			}
		})
	}
}

// TestDiskMaintenanceClearsAlertBelowSeventy: an open usage alert survives a
// reading between the clear and raise thresholds and is cleared only once the
// disk is under ClearAt.
func TestDiskMaintenanceClearsAlertBelowSeventy(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 85}}

	d.runDiskMaintenance()
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Fatal("no alert open after a disk read at 85%, want diskmaint:mac")
	}

	// 75% is under the raise threshold but over the clear one: the alert stays.
	diskMaintNextDay(d, 1)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 75}}
	d.runDiskMaintenance()
	if clears := f.notify.Clears(); len(clears) != 0 {
		t.Fatalf("clears = %+v at 75%%, want none until the disk is under 70%%", clears)
	}
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Error("the alert closed without a reading under the clear threshold")
	}

	diskMaintNextDay(d, 2)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 65}}
	d.runDiskMaintenance()

	clears := f.notify.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears = %+v, want one", clears)
	}
	if got := clears[0].Fingerprints; len(got) != 1 || got[0] != "diskmaint:mac" {
		t.Errorf("cleared %v, want diskmaint:mac", got)
	}
	if diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Error("the alert is still open after a reading under the clear threshold")
	}
}

// TestDiskMaintenanceUnreadableDiskRaisesUnreadableAlert: a disk the patrol
// cannot read is not a quiet disk. It gets its own alert, and the pass refuses
// to plan with a keep it could not decide.
func TestDiskMaintenanceUnreadableDiskRaisesUnreadableAlert(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	f.readings = []disks.Reading{
		{Name: "mac", UsedPct: 40},
		{Name: "colima", Err: errors.New("ssh: connect: connection refused")},
	}

	d.runDiskMaintenance()

	if !diskMaintAlertIsOpen(f.notify, "diskmaint:colima:unreadable") {
		t.Errorf("escalations = %+v, want an open diskmaint:colima:unreadable alert",
			f.notify.Escalations())
	}
	if len(f.target.policies) != 0 {
		t.Errorf("planned with an unreadable disk, want no plan: %+v", f.target.policies)
	}
}

// TestDiskMaintenanceTightensKeepOverEighty: a disk at the raise threshold
// plans with the tightened keep; a roomier one plans with the configured base.
func TestDiskMaintenanceTightensKeepOverEighty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pct  float64
		want int
	}{
		{"over the threshold", 91, 3},
		{"at the threshold", 80, 3},
		{"room to spare", 70, diskMaintDefaultKeep},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintTestDaemon(t)
			f.readings = []disks.Reading{
				{Name: "mac", UsedPct: 40},
				{Name: "docker-desktop", UsedPct: tt.pct},
			}

			d.runDiskMaintenance()

			if len(f.target.policies) != 1 {
				t.Fatalf("planned %d times, want 1", len(f.target.policies))
			}
			if got := f.target.policies[0].Keep; got != tt.want {
				t.Errorf("keep = %d at %.1f%% used, want %d", got, tt.pct, tt.want)
			}
		})
	}
}

// TestDiskMaintenanceDiskAlertsDoNotNeedApplyMode: the alerts are an alarm,
// not a change: the default dry run raises them and still applies nothing.
func TestDiskMaintenanceDiskAlertsDoNotNeedApplyMode(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	d.patrolConfig = diskMaintEnabled("04:30", "") // dry-run is the default
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 85}}

	d.runDiskMaintenance()

	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Error("a dry run raised no alert for a disk at 85%")
	}
	if len(f.target.policies) != 1 || f.target.policies[0].Apply {
		t.Errorf("policies = %+v, want one planned as a dry run", f.target.policies)
	}
	if len(f.target.applied) != 0 {
		t.Errorf("applied %d plans in a dry run, want 0", len(f.target.applied))
	}
}

// TestDiskMaintenanceReadsTheThreeDisks: the production read path reads each
// watched disk exactly once through its own df reader, and a nearly full one
// tightens the planned keep — so the parsed share reaches the plan.
func TestDiskMaintenanceReadsTheThreeDisks(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	calls := map[string]int{}
	reader := func(name, out string) DFReader {
		return func(context.Context) ([]byte, error) {
			calls[name]++
			return []byte(out), nil
		}
	}
	d.diskMaint.readDisks = nil // the production read, with these df commands
	d.diskMaint.dfReaders = map[string]DFReader{
		"mac": reader("mac", "Filesystem 1024-blocks Used Available Capacity Mounted on\n"+
			"/dev/disk3s1 1000000 910000 90000 91% /\n"),
		"colima":         reader("colima", diskMaintHealthyDF),
		"docker-desktop": reader("docker-desktop", diskMaintHealthyDF),
	}

	d.runDiskMaintenance()

	for _, name := range []string{"mac", "colima", "docker-desktop"} {
		if calls[name] != 1 {
			t.Errorf("%s read %d times, want 1", name, calls[name])
		}
	}
	if len(f.target.policies) != 1 {
		t.Fatalf("planned %d times, want 1", len(f.target.policies))
	}
	if got := f.target.policies[0].Keep; got != 3 {
		t.Errorf("keep = %d with the mac disk at 91%%, want 3", got)
	}
}

// TestDiskMaintenanceUnparsableDFIsUnreadable: a read that fails and output
// that names no capacity both read as an unreadable disk, never as an empty
// one.
func TestDiskMaintenanceUnparsableDFIsUnreadable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		df   DFReader
	}{
		{"the read fails", func(context.Context) ([]byte, error) {
			return nil, errors.New("colima: exit status 1: cannot connect")
		}},
		{"the output names no capacity", func(context.Context) ([]byte, error) {
			return []byte("Filesystem Used Available Mounted on\n"), nil
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, f := diskMaintTestDaemon(t)
			d.diskMaint.readDisks = nil
			// Every watched disk is named: one left out runs its real df, and
			// the unit tier refuses the docker read (gt-dl4nv.16).
			healthy := func(context.Context) ([]byte, error) { return []byte(diskMaintHealthyDF), nil }
			d.diskMaint.dfReaders = map[string]DFReader{
				"mac":            tt.df,
				"colima":         healthy,
				"docker-desktop": healthy,
			}

			d.runDiskMaintenance()

			if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac:unreadable") {
				t.Errorf("escalations = %+v, want an open diskmaint:mac:unreadable alert",
					f.notify.Escalations())
			}
		})
	}
}

// TestDiskMaintenanceFailedClearStaysOpen: a clear the notifier refused leaves
// the key open, so the patrol raises or clears it again rather than forgetting
// an alert the town still holds.
func TestDiskMaintenanceFailedClearStaysOpen(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintTestDaemon(t)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 85}}
	d.runDiskMaintenance()
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Fatal("no alert open after a disk read at 85%, want diskmaint:mac")
	}

	f.notify.Fail(notifyfake.KindClear, errors.New("gt clear: exit status 1"))
	diskMaintNextDay(d, 1)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 65}}
	d.runDiskMaintenance()
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Fatal("a refused clear closed the alert: the patrol forgot what the town still holds")
	}

	f.notify.Fail(notifyfake.KindClear, nil)
	diskMaintNextDay(d, 2)
	d.runDiskMaintenance()
	if diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Error("the alert is still open after a readable pass under the clear threshold")
	}
}

// TestDiskMaintenanceFailedClearIsLogged: a clear the notifier refused is
// named in the patrol's log, so a key the town still holds is not dropped in
// silence. The raise half logs its own refusals, and the clear half does too.
func TestDiskMaintenanceFailedClearIsLogged(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 85}}
	d.runDiskMaintenance()
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Fatal("no alert open after a disk read at 85%, want diskmaint:mac")
	}

	f.notify.Fail(notifyfake.KindClear, errors.New("gt clear: exit status 1"))
	diskMaintNextDay(d, 1)
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 65}}
	d.runDiskMaintenance()

	if !strings.Contains(logs.String(), "disk_maintenance: alert diskmaint:mac was not cleared") {
		t.Errorf("log = %q, want the refused clear named with its key", logs.String())
	}
}

// TestDiskMaintenanceProductionDFVectors: the patrol measures each watched
// disk with its production command line, so a renamed container, profile or
// mount point fails here rather than in a pass that reads the wrong filesystem
// or none.
func TestDiskMaintenanceProductionDFVectors(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	d.diskMaint.readDisks = nil // the production read, with these df commands
	var ran [][]string
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		ran = append(ran, cmd.Args)
		return []byte(diskMaintHealthyDF), nil, nil
	}

	d.runDiskMaintenance()

	want := [][]string{
		{"df", "-P", "/"},
		{"colima", "ssh", "-p", "gt-sbx", "--", "df", "-P", "/mnt/lima-colima-gt-sbx"},
		{"docker", "exec", "forgejo-runner-daemon-1", "df", "-P", "/"},
	}
	if len(ran) != len(want) {
		t.Fatalf("ran %v, want the %d disks' df commands", ran, len(want))
	}
	for i := range want {
		if !slices.Equal(ran[i], want[i]) {
			t.Errorf("disk %d ran %v, want %v", i, ran[i], want[i])
		}
	}
}

// TestDiskMaintenanceUndeliveredAlertIsRaisedAgain: an alert the notifier
// refused is not recorded as raised, so the next pass raises it again instead
// of leaving the condition silently unannounced.
func TestDiskMaintenanceUndeliveredAlertIsRaisedAgain(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	clk := newFixedClock()
	d.clock = clk
	f.readings = []disks.Reading{{Name: "mac", UsedPct: 85}}
	f.notify.Fail(notifyfake.KindEscalate, errors.New("gt escalate: exit status 1"))

	// The failed delivery retries on the daemon's clock, so the test drives it.
	runOnClock(t, clk, time.Second, d.runDiskMaintenance)
	if diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Fatal("an undelivered alert is open: the patrol recorded a raise that never arrived")
	}
	if !strings.Contains(logs.String(), "diskmaint:mac") {
		t.Errorf("log = %q, want the undelivered alert named", logs.String())
	}

	f.notify.Fail(notifyfake.KindEscalate, nil)
	diskMaintNextDay(d, 1)
	runOnClock(t, clk, time.Second, d.runDiskMaintenance)

	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Errorf("escalations = %+v, want the alert raised on the pass after the failed delivery",
			f.notify.Escalations())
	}
}
