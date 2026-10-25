package daemon

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

func TestResourceSamplerDefaultsOn(t *testing.T) {
	t.Parallel()
	if !IsPatrolEnabled(nil, "resource_sampler") ||
		!IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "resource_sampler") {
		t.Error("resource_sampler must run when the daemon config does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "resource_sampler") {
		t.Error("an explicit enabled=false must turn resource_sampler off")
	}
}

func TestResourceSamplerInterval(t *testing.T) {
	t.Parallel()
	if got := resourceSamplerInterval(nil); got != 30*time.Second {
		t.Errorf("default interval = %v, want 30s", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: true, Interval: "10s"}}}
	if got := resourceSamplerInterval(cfg); got != 10*time.Second {
		t.Errorf("configured interval = %v, want 10s", got)
	}
	cfg.Patrols.ResourceSampler.Interval = "soon"
	if got := resourceSamplerInterval(cfg); got != 30*time.Second {
		t.Errorf("invalid interval = %v, want the 30s default", got)
	}
}

// fakeResourceTicker stands in for the host's process list and counts ticks.
type fakeResourceTicker struct {
	ticks int
	err   error
}

func (f *fakeResourceTicker) Tick(context.Context) ([]resourcesampler.Row, error) {
	f.ticks++
	return nil, f.err
}

// samplerSpec is what the daemon handed the seam when it built the sampler.
type samplerSpec struct {
	dir      string
	interval time.Duration
}

// newSamplerDaemon is a daemon whose sampler is fake and whose clock a test
// moves by hand.
func newSamplerDaemon(t *testing.T, town string, clock clockwork.Clock, config *DaemonPatrolConfig) (*Daemon, *fakeResourceTicker, *samplerSpec) {
	t.Helper()
	fake := &fakeResourceTicker{}
	spec := &samplerSpec{}
	d := &Daemon{
		config:       &Config{TownRoot: town},
		patrolConfig: config,
		logger:       log.New(io.Discard, "", 0),
		clock:        clock,
	}
	d.newResourceSampler = func(metricsDir string, _ resourcesampler.Clock, interval time.Duration) resourceTicker {
		spec.dir, spec.interval = metricsDir, interval
		return fake
	}
	return d, fake, spec
}

// The patrol samples once per interval and reads nothing in between, whatever
// the trigger's cadence is.
func TestTriggerResourceSamplerTicksOncePerInterval(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	config := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: true, Interval: "30s"}}}
	d, fake, spec := newSamplerDaemon(t, town, clock, config)

	d.triggerResourceSampler() // first tick: samples
	d.triggerResourceSampler() // no time has passed
	clock.Advance(29 * time.Second)
	d.triggerResourceSampler()
	if fake.ticks != 1 {
		t.Fatalf("ticks inside the first interval = %d, want 1", fake.ticks)
	}
	clock.Advance(time.Second)
	d.triggerResourceSampler()
	if fake.ticks != 2 {
		t.Errorf("ticks after one interval = %d, want 2", fake.ticks)
	}
	if want := filepath.Join(town, ".runtime", "metrics"); spec.dir != want {
		t.Errorf("sampler metrics dir = %q, want %q", spec.dir, want)
	}
	if spec.interval != 30*time.Second {
		t.Errorf("sampler interval = %v, want 30s", spec.interval)
	}
}

// A switched-off patrol neither builds a sampler nor ticks one.
func TestTriggerResourceSamplerOffTicksNothing(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	config := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: false}}}
	d, fake, _ := newSamplerDaemon(t, t.TempDir(), clock, config)

	d.triggerResourceSampler()
	clock.Advance(time.Minute)
	d.triggerResourceSampler()
	if fake.ticks != 0 {
		t.Errorf("a disabled patrol ticked %d times", fake.ticks)
	}
	if d.samplerPatrol != nil {
		t.Error("a disabled patrol built a sampler")
	}
}

// A failed tick is logged and counted as the interval's sample: the next check
// inside the interval stays quiet instead of retrying every trigger.
func TestTriggerResourceSamplerLogsAFailedTickOnce(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	config := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: true}}}
	d, fake, _ := newSamplerDaemon(t, t.TempDir(), clock, config)
	fake.err = context.Canceled

	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	d.triggerResourceSampler()
	d.triggerResourceSampler()
	if fake.ticks != 1 {
		t.Errorf("ticks = %d, want 1", fake.ticks)
	}
	if got := logs.String(); !strings.Contains(got, "resource_sampler: ") {
		t.Errorf("log = %q, want the sampler error", got)
	}
}
