package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/hostprobe"
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
		"colima":         reader("colima", diskMaintNodeStats(100000)),
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
				"colima":         func(context.Context) ([]byte, error) { return []byte(diskMaintNodeStats(100000)), nil },
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
// disk with its production command line, so a renamed container or mount point
// fails here rather than in a pass that reads the wrong filesystem or none.
func TestDiskMaintenanceProductionDFVectors(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	d.diskMaint.readDisks = nil // the production read, with these commands
	var ran [][]string
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		ran = append(ran, cmd.Args)
		if len(cmd.Args) > 4 && cmd.Args[3] == "get" && cmd.Args[4] == "nodes" {
			return []byte("node/kind-worker\n"), nil, nil
		}
		if len(cmd.Args) > 4 && cmd.Args[3] == "get" && cmd.Args[4] == "--raw" {
			return []byte(diskMaintNodeStats(100000)), nil, nil
		}
		return []byte(diskMaintHealthyDF), nil, nil
	}

	d.runDiskMaintenance()

	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, d.diskMaint.getenv)
	want := [][]string{
		{"df", "-P", "/"},
		{"kubectl", "--kubeconfig", kubeconfig, "get", "nodes", "-o", "name"},
		{"kubectl", "--kubeconfig", kubeconfig, "get", "--raw", "/api/v1/nodes/kind-worker/proxy/stats/summary"},
		{"docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/"},
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

// diskMaintNodeStats is the kubelet stats summary of a node whose filesystem
// holds 160000 bytes, used as given.
func diskMaintNodeStats(used int64) string {
	return fmt.Sprintf(`{"node":{"fs":{"capacityBytes":160000,"usedBytes":%d}}}`, used)
}

// TestDiskMaintSandboxDiskReadsNodeStats: the sandbox disk is the kubelet node
// stats' worst node, read through kubectl with the patrol's kubeconfig; any
// node the cluster lists whose stats cannot be read, or a node listing that
// cannot be trusted, is an unreadable disk rather than a lower reading.
func TestDiskMaintSandboxDiskReadsNodeStats(t *testing.T) {
	t.Parallel()
	type sandboxRead struct {
		container    bool
		nodes        string
		nodesErr     error
		summaries    map[string]string
		summaryErrs  map[string]error
		wantPct      float64
		wantErr      bool
		wantErrNames []string
	}
	cases := map[string]sandboxRead{
		"the busier node wins": {
			nodes:     "node/kind-worker\nnode/kind-worker-2\n",
			summaries: map[string]string{"kind-worker": diskMaintNodeStats(80000), "kind-worker-2": diskMaintNodeStats(120000)},
			wantPct:   75,
		},
		"the busier node wins listed second to last": {
			nodes:     "node/kind-worker-2\nnode/kind-worker\n",
			summaries: map[string]string{"kind-worker": diskMaintNodeStats(120000), "kind-worker-2": diskMaintNodeStats(80000)},
			wantPct:   75,
		},
		"a runner error on the first node": {
			nodes:        "node/kind-worker\nnode/kind-worker-2\n",
			summaries:    map[string]string{"kind-worker-2": diskMaintNodeStats(120000)},
			summaryErrs:  map[string]error{"kind-worker": errors.New("the connection to the server was refused")},
			wantErr:      true,
			wantErrNames: []string{"kind-worker"},
		},
		"a runner error on the second node": {
			nodes:        "node/kind-worker\nnode/kind-worker-2\n",
			summaries:    map[string]string{"kind-worker": diskMaintNodeStats(80000)},
			summaryErrs:  map[string]error{"kind-worker-2": errors.New("the connection to the server was refused")},
			wantErr:      true,
			wantErrNames: []string{"kind-worker-2"},
		},
		"a summary the parser refuses": {
			nodes:        "node/kind-worker\n",
			summaries:    map[string]string{"kind-worker": `{"node":{"nodeName":"kind-worker"}}`},
			wantErr:      true,
			wantErrNames: []string{"kind-worker", "node.fs"},
		},
		"a node name that is not a name": {
			nodes:        "node/../etc\n",
			summaries:    map[string]string{"../etc": diskMaintNodeStats(120000)},
			wantErr:      true,
			wantErrNames: []string{"../etc", "not a node name"},
		},
		"a listing line that is not a node": {
			nodes:        "kind-worker\n",
			summaries:    map[string]string{"kind-worker": diskMaintNodeStats(120000)},
			wantErr:      true,
			wantErrNames: []string{"kind-worker", "node/name"},
		},
		"the cluster lists no nodes": {
			nodes:        "\n\n",
			wantErr:      true,
			wantErrNames: []string{"no nodes"},
		},
		"the node listing fails": {
			nodesErr:     errors.New("the connection to the server was refused"),
			wantErr:      true,
			wantErrNames: []string{"nodes"},
		},
		// In the town's container the host's kubeconfig names a loopback the
		// container cannot reach; the read must use the derived one.
		"container mode uses the container kubeconfig": {
			container: true,
			nodes:     "node/kind-worker\n",
			summaries: map[string]string{"kind-worker": diskMaintNodeStats(120000)},
			wantPct:   75,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			d := &Daemon{config: &Config{TownRoot: town}}
			d.diskMaint.getenv = func(key string) string {
				if tc.container && key == agentconfig.ContainerModeEnv {
					return agentconfig.SupervisorContainer
				}
				return ""
			}
			healthy := func(context.Context) ([]byte, error) { return []byte(diskMaintHealthyDF), nil }
			d.diskMaint.dfReaders = map[string]DFReader{"mac": healthy, "docker-desktop": healthy}

			var kubectl [][]string
			d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
				args := cmd.Args
				if args[0] != "kubectl" {
					return nil, []byte("unexpected command"), fmt.Errorf("unexpected command %q", args)
				}
				kubectl = append(kubectl, args)
				switch {
				case len(args) > 4 && args[3] == "get" && args[4] == "nodes":
					return []byte(tc.nodes), nil, tc.nodesErr
				case len(args) > 4 && args[3] == "get" && args[4] == "--raw":
					node := strings.TrimSuffix(strings.TrimPrefix(args[5], "/api/v1/nodes/"), "/proxy/stats/summary")
					if err := tc.summaryErrs[node]; err != nil {
						return nil, []byte(err.Error()), err
					}
					summary, ok := tc.summaries[node]
					if !ok {
						return nil, []byte("no summary"), fmt.Errorf("no summary for node %s", node)
					}
					return []byte(summary), nil, nil
				}
				return nil, []byte("unexpected kubectl"), fmt.Errorf("unexpected kubectl %q", args)
			}

			readings := d.diskMaintenanceSeams().readDisks(context.Background())

			var got disks.Reading
			for _, r := range readings {
				if r.Name == "colima" {
					got = r
				}
			}
			if tc.wantErr {
				if got.Err == nil {
					t.Fatalf("sandbox disk reading = %.1f%%, want an error", got.UsedPct)
				}
				for _, want := range tc.wantErrNames {
					if !strings.Contains(got.Err.Error(), want) {
						t.Errorf("reading error %q does not name %s", got.Err, want)
					}
				}
			} else {
				if got.Err != nil {
					t.Fatalf("sandbox disk reading = error %v, want %.1f%%", got.Err, tc.wantPct)
				}
				if got.UsedPct != tc.wantPct {
					t.Fatalf("sandbox disk reading = %.1f%%, want %.1f%%", got.UsedPct, tc.wantPct)
				}
			}

			wantKubeconfig := filepath.Join(town, ".runtime", "sbx-k8s", "kubeconfig")
			if tc.container {
				wantKubeconfig = filepath.Join(town, ".runtime", "container", "kubeconfig.yaml")
			}
			if len(kubectl) == 0 {
				t.Fatal("the sandbox disk ran no kubectl")
			}
			for _, args := range kubectl {
				if len(args) < 3 || args[1] != "--kubeconfig" || args[2] != wantKubeconfig {
					t.Errorf("kubectl ran %q, want --kubeconfig %s", args, wantKubeconfig)
				}
			}
		})
	}
}

// diskMaintContainerDaemon is a daemon in container mode whose sandbox disk
// reads healthily and whose exec seam refuses every command: the Mac and
// Docker Desktop readings must come from the probe file, never from df.
//
// The sandbox disk is injected with a kubelet stats summary, the reading its
// parser takes, so the pass reads it without running kubectl.
func diskMaintContainerDaemon(t *testing.T) (*Daemon, *bytes.Buffer, *diskMaintFakes) {
	t.Helper()
	d, logs, f := diskMaintTestDaemon(t)
	d.containerMode = true
	d.diskMaint.readDisks = nil // the production read
	d.diskMaint.dfReaders = map[string]DFReader{
		"colima": func(context.Context) ([]byte, error) { return []byte(diskMaintNodeStats(16000)), nil },
	}
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
		return nil, nil, errors.New("unexpected command: a host disk must not run df in container mode")
	}
	return d, logs, f
}

// writeDiskMaintProbe publishes a probe reading where the host agent writes it.
func writeDiskMaintProbe(t *testing.T, town string, p hostprobe.Probes) {
	t.Helper()
	path := hostprobe.Path(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the probe directory: %v", err)
	}
	if err := hostprobe.Write(path, p); err != nil {
		t.Fatalf("write the probe file: %v", err)
	}
}

// TestDiskMaintHostReadingsFromProbes: in container mode the Mac volume and the
// Docker Desktop disk come from a fresh probe file, and both readings reach the
// alerts and the keep.
func TestDiskMaintHostReadingsFromProbes(t *testing.T) {
	t.Parallel()
	d, _, f := diskMaintContainerDaemon(t)
	writeDiskMaintProbe(t, d.config.TownRoot, hostprobe.Probes{
		At:         diskMaintNow,
		Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 91}},
		DockerDisk: hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 82}},
	})

	d.runDiskMaintenance()

	if !diskMaintAlertIsOpen(f.notify, "diskmaint:mac") {
		t.Errorf("escalations = %+v, want an open diskmaint:mac alert from the probe reading",
			f.notify.Escalations())
	}
	if !diskMaintAlertIsOpen(f.notify, "diskmaint:docker-desktop") {
		t.Errorf("escalations = %+v, want an open diskmaint:docker-desktop alert from the probe reading",
			f.notify.Escalations())
	}
	if len(f.target.policies) != 1 {
		t.Fatalf("planned %d times, want 1", len(f.target.policies))
	}
	if got := f.target.policies[0].Keep; got != 3 {
		t.Errorf("keep = %d with the Mac volume at 91%% in the probe, want 3", got)
	}
}

// TestDiskMaintHostDisksUnknownIsNotAnError: a stale file, an absent file and a
// field the agent recorded as failed each leave the two readings out, log the
// unknown line once, raise no unreadable alert and let the pass plan without
// them (gt-1pyid).
func TestDiskMaintHostDisksUnknownIsNotAnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stage func(t *testing.T, town string)
	}{
		{"a stale file", func(t *testing.T, town string) {
			writeDiskMaintProbe(t, town, hostprobe.Probes{
				At:         diskMaintNow.Add(-fileHostProbeMaxAge - time.Second),
				Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 95}},
				DockerDisk: hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 95}},
			})
		}},
		{"an absent file", func(*testing.T, string) {}},
		{"a failed field", func(t *testing.T, town string) {
			writeDiskMaintProbe(t, town, hostprobe.Probes{
				At:         diskMaintNow,
				Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 95}},
				DockerDisk: hostprobe.Field[hostprobe.Disk]{Err: "docker exec gt-dolt df: exit status 1"},
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, logs, f := diskMaintContainerDaemon(t)
			tc.stage(t, d.config.TownRoot)

			d.runDiskMaintenance()

			if got := strings.Count(logs.String(), "disk_maintenance: host disks unknown:"); got != 1 {
				t.Errorf("logged %d host-disks-unknown lines, want 1: %q", got, logs.String())
			}
			for _, key := range []string{
				"diskmaint:mac", "diskmaint:mac:unreadable",
				"diskmaint:docker-desktop", "diskmaint:docker-desktop:unreadable",
			} {
				if diskMaintAlertIsOpen(f.notify, key) {
					t.Errorf("an alert is open under %s for a disk the agent did not report", key)
				}
			}
			if len(f.target.policies) != 1 {
				t.Fatalf("planned %d times, want the pass to plan without the host disks", len(f.target.policies))
			}
		})
	}
}

// TestDiskMaintContainerModeRunsNoHostEngineCommand: a reading pass in
// container mode never asks the host's container engine client or its VM tool
// for a disk. The Mac and Docker Desktop disks come from the host agent's probe
// file and the sandbox disk from the cluster's node stats, so a fresh probe and
// a fake kubectl alone read all three.
func TestDiskMaintContainerModeRunsNoHostEngineCommand(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	d.containerMode = true
	d.diskMaint.readDisks = nil // the production read
	d.diskMaint.dfReaders = nil
	writeDiskMaintProbe(t, d.config.TownRoot, hostprobe.Probes{
		At:         diskMaintNow,
		Disk:       hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 62}},
		DockerDisk: hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 71}},
	})

	var ran []string
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		program := filepath.Base(cmd.Args[0])
		ran = append(ran, program)
		switch program {
		case "docker", "colima", "limactl":
			return nil, []byte(program + " is not available in the town's container"),
				fmt.Errorf("%s must not run in container mode", program)
		case "kubectl":
			switch {
			case len(cmd.Args) > 4 && cmd.Args[3] == "get" && cmd.Args[4] == "nodes":
				return []byte("node/kind-worker\n"), nil, nil
			case len(cmd.Args) > 4 && cmd.Args[3] == "get" && cmd.Args[4] == "--raw":
				return []byte(diskMaintNodeStats(120000)), nil, nil
			}
		}
		return nil, []byte("unexpected command"), fmt.Errorf("unexpected command %q", cmd.Args)
	}

	readings := d.diskMaintenanceSeams().readDisks(context.Background())

	want := map[string]float64{"mac": 62, "colima": 75, "docker-desktop": 71}
	if len(readings) != len(want) {
		t.Fatalf("read %d disks (%+v), want %d", len(readings), readings, len(want))
	}
	for _, r := range readings {
		pct, ok := want[r.Name]
		if !ok {
			t.Errorf("read an unexpected disk %q", r.Name)
			continue
		}
		if r.Err != nil {
			t.Errorf("disk %s = error %v, want %.0f%%", r.Name, r.Err, pct)
			continue
		}
		if r.UsedPct != pct {
			t.Errorf("disk %s = %.0f%%, want %.0f%%", r.Name, r.UsedPct, pct)
		}
	}
	for _, program := range ran {
		if program != "kubectl" {
			t.Errorf("the reading pass ran %s; in container mode only the cluster's kubectl reads a disk", program)
		}
	}
}

// TestDiskMaintHostModeEngineDiskUsesTheProbeContainer: outside container mode
// the engine's disk is read by exec'ing into the container the host agent's
// probe measures, named once in hostprobe.
func TestDiskMaintHostModeEngineDiskUsesTheProbeContainer(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	d.diskMaint.readDisks = nil // the production read
	d.diskMaint.dfReaders = nil

	var dockerArgv []string
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		switch filepath.Base(cmd.Args[0]) {
		case "docker":
			dockerArgv = slices.Clone(cmd.Args)
			return []byte(diskMaintHealthyDF), nil, nil
		case "kubectl":
			if len(cmd.Args) > 4 && cmd.Args[3] == "get" && cmd.Args[4] == "nodes" {
				return []byte("node/kind-worker\n"), nil, nil
			}
			return []byte(diskMaintNodeStats(16000)), nil, nil
		}
		return []byte(diskMaintHealthyDF), nil, nil
	}

	d.diskMaintenanceSeams().readDisks(context.Background())

	want := []string{"docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/"}
	if !slices.Equal(dockerArgv, want) {
		t.Fatalf("the engine disk read ran %q, want %q", dockerArgv, want)
	}
}

// TestRunnerDefaultNamesTheProbeContainer: the patrol's non-container reading
// of the Docker Desktop disk execs into the container the host agent's probe
// measures, named once in hostprobe. It runs the reader diskMaintDisks builds,
// so a default changed at the call site is caught.
func TestRunnerDefaultNamesTheProbeContainer(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	var ran []string
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		ran = slices.Clone(cmd.Args)
		return []byte(diskMaintHealthyDF), nil, nil
	}

	for _, disk := range d.diskMaintDisks(filepath.Join("scratch", "kubeconfig")) {
		if disk.name != diskMaintDiskDockerDesktop {
			continue
		}
		if _, err := disk.df(context.Background()); err != nil {
			t.Fatalf("read the Docker Desktop disk: %v", err)
		}
		want := []string{"docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/"}
		if !slices.Equal(ran, want) {
			t.Fatalf("the Docker Desktop read ran %q, want %q", ran, want)
		}
		if hostprobe.DockerDiskContainer == "forgejo-runner-daemon-1" {
			t.Fatal("hostprobe.DockerDiskContainer is the retired runner container")
		}
		return
	}
	t.Fatal("the patrol has no Docker Desktop disk")
}

// TestRetiredRunnerNameIsGoneFromCode: the retired Docker Forgejo runner is
// named in no production file under internal, cmd, deploy or docs, so a default
// added outside the files this bead edited cannot hide. The whole tree is
// walked; docs/plans keeps the plan history and is exempt by name.
func TestRetiredRunnerNameIsGoneFromCode(t *testing.T) {
	t.Parallel()
	const retired = "forgejo-runner-daemon-1"
	root := filepath.Join("..", "..")
	exempt := filepath.Join(root, "docs", "plans")
	scanned := 0
	for _, dir := range []string{"internal", "cmd", "deploy", "docs"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path == exempt {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if strings.Contains(string(data), retired) {
				t.Errorf("%s names the retired container %s", path, retired)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned %d files; the walk is not rooted at the repository", scanned)
	}
}

// diskMaintEscalationsFor counts the escalations the patrol raised under key,
// so a test can tell one firing from a repeat under the same fingerprint.
func diskMaintEscalationsFor(rec *notifyfake.Recorder, key string) int {
	n := 0
	for _, c := range rec.Escalations() {
		if c.Escalation.Fingerprint == key {
			n++
		}
	}
	return n
}

// TestDiskMaintUnreadableIsRaisedOncePerOutage: a disk that stays unreadable
// raises one alert, repeats it in the log at most once an hour, and clears on
// the first pass that reads it; a later outage raises again.
func TestDiskMaintUnreadableIsRaisedOncePerOutage(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	// A target with its own cadence makes every pass due, so the outage is
	// observed pass by pass rather than once a day.
	d.patrolConfig = diskMaintCadence("registry", "5m")
	now := diskMaintNow
	d.diskMaint.now = func() time.Time { return now }
	unreadable := func() []disks.Reading {
		return []disks.Reading{
			{Name: "mac", UsedPct: 40},
			{Name: "colima", Err: errors.New("the sandbox cluster cannot be reached")},
			{Name: "docker-desktop", UsedPct: 40},
		}
	}
	f.readings = unreadable()

	// Five passes inside the first hour: the alert opens once and the log says
	// nothing more, because the raise is the notice.
	for i := 0; i < 5; i++ {
		d.runDiskMaintenance()
		now = now.Add(5 * time.Minute)
	}
	if got := diskMaintEscalationsFor(f.notify, "diskmaint:colima:unreadable"); got != 1 {
		t.Fatalf("raised the unreadable alert %d times in the first hour, want 1", got)
	}
	if got := strings.Count(logs.String(), "is still unreadable"); got != 0 {
		t.Errorf("logged the still-unreadable line %d times inside the first hour, want none: %q", got, logs.String())
	}

	// An hour after the raise, one reminder line; five minutes later, none.
	now = diskMaintNow.Add(diskMaintStillUnreadableInterval)
	d.runDiskMaintenance()
	if got := strings.Count(logs.String(), "is still unreadable"); got != 1 {
		t.Fatalf("logged the still-unreadable line %d times an hour into the outage, want 1: %q", got, logs.String())
	}
	now = now.Add(5 * time.Minute)
	d.runDiskMaintenance()
	if got := strings.Count(logs.String(), "is still unreadable"); got != 1 {
		t.Fatalf("logged the still-unreadable line %d times five minutes later, want the hourly line once: %q", got, logs.String())
	}
	if got := diskMaintEscalationsFor(f.notify, "diskmaint:colima:unreadable"); got != 1 {
		t.Fatalf("raised the unreadable alert %d times while it stayed open, want 1", got)
	}

	// The first pass that reads the disk clears the alert.
	now = now.Add(5 * time.Minute)
	f.readings = []disks.Reading{
		{Name: "mac", UsedPct: 40},
		{Name: "colima", UsedPct: 40},
		{Name: "docker-desktop", UsedPct: 40},
	}
	d.runDiskMaintenance()
	if diskMaintAlertIsOpen(f.notify, "diskmaint:colima:unreadable") {
		t.Error("the unreadable alert is still open after a pass read the disk")
	}

	// A second outage is a new outage, so it raises the alert again.
	now = now.Add(5 * time.Minute)
	f.readings = unreadable()
	d.runDiskMaintenance()
	if got := diskMaintEscalationsFor(f.notify, "diskmaint:colima:unreadable"); got != 2 {
		t.Errorf("raised the unreadable alert %d times across two outages, want 2", got)
	}
}

// TestDiskMaintUnreadableFailedRaiseLogsTheReset: when the notifier refuses an
// unreadable alert's raise, the next pass retries it and restarts the hourly
// clock. That reset pushes the reminder out, so it gets its own line rather
// than making the outage quieter each time the raise fails.
func TestDiskMaintUnreadableFailedRaiseLogsTheReset(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	clk := newFixedClock()
	d.clock = clk // the refused raise retries on this clock, so the test drives it
	d.patrolConfig = diskMaintCadence("registry", "5m")
	now := diskMaintNow
	d.diskMaint.now = func() time.Time { return now }
	f.readings = []disks.Reading{{Name: "colima", Err: errors.New("ssh: i/o timeout")}}
	f.notify.Fail(notifyfake.KindEscalate, errors.New("the notifier is down"))

	runOnClock(t, clk, time.Second, d.runDiskMaintenance)
	if got := strings.Count(logs.String(), "alert raised again"); got != 0 {
		t.Fatalf("logged the reset %d times on the first attempt, want none: %q", got, logs.String())
	}

	now = now.Add(5 * time.Minute)
	runOnClock(t, clk, time.Second, d.runDiskMaintenance)
	if got := strings.Count(logs.String(), "alert raised again"); got != 1 {
		t.Fatalf("logged the reset %d times after the refused raise was retried, want 1: %q", got, logs.String())
	}
}
