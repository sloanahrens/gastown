package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/jobledger"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// ciCacheTestContext is the control cluster context the fixture's runner names.
	ciCacheTestContext = "gt-control"
	// ciCacheTestPod is the runner pod the fixture's pod list holds.
	ciCacheTestPod = "forgejo-runner-7d9f8c-abcde"
	// ciCacheTestMinutes is the fixture's ci_cache_max_age, 12h, in minutes.
	ciCacheTestMinutes = "720"
	// ciCacheTestGocacheRoot is the ci-gocache volume's root, spelled out so the
	// argv test pins the path the trim has always execed into.
	ciCacheTestGocacheRoot = "/var/lib/docker/volumes/ci-gocache/_data"
	// ciCacheTestNpmCapKiB is the npm volume's cap, 1 GiB in KiB, as the capped
	// script's argv carries it.
	ciCacheTestNpmCapKiB = "1048576"
	// ciCacheTestGomodCapKiB is the ci-gomod volume's cap, 5 GiB in KiB, as the
	// module script's argv carries it.
	ciCacheTestGomodCapKiB = "5242880"
)

// ciCacheTestStart is the fixture's first instant: midday, outside the daily
// window, so the only trim due is the CI trim.
var ciCacheTestStart = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// ciCacheTestEvery is the CI trim's cadence the fixture pins: every volume's
// next attempt, after a verdict or a failure, is this far after its conclusion.
const ciCacheTestEvery = time.Hour

// ciCachePodsRunningJSON is one runner pod whose dind container is running.
const ciCachePodsRunningJSON = `{"items":[{"metadata":{"name":"` + ciCacheTestPod + `"},` +
	`"status":{"containerStatuses":[` +
	`{"name":"runner","state":{"running":{"startedAt":"2026-11-06T01:00:00Z"}}},` +
	`{"name":"dind","state":{"running":{"startedAt":"2026-11-06T01:00:00Z"}}}]}}]}`

// ciCachePodsDindWaitingJSON is the same pod with its dind container not yet up.
const ciCachePodsDindWaitingJSON = `{"items":[{"metadata":{"name":"` + ciCacheTestPod + `"},` +
	`"status":{"containerStatuses":[{"name":"dind","state":{"waiting":{"reason":"ContainerCreating"}}}]}}]}`

// ciCachePodsDindInitRunningJSON is the same pod with dind a native sidecar.
const ciCachePodsDindInitRunningJSON = `{"items":[{"metadata":{"name":"` + ciCacheTestPod + `"},` +
	`"status":{"initContainerStatuses":[` +
	`{"name":"dind","state":{"running":{"startedAt":"2026-11-06T01:00:00Z"}}}]}}]}`

// ciCachePodsDindDownJSON is the same pod with dind running in neither list.
const ciCachePodsDindDownJSON = `{"items":[{"metadata":{"name":"` + ciCacheTestPod + `"},` +
	`"status":{"containerStatuses":[{"name":"dind","state":{"waiting":{"reason":"ContainerCreating"}}}],` +
	`"initContainerStatuses":[{"name":"dind","state":{"terminated":{"exitCode":1}}}]}}]}`

// ciCacheFixture is the daemon a CI cache trim test runs: the patrol on, a
// kubernetes runner named on the tier sweep, kubectl answered in process, and a
// clock and a dispatch the test drives.
type ciCacheFixture struct {
	d    *Daemon
	cli  *fakeCLI
	logs *bytes.Buffer
	now  time.Time
	// notify records the escalations a failed volume's item sends.
	notify *notifyfake.Recorder
	// pending holds the passes the patrol dispatched and the test has not run.
	pending []func()
	// pods is the reply the pod list gets. exec is the reply the exec into the
	// ci-gocache volume gets, and npmExec and gomodExec the ones into the ci-npm
	// and ci-gomod volumes; each exec is answered by the root it names.
	pods      cliReply
	exec      cliReply
	npmExec   cliReply
	gomodExec cliReply
}

// newCICacheFixture builds the fixture for mode: a running runner pod and a
// reply the ci-gocache volume's exec gets, the apply's three figures or, in a
// dry run, a list reply, which a test may replace before it ticks. The npm and
// module volumes read under their caps by default, so they raise no item unless
// a test makes them.
func newCICacheFixture(t *testing.T, mode string) *ciCacheFixture {
	t.Helper()
	var logs bytes.Buffer
	f := &ciCacheFixture{
		logs:      &logs,
		now:       ciCacheTestStart,
		notify:    notifyfake.New(),
		pods:      cliReply{stdout: ciCachePodsRunningJSON},
		exec:      ciCacheGocacheExecReply(mode, 40<<20, 3, 10<<20),
		npmExec:   ciCacheReply(512<<10, 0, 512<<10),
		gomodExec: ciCacheReply(512<<10, 0, 512<<10),
	}
	f.cli = newFakeCLIFor(func(c cliCall) cliReply {
		verb := cacheTrimVerb(c.args)
		switch {
		case strings.HasPrefix(verb, "get pods "):
			return f.pods
		case strings.HasPrefix(verb, "exec "):
			switch {
			case slices.Contains(c.args, ciCacheVolumeRoot(ciCacheNpmVolume)):
				return f.npmExec
			case slices.Contains(c.args, ciCacheVolumeRoot(ciCacheGomodVolume)):
				return f.gomodExec
			}
			return f.exec
		}
		return cliReply{}
	})

	patrol := diskMaintEnabled("", mode)
	// The fixture pins the cadence it asserts, so the default cannot move it.
	patrol.Patrols.DiskMaintenance.CICacheTrimInterval = ciCacheTestEvery.String()
	patrol.Patrols.TierSweep = &agentconfig.TierSweepConfig{
		Enabled: true,
		Runner: &agentconfig.TierSweepRunnerConfig{
			Backend: agentconfig.TierSweepRunnerKubernetes,
			Context: ciCacheTestContext,
		},
	}
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&logs, "", 0),
		patrolConfig: patrol,
		notifier:     f.notify,
	}
	d.execCmd = f.cli.run
	d.maint.quiet = func(*Daemon) (bool, string) { return true, "" }
	d.diskMaint.now = func() time.Time { return f.now }
	d.diskMaint.getenv = func(string) string { return "" }
	d.diskMaint.environ = func() []string { return []string{"PATH=/usr/bin", "HOME=/home/polecat"} }
	d.diskMaint.targets = func(context.Context, *Daemon, string) (map[string]diskmaint.Target, error) {
		return map[string]diskmaint.Target{}, nil
	}
	// The cache trim ran a moment before these tests start, so it is not due for
	// two hours and stays out of their calls; the disk reads are stubbed for the
	// pass that runs when it comes due.
	d.diskMaint.trim = func(context.Context, string) bool { return true }
	d.diskMaint.readDisks = func(context.Context) []disks.Reading { return nil }
	d.diskMaintSchedule.record(diskMaintCacheTrimName, ciCacheTestStart)
	// The engine prune runs on the same patrol with a kubernetes runner, and these
	// tests count the CI cache trim's kubectl calls and log lines alone. Its
	// interval is held past their span, with its last run recorded at the start, as
	// the trim above is (gt-i02983).
	patrol.Patrols.DiskMaintenance.CIEnginePruneInterval = "1000h"
	d.diskMaintSchedule.record(ciEnginePruneName, ciCacheTestStart)
	// The sweep-runs prune is held the same way (gt-hhxczt).
	patrol.Patrols.DiskMaintenance.SweepRunsPruneInterval = "1000h"
	d.diskMaintSchedule.record(sweepRunsPruneName, ciCacheTestStart)
	d.diskMaint.dispatch = func(fn func()) { f.pending = append(f.pending, fn) }
	f.d = d
	return f
}

// ciCacheReply is the script's three-line reply for the given KiB figures.
func ciCacheReply(beforeKiB, removed, afterKiB int64) cliReply {
	return cliReply{stdout: fmt.Sprintf("before_kib=%d\nremoved=%d\nafter_kib=%d\n", beforeKiB, removed, afterKiB)}
}

// ciCacheGocacheExecReply is the reply a pass of mode gets from the ci-gocache
// volume's exec. An apply reads the three figures alone. A dry run reads the LRU
// list program's selection: one file line per removed file under the root, then
// the three figures, with the size after equal to the size before as the list
// program reports it. afterKiB is an apply's figure only.
func ciCacheGocacheExecReply(mode string, beforeKiB, removed, afterKiB int64) cliReply {
	if mode == agentconfig.DiskMaintenanceModeApply {
		return ciCacheReply(beforeKiB, removed, afterKiB)
	}
	var b strings.Builder
	for i := int64(0); i < removed; i++ {
		fmt.Fprintf(&b, "%s/%02x/file-%d\n", ciCacheTestGocacheRoot, i, i)
	}
	fmt.Fprintf(&b, "before_kib=%d\nremoved=%d\nafter_kib=%d\n", beforeKiB, removed, beforeKiB)
	return cliReply{stdout: b.String()}
}

// at moves the fixture's clock to offset after its first instant.
func (f *ciCacheFixture) at(offset time.Duration) {
	f.now = ciCacheTestStart.Add(offset)
}

// tick is one patrol tick.
func (f *ciCacheFixture) tick() {
	f.d.runDiskMaintenance()
}

// drain runs the passes dispatched so far, in dispatch order.
func (f *ciCacheFixture) drain() {
	for len(f.pending) > 0 {
		fn := f.pending[0]
		f.pending = f.pending[1:]
		fn()
	}
}

// tickAFloorLater moves the fixture's clock to a floor and a minute after now,
// then ticks and drains: the trim's cadence is shorter than the floor, so each
// volume is read again, more than a floor past the reading before.
func (f *ciCacheFixture) tickAFloorLater() {
	f.now = f.now.Add((ciCacheLRUFloorMinutes + 1) * time.Minute)
	f.tick()
	f.drain()
}

// execs returns the exec calls the fixture recorded.
func (f *ciCacheFixture) execs() []cliCall {
	return f.callsWith("exec ")
}

// callsWith returns the recorded calls whose verb begins with prefix.
func (f *ciCacheFixture) callsWith(prefix string) []cliCall {
	var out []cliCall
	for _, c := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(c.args), prefix) {
			out = append(out, c)
		}
	}
	return out
}

// volumeExecs counts the execs the fixture recorded into the named volume.
func volumeExecs(f *ciCacheFixture, name string) int {
	n := 0
	for _, c := range f.execs() {
		if slices.Contains(c.args, ciCacheVolumeRoot(name)) {
			n++
		}
	}
	return n
}

// TestCICacheTrimArgv: each volume's exec names the runner's dind container, the
// control context, the volume's path and the age in minutes; the ci-gocache argv
// is the LRU apply program at the default cap, and the capped ci-npm argv adds
// its cap; a dry run sends no deletion; and the kubectl calls name the
// kubeconfig and the context.
func TestCICacheTrimArgv(t *testing.T) {
	t.Parallel()
	t.Run("apply removes under each volume with the age from the setting", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.tick()
		f.drain()

		execs := f.execs()
		if len(execs) != len(ciCacheVolumes) {
			t.Fatalf("ran %d execs, want %d: one per volume", len(execs), len(ciCacheVolumes))
		}
		argv := execs[0].args
		wantPrefix := diskmaint.KubectlArgs(diskMaintKubeconfig(f.d.config.TownRoot, func(string) string { return "" }), ciCacheTestContext)
		if !slices.Equal(argv[:4], wantPrefix) {
			t.Fatalf("kubectl prefix = %q, want the kubeconfig and the control context %q", argv[:4], wantPrefix)
		}
		// The default ci_gocache_cap_bytes, 40 GiB, in KiB as the volume is sized.
		want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
			"sh", "-c", ciCacheLRUTrimScript, "ci-cache-trim", "apply", ciCacheTestGocacheRoot, ciCacheTestMinutes,
			strconv.Itoa(40 << 20), strconv.Itoa(ciCacheLRUFloorMinutes)}
		if !slices.Equal(argv[4:], want) {
			t.Errorf("exec argv = %q\nwant %q", argv[4:], want)
		}

		npm := execs[1].args
		wantNpm := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
			"sh", "-c", ciCacheCappedTrimScript, "ci-cache-trim", ciCacheVolumeRoot(ciCacheNpmVolume), ciCacheTestMinutes, ciCacheTestNpmCapKiB}
		if !slices.Equal(npm[4:], wantNpm) {
			t.Errorf("npm exec argv = %q\nwant %q", npm[4:], wantNpm)
		}
	})

	t.Run("dry run sends no deletion", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		f.tick()
		f.drain()

		execs := f.execs()
		if len(execs) != len(ciCacheVolumes) {
			t.Fatalf("ran %d execs, want %d: one per volume", len(execs), len(ciCacheVolumes))
		}
		// The LRU program carries both branches and the mode argument selects one;
		// a dry run passes list, and the other dry-run programs remove nothing.
		// Scanning the argv for a deletion would match the LRU script's own apply
		// branch.
		for _, c := range execs {
			for _, arg := range c.args {
				if arg == agentconfig.DiskMaintenanceModeApply {
					t.Errorf("a dry run passed the apply mode: %q", cacheTrimVerb(c.args))
				}
			}
		}
		for _, c := range f.cli.recorded() {
			if strings.HasPrefix(cacheTrimVerb(c.args), "delete ") {
				t.Errorf("a dry run ran a delete: %q", cacheTrimVerb(c.args))
			}
		}
	})
}

// TestCICacheTrimGocacheRunsTheLRUProgram: an apply pass execs the LRU program
// over ci-gocache with apply, the volume root, the age in minutes, the cap from
// ci_gocache_cap_bytes and the floor; ci-npm and ci-gomod keep the programs they
// ran before.
func TestCICacheTrimGocacheRunsTheLRUProgram(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	f.d.patrolConfig.Patrols.DiskMaintenance.CIGocacheCapBytes = "20GB"
	f.tick()
	f.drain()

	if n := len(f.execs()); n != len(ciCacheVolumes) {
		t.Fatalf("ran %d execs, want %d: one per volume", n, len(ciCacheVolumes))
	}
	argvOf := func(root string) []string {
		for _, c := range f.execs() {
			if slices.Contains(c.args, root) {
				return c.args[4:]
			}
		}
		return nil
	}

	// The cap is ci_gocache_cap_bytes, 20 GiB, in KiB as the volume is sized.
	wantGocache := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciCacheLRUTrimScript, "ci-cache-trim", "apply", ciCacheTestGocacheRoot,
		ciCacheTestMinutes, strconv.Itoa(20 << 20), strconv.Itoa(ciCacheLRUFloorMinutes)}
	if got := argvOf(ciCacheTestGocacheRoot); !slices.Equal(got, wantGocache) {
		t.Errorf("ci-gocache exec argv = %q\nwant %q", got, wantGocache)
	}

	wantNpm := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciCacheCappedTrimScript, "ci-cache-trim", ciCacheVolumeRoot(ciCacheNpmVolume), ciCacheTestMinutes, ciCacheTestNpmCapKiB}
	if got := argvOf(ciCacheVolumeRoot(ciCacheNpmVolume)); !slices.Equal(got, wantNpm) {
		t.Errorf("ci-npm exec argv = %q\nwant %q", got, wantNpm)
	}

	wantGomod := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciCacheModuleTrimScript, "ci-cache-trim", ciCacheVolumeRoot(ciCacheGomodVolume), ciCacheTestMinutes, ciCacheTestGomodCapKiB}
	if got := argvOf(ciCacheVolumeRoot(ciCacheGomodVolume)); !slices.Equal(got, wantGomod) {
		t.Errorf("ci-gomod exec argv = %q\nwant %q", got, wantGomod)
	}
}

// TestCICacheTrimGocacheDryRunReadsTheListReply: a dry run of ci-gocache runs
// the LRU program in list mode, at the setting's cap and the floor, and reads
// its reply as a selection: the volume is reported with the one figure the list
// mode knows and the cap, its removal logged as changing nothing, and no file
// name is logged.
func TestCICacheTrimGocacheDryRunReadsTheListReply(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
	// The list program reports after equal to before, since the list mode knows
	// only the size it starts from. The line must not print that figure twice.
	const beforeKiB = int64(40 << 10)
	f.exec = cliReply{stdout: strings.Join([]string{
		ciCacheTestGocacheRoot + "/00/old-1",
		ciCacheTestGocacheRoot + "/ab/old-2",
		fmt.Sprintf("before_kib=%d", beforeKiB),
		"removed=2",
		fmt.Sprintf("after_kib=%d", beforeKiB),
	}, "\n") + "\n"}
	f.tick()
	f.drain()

	var got []string
	for _, c := range f.execs() {
		if slices.Contains(c.args, ciCacheTestGocacheRoot) {
			got = c.args[4:]
		}
	}
	want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciCacheLRUTrimScript, "ci-cache-trim", "list", ciCacheTestGocacheRoot,
		ciCacheTestMinutes, strconv.Itoa(40 << 20), strconv.Itoa(ciCacheLRUFloorMinutes)}
	if !slices.Equal(got, want) {
		t.Errorf("ci-gocache dry-run argv = %q\nwant %q", got, want)
	}

	logs := f.logs.String()
	wantLine := fmt.Sprintf("%s: would remove 2 files of %s, against its cap of %s; nothing removed",
		ciCacheGocacheVolume, polecatimage.Human(beforeKiB*1024), polecatimage.Human(40<<30))
	if !strings.Contains(logs, wantLine) {
		t.Errorf("the dry run does not report the one figure and the cap (%q):\n%s", wantLine, logs)
	}
	before := polecatimage.Human(beforeKiB * 1024)
	if repeated := before + " to " + before; strings.Contains(logs, repeated) {
		t.Errorf("the dry run says the list changes the volume's size (%q):\n%s", repeated, logs)
	}
	for _, name := range []string{"old-1", "old-2"} {
		if strings.Contains(logs, name) {
			t.Errorf("the dry run logged the file name %q:\n%s", name, logs)
		}
	}
}

// TestCICacheTrimGocacheDryRunRefusedReply: a dry-run reply the selection parser
// refuses is a failed run for ci-gocache that does not stop the next volume, and
// an apply keeps reading the three-line reply, refusing file lines.
func TestCICacheTrimGocacheDryRunRefusedReply(t *testing.T) {
	t.Parallel()

	t.Run("a refused selection reply fails ci-gocache and not the next volume", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		// The reply says one file and names none: the selection parser refuses the
		// count at line 1, where the file lines should have ended.
		f.exec = ciCacheReply(40<<20, 1, 40<<20)
		f.tick()
		f.drain()

		if n := len(f.execs()); n != len(ciCacheVolumes) {
			t.Errorf("ran %d execs, want %d: a refusal stops no other volume", n, len(ciCacheVolumes))
		}
		logs := f.logs.String()
		if want := "ci-gocache: the volume's reply could not be read: the selection reply fails its file count check at line 1"; !strings.Contains(logs, want) {
			t.Errorf("logs do not name the refusal's rule and line (%q):\n%s", want, logs)
		}
		if !strings.Contains(logs, ciCacheNpmVolume+": would remove") {
			t.Errorf("the refusal stopped a volume after it:\n%s", logs)
		}
		if strings.Contains(logs, ciCacheGocacheVolume+": would remove") {
			t.Errorf("a refused volume was reported as a reading:\n%s", logs)
		}
		if got := f.d.diskMaintSchedule.retryAtOf(ciCacheVolumeSchedule(ciCacheVolumes[0])); !got.IsZero() {
			t.Errorf("retry at = %v, want none: a failed run is not retried on a timer", got)
		}
	})

	t.Run("a refused file line is logged by its rule and line, never by its path", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		// A file line outside the volume root is refused rather than removed; the
		// line's own path never reaches the log.
		f.exec = cliReply{stdout: strings.Join([]string{
			"/etc/not-the-cache/secret-name",
			"before_kib=40960",
			"removed=1",
			"after_kib=40960",
		}, "\n") + "\n"}
		f.tick()
		f.drain()

		logs := f.logs.String()
		if want := "ci-gocache: the volume's reply could not be read: the selection reply fails its inside the root check at line 1"; !strings.Contains(logs, want) {
			t.Errorf("logs do not name the refusal's rule and line (%q):\n%s", want, logs)
		}
		if strings.Contains(logs, "secret-name") {
			t.Errorf("the refusal logged the refused line's path:\n%s", logs)
		}
	})

	t.Run("an apply still refuses a selection reply", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = cliReply{stdout: strings.Join([]string{
			ciCacheTestGocacheRoot + "/00/old-1",
			"before_kib=40960",
			"removed=1",
			"after_kib=40960",
		}, "\n") + "\n"}
		f.tick()
		f.drain()

		logs := f.logs.String()
		if want := "the reply's line 1 is not before_kib=N"; !strings.Contains(logs, want) {
			t.Errorf("apply does not refuse the file lines (%q):\n%s", want, logs)
		}
		if strings.Contains(logs, ciCacheGocacheVolume+": removed ") {
			t.Errorf("an apply read a selection reply as a removal:\n%s", logs)
		}
	})
}

// TestCICacheTrimGocacheRefusedCap: a ci_gocache_cap_bytes the patrol cannot act
// on trims nothing and names the setting.
func TestCICacheTrimGocacheRefusedCap(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	f.d.patrolConfig.Patrols.DiskMaintenance.CIGocacheCapBytes = "0GB"
	f.tick()
	f.drain()

	if n := len(f.cli.recorded()); n != 0 {
		t.Errorf("a refused cap made %d kubectl call(s), want none", n)
	}
	if !strings.Contains(f.logs.String(), "ci_gocache_cap_bytes") {
		t.Errorf("logs do not name the refused setting:\n%s", f.logs.String())
	}
}

// TestCICacheTrimGocacheRefusesAnArgvItCannotBuild: the LRU program's arguments
// are built by ciCacheLRUArgv, which refuses a negative number in either mode; a
// row it refuses is a failed run that execs nothing.
func TestCICacheTrimGocacheRefusesAnArgvItCannotBuild(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{agentconfig.DiskMaintenanceModeApply, agentconfig.DiskMaintenanceModeDryRun} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, mode)
			row := ciCacheVolume{name: ciCacheGocacheVolume, root: ciCacheTestGocacheRoot, lru: true, capKiB: -1}
			at := ciCacheExecTarget{kubeconfig: "kubeconfig", kubeContext: ciCacheTestContext, pod: ciCacheTestPod}

			ok, why := f.d.ciCacheTrimVolume(context.Background(), mode, at, row, 12*time.Hour)
			if ok {
				t.Errorf("a row whose LRU arguments cannot be built reached a verdict")
			}
			if n := len(f.execs()); n != 0 {
				t.Errorf("a refused argv made %d exec(s), want none", n)
			}
			if want := "the volume's program could not be built"; !strings.Contains(f.logs.String(), want) {
				t.Errorf("logs do not carry %q:\n%s", want, f.logs.String())
			}
			if wantReason := "the volume's program could not be built: "; !strings.Contains(why, wantReason) {
				t.Errorf("the refusal's reason = %q, want it to carry %q", why, wantReason)
			}
		})
	}
}

// TestCICacheTrimConstantsMatchTheRunnerManifest holds the runner names and the
// dind mount the trim execs into equal to the runner's deployment manifest.
func TestCICacheTrimConstantsMatchTheRunnerManifest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "forgejo-runner", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatalf("reading the runner manifest: %v", err)
	}
	type manifestContainer struct {
		Name         string `yaml:"name"`
		VolumeMounts []struct {
			Name      string `yaml:"name"`
			MountPath string `yaml:"mountPath"`
		} `yaml:"volumeMounts"`
	}
	var m struct {
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Template struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
				Spec struct {
					// The engine is a native sidecar, so it is an init container.
					InitContainers []manifestContainer `yaml:"initContainers"`
					Containers     []manifestContainer `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decoding the runner manifest: %v", err)
	}
	if m.Metadata.Name != ciCacheDeployment {
		t.Errorf("deployment name = %q, the trim names %q", m.Metadata.Name, ciCacheDeployment)
	}
	if m.Metadata.Namespace != ciCacheNamespace {
		t.Errorf("namespace = %q, the trim names %q", m.Metadata.Namespace, ciCacheNamespace)
	}
	if got := "app.kubernetes.io/name=" + m.Spec.Template.Metadata.Labels["app.kubernetes.io/name"]; got != ciCachePodLabel {
		t.Errorf("pod label = %q, the trim selects %q", got, ciCachePodLabel)
	}
	var mounted bool
	for _, c := range append(m.Spec.Template.Spec.InitContainers, m.Spec.Template.Spec.Containers...) {
		if c.Name != ciCacheContainer {
			continue
		}
		for _, mount := range c.VolumeMounts {
			if mount.MountPath == ciCacheDindMount {
				mounted = true
			}
		}
	}
	if !mounted {
		t.Errorf("no %q container mounts %q; the trim's volume root is under it", ciCacheContainer, ciCacheDindMount)
	}

	// Each volume the trim runs over is one the runner's rendered config lists as a
	// valid volume: the names are gocacheVolume and npmCacheVolume in runner.go,
	// and the golden of its config is the file this test reads.
	rawConfig, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "forgejo-runner", "testdata", "config.yaml"))
	if err != nil {
		t.Fatalf("reading the runner's config: %v", err)
	}
	var cfg struct {
		Container struct {
			ValidVolumes []string `yaml:"valid_volumes"`
		} `yaml:"container"`
	}
	if err := yaml.Unmarshal(rawConfig, &cfg); err != nil {
		t.Fatalf("decoding the runner's config: %v", err)
	}
	for _, v := range ciCacheVolumes {
		if !slices.Contains(cfg.Container.ValidVolumes, v.name) {
			t.Errorf("the trim runs over volume %q, which the runner's valid_volumes %q do not name", v.name, cfg.Container.ValidVolumes)
		}
	}
}

// TestCICacheTrimSchedule: due an interval after the last verdict and not before,
// with one pass in flight at a time, a re-read of the due check when a pass
// starts, and a failed run's next attempt at the same full interval.
func TestCICacheTrimSchedule(t *testing.T) {
	t.Parallel()
	t.Run("due after the interval, one pass at a time", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)

		f.tick() // never run: due at once
		if len(f.pending) != 1 {
			t.Fatalf("a never-run trim dispatched %d passes, want 1", len(f.pending))
		}
		f.tick() // the pass is still pending: the single-flight guard holds
		if len(f.pending) != 1 {
			t.Fatalf("a second tick while the pass runs dispatched %d passes, want 1", len(f.pending))
		}
		f.drain()
		if n := len(f.execs()); n != len(ciCacheVolumes) {
			t.Fatalf("ran %d execs, want %d: one per volume", n, len(ciCacheVolumes))
		}

		f.at(15 * time.Minute)
		f.tick()
		if len(f.pending) != 0 {
			t.Fatalf("a trim that concluded at 0m dispatched again at 15m: the full interval is 1h")
		}
		f.at(59 * time.Minute)
		f.tick()
		if len(f.pending) != 0 {
			t.Fatalf("a minute short of the interval dispatched a pass")
		}
		f.at(60 * time.Minute)
		f.tick()
		f.drain()
		if n := len(f.execs()); n != 2*len(ciCacheVolumes) {
			t.Fatalf("ran %d execs at 60m, want %d: due an hour after its last verdict", n, 2*len(ciCacheVolumes))
		}
	})

	t.Run("a busy town still dispatches it", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.d.maint.quiet = func(*Daemon) (bool, string) { return false, "a landing is in flight" }
		f.tick()
		f.drain()
		if n := len(f.execs()); n != len(ciCacheVolumes) {
			t.Fatalf("a busy town ran %d execs, want %d: the CI trim is off the quiet guard", n, len(ciCacheVolumes))
		}
		if strings.Contains(f.logs.String(), "skipped") {
			t.Errorf("the busy tick logged a skip for the CI trim, which is not held back: %q", f.logs.String())
		}
	})

	t.Run("a pass re-reads the due check when it starts", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.tick()
		// Another pass concludes every volume's trim after this tick's decision.
		for _, v := range ciCacheVolumes {
			f.d.diskMaintSchedule.record(ciCacheVolumeSchedule(v), ciCacheTestStart)
		}
		f.drain()
		if n := len(f.execs()); n != 0 {
			t.Fatalf("a pass that found the trim spent ran %d execs, want 0", n)
		}
	})

	t.Run("a failed run's next attempt is the full interval", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}
		f.tick()
		f.drain()

		f.at(ciCacheTestEvery - time.Minute)
		f.tick()
		if len(f.pending) != 0 {
			t.Fatalf("a failed run was retried a minute short of the interval: a failure is not retried on a timer")
		}
		f.at(ciCacheTestEvery)
		f.tick()
		if len(f.pending) != 1 {
			t.Fatalf("a failed run was not tried again at its normal cadence: dispatched %d", len(f.pending))
		}
	})

}

// TestCICacheTrimFailureRaisesOncePerVolume: a failed trim of one volume records
// that volume's full interval, asks for no retry and raises one item keyed to
// that volume; another volume's cadence is not moved, and a success at the full
// interval clears the item (gt-76cz9d).
func TestCICacheTrimFailureRaisesOncePerVolume(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	f.exec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}
	f.tick()
	f.drain()

	failed := ciCacheVolumes[0]
	failedKey := diskMaintFailedKey(ciCacheVolumeSchedule(failed))
	open := f.notify.OpenEscalations(failedKey)
	if len(open) != 1 {
		t.Fatalf("open items for %s = %+v, want one", failed.name, open)
	}
	if got := f.d.diskMaintSchedule.lastRunAt(ciCacheVolumeSchedule(failed)); !got.Equal(ciCacheTestStart) {
		t.Errorf("the failed volume's last run = %v, want %v: a failure concludes the run", got, ciCacheTestStart)
	}
	if got := f.d.diskMaintSchedule.retryAtOf(ciCacheVolumeSchedule(failed)); !got.IsZero() {
		t.Errorf("the failed volume asked for a retry at %v, want none: its next attempt is its normal cadence", got)
	}

	// Each other volume reached a verdict at the same instant and raised none.
	other := ciCacheVolumes[1]
	if got := f.d.diskMaintSchedule.lastRunAt(ciCacheVolumeSchedule(other)); !got.Equal(ciCacheTestStart) {
		t.Errorf("%s's last run = %v, want %v", other.name, got, ciCacheTestStart)
	}
	if got := f.notify.OpenEscalations(diskMaintFailedKey(ciCacheVolumeSchedule(other))); len(got) != 0 {
		t.Errorf("%s raised %d item(s), want none: its trim reached a verdict", other.name, len(got))
	}

	// The next attempt is its normal cadence, and its verdict clears the item.
	f.exec = ciCacheGocacheExecReply(agentconfig.DiskMaintenanceModeApply, 40<<20, 3, 10<<20)
	f.at(ciCacheTestEvery)
	f.tick()
	f.drain()
	if n := volumeExecs(f, ciCacheGocacheVolume); n != 2 {
		t.Errorf("the failed volume ran %d times at the full interval, want 2", n)
	}
	if open := f.notify.OpenEscalations(failedKey); len(open) != 0 {
		t.Errorf("the item is still open after a verdict: %+v", open)
	}
}

// TestCICacheTrimFailuresAreFailedRuns: a run that cannot read the volume, reach
// the container or make sense of the reply is a failed run with its own line,
// held to once an hour, and never read as a volume with nothing to trim.
func TestCICacheTrimFailuresAreFailedRuns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pods cliReply
		exec cliReply
		want string
		// execs is how many exec calls the failure reaches: a fault on one volume
		// does not stop the next, so the exec cases reach every volume.
		execs int
	}{
		{
			name:  "the volume path is not in the container",
			exec:  cliReply{stderr: "volume path not found\n", code: ciCacheMissingPathExit},
			want:  "is not in the dind container",
			execs: len(ciCacheVolumes),
		},
		{
			name:  "the dind container is not running",
			pods:  cliReply{stdout: ciCachePodsDindWaitingJSON},
			want:  "is not running in forgejo",
			execs: 0,
		},
		{
			name:  "the exec fails",
			exec:  cliReply{stderr: "error: unable to upgrade connection: container not found\n", code: 1},
			want:  "the exec into the dind container failed",
			execs: len(ciCacheVolumes),
		},
		{
			name:  "the reply cannot be read",
			exec:  cliReply{stdout: "du: cannot access the path\n"},
			want:  "the volume's reply could not be read",
			execs: len(ciCacheVolumes),
		},
		{
			name:  "the runner's pods cannot be read",
			pods:  cliReply{stderr: "error: the server is unavailable\n", code: 1},
			want:  "the runner's pods could not be read",
			execs: 0,
		},
		{
			name:  "the runner's pod list does not decode",
			pods:  cliReply{stdout: "not json"},
			want:  "the runner's pods could not be decoded",
			execs: 0,
		},
		{
			name:  "the runner's pod list names no item list",
			pods:  cliReply{stdout: "{}"},
			want:  "the reply names no item list",
			execs: 0,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			// Half the log hold's window, so the second attempt lands inside it.
			f.d.patrolConfig.Patrols.DiskMaintenance.CICacheTrimInterval = "30m"
			if tt.pods != (cliReply{}) {
				f.pods = tt.pods
			}
			if tt.exec != (cliReply{}) {
				f.exec = tt.exec
			}
			f.tick()
			f.drain()

			if n := len(f.execs()); n != tt.execs {
				t.Errorf("ran %d execs, want %d", n, tt.execs)
			}
			if !strings.Contains(f.logs.String(), tt.want) {
				t.Errorf("logs do not name the failure %q:\n%s", tt.want, f.logs.String())
			}
			// The failure cases fault the ci-gocache volume; the ci-npm volume reads
			// under its cap and may report its own removal line.
			if strings.Contains(f.logs.String(), ciCacheVolumes[0].name+": removed ") {
				t.Errorf("a failed volume reported a removal:\n%s", f.logs.String())
			}
			if n := len(f.d.cacheTrimNoOp.observed()); n != 0 {
				t.Errorf("a failed run raised %d no-op item(s): a failure is not a reading", n)
			}
			if got := f.d.diskMaintSchedule.retryAtOf(ciCacheVolumeSchedule(ciCacheVolumes[0])); !got.IsZero() {
				t.Errorf("retry at = %v, want none: a failure is not retried on a timer", got)
			}

			// The same failure at the next attempt, inside the hour, is held to
			// the one line.
			f.at(30 * time.Minute)
			f.tick()
			f.drain()
			if got, want := f.d.diskMaintSchedule.lastRunAt(ciCacheVolumeSchedule(ciCacheVolumes[0])), ciCacheTestStart.Add(30*time.Minute); !got.Equal(want) {
				t.Errorf("the failed volume's last run = %v, want %v: the next attempt is the normal cadence", got, want)
			}
			if n := strings.Count(f.logs.String(), tt.want); n != 1 {
				t.Errorf("the failure was logged %d times across two attempts, want once an hour", n)
			}
		})
	}
}

// TestCICacheTrimPodNamesTheRunningPod: the trim execs into the runner pod whose
// dind container is running, whether dind is a regular container or a native
// sidecar reported in initContainerStatuses. A pod with dind up in neither list
// refuses as not running, and a reply with no item list refuses.
func TestCICacheTrimPodNamesTheRunningPod(t *testing.T) {
	t.Parallel()
	// want empty means the pod is named and exec'd into; otherwise the run
	// refuses without an exec and logs want.
	tests := []struct {
		name string
		pods cliReply
		want string
	}{
		{name: "dind runs as a regular container", pods: cliReply{stdout: ciCachePodsRunningJSON}},
		{name: "dind runs as an init container", pods: cliReply{stdout: ciCachePodsDindInitRunningJSON}},
		{name: "dind is running in neither list", pods: cliReply{stdout: ciCachePodsDindDownJSON}, want: "is not running in forgejo"},
		{name: "the reply names no item list", pods: cliReply{stdout: "{}"}, want: "the reply names no item list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			f.pods = tt.pods
			f.tick()
			f.drain()

			execs := f.execs()
			if tt.want == "" {
				if len(execs) != len(ciCacheVolumes) {
					t.Fatalf("ran %d execs, want %d: one per volume", len(execs), len(ciCacheVolumes))
				}
				for _, c := range execs {
					if !slices.Contains(c.args, ciCacheTestPod) {
						t.Errorf("exec argv does not name the running pod %s: %q", ciCacheTestPod, cacheTrimVerb(c.args))
					}
				}
				return
			}
			if len(execs) != 0 {
				t.Errorf("ran %d execs, want none: no running pod was named", len(execs))
			}
			if !strings.Contains(f.logs.String(), tt.want) {
				t.Errorf("logs do not name the refusal %q:\n%s", tt.want, f.logs.String())
			}
		})
	}
}

// TestCICacheTrimHoldsAFailureWhoseTextVaries: a failure that carries the failing
// command's own output is logged once in the hour, though each retry's output
// differs. Keyed on the expanded line, every retry would be a new line and go
// through the hold.
func TestCICacheTrimHoldsAFailureWhoseTextVaries(t *testing.T) {
	t.Parallel()
	const want = "the exec into the dind container failed"
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	// Half the log hold's window, so the next attempt lands inside it.
	f.d.patrolConfig.Patrols.DiskMaintenance.CICacheTrimInterval = "30m"
	f.exec = cliReply{stderr: "error: container not found: 1a2b3c\n", code: 1}
	f.tick()
	f.drain()

	f.exec = cliReply{stderr: "error: container not found: 9f8e7d\n", code: 1}
	f.at(30 * time.Minute)
	f.tick()
	f.drain()

	if n := volumeExecs(f, ciCacheGocacheVolume); n != 2 {
		t.Fatalf("ci-gocache ran %d execs, want 2: the next attempt must run", n)
	}
	if n := strings.Count(f.logs.String(), want); n != 1 {
		t.Errorf("the failure was logged %d times across two attempts with different output, want once an hour:\n%s", n, f.logs.String())
	}
}

// TestCICacheTrimHoldsEachFaultKindApart: two different faults that share a log
// line's shape are each logged within the hour. A held fault must not hide a new
// one of another kind.
func TestCICacheTrimHoldsEachFaultKindApart(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	// Half the log hold's window, so the next attempt lands inside it.
	f.d.patrolConfig.Patrols.DiskMaintenance.CICacheTrimInterval = "30m"
	f.pods = cliReply{stdout: ciCachePodsDindWaitingJSON}
	f.tick()
	f.drain()

	f.pods = cliReply{stderr: "error: the server is unavailable\n", code: 1}
	f.at(30 * time.Minute)
	f.tick()
	f.drain()

	for _, want := range []string{"is not running in forgejo", "the runner's pods could not be read"} {
		if n := strings.Count(f.logs.String(), want); n != 1 {
			t.Errorf("logged %q %d times, want once: a different fault was hidden:\n%s", want, n, f.logs.String())
		}
	}
}

// TestCICacheTrimPassFailureItemCarriesTheReason pins gt-x7hl2a: a pass that fails
// before it tries any volume puts the error's first line, not the fixed sentence,
// into each due volume's item.
func TestCICacheTrimPassFailureItemCarriesTheReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *ciCacheFixture)
		want  string
	}{
		{"a pod listing failure", func(f *ciCacheFixture) { f.pods = cliReply{stderr: "error: the server is unavailable\n", code: 1} }, "the runner's pods could not be read: exit status 1: error: the server is unavailable"},
		{"a context refusal", func(f *ciCacheFixture) { f.d.patrolConfig.Patrols.TierSweep.Runner.Context = "" }, errCICacheTrimNoContext.Error()},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			due := f.d.ciCacheDueVolumes(f.now, ciCacheTestEvery)
			tt.setup(f)
			f.tick()
			f.drain()
			for _, v := range due {
				open := f.notify.OpenEscalations(diskMaintFailedKey(ciCacheVolumeSchedule(v)))
				if len(open) != 1 || !strings.Contains(open[0].Reason, tt.want) || strings.Contains(open[0].Reason, ciCacheTrimNoReason) {
					t.Fatalf("%s's item = %+v, want one carrying %q and not the fixed sentence", v.name, open, tt.want)
				}
			}
		})
	}
}

// TestCICacheTrimVolumeFailureItemCarriesTheReason pins gt-u8n4tx: a volume's own
// failed trim puts its failure's first line, not the fixed sentence, into that
// volume's item; a refusal with no error text to show carries its own sentence.
func TestCICacheTrimVolumeFailureItemCarriesTheReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		exec cliReply
		want string
	}{
		{"an exec failure carries its error's first line", cliReply{startErr: errors.New("fork/exec /usr/bin/kubectl: no such file or directory\na later line")}, "the exec into the dind container failed: fork/exec /usr/bin/kubectl: no such file or directory"},
		{"a missing path carries the refusal's sentence", cliReply{stderr: "volume path not found\n", code: ciCacheMissingPathExit}, "the volume path " + ciCacheTestGocacheRoot + " is not in the " + ciCacheContainer + " container"},
		{"an unreadable reply carries its refusal", cliReply{stdout: "du: cannot access the path\n"}, "the volume's reply could not be read: the reply has 1 line(s), want three"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			f.exec = tt.exec
			f.tick()
			f.drain()

			failed := ciCacheVolumes[0]
			open := f.notify.OpenEscalations(diskMaintFailedKey(ciCacheVolumeSchedule(failed)))
			if len(open) != 1 || !strings.Contains(open[0].Reason, tt.want) {
				t.Fatalf("%s's items = %+v, want one carrying %q", failed.name, open, tt.want)
			}
			if strings.Contains(open[0].Reason, ciCacheTrimNoReason) || strings.Contains(open[0].Reason, "a later line") {
				t.Errorf("%s's item carries more than its failure's first line: %q", failed.name, open[0].Reason)
			}
			// The other due volumes reached a verdict: their items are unchanged.
			if other := ciCacheVolumes[1]; len(f.notify.OpenEscalations(diskMaintFailedKey(ciCacheVolumeSchedule(other)))) != 0 {
				t.Errorf("%s raised an item, want none: another volume's item is unchanged", other.name)
			}
		})
	}
}

// TestCICacheTrimRefusesASettingItCannotActOn: a setting the patrol cannot act
// on, or a runner that names no control cluster, runs nothing and says why.
func TestCICacheTrimRefusesASettingItCannotActOn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(f *ciCacheFixture)
		want   string
		// retry is whether the refused run concludes each due volume as a failed
		// run at the full interval.
		retry bool
	}{
		{
			name: "a zero interval",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.DiskMaintenance.CICacheTrimInterval = "0s"
			},
			want: "ci_cache_trim_interval",
		},
		{
			name: "a zero max age",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.DiskMaintenance.CICacheMaxAge = "0h"
			},
			want: "ci_cache_max_age",
		},
		{
			// A town with no kubernetes runner has no control cluster, so the trim
			// is not configured: it neither runs nor says anything.
			name: "a runner on the docker backend is not configured",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.TierSweep.Runner.Backend = agentconfig.TierSweepRunnerDocker
			},
		},
		{
			name: "a town with no tier sweep runner is not configured",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.TierSweep = nil
			},
		},
		{
			name: "a runner with no context names no control cluster",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.TierSweep.Runner.Context = ""
			},
			want:  "names no control cluster",
			retry: true,
		},
		{
			name: "a context that reads as a flag is refused",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.TierSweep.Runner.Context = "--kubeconfig=/tmp/x"
			},
			want:  "names no control cluster",
			retry: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			tt.mutate(f)
			f.tick()
			f.drain()

			if n := len(f.cli.recorded()); n != 0 {
				t.Errorf("a refused or unconfigured trim made %d kubectl call(s), want none", n)
			}
			if tt.want == "" {
				if f.logs.Len() != 0 {
					t.Errorf("an unconfigured trim said something:\n%s", f.logs.String())
				}
				return
			}
			if !strings.Contains(f.logs.String(), tt.want) {
				t.Errorf("logs do not name the refusal %q:\n%s", tt.want, f.logs.String())
			}
			if !tt.retry {
				return
			}
			if got := f.d.diskMaintSchedule.retryAtOf(ciCacheVolumeSchedule(ciCacheVolumes[0])); !got.IsZero() {
				t.Errorf("retry at = %v, want none: a refused run is not retried on a timer", got)
			}
		})
	}
}

// TestCICacheTrimNoopItem: a trim that removed no file while ci-gocache stays
// over its cap raises one low attention item; a repeat keeps that one item; a
// later reading under the cap clears it; a trim that removed files over the cap
// raises it too, since the LRU floor leaves a fast-written volume over its cap
// whatever it removed; and a dry run raises nothing.
func TestCICacheTrimNoopItem(t *testing.T) {
	t.Parallel()
	const overCap = 45 << 20 // KiB: 45 GiB, over the default 40 GiB ci_gocache_cap_bytes
	const underCap = 35 << 20

	t.Run("one item for a trim that removed nothing over the cap, cleared under it", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Fatalf("items after the first over-cap trim = %+v, want none until the run lasts the floor", items)
		}

		f.tickAFloorLater()

		items := f.d.cacheTrimNoOp.observed()
		if len(items) != 1 || items[0].Key != ciCacheNoOpKey {
			t.Fatalf("items after a floor's run over the cap = %+v, want one item keyed %q", items, ciCacheNoOpKey)
		}
		if items[0].Kind != attention.KindCacheTrimNoOp || items[0].Severity != attention.SeverityLow {
			t.Errorf("item kind %q severity %q, want the cache trim kind at low severity", items[0].Kind, items[0].Severity)
		}

		f.at(3 * time.Hour)
		f.exec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 1 {
			t.Fatalf("items after a repeat = %d, want still one", len(items))
		}

		f.at(4 * time.Hour)
		f.exec = ciCacheReply(overCap, 5, underCap)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("items after a reading under the cap = %+v, want none", items)
		}
	})

	t.Run("a trim that removed files over the cap raises the item", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overCap, 7, overCap)
		f.tick()
		f.drain()
		f.tickAFloorLater()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{ciCacheNoOpKey}) {
			t.Errorf("a trim that removed files and ended over the cap raised %v, want one item keyed %q", got, ciCacheNoOpKey)
		}
	})

	t.Run("a dry run raises nothing", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		f.exec = ciCacheGocacheExecReply(agentconfig.DiskMaintenanceModeDryRun, overCap, 0, overCap)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("a dry run raised %+v", items)
		}
	})
}

// TestCICacheTrimReplyMustBeExact: the reply is the three figures, in order, and
// a reply that is not is never read as zeros. The young_kib line a reply may
// carry after them is TestCICacheYoungReply's.
func TestCICacheTrimReplyMustBeExact(t *testing.T) {
	t.Parallel()
	good, err := parseCICacheTrimReply([]byte("before_kib=5\nremoved=2\nafter_kib=3\n"))
	if err != nil {
		t.Fatalf("a well-formed reply does not parse: %v", err)
	}
	if good.beforeKiB != 5 || good.removed != 2 || good.afterKiB != 3 {
		t.Errorf("parsed %+v, want before 5, removed 2, after 3", good)
	}
	for name, out := range map[string]string{
		"empty":             "",
		"keys out of order": "removed=2\nbefore_kib=5\nafter_kib=3\n",
		"extra line":        "before_kib=5\nremoved=2\nafter_kib=3\nnoise\n",
		"not a number":      "before_kib=five\nremoved=2\nafter_kib=3\n",
		"negative":          "before_kib=5\nremoved=-1\nafter_kib=3\n",
	} {
		if _, err := parseCICacheTrimReply([]byte(out)); err == nil {
			t.Errorf("%s reply parsed without an error", name)
		}
	}
}

// TestCICacheTrimSelection: the list program's reply is one line per selected
// file and then the figures. Every file line must be a clean absolute path
// strictly inside the volume root, and there is one for each removed file. A
// reply that breaks a rule is refused as a *ciCacheSelectionError carrying the
// rule and the line number, never the file name.
func TestCICacheTrimSelection(t *testing.T) {
	t.Parallel()
	const root = "/vol/ci-gocache/_data"

	t.Run("a reply of file lines and the three figures returns the figures", func(t *testing.T) {
		t.Parallel()
		got, err := parseCICacheTrimSelection([]byte(
			root+"/aa/one\n"+root+"/bb/two\nbefore_kib=9\nremoved=2\nafter_kib=5\n"), root)
		if err != nil {
			t.Fatalf("a well-formed selection does not parse: %v", err)
		}
		if want := (ciCacheTrimReply{beforeKiB: 9, removed: 2, afterKiB: 5}); got != want {
			t.Errorf("parsed %+v, want %+v", got, want)
		}
	})

	t.Run("a reply with no file lines returns removed 0", func(t *testing.T) {
		t.Parallel()
		got, err := parseCICacheTrimSelection([]byte("before_kib=9\nremoved=0\nafter_kib=9\n"), root)
		if err != nil || got.removed != 0 {
			t.Errorf("a reply with no file lines = %+v, %v; want removed 0 and no error", got, err)
		}
	})

	t.Run("an empty root is refused", func(t *testing.T) {
		t.Parallel()
		if _, err := parseCICacheTrimSelection([]byte("before_kib=9\nremoved=0\nafter_kib=9\n"), ""); err == nil {
			t.Error("an empty root parsed without an error")
		}
	})

	tests := []struct {
		name string
		out  string
		rule string
		line int
	}{
		{
			name: "an empty line is refused",
			out:  "\n" + root + "/aa/one\nbefore_kib=9\nremoved=2\nafter_kib=9\n",
			rule: ciCacheSelectionRuleEmpty,
			line: 1,
		},
		{
			name: "a file line outside the root is refused",
			out:  root + "/aa/one\n/vol/ci-npm/_data/other\nbefore_kib=9\nremoved=2\nafter_kib=9\n",
			rule: ciCacheSelectionRuleOutsideRoot,
			line: 2,
		},
		{
			name: "a .. in the middle of a path is refused",
			out:  root + "/aa/../etc/x\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleNotClean,
			line: 1,
		},
		{
			name: "a .. at the end of a path is refused",
			out:  root + "/aa/..\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleNotClean,
			line: 1,
		},
		{
			name: "a sibling sharing the root as a string prefix is refused",
			out:  "/vol/ci-gocache/_data2/other\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleOutsideRoot,
			line: 1,
		},
		{
			name: "a relative path is refused",
			out:  "aa/one\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleNotAbsolute,
			line: 1,
		},
		{
			name: "a path with a trailing slash is refused",
			out:  root + "/aa/one/\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleNotClean,
			line: 1,
		},
		{
			name: "the root itself is refused",
			out:  root + "\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleOutsideRoot,
			line: 1,
		},
		{
			name: "fewer file lines than removed is refused",
			out:  root + "/aa/one\nbefore_kib=9\nremoved=2\nafter_kib=9\n",
			rule: ciCacheSelectionRuleCount,
			line: 2,
		},
		{
			name: "more file lines than removed is refused",
			out:  root + "/aa/one\n" + root + "/bb/two\nbefore_kib=9\nremoved=1\nafter_kib=9\n",
			rule: ciCacheSelectionRuleCount,
			line: 3,
		},
		{
			name: "a reply with fewer than three lines is refused",
			out:  "before_kib=9\nremoved=1\n",
			rule: ciCacheSelectionRuleShape,
			line: 3,
		},
		{
			name: "a reply whose last three lines are not the three figures is refused",
			out:  root + "/aa/one\nbefore_kib=9\nremoved=1\nnoise\n",
			rule: ciCacheSelectionRuleShape,
			line: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCICacheTrimSelection([]byte(tc.out), root)
			if err == nil {
				t.Fatalf("parsed %+v without an error, want a refusal", got)
			}
			var sel *ciCacheSelectionError
			if !errors.As(err, &sel) {
				t.Fatalf("refused with %T (%v), want *ciCacheSelectionError", err, err)
			}
			if sel.Rule != tc.rule || sel.Line != tc.line {
				t.Errorf("refused with rule %q line %d, want rule %q line %d", sel.Rule, sel.Line, tc.rule, tc.line)
			}
			if strings.Contains(err.Error(), "/") {
				t.Errorf("refusal %q holds a path", err)
			}
		})
	}
}

// TestCICacheYoungReply: parseCICacheTrimReply reads the three figures alone,
// which every volume except ci-gocache prints, and the young_kib line the LRU
// program appends after after_kib. A fourth line that is not young_kib=N, a
// young_kib that is not a whole number or is negative, and a fifth line are
// refused as the three-line reply's bad figures are.
func TestCICacheYoungReply(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		out  string
		want *ciCacheTrimReply // nil: the reply is refused
	}{
		{"three lines carry no young figure", "before_kib=9\nremoved=2\nafter_kib=5\n",
			&ciCacheTrimReply{beforeKiB: 9, removed: 2, afterKiB: 5}},
		{"a fourth young_kib line is read as the young figure", "before_kib=9\nremoved=2\nafter_kib=5\nyoung_kib=4\n",
			&ciCacheTrimReply{beforeKiB: 9, removed: 2, afterKiB: 5, youngKiB: 4, youngReported: true}},
		{"a fourth line that is not young_kib is refused", "before_kib=9\nremoved=2\nafter_kib=5\nnoise\n", nil},
		{"a young that is not a number is refused", "before_kib=9\nremoved=2\nafter_kib=5\nyoung_kib=few\n", nil},
		{"a negative young is refused", "before_kib=9\nremoved=2\nafter_kib=5\nyoung_kib=-1\n", nil},
		{"a fifth line is refused", "before_kib=9\nremoved=2\nafter_kib=5\nyoung_kib=4\nnoise\n", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCICacheTrimReply([]byte(tt.out))
			if tt.want == nil {
				if err == nil {
					t.Errorf("parsed %+v without an error, want a refusal", got)
				}
				return
			}
			if err != nil || got != *tt.want {
				t.Errorf("parsed %+v, %v; want %+v", got, err, *tt.want)
			}
		})
	}
}

// TestCICacheYoungSelection: the selection parser reads the same figures after
// its file lines, with or without the young_kib line, and counts the file lines
// that precede them. A bad fourth figure line is a shape refusal at the line the
// figures begin.
func TestCICacheYoungSelection(t *testing.T) {
	t.Parallel()
	const root = "/vol/ci-gocache/_data"
	const figures = "before_kib=9\nremoved=2\nafter_kib=5\n"
	const two = root + "/aa/one\n" + root + "/bb/two\n"
	for _, tt := range []struct {
		name          string
		out           string
		young         int64
		youngReported bool
		rule          string // empty: the reply parses
		line          int
	}{
		{name: "file lines and three figures carry no young figure", out: two + figures},
		{name: "the young line carries the figure", out: two + figures + "young_kib=4\n", young: 4, youngReported: true},
		{name: "a count that does not match the file lines is refused", out: root + "/aa/one\n" + figures + "young_kib=4\n",
			rule: ciCacheSelectionRuleCount, line: 2},
		{name: "a bad fourth figure is refused", out: two + figures + "young_kib=few\n",
			rule: ciCacheSelectionRuleShape, line: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCICacheTrimSelection([]byte(tt.out), root)
			if tt.rule != "" {
				var sel *ciCacheSelectionError
				if !errors.As(err, &sel) || sel.Rule != tt.rule || sel.Line != tt.line {
					t.Fatalf("refused with %v, want rule %q at line %d", err, tt.rule, tt.line)
				}
				return
			}
			if err != nil || got.youngKiB != tt.young || got.youngReported != tt.youngReported || got.removed != 2 {
				t.Errorf("parsed %+v, %v; want removed 2 and young %d (reported %t)", got, err, tt.young, tt.youngReported)
			}
		})
	}
}

// TestCICacheTrimVolumesRunInTableOrderAndOneFailureDoesNotStopTheNext: a pass
// execs once per volume in table order. A failed exec on the first volume is
// logged under that volume, still leaves the second to run, and fails the pass.
func TestCICacheTrimVolumesRunInTableOrderAndOneFailureDoesNotStopTheNext(t *testing.T) {
	t.Parallel()
	t.Run("a failed first exec still leaves the second, and the pass returns false", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}

		if ok := f.d.ciCacheTrim(context.Background(), agentconfig.DiskMaintenanceModeApply); ok {
			t.Errorf("the pass returned true with its first volume failed")
		}
		execs := f.execs()
		if len(execs) != len(ciCacheVolumes) {
			t.Fatalf("ran %d execs, want %d: a failure must not stop the next volume", len(execs), len(ciCacheVolumes))
		}
		for i, v := range ciCacheVolumes {
			if !slices.Contains(execs[i].args, v.root) {
				t.Errorf("exec %d does not name volume %s at %s: the pass runs in table order", i, v.name, v.root)
			}
		}
		if want := ciCacheVolumes[0].name + ": the exec into the dind container failed"; !strings.Contains(f.logs.String(), want) {
			t.Errorf("logs do not name the failed volume %q:\n%s", want, f.logs.String())
		}
	})

	t.Run("every volume succeeds, logs one line each, and the pass returns true", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		if ok := f.d.ciCacheTrim(context.Background(), agentconfig.DiskMaintenanceModeApply); !ok {
			t.Errorf("the pass returned false with every volume trimmed:\n%s", f.logs.String())
		}
		if n := strings.Count(f.logs.String(), ": removed "); n != len(ciCacheVolumes) {
			t.Errorf("logged %d removal lines, want one per volume (%d):\n%s", n, len(ciCacheVolumes), f.logs.String())
		}
	})
}

// TestCICacheTrimCappedScriptReportsAndRemovesNothingUnderTheCap: the capped
// volume's exec carries its cap in KiB. A reply at or under the cap is a verdict
// that raises no item; a reply over the cap that removed nothing raises the
// volume's item. That the script removes nothing at or under the cap is the
// integration tier's: it runs the script.
func TestCICacheTrimCappedScriptReportsAndRemovesNothingUnderTheCap(t *testing.T) {
	t.Parallel()
	const capKiB = ciCacheNpmCapKiB
	npmRoot := ciCacheVolumeRoot(ciCacheNpmVolume)
	cases := []struct {
		name     string
		afterKiB int64
		raised   bool
	}{
		{name: "a reply at the cap", afterKiB: capKiB},
		{name: "a reply under the cap", afterKiB: capKiB - 1024},
		{name: "a reply over the cap", afterKiB: capKiB + 1024, raised: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			f.npmExec = ciCacheReply(tt.afterKiB, 0, tt.afterKiB)
			if ok := f.d.ciCacheTrim(context.Background(), agentconfig.DiskMaintenanceModeApply); !ok {
				t.Fatalf("the pass returned false for a reply that parses:\n%s", f.logs.String())
			}

			var npm []string
			for _, c := range f.execs() {
				if slices.Contains(c.args, npmRoot) {
					npm = c.args
				}
			}
			if npm == nil {
				t.Fatalf("the pass ran no exec into the %s volume", ciCacheNpmVolume)
			}
			want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
				"sh", "-c", ciCacheCappedTrimScript, "ci-cache-trim", npmRoot, ciCacheTestMinutes, ciCacheTestNpmCapKiB}
			if !slices.Equal(npm[4:], want) {
				t.Errorf("npm exec argv = %q\nwant %q", npm[4:], want)
			}

			items := f.d.cacheTrimNoOp.observed()
			if !tt.raised {
				if len(items) != 0 {
					t.Errorf("a reply %s raised %+v, want no item", tt.name, items)
				}
				return
			}
			if got := noopKeys(items); !slices.Equal(got, []string{ciCacheNoOpKeyOf(ciCacheVolumes[1])}) {
				t.Errorf("items = %v, want the capped volume's item", got)
			}
		})
	}
}

// TestCICacheTrimNoopItemPerVolume: ci-gocache keeps the no-op item it has
// always had, measured against its cap. A capped volume raises its own item over
// its cap, clears it at or under the cap, and leaves the other volume's item
// alone.
func TestCICacheTrimNoopItemPerVolume(t *testing.T) {
	t.Parallel()
	const overGocacheCap = 45 << 20 // KiB: 45 GiB, over the default 40 GiB ci_gocache_cap_bytes
	const overCap = 2 << 20         // KiB: 2 GiB, over the 1 GiB npm cap
	const npmKey = "ci-cache-trim-noop-ci-npm"

	t.Run("a capped volume raises its own item over its cap", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.npmExec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()

		items := f.d.cacheTrimNoOp.observed()
		if got := noopKeys(items); !slices.Equal(got, []string{npmKey}) {
			t.Fatalf("items = %v, want one item keyed %q", got, npmKey)
		}
		if items[0].Kind != attention.KindCacheTrimNoOp || items[0].Severity != attention.SeverityLow {
			t.Errorf("item kind %q severity %q, want the cache trim kind at low severity", items[0].Kind, items[0].Severity)
		}
		if !strings.Contains(items[0].Summary, ciCacheNpmVolume) {
			t.Errorf("summary %q does not name the volume", items[0].Summary)
		}
	})

	t.Run("the ci-gocache item and the capped item clear apart", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overGocacheCap, 0, overGocacheCap)
		f.npmExec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{npmKey}) {
			t.Fatalf("items after the first trim = %v, want only the capped volume's item until the gocache run lasts the floor", got)
		}

		f.tickAFloorLater()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{ciCacheNoOpKey, npmKey}) {
			t.Fatalf("items after a floor's run = %v, want both volumes' items", got)
		}

		f.at(3 * time.Hour)
		f.npmExec = ciCacheReply(ciCacheNpmCapKiB, 0, ciCacheNpmCapKiB)
		f.tick()
		f.drain()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{ciCacheNoOpKey}) {
			t.Errorf("items after the npm volume is at its cap = %v, want only the ci-gocache item", got)
		}
	})

	t.Run("a capped volume that removed files raises nothing", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.npmExec = ciCacheReply(overCap, 5, overCap)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("a capped volume that removed files raised %+v", items)
		}
	})
}

// TestCICacheTrimNoopItemUncappedVolumeRaisesNothing: a volume with no cap and no
// lru program has no threshold to measure against, so no reading raises its
// item. Every row the table holds is capped or lru, so this volume is not in the
// table; the test pins that such a volume stays silent.
func TestCICacheTrimNoopItemUncappedVolumeRaisesNothing(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	const overGiB = 40 << 20 // KiB: 40 GiB, over every cap the table holds
	uncapped := ciCacheVolume{name: "ci-scratch", root: ciCacheTestGocacheRoot}
	f.d.ciCacheTrimMeasure(agentconfig.DiskMaintenanceModeApply, uncapped, ciCacheTrimReply{beforeKiB: overGiB, afterKiB: overGiB})
	if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
		t.Errorf("an uncapped volume over 40 GiB raised %+v, want none", items)
	}
}

// TestCICacheTrimIgnoresTheWarnFigure: the pass no longer reads
// ci_cache_warn_bytes, since no volume is measured against it. A value the daemon
// used to refuse does not stop a trim: the trim runs and execs into the volumes.
func TestCICacheTrimIgnoresTheWarnFigure(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	f.d.patrolConfig.Patrols.DiskMaintenance.CICacheWarnBytes = "0GB"
	f.tick()
	f.drain()
	if n := len(f.execs()); n == 0 {
		t.Errorf("a trim with a zero warn figure made no exec, want the trim to run")
	}
}

// TestCICacheTrimGocacheNoopOverCap: the ci-gocache volume's no-op item is
// measured against its cap, the setting the pass copies into the row. A run over
// the cap raises the item, whether it removed no file or removed files, since the
// LRU floor leaves a fast-written volume over its cap; a reading at or under the
// cap clears it.
func TestCICacheTrimGocacheNoopOverCap(t *testing.T) {
	t.Parallel()
	const (
		capKiB            = 40 << 20 // KiB: the default ci_gocache_cap_bytes, 40 GiB
		betweenWarnAndCap = 35 << 20 // KiB: under the cap, so the item clears
		overCap           = 45 << 20
	)

	cases := []struct {
		name     string
		afterKiB int64
		removed  int64
		raised   bool
	}{
		{name: "a run that removed nothing over the cap raises the item", afterKiB: overCap, raised: true},
		{name: "a reading at the cap", afterKiB: capKiB},
		{name: "a reading under the cap", afterKiB: betweenWarnAndCap},
		{name: "a run that removed files while over the cap", afterKiB: overCap, removed: 7, raised: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			f.exec = ciCacheReply(tt.afterKiB, tt.removed, tt.afterKiB)
			f.tick()
			f.drain()
			if tt.raised {
				// An over-cap reading is read again a floor later, with the same reply,
				// before the item is looked for.
				f.tickAFloorLater()
			}

			items := f.d.cacheTrimNoOp.observed()
			if !tt.raised {
				if len(items) != 0 {
					t.Errorf("%s raised %+v, want no item (after %d KiB, %d removed, cap %s)",
						tt.name, items, tt.afterKiB, tt.removed, polecatimage.Human(capKiB*1024))
				}
				return
			}
			if got := noopKeys(items); !slices.Equal(got, []string{ciCacheNoOpKey}) {
				t.Fatalf("items = %v, want one item keyed %q for ci-gocache", got, ciCacheNoOpKey)
			}
			if items[0].Kind != attention.KindCacheTrimNoOp || items[0].Severity != attention.SeverityLow {
				t.Errorf("item kind %q severity %q, want the cache trim kind at low severity", items[0].Kind, items[0].Severity)
			}
			for _, want := range []string{ciCacheGocacheVolume, "its cap of " + polecatimage.Human(capKiB*1024)} {
				if !strings.Contains(items[0].Summary, want) {
					t.Errorf("summary %q does not carry %q", items[0].Summary, want)
				}
			}
		})
	}

	t.Run("a later reading under the cap clears the item", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()
		f.tickAFloorLater()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 1 {
			t.Fatalf("items over the cap = %+v, want one", items)
		}

		f.at(3 * time.Hour)
		f.exec = ciCacheReply(betweenWarnAndCap, 0, betweenWarnAndCap)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("items after a reading under the cap = %+v, want none", items)
		}
	})
}

// TestCICacheTrimGocacheOverCapAfterRemovals: the ci-gocache volume is trimmed by
// the LRU program, which never selects a file younger than its floor. A volume
// that writes more than its cap within the floor therefore ends every trim over
// the cap with files removed, and that reading raises the volume's item exactly
// as a run that removed nothing does: over the cap raises it whatever the removed
// count, at or under the cap clears it, and the summary is the one a run that
// removed nothing raises.
func TestCICacheTrimGocacheOverCapAfterRemovals(t *testing.T) {
	t.Parallel()
	const (
		capKiB  = 40 << 20 // KiB: the default ci_gocache_cap_bytes
		overCap = 55 << 20
	)

	// raiseOverCap runs one apply pass whose reply removed removed files and ended
	// over the cap, reads the same reply again a floor later, and returns the item
	// it raised.
	raiseOverCap := func(t *testing.T, f *ciCacheFixture, removed int64) attention.Item {
		t.Helper()
		f.exec = ciCacheReply(overCap, removed, overCap)
		f.tick()
		f.drain()
		f.tickAFloorLater()
		items := f.d.cacheTrimNoOp.observed()
		if got := noopKeys(items); !slices.Equal(got, []string{ciCacheNoOpKey}) {
			t.Fatalf("a run that removed %d files and ended over the cap raised %v, want one item keyed %q",
				removed, got, ciCacheNoOpKey)
		}
		return items[0]
	}

	t.Run("a run that removed files over the cap raises the item's existing summary", func(t *testing.T) {
		t.Parallel()
		withoutFiles := raiseOverCap(t, newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply), 0)
		withFiles := raiseOverCap(t, newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply), 19000)
		if withFiles.Kind != withoutFiles.Kind || withFiles.Severity != withoutFiles.Severity {
			t.Errorf("the item for a run that removed files is kind %q severity %q, want the kind %q severity %q a run that removed nothing raises",
				withFiles.Kind, withFiles.Severity, withoutFiles.Kind, withoutFiles.Severity)
		}
		if withFiles.Summary != withoutFiles.Summary {
			t.Errorf("the summary for a run that removed files = %q, want the item's existing summary %q",
				withFiles.Summary, withoutFiles.Summary)
		}
	})

	t.Run("a later reading at or under the cap clears the item", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name     string
			afterKiB int64
		}{
			{name: "at the cap", afterKiB: capKiB},
			{name: "under the cap", afterKiB: capKiB - 1024},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
				raiseOverCap(t, f, 19000)

				f.at(3 * time.Hour)
				f.exec = ciCacheReply(tt.afterKiB, 19000, tt.afterKiB)
				f.tick()
				f.drain()
				if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
					t.Errorf("a later reading %s left %+v, want none", tt.name, items)
				}
			})
		}
	})
}

// TestCICacheTrimGocacheNoopNeedsTheFloor: the ci-gocache volume raises its item
// only when its run of over-cap readings has lasted ciCacheLRUFloorMinutes. The
// run starts at its first over-cap reading, so one reading, or two readings less
// than the floor apart, raise nothing; a reading under the cap ends the run. Each
// row is read once with removed nothing and once with files removed, since the
// floor applies to both.
func TestCICacheTrimGocacheNoopNeedsTheFloor(t *testing.T) {
	t.Parallel()
	const (
		overCap  = 45 << 20 // KiB: over the default 40 GiB ci_gocache_cap_bytes
		underCap = 35 << 20
		cadence  = ciCacheTestEvery
		floor    = ciCacheLRUFloorMinutes * time.Minute
	)

	// reading is one trim's reading, gap after the one before it.
	type reading struct {
		gap  time.Duration
		over bool
	}
	cases := []struct {
		name     string
		readings []reading
		raised   bool
	}{
		{
			name:     "one over-cap trim raises nothing",
			readings: []reading{{0, true}},
		},
		{
			name:     "two over-cap trims an hour apart raise nothing",
			readings: []reading{{0, true}, {cadence, true}},
		},
		{
			name:     "two over-cap trims a minute under the floor apart raise nothing",
			readings: []reading{{0, true}, {floor - time.Minute, true}},
		},
		{
			name:     "two over-cap trims the floor apart raise the item",
			readings: []reading{{0, true}, {floor, true}},
			raised:   true,
		},
		{
			name:     "the floor counts from the run's first over-cap reading",
			readings: []reading{{0, true}, {cadence, true}, {cadence, true}},
			raised:   true,
		},
		{
			name:     "a reading under the cap restarts the run",
			readings: []reading{{0, true}, {cadence, false}, {cadence, true}, {cadence, true}},
		},
	}
	for _, removed := range []int64{0, 7} {
		for _, tt := range cases {
			t.Run(fmt.Sprintf("%s, removing %d files", tt.name, removed), func(t *testing.T) {
				t.Parallel()
				f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
				for _, r := range tt.readings {
					f.now = f.now.Add(r.gap)
					after, gone := int64(underCap), int64(0)
					if r.over {
						after, gone = overCap, removed
					}
					f.exec = ciCacheReply(after, gone, after)
					f.tick()
					f.drain()
				}
				// A reading the cadence skipped tests nothing, so every row must read
				// the volume once per reading it lists.
				if got := volumeExecs(f, ciCacheGocacheVolume); got != len(tt.readings) {
					t.Fatalf("the volume was read %d times, want %d", got, len(tt.readings))
				}
				want := []string(nil)
				if tt.raised {
					want = []string{ciCacheNoOpKey}
				}
				if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, want) {
					t.Errorf("items after the last reading = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestCICacheTrimGocacheNoopRunEndsUnderCap: a reading at or under the cap clears
// the ci-gocache item a run raised, and ends that run. The next over-cap reading
// starts a new run, so it raises nothing, and the item comes back only once a
// floor has passed from that new run's first reading. Each step checks the item
// after its own reading, and that the volume was read, since a reading the
// cadence skipped tests nothing.
func TestCICacheTrimGocacheNoopRunEndsUnderCap(t *testing.T) {
	t.Parallel()
	const (
		overCap  = 45 << 20 // KiB: over the default 40 GiB ci_gocache_cap_bytes
		underCap = 35 << 20
		cadence  = ciCacheTestEvery
		floor    = ciCacheLRUFloorMinutes * time.Minute
	)

	// step is one trim's reading, gap after the one before it, and whether the
	// item is raised once the reading is measured.
	type step struct {
		name   string
		gap    time.Duration
		over   bool
		raised bool
	}
	steps := []step{
		{"the first over-cap reading raises nothing", 0, true, false},
		{"a second over-cap reading a floor later raises the item", floor, true, true},
		{"an under-cap reading clears the item", cadence, false, false},
		{"the next over-cap reading starts a new run and raises nothing", cadence, true, false},
		{"an over-cap reading a cadence into the new run raises nothing", cadence, true, false},
		{"an over-cap reading a floor past the new run's start raises the item again", cadence, true, true},
	}
	for _, removed := range []int64{0, 7} {
		t.Run(fmt.Sprintf("removing %d files", removed), func(t *testing.T) {
			t.Parallel()
			f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
			for i, s := range steps {
				f.now = f.now.Add(s.gap)
				after, gone := int64(underCap), int64(0)
				if s.over {
					after, gone = overCap, removed
				}
				f.exec = ciCacheReply(after, gone, after)
				f.tick()
				f.drain()
				if got := volumeExecs(f, ciCacheGocacheVolume); got != i+1 {
					t.Fatalf("%s: the volume was read %d times, want %d", s.name, got, i+1)
				}
				want := []string(nil)
				if s.raised {
					want = []string{ciCacheNoOpKey}
				}
				if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, want) {
					t.Errorf("%s: items = %v, want %v", s.name, got, want)
				}
			}
		})
	}
}

// TestCICacheTrimGocacheNoopFailedTrimKeepsTheRun: a failed trim between two
// over-cap ci-gocache readings leaves the run alone. A failure runs no
// measurement, so the run still dates from the first over-cap reading. The
// second successful reading comes a cadence after the failure, which is past the
// floor from the first, so it raises the item. A failure that reset the run
// would start it over at the second reading and raise nothing.
func TestCICacheTrimGocacheNoopFailedTrimKeepsTheRun(t *testing.T) {
	t.Parallel()
	const (
		overCap = 45 << 20 // KiB: over the default 40 GiB ci_gocache_cap_bytes
		cadence = ciCacheTestEvery
	)
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)

	f.exec = ciCacheReply(overCap, 0, overCap)
	f.tick()
	f.drain()
	if got := noopKeys(f.d.cacheTrimNoOp.observed()); len(got) != 0 {
		t.Fatalf("the first over-cap reading raised %v, want none", got)
	}

	f.now = f.now.Add(cadence)
	f.exec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}
	f.tick()
	f.drain()
	if got := volumeExecs(f, ciCacheGocacheVolume); got != 2 {
		t.Fatalf("the volume was read %d times after the failed trim, want 2", got)
	}
	if got := noopKeys(f.d.cacheTrimNoOp.observed()); len(got) != 0 {
		t.Fatalf("the failed trim raised %v, want none", got)
	}

	// The failure concluded at the cadence's end, so its next attempt is a
	// cadence after that, which is past the floor from the first reading.
	f.now = f.now.Add(cadence)
	f.exec = ciCacheReply(overCap, 0, overCap)
	f.tick()
	f.drain()
	if got := volumeExecs(f, ciCacheGocacheVolume); got != 3 {
		t.Fatalf("the volume was read %d times after the retry, want 3", got)
	}
	want := []string{ciCacheNoOpKey}
	if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, want) {
		t.Errorf("items after the second successful reading = %v, want %v", got, want)
	}
}

// TestCICacheTrimGocacheNoopSummaryNamesTheFloor: the ci-gocache volume is
// trimmed by the LRU program, which never selects a file younger than
// ciCacheLRUFloorMinutes. A volume still over its cap after a trim therefore
// holds only writes from inside that floor, and its no-op item's summary says
// so: it names the figure left, the cap and the floor in minutes. A volume that
// is not lru keeps the summary that names the max age instead.
func TestCICacheTrimGocacheNoopSummaryNamesTheFloor(t *testing.T) {
	t.Parallel()
	const (
		capKiB  = 40 << 20 // KiB: the default ci_gocache_cap_bytes
		overCap = 45 << 20
	)

	t.Run("the raised item names the figure left, the cap and the floor", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()
		f.tickAFloorLater()

		items := f.d.cacheTrimNoOp.observed()
		if got := noopKeys(items); !slices.Equal(got, []string{ciCacheNoOpKey}) {
			t.Fatalf("items = %v, want one item keyed %q for ci-gocache", got, ciCacheNoOpKey)
		}
		want := fmt.Sprintf("%s holds %s, over its cap of %s: what is left was written inside the last %d minutes, which the trim keeps",
			ciCacheGocacheVolume, polecatimage.Human(overCap*1024), polecatimage.Human(capKiB*1024), ciCacheLRUFloorMinutes)
		if items[0].Summary != want {
			t.Errorf("summary = %q, want %q", items[0].Summary, want)
		}
	})

	t.Run("a volume that is not lru keeps its old summary", func(t *testing.T) {
		t.Parallel()
		const (
			npmCapKiB  = 1 << 20 // KiB: the npm volume's cap
			overNpmCap = 2 << 20
		)
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.npmExec = ciCacheReply(overNpmCap, 0, overNpmCap)
		f.tick()
		f.drain()

		npmKey := ciCacheNoOpKey + "-" + ciCacheNpmVolume
		items := f.d.cacheTrimNoOp.observed()
		if got := noopKeys(items); !slices.Equal(got, []string{npmKey}) {
			t.Fatalf("items = %v, want one item keyed %q for ci-npm", got, npmKey)
		}
		want := fmt.Sprintf("CI cache trim of %s removed no files while the volume holds %s, over its cap of %s: nothing there is older than the max age, or the trim is not reaching it",
			ciCacheNpmVolume, polecatimage.Human(overNpmCap*1024), polecatimage.Human(npmCapKiB*1024))
		if items[0].Summary != want {
			t.Errorf("summary = %q, want the old text %q", items[0].Summary, want)
		}
	})
}

// TestCICacheTrimGocacheNoopLogNamesTheCap: the ci-gocache volume's run line
// names its cap beside the before and after sizes, and carries no path or file
// name.
func TestCICacheTrimGocacheNoopLogNamesTheCap(t *testing.T) {
	t.Parallel()
	const (
		capKiB   = 40 << 20
		afterKiB = int64(45 << 20)
	)
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	f.exec = ciCacheReply(afterKiB, 0, afterKiB)
	f.tick()
	f.drain()

	want := fmt.Sprintf("%s: removed 0 files, %s to %s, against its cap of %s",
		ciCacheGocacheVolume, polecatimage.Human(afterKiB*1024), polecatimage.Human(afterKiB*1024), polecatimage.Human(capKiB*1024))
	if !strings.Contains(f.logs.String(), want) {
		t.Fatalf("the run line does not name the cap beside the sizes (%q):\n%s", want, f.logs.String())
	}
	if strings.Contains(f.logs.String(), "/") {
		t.Errorf("a log line holds a path:\n%s", f.logs.String())
	}
}

// gomodRow is the table's ci-gomod row, which the module tests read.
func gomodRow(t *testing.T) ciCacheVolume {
	t.Helper()
	i := slices.IndexFunc(ciCacheVolumes, func(v ciCacheVolume) bool { return v.name == ciCacheGomodVolume })
	if i < 0 {
		t.Fatalf("the table has no %s row", ciCacheGomodVolume)
	}
	return ciCacheVolumes[i]
}

// TestCICacheTrimGomodRowIsCappedAtFiveGiB: the ci-gomod row carries the 5 GiB
// cap, and its apply execs the module script with the volume root, the age and
// that cap as its argv; its dry run execs the module listing with the same cap.
func TestCICacheTrimGomodRowIsCappedAtFiveGiB(t *testing.T) {
	t.Parallel()
	row := gomodRow(t)
	if row.capKiB != 5<<20 {
		t.Errorf("%s cap = %d KiB, want 5 GiB (%d KiB)", ciCacheGomodVolume, row.capKiB, 5<<20)
	}

	t.Run("apply runs the module script with the cap", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.tick()
		f.drain()

		var gomod []string
		for _, c := range f.execs() {
			if slices.Contains(c.args, row.root) {
				gomod = c.args
			}
		}
		if gomod == nil {
			t.Fatalf("the pass ran no exec into %s", ciCacheGomodVolume)
		}
		want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
			"sh", "-c", ciCacheModuleTrimScript, "ci-cache-trim", row.root, ciCacheTestMinutes, ciCacheTestGomodCapKiB}
		if !slices.Equal(gomod[4:], want) {
			t.Errorf("gomod exec argv = %q\nwant %q", gomod[4:], want)
		}
	})

	t.Run("dry run lists with the same cap and sends no deletion", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		f.tick()
		f.drain()

		var gomod []string
		for _, c := range f.execs() {
			if slices.Contains(c.args, row.root) {
				gomod = c.args
			}
		}
		if gomod == nil {
			t.Fatalf("the pass ran no exec into %s", ciCacheGomodVolume)
		}
		want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
			"sh", "-c", ciCacheModuleListScript, "ci-cache-trim", row.root, ciCacheTestMinutes, ciCacheTestGomodCapKiB}
		if !slices.Equal(gomod[4:], want) {
			t.Errorf("gomod dry-run argv = %q\nwant %q", gomod[4:], want)
		}
	})
}

// TestCICacheTrimModuleScriptNeverNamesTheDownloadCache: both module programs
// prune ./cache before they match a module directory, and every rm they run is
// rm -rf, so none removes a single file. The integration tier runs the text.
func TestCICacheTrimModuleScriptNeverNamesTheDownloadCache(t *testing.T) {
	t.Parallel()
	for name, script := range map[string]string{
		"apply":   ciCacheModuleTrimScript,
		"dry run": ciCacheModuleListScript,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prune := strings.Index(script, "-path ./cache -prune")
			match := strings.Index(script, "-name '*@*'")
			if prune < 0 || match < 0 || prune > match {
				t.Errorf("the walk does not prune ./cache before it matches a module: prune at %d, match at %d", prune, match)
			}
			for rest := script; ; {
				i := strings.Index(rest, "rm ")
				if i < 0 {
					break
				}
				if !strings.HasPrefix(rest[i:], "rm -rf ") {
					t.Errorf("the script runs %q, want only rm -rf", rest[i:min(i+12, len(rest))])
				}
				rest = rest[i+len("rm "):]
			}
		})
	}
}

// TestCICacheTrimModuleScriptRemovesNULSafely: the module apply lists its
// directories NUL-terminated and removes them with xargs -0, so a name holding a
// space or a line break is one entry. A loop over the list would split a name at
// a line break, so the apply has none.
func TestCICacheTrimModuleScriptRemovesNULSafely(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"-print0", "xargs -0 rm -rf"} {
		if !strings.Contains(ciCacheModuleTrimScript, want) {
			t.Errorf("the module apply does not contain %q", want)
		}
	}
	for _, loop := range []string{"while ", "for "} {
		if strings.Contains(ciCacheModuleTrimScript, loop) {
			t.Errorf("the module apply loops with %q over its list, which splits a name at a line break", loop)
		}
	}
}

// TestCICacheTrimModuleVolumeReadings: the module volume's apply logs the count
// of module directories it removed, and raises its own no-op item over its cap
// when it removed none, keyed apart from the other volumes' items.
func TestCICacheTrimModuleVolumeReadings(t *testing.T) {
	t.Parallel()
	row := gomodRow(t)
	key := ciCacheNoOpKeyOf(row)

	t.Run("the apply logs the module directories it removed", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.gomodExec = ciCacheReply(6<<20, 2, 5<<20)
		f.tick()
		f.drain()
		if want := ciCacheGomodVolume + ": removed 2 module directories"; !strings.Contains(f.logs.String(), want) {
			t.Errorf("logs do not carry %q:\n%s", want, f.logs.String())
		}
	})

	t.Run("over the cap with nothing removed raises the item, under the cap clears it", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.gomodExec = ciCacheReply(6<<20, 0, 6<<20)
		f.tick()
		f.drain()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{key}) {
			t.Fatalf("items = %v, want one item keyed %q", got, key)
		}

		f.at(time.Hour)
		f.gomodExec = ciCacheReply(5<<20, 0, 5<<20)
		f.tick()
		f.drain()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); len(got) != 0 {
			t.Errorf("items after the module volume is at its cap = %v, want none", got)
		}
	})
}

// noopKeys returns the keys of the items, in the order observed returns them.
func noopKeys(items []attention.Item) []string {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	return keys
}

// TestCICacheTrimFloorCoversTheGoTouchInterval: the floor exceeds Go's touch
// interval by the margin, which is at least fifteen minutes.
func TestCICacheTrimFloorCoversTheGoTouchInterval(t *testing.T) {
	t.Parallel()
	if got := ciCacheLRUFloorMinutes - ciCacheGoTouchIntervalMinutes; got != ciCacheGateMarginMinutes {
		t.Errorf("the floor exceeds Go's %d minute touch interval by %d, want the %d minute margin",
			ciCacheGoTouchIntervalMinutes, got, ciCacheGateMarginMinutes)
	}
	if ciCacheGateMarginMinutes < 15 {
		t.Errorf("ciCacheGateMarginMinutes = %d, want at least 15", ciCacheGateMarginMinutes)
	}
}

// TestCICacheTrimFloorIsInTheArgv: the apply's argv and the dry run's list both
// carry the floor of 75 minutes as their last element, where the LRU program
// reads it.
func TestCICacheTrimFloorIsInTheArgv(t *testing.T) {
	t.Parallel()
	row := ciCacheVolume{name: ciCacheGocacheVolume, root: ciCacheTestGocacheRoot, lru: true, capKiB: 40 << 20}
	for _, mode := range []string{agentconfig.DiskMaintenanceModeApply, agentconfig.DiskMaintenanceModeDryRun} {
		program, err := ciCacheTrimProgram(mode, row, 720)
		if err != nil {
			t.Fatalf("ciCacheTrimProgram(%s) = %v, want nil", mode, err)
		}
		if got := program[len(program)-1]; got != "75" {
			t.Errorf("%s argv carries the floor %q, want 75", mode, got)
		}
	}
}

// TestCICacheTrimLRUArgv: the LRU program's argv is built from integers, so the
// numbers the script computes with are digits; a negative number, an empty root
// and a mode the program does not have are refused rather than passed on. The
// valid root in the negative-number rows leaves the number check as the one
// their names claim to exercise.
func TestCICacheTrimLRUArgv(t *testing.T) {
	t.Parallel()
	root := ciCacheVolumeRoot(ciCacheGocacheVolume)
	good, err := ciCacheLRUArgv("list", root, 120, 41943040, ciCacheLRUFloorMinutes)
	if err != nil {
		t.Fatalf("ciCacheLRUArgv = %v, want nil", err)
	}
	want := []string{"-c", ciCacheLRUTrimScript, "ci-cache-trim", "list", root, "120", "41943040", "75"}
	if !slices.Equal(good, want) {
		t.Errorf("argv = %q, want %q", good, want)
	}
	apply, err := ciCacheLRUArgv("apply", root, 120, 41943040, ciCacheLRUFloorMinutes)
	if err != nil {
		t.Fatalf("ciCacheLRUArgv(apply) = %v, want nil", err)
	}
	wantApply := []string{"-c", ciCacheLRUTrimScript, "ci-cache-trim", "apply", root, "120", "41943040", "75"}
	if !slices.Equal(apply, wantApply) {
		t.Errorf("apply argv = %q, want %q", apply, wantApply)
	}
	cases := []struct {
		name  string
		mode  string
		root  string
		age   int64
		cap   int64
		floor int64
	}{
		{name: "a negative age", mode: "list", root: root, age: -1, cap: 1, floor: 60},
		{name: "a negative cap", mode: "list", root: root, age: 120, cap: -1, floor: 60},
		{name: "a negative floor", mode: "list", root: root, age: 120, cap: 1, floor: -60},
		{name: "an empty root", mode: "list", root: "", age: 120, cap: 1, floor: 60},
		{name: "a mode the program does not have", mode: "delete", root: root, age: 120, cap: 1, floor: 60},
	}
	for _, tc := range cases {
		if argv, err := ciCacheLRUArgv(tc.mode, tc.root, tc.age, tc.cap, tc.floor); err == nil {
			t.Errorf("%s: argv %q, want an error", tc.name, argv)
		}
	}
}

// TestCICacheLRUArgvRefusesAnyRootButAVolumeRoot: the program's root is the one
// path its find reads and removes under, so the argv builder accepts only a
// volume's own data path and refuses every other root, naming the rule. The
// refusal includes the dind data root and its volumes directory, a relative
// path, a name that is not plain, and paths that only look like a volume root
// (gt-vbtu0m).
func TestCICacheLRUArgvRefusesAnyRootButAVolumeRoot(t *testing.T) {
	t.Parallel()
	root := ciCacheVolumeRoot(ciCacheGocacheVolume)
	argv, err := ciCacheLRUArgv("list", root, 120, 41943040, ciCacheLRUFloorMinutes)
	if err != nil {
		t.Fatalf("ciCacheLRUArgv(list, %q) = %v, want nil", root, err)
	}
	if !slices.Contains(argv, root) {
		t.Errorf("argv %q does not carry the accepted root %q", argv, root)
	}
	cases := []struct{ name, root string }{
		{name: "the root itself", root: "/"},
		{name: "the dind data root", root: ciCacheDindMount},
		{name: "the volumes directory", root: ciCacheDindMount + "/volumes"},
		{name: "the volumes directory with a trailing slash", root: ciCacheDindMount + "/volumes/"},
		{name: "the volume mount without its data directory", root: ciCacheDindMount + "/volumes/" + ciCacheGocacheVolume},
		{name: "a volume directory with another leaf", root: ciCacheDindMount + "/volumes/" + ciCacheGocacheVolume + "/data"},
		{name: "a relative path", root: "x/_data"},
		{name: "a volume root with a trailing slash", root: root + "/"},
		{name: "a name with a double dot inside", root: ciCacheDindMount + "/volumes/a..b/_data"},
		{name: "a name that is a double dot", root: ciCacheDindMount + "/volumes/../_data"},
		{name: "a name that is a single dot", root: ciCacheDindMount + "/volumes/./_data"},
		{name: "a name with a slash", root: ciCacheDindMount + "/volumes/a/b/_data"},
		{name: "a double slash before the name", root: ciCacheDindMount + "/volumes//ci-gocache/_data"},
		{name: "a name with a space", root: ciCacheDindMount + "/volumes/ci gocache/_data"},
		{name: "a root with a newline", root: root + "\n"},
		{name: "a path that steps above the volumes directory", root: ciCacheDindMount + "/../docker/volumes/ci-gocache/_data"},
		{name: "a prefix longer than the path between prefix and suffix", root: ciCacheDindMount + "/volumes/_data"},
		{name: "a sibling of the volumes directory", root: "/var/lib/docker2/volumes/ci-gocache/_data"},
		{name: "a path of the right shape under another root", root: "/" + strings.Repeat("x", 23) + ciCacheGocacheVolume + "/_data"},
	}
	for _, tc := range cases {
		argv, err := ciCacheLRUArgv("list", tc.root, 120, 41943040, ciCacheLRUFloorMinutes)
		if err == nil {
			t.Errorf("%s (%q): argv %q, want an error", tc.name, tc.root, argv)
			continue
		}
		if argv != nil {
			t.Errorf("%s (%q): argv %q returned with the error, want no argv", tc.name, tc.root, argv)
		}
		if !strings.Contains(err.Error(), "volume root") {
			t.Errorf("%s (%q): error %q does not name the volume-root rule", tc.name, tc.root, err)
		}
	}
}

// ciCacheLedgerExecSpan is how long each fake exec takes on the fixture's clock.
const ciCacheLedgerExecSpan = 2 * time.Minute

// ciCacheLedgerClockExecs makes each volume's exec take ciCacheLedgerExecSpan on
// the fixture's clock, and returns the instant each one started, by root.
func ciCacheLedgerClockExecs(f *ciCacheFixture) map[string]time.Time {
	starts := map[string]time.Time{}
	run := f.cli.run
	f.d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		if strings.HasPrefix(cacheTrimVerb(cmd.Args[1:]), "exec ") {
			for _, v := range ciCacheVolumes {
				if slices.Contains(cmd.Args, v.root) {
					starts[v.root] = f.now
				}
			}
			f.now = f.now.Add(ciCacheLedgerExecSpan)
		}
		return run(cmd)
	}
	return starts
}

// ciCacheLedgerRuns reads the job ledger's runs of one volume's trim job.
func ciCacheLedgerRuns(t *testing.T, f *ciCacheFixture, name string) []jobledger.Run {
	t.Helper()
	ledger, err := jobledger.NewLedger(f.d.config.TownRoot)
	if err != nil {
		t.Fatalf("opening the job ledger: %v", err)
	}
	runs, err := ledger.Tail(ciCacheTrimName+":"+name, 10)
	if err != nil {
		t.Fatalf("reading the job ledger: %v", err)
	}
	return runs
}

// TestCICacheLedgerRecordsEachApplyRun: an apply appends one run per volume under
// ci_cache_trim:<volume>, its times bracketing that volume's exec, with the reply's
// figures in bytes. young_bytes is only in a reply that reports young_kib, cap_bytes
// is on each capped volume, and a volume that grew records removed_bytes 0.
func TestCICacheLedgerRecordsEachApplyRun(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	f.exec = cliReply{stdout: "before_kib=3000\nremoved=3\nafter_kib=1000\nyoung_kib=400\n"}
	f.npmExec = ciCacheReply(2<<20, 4, 1<<20)
	f.gomodExec = ciCacheReply(1000, 0, 1500)
	starts := ciCacheLedgerClockExecs(f)
	f.tick()
	f.drain()

	_, _, gocacheCap, err := ciCacheTrimSettings(f.d.patrol())
	if err != nil {
		t.Fatalf("reading the trim settings: %v", err)
	}
	want := map[string]map[string]int64{
		ciCacheGocacheVolume: {"claim_bytes_before": 3000 << 10, "claim_bytes_after": 1000 << 10, "removed_files": 3,
			"removed_bytes": 2000 << 10, "young_bytes": 400 << 10, "cap_bytes": (gocacheCap / 1024) * 1024},
		ciCacheNpmVolume: {"claim_bytes_before": 2 << 30, "claim_bytes_after": 1 << 30, "removed_files": 4,
			"removed_bytes": 1 << 30, "cap_bytes": ciCacheNpmCapKiB * 1024},
		ciCacheGomodVolume: {"claim_bytes_before": 1000 << 10, "claim_bytes_after": 1500 << 10, "removed_files": 0,
			"removed_bytes": 0, "cap_bytes": ciCacheGomodCapKiB * 1024},
	}
	for name, stats := range want {
		runs := ciCacheLedgerRuns(t, f, name)
		if len(runs) != 1 {
			t.Errorf("%s: the ledger holds %d run(s), want one\n%s", name, len(runs), f.logs.String())
			continue
		}
		run, root := runs[0], ciCacheVolumeRoot(name)
		if run.Outcome != ciCacheTrimOutcomeOK || !run.StartedAt.Equal(starts[root]) ||
			!run.EndedAt.Equal(starts[root].Add(ciCacheLedgerExecSpan)) || !maps.Equal(run.Stats, stats) {
			t.Errorf("%s: run %+v, want outcome %q from %v for %v with stats %v",
				name, run, ciCacheTrimOutcomeOK, starts[root], ciCacheLedgerExecSpan, stats)
		}
	}
}

// TestCICacheLedgerUncappedVolumeHasNoCapBytes: a volume with no cap records its
// run with no cap_bytes. The trim table has no uncapped volume, so this drives the
// volume's trim directly, the production call that writes the run.
func TestCICacheLedgerUncappedVolumeHasNoCapBytes(t *testing.T) {
	t.Parallel()
	f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
	_, maxAge, _, err := ciCacheTrimSettings(f.d.patrol())
	if err != nil {
		t.Fatalf("reading the trim settings: %v", err)
	}
	v := ciCacheVolume{name: "ci-scratch", root: ciCacheVolumeRoot("ci-scratch")}
	at := ciCacheExecTarget{kubeContext: ciCacheTestContext, pod: ciCacheTestPod}
	if ok, why := f.d.ciCacheTrimVolume(context.Background(), agentconfig.DiskMaintenanceModeApply, at, v, maxAge); !ok {
		t.Fatalf("the trim reached no verdict: %s\n%s", why, f.logs.String())
	}
	runs := ciCacheLedgerRuns(t, f, v.name)
	if len(runs) != 1 {
		t.Fatalf("the ledger holds %d run(s), want one\n%s", len(runs), f.logs.String())
	}
	if _, ok := runs[0].Stats["cap_bytes"]; ok {
		t.Errorf("an uncapped volume recorded cap_bytes, want none: %v", runs[0].Stats)
	}
}
