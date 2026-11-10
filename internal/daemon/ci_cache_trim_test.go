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
)

const (
	// ciCacheTestContext is the control cluster context the fixture's runner names.
	ciCacheTestContext = "gt-control"
	// ciCacheTestPod is the runner pod the fixture's pod list holds.
	ciCacheTestPod = "forgejo-runner-7d9f8c-abcde"
	// ciCacheTestMinutes is the fixture's ci_cache_max_age, 12h, in minutes.
	ciCacheTestMinutes = "720"
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
	// pods and exec are the replies the pod list and the exec get.
	pods cliReply
	exec cliReply
}

// newCICacheFixture builds the fixture for mode: a running runner pod and a
// reply of an apply's figures, which a test may replace before it ticks.
func newCICacheFixture(t *testing.T, mode string) *ciCacheFixture {
	t.Helper()
	var logs bytes.Buffer
	f := &ciCacheFixture{
		logs: &logs,
		now:  ciCacheTestStart,
		pods: cliReply{stdout: ciCachePodsRunningJSON},
		exec: ciCacheReply(40<<20, 3, 10<<20),
	}
	f.cli = newFakeCLIFor(func(c cliCall) cliReply {
		verb := cacheTrimVerb(c.args)
		switch {
		case strings.HasPrefix(verb, "get pods "):
			return f.pods
		case strings.HasPrefix(verb, "exec "):
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

// TestCICacheTrimArgv: the exec names the runner's dind container, the control
// context, the one volume path and the age in minutes; a dry run sends no
// deletion; and the kubectl calls name the kubeconfig and the context.
func TestCICacheTrimArgv(t *testing.T) {
	t.Parallel()
	t.Run("apply removes under the volume with the age from the setting", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.tick()
		f.drain()

		execs := f.execs()
		if len(execs) != 1 {
			t.Fatalf("ran %d execs, want 1", len(execs))
		}
		argv := execs[0].args
		wantPrefix := diskmaint.KubectlArgs(diskMaintKubeconfig(f.d.config.TownRoot, func(string) string { return "" }), ciCacheTestContext)
		if !slices.Equal(argv[:4], wantPrefix) {
			t.Fatalf("kubectl prefix = %q, want the kubeconfig and the control context %q", argv[:4], wantPrefix)
		}
		want := []string{"exec", "-n", ciCacheNamespace, ciCacheTestPod, "-c", ciCacheContainer, "--",
			"sh", "-c", ciCacheTrimScript, "ci-cache-trim", ciCacheRoot, ciCacheTestMinutes}
		if !slices.Equal(argv[4:], want) {
			t.Errorf("exec argv = %q\nwant %q", argv[4:], want)
		}
	})

	t.Run("dry run sends no deletion", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeDryRun)
		f.tick()
		f.drain()

		execs := f.execs()
		if len(execs) != 1 {
			t.Fatalf("ran %d execs, want 1", len(execs))
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
		if n := len(f.execs()); n != 1 {
			t.Fatalf("ran %d execs, want 1", n)
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
		if n := len(f.execs()); n != 2 {
			t.Fatalf("ran %d execs at 60m, want 2: due an hour after its last verdict", n)
		}
	})

	t.Run("a busy town still dispatches it", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.d.maint.quiet = func(*Daemon) (bool, string) { return false, "a landing is in flight" }
		f.tick()
		f.drain()
		if n := len(f.execs()); n != 1 {
			t.Fatalf("a busy town ran %d execs, want 1: the CI trim is off the quiet guard", n)
		}
		if strings.Contains(f.logs.String(), "skipped") {
			t.Errorf("the busy tick logged a skip for the CI trim, which is not held back: %q", f.logs.String())
		}
	})

	t.Run("a pass re-reads the due check when it starts", func(t *testing.T) {
		t.Parallel()
		f := newCICacheFixture(t, agentconfig.DiskMaintenanceModeApply)
		f.tick()
		// Another pass concludes the trim after this tick's decision.
		f.d.diskMaintSchedule.record(ciCacheTrimName, ciCacheTestStart)
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
		// execs is how many exec calls the failure reaches.
		execs int
	}{
		{
			name:  "the volume path is not in the container",
			exec:  cliReply{stderr: "volume path not found\n", code: ciCacheMissingPathExit},
			want:  "is not in the dind container",
			execs: 1,
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
			execs: 1,
		},
		{
			name:  "the reply cannot be read",
			exec:  cliReply{stdout: "du: cannot access the path\n"},
			want:  "the volume's reply could not be read",
			execs: 1,
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
			if strings.Contains(f.logs.String(), "ci cache trim: removed") {
				t.Errorf("a failed run reported a removal:\n%s", f.logs.String())
			}
			if n := len(f.d.cacheTrimNoOp.observed()); n != 0 {
				t.Errorf("a failed run raised %d attention item(s): a failure is not a reading", n)
			}
			if got, want := f.d.diskMaintSchedule.retryAtOf(ciCacheTrimName), ciCacheTestStart.Add(cacheTrimRetryInterval); !got.Equal(want) {
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

	if n := len(f.execs()); n != 2 {
		t.Fatalf("ran %d execs, want 2: the retry must run", n)
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
			want: "names no control cluster",
		},
		{
			name: "a context that reads as a flag is refused",
			mutate: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.TierSweep.Runner.Context = "--kubeconfig=/tmp/x"
			},
			want: "names no control cluster",
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
