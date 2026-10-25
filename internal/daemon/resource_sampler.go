package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// The resource_sampler patrol appends one row per host component every
// interval to <town>/.runtime/metrics/resources/<UTC date>.jsonl, the file the
// metrics snapshot's resources group reads (gt-qqwey). The sampler measures;
// this patrol only decides when the next tick runs.
const defaultResourceSamplerInterval = resourcesampler.DefaultInterval

// minResourceSamplerInterval bounds how often a configured interval can sample:
// a shorter one only loads the host with ps.
const minResourceSamplerInterval = 5 * time.Second

// resourceSamplerTimeout bounds one sample, so a stuck ps cannot hold the
// daemon's select loop past it.
const resourceSamplerTimeout = 10 * time.Second

// resourceSamplerInterval is the period between two samples: the configured
// patrols.resource_sampler.interval, clamped up to minResourceSamplerInterval,
// or the sampler's default when the key is absent or does not parse.
func resourceSamplerInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.ResourceSampler != nil {
		if d, err := time.ParseDuration(config.Patrols.ResourceSampler.Interval); err == nil && d > 0 {
			return max(d, minResourceSamplerInterval)
		}
	}
	return defaultResourceSamplerInterval
}

// resourceTicker is the one call the patrol makes of a sampler, so a test can
// stand a fake in for the host's process list.
type resourceTicker interface {
	Tick(ctx context.Context) ([]resourcesampler.Row, error)
}

// newResourceSamplerFn builds the sampler a patrol ticks. Nil means the host's
// own: the real ps, the daemon's clock and the metrics directory.
type newResourceSamplerFn func(metricsDir string, clock resourcesampler.Clock, interval time.Duration) resourceTicker

// resourceSamplerPatrol paces the samples. The pacing is in memory, not in the
// daemon's persisted last-run record: that record's comparison rounds to the
// minute (patrol_last_run.go), and a sampler ticks faster than that. A restart
// costs one interval before the first sample, which is also when a new sampler
// has no previous tick to measure a CPU delta against.
type resourceSamplerPatrol struct {
	sampler  resourceTicker
	interval time.Duration
	last     time.Time
}

// tick samples the host once when the last sample is due, and reports whether
// it did. Due is an interval less a tenth of it: the daemon's ticker delivers
// late by a little, and a strict interval would then skip every other sample.
func (p *resourceSamplerPatrol) tick(ctx context.Context, now time.Time) (bool, error) {
	if !p.last.IsZero() && now.Sub(p.last) < p.interval-p.interval/10 {
		return false, nil
	}
	p.last = now
	_, err := p.sampler.Tick(ctx)
	return true, err
}

// triggerResourceSampler runs one due sample. It runs on the sampler's own
// ticker (daemon.go): the general heartbeat is minutes apart, and a row's
// cpu_pct is the delta over the interval since the previous row.
func (d *Daemon) triggerResourceSampler() {
	if d.config == nil || !d.isPatrolActive("resource_sampler") {
		return
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, resourceSamplerTimeout)
	defer cancel()
	if _, err := d.resourceSampler().tick(ctx, d.clk().Now()); err != nil {
		d.logger.Printf("resource_sampler: %v", err)
	}
}

// resourceSampler returns the patrol, building the sampler on first use.
func (d *Daemon) resourceSampler() *resourceSamplerPatrol {
	interval := resourceSamplerInterval(d.patrolConfig)

	d.resourceSamplerMu.Lock()
	defer d.resourceSamplerMu.Unlock()
	if d.samplerPatrol == nil {
		build := d.newResourceSampler
		if build == nil {
			build = func(metricsDir string, clock resourcesampler.Clock, interval time.Duration) resourceTicker {
				return resourcesampler.New(metricsDir, resourcesampler.PSLister{}, clock, interval)
			}
		}
		d.samplerPatrol = &resourceSamplerPatrol{
			sampler:  build(filepath.Join(d.config.TownRoot, ".runtime", "metrics"), d.clk(), interval),
			interval: interval,
		}
	}
	return d.samplerPatrol
}
