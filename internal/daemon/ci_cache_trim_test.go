package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
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

// ciCachePodsRunningJSON is one runner pod whose dind container is running.
const ciCachePodsRunningJSON = `{"items":[{"metadata":{"name":"` + ciCacheTestPod + `"},` +
	`"status":{"containerStatuses":[` +
	`{"name":"runner","state":{"running":{"startedAt":"2026-11-06T01:00:00Z"}}},` +
	`{"name":"dind","state":{"running":{"startedAt":"2026-11-06T01:00:00Z"}}}]}}]}`

// ciCachePodsDindWaitingJSON is the same pod with its dind container not yet up.
const ciCachePodsDindWaitingJSON = `{"items":[{"metadata":{"name":"` + ciCacheTestPod + `"},` +
	`"status":{"containerStatuses":[{"name":"dind","state":{"waiting":{"reason":"ContainerCreating"}}}]}}]}`

// ciCacheFixture is the daemon a CI cache trim test runs: the patrol on, a
// kubernetes runner named on the tier sweep, kubectl answered in process, and a
// clock and a dispatch the test drives.
type ciCacheFixture struct {
	d    *Daemon
	cli  *fakeCLI
	logs *bytes.Buffer
	now  time.Time
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
// reply of an apply's figures, which a test may replace before it ticks. The npm
// and module volumes read under their caps by default, so they raise no item
// unless a test makes them.
func newCICacheFixture(t *testing.T, mode string) *ciCacheFixture {
	t.Helper()
	var logs bytes.Buffer
	f := &ciCacheFixture{
		logs:      &logs,
		now:       ciCacheTestStart,
		pods:      cliReply{stdout: ciCachePodsRunningJSON},
		exec:      ciCacheReply(40<<20, 3, 10<<20),
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
	d.diskMaint.dispatch = func(fn func()) { f.pending = append(f.pending, fn) }
	f.d = d
	return f
}

// ciCacheReply is the script's three-line reply for the given KiB figures.
func ciCacheReply(beforeKiB, removed, afterKiB int64) cliReply {
	return cliReply{stdout: fmt.Sprintf("before_kib=%d\nremoved=%d\nafter_kib=%d\n", beforeKiB, removed, afterKiB)}
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
// control context, the volume's path and the age in minutes; the uncapped
// ci-gocache argv is the one the trim has always sent, and the capped ci-npm argv
// adds the cap; a dry run sends no deletion; and the kubectl calls name the
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
		want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
			"sh", "-c", ciCacheTrimScript, "ci-cache-trim", ciCacheTestGocacheRoot, ciCacheTestMinutes}
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
		for _, arg := range execs[0].args {
			if strings.Contains(arg, "-delete") {
				t.Errorf("a dry run sent a deletion: %q", arg)
			}
		}
		for _, c := range f.cli.recorded() {
			if strings.Contains(cacheTrimVerb(c.args), "delete") {
				t.Errorf("a dry run ran a delete: %q", cacheTrimVerb(c.args))
			}
		}
	})
}

// TestCICacheTrimConstantsMatchTheRunnerManifest holds the runner names and the
// dind mount the trim execs into equal to the runner's deployment manifest.
func TestCICacheTrimConstantsMatchTheRunnerManifest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "forgejo-runner", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatalf("reading the runner manifest: %v", err)
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
					Containers []struct {
						Name         string `yaml:"name"`
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
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
	for _, c := range m.Spec.Template.Spec.Containers {
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
// starts, and a failed run retried in fifteen minutes.
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

	t.Run("a failed run is retried in fifteen minutes", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}
		f.tick()
		f.drain()

		f.at(14 * time.Minute)
		f.tick()
		if len(f.pending) != 0 {
			t.Fatalf("a failed run was retried at 14m: the retry is 15 minutes out")
		}
		f.at(15 * time.Minute)
		f.tick()
		if len(f.pending) != 1 {
			t.Fatalf("a failed run was not retried at 15m: dispatched %d", len(f.pending))
		}
	})

	t.Run("a failed ci-npm does not move ci-gocache's cadence", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.npmExec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}
		f.tick()
		f.drain()

		f.at(15 * time.Minute)
		f.tick()
		f.drain()
		if n := volumeExecs(f, ciCacheGocacheVolume); n != 1 {
			t.Fatalf("ci-gocache ran %d times, want 1: its verdict at 0m stands, and ci-npm's failure must not retry it at 15m", n)
		}
		if n := volumeExecs(f, ciCacheNpmVolume); n != 2 {
			t.Fatalf("ci-npm ran %d times, want 2: the failed volume is retried in fifteen minutes", n)
		}
	})

	t.Run("a failed ci-gocache does not move ci-npm's cadence", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = cliReply{stderr: "error: unable to upgrade connection\n", code: 1}
		f.tick()
		f.drain()

		f.at(15 * time.Minute)
		f.tick()
		f.drain()
		if n := volumeExecs(f, ciCacheNpmVolume); n != 1 {
			t.Fatalf("ci-npm ran %d times, want 1: its verdict at 0m stands, and ci-gocache's failure must not retry it at 15m", n)
		}
		if n := volumeExecs(f, ciCacheGocacheVolume); n != 2 {
			t.Fatalf("ci-gocache ran %d times, want 2: the failed volume is retried in fifteen minutes", n)
		}
	})
}

// TestCICacheTrimFailuresAreFailedRuns: a run that cannot read the volume, reach
// the container or make sense of the reply is a failed run with its own line,
// logged once an hour, retried soon, and never read as a volume with nothing to
// trim.
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
				t.Errorf("a failed run raised %d attention item(s): a failure is not a reading", n)
			}
			if got, want := f.d.diskMaintSchedule.retryAtOf(ciCacheVolumeSchedule(ciCacheVolumes[0])), ciCacheTestStart.Add(cacheTrimRetryInterval); !got.Equal(want) {
				t.Errorf("retry at %v, want %v: a failed run is retried in fifteen minutes", got, want)
			}

			// The same failure a retry later is logged once in the hour.
			f.at(cacheTrimRetryInterval)
			f.tick()
			f.drain()
			if n := strings.Count(f.logs.String(), tt.want); n != 1 {
				t.Errorf("the failure was logged %d times across two attempts, want once an hour", n)
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
	f.exec = cliReply{stderr: "error: container not found: 1a2b3c\n", code: 1}
	f.tick()
	f.drain()

	f.exec = cliReply{stderr: "error: container not found: 9f8e7d\n", code: 1}
	f.at(cacheTrimRetryInterval)
	f.tick()
	f.drain()

	// Only the failed volume is due at the retry: ci-npm keeps its verdict.
	if n := volumeExecs(f, ciCacheGocacheVolume); n != 2 {
		t.Fatalf("ci-gocache ran %d execs, want 2: the retry must run", n)
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
	f.pods = cliReply{stdout: ciCachePodsDindWaitingJSON}
	f.tick()
	f.drain()

	f.pods = cliReply{stderr: "error: the server is unavailable\n", code: 1}
	f.at(cacheTrimRetryInterval)
	f.tick()
	f.drain()

	for _, want := range []string{"is not running in forgejo", "the runner's pods could not be read"} {
		if n := strings.Count(f.logs.String(), want); n != 1 {
			t.Errorf("logged %q %d times, want once: a different fault was hidden:\n%s", want, n, f.logs.String())
		}
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
		// retry is whether the refused run is tried again in fifteen minutes.
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
			name: "a zero warn figure",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.DiskMaintenance.CICacheWarnBytes = "0GB"
			},
			want: "ci_cache_warn_bytes",
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
			if got, want := f.d.diskMaintSchedule.retryAtOf(ciCacheVolumeSchedule(ciCacheVolumes[0])), ciCacheTestStart.Add(cacheTrimRetryInterval); tt.retry && !got.Equal(want) {
				t.Errorf("retry at %v, want %v: a refused run is tried again in fifteen minutes", got, want)
			}
		})
	}
}

// TestCICacheTrimNoopItem: a trim that removed no file while the volume stays
// over the warn figure raises one low attention item; a repeat keeps that one
// item; a later reading under the figure clears it; a trim that removed files,
// and a dry run, raise nothing.
func TestCICacheTrimNoopItem(t *testing.T) {
	t.Parallel()
	const overWarn = 40 << 20 // KiB: 40 GB, over the default 30 GB warn figure
	const underWarn = 10 << 20

	t.Run("one item for a trim that removed nothing over the warn figure, cleared under it", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overWarn, 0, overWarn)
		f.tick()
		f.drain()

		items := f.d.cacheTrimNoOp.observed()
		if len(items) != 1 || items[0].Key != ciCacheNoOpKey {
			t.Fatalf("items after the first no-op = %+v, want one item keyed %q", items, ciCacheNoOpKey)
		}
		if items[0].Kind != attention.KindCacheTrimNoOp || items[0].Severity != attention.SeverityLow {
			t.Errorf("item kind %q severity %q, want the cache trim kind at low severity", items[0].Kind, items[0].Severity)
		}

		f.at(time.Hour)
		f.exec = ciCacheReply(overWarn, 0, overWarn)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 1 {
			t.Fatalf("items after a repeat = %d, want still one", len(items))
		}

		f.at(2 * time.Hour)
		f.exec = ciCacheReply(overWarn, 5, underWarn)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("items after a reading under the figure = %+v, want none", items)
		}
	})

	t.Run("the uncapped volume's item keeps its summary text", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overWarn, 0, overWarn)
		f.tick()
		f.drain()

		_, _, warn, err := ciCacheTrimSettings(f.d.patrolConfig)
		if err != nil {
			t.Fatalf("reading the settings: %v", err)
		}
		items := f.d.cacheTrimNoOp.observed()
		if len(items) != 1 {
			t.Fatalf("items = %+v, want one item", items)
		}
		want := fmt.Sprintf("CI cache trim removed no file while the Go build cache holds %s, over the warn figure of %s: nothing there is older than the max age, or the trim is not reaching it",
			polecatimage.Human(overWarn*1024), polecatimage.Human(warn))
		if items[0].Summary != want {
			t.Errorf("summary = %q\nwant %q: the uncapped volume's item text is unchanged", items[0].Summary, want)
		}
	})

	t.Run("a trim that removed files raises nothing", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overWarn, 7, overWarn)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("a trim that removed files raised %+v", items)
		}
	})

	t.Run("a dry run raises nothing", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		f.exec = ciCacheReply(overWarn, 0, overWarn)
		f.tick()
		f.drain()
		if items := f.d.cacheTrimNoOp.observed(); len(items) != 0 {
			t.Errorf("a dry run raised %+v", items)
		}
	})
}

// TestCICacheTrimReplyMustBeExact: the reply is exactly the three figures, in
// order, and a reply that is not is never read as zeros.
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

// TestCICacheTrimNoopItemPerVolume: the uncapped volume keeps the no-op item it
// has always had, measured against the warn figure. A capped volume raises its
// own item over its cap, even under the warn figure, clears it at or under the
// cap, and leaves the other volume's item alone.
func TestCICacheTrimNoopItemPerVolume(t *testing.T) {
	t.Parallel()
	const overWarn = 40 << 20 // KiB: 40 GB, over the default 30 GB warn figure
	const overCap = 2 << 20   // KiB: 2 GiB, over the 1 GiB npm cap and under the warn figure
	const npmKey = "ci-cache-trim-noop-ci-npm"

	t.Run("a capped volume raises its own item over its cap, under the warn figure", func(t *testing.T) {
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

	t.Run("the uncapped item and the capped item clear apart", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.exec = ciCacheReply(overWarn, 0, overWarn)
		f.npmExec = ciCacheReply(overCap, 0, overCap)
		f.tick()
		f.drain()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{ciCacheNoOpKey, npmKey}) {
			t.Fatalf("items = %v, want both volumes' items", got)
		}

		f.at(time.Hour)
		f.npmExec = ciCacheReply(ciCacheNpmCapKiB, 0, ciCacheNpmCapKiB)
		f.tick()
		f.drain()
		if got := noopKeys(f.d.cacheTrimNoOp.observed()); !slices.Equal(got, []string{ciCacheNoOpKey}) {
			t.Errorf("items after the npm volume is at its cap = %v, want only the uncapped volume's item", got)
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
