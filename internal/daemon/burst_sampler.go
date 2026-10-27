package daemon

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/metrics"
)

// The burst sampler drives metrics.RunBurst at the row's own cadence: it takes
// one sample, waits the row's IntervalS, and samples again, so a town's
// bursts.jsonl holds the spike the hourly snapshot misses (gt-ztem6). metrics
// owns the reading and the row's shape; this file owns only when the next
// sample runs, and it takes every wait from the daemon's clock rather than a
// timer of its own, so a test drives a fake clock instead of sleeping.

// burstSamplerJoinTimeout bounds the shutdown join: a sample the cancel cannot
// reach, such as a bd child that ignores its context, costs the bound and not
// the daemon's shutdown.
const burstSamplerJoinTimeout = 5 * time.Second

// burstSamplerSeams replaces the sampler's sample, its wait and its Env in
// tests; the zero value is production.
type burstSamplerSeams struct {
	// run takes one sample and appends it; nil is metrics.RunBurst.
	run func(ctx context.Context, env metrics.Env) (metrics.BurstRow, string, error)
	// wait blocks before the next sample; nil waits on the daemon's clock.
	wait func(ctx context.Context, d time.Duration)
	// env builds a sample's Env; nil is metrics.OSEnv.
	env func(town string) metrics.Env
}

// burstSampler paces metrics.RunBurst for one town. started keeps the loop
// single: two samplers would write two rows per cadence and pay the sample's
// price twice.
type burstSampler struct {
	town    string
	logf    func(format string, args ...any)
	clock   clockwork.Clock
	seams   burstSamplerSeams
	started atomic.Bool
	done    chan struct{}
}

// startBurstSamplerIfEnabled starts the burst sampler when the town's metrics
// sampler is on, and returns the func that stops and joins its loop.
//
// patrols.resource_sampler.enabled is the switch because the burst rows are
// read beside the resource rows as one town's metrics: a town that turned its
// sampler off would otherwise get a second log written behind it. The sample's
// cost is what makes the switch worth obeying: four process-table reads, a
// one-second top window, one daemon-log tail and one bd read per rig.
func (d *Daemon) startBurstSamplerIfEnabled() (stop func()) {
	if d.config == nil || !d.isPatrolActive("resource_sampler") {
		return func() {}
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return d.burstSampler().start(ctx)
}

// burstSampler returns the daemon's one burst sampler, building it on first
// start.
func (d *Daemon) burstSampler() *burstSampler {
	d.burstMu.Lock()
	defer d.burstMu.Unlock()
	if d.burst != nil {
		return d.burst
	}
	s := &burstSampler{
		town:  d.config.TownRoot,
		logf:  func(string, ...any) {},
		clock: d.clk(),
		seams: d.burstSeams,
		done:  make(chan struct{}),
	}
	if d.logger != nil {
		s.logf = d.logger.Printf
	}
	if s.seams.run == nil {
		s.seams.run = metrics.RunBurst
	}
	if s.seams.env == nil {
		s.seams.env = metrics.OSEnv
	}
	if s.seams.wait == nil {
		s.seams.wait = s.wait
	}
	d.burst = s
	return s
}

// start launches the sampler's loop and returns the func that stops it.
//
// The stop cancels the loop and joins it, so a daemon that returns from Run
// leaves no sample behind it. started stays set after a stop: the daemon
// starts the sampler once, in Run.
func (s *burstSampler) start(ctx context.Context) (stop func()) {
	if !s.started.CompareAndSwap(false, true) {
		return func() {}
	}
	runCtx, cancel := context.WithCancel(ctx)
	go s.run(runCtx)
	return func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(burstSamplerJoinTimeout):
			s.logf("burst_sampler: the loop has not ended %v after its stop", burstSamplerJoinTimeout)
		}
	}
}

// run samples, waits the row's cadence, and samples again until ctx ends.
func (s *burstSampler) run(ctx context.Context) {
	defer close(s.done)
	lastFailure := ""
	for {
		row, _, err := s.seams.run(ctx, s.seams.env(s.town))
		if err != nil {
			s.reportFailure(err, &lastFailure)
		} else {
			lastFailure = ""
		}
		s.seams.wait(ctx, burstRowWait(row))
		if ctx.Err() != nil {
			return
		}
	}
}

// reportFailure logs a failed sample once per distinct failure. A host the
// sampler cannot read retries every 15 s, and the same line every tick for
// hours buries the log's next real line; a good sample forgets the failure, so
// one that returns after a recovery is logged again.
func (s *burstSampler) reportFailure(err error, last *string) {
	if msg := err.Error(); msg != *last {
		*last = msg
		s.logf("burst_sampler: %v", err)
	}
}

// wait blocks until the next sample is due on the daemon's clock, or returns
// when ctx ends, so a shutdown does not sit out a 60 s cadence.
func (s *burstSampler) wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-s.clock.After(d):
	}
}

// burstRowWait is the gap before the next sample: the row's own IntervalS, and
// the fast cadence for a row that names none, which is what a failed sample
// leaves behind — a host the sampler could not read is a host to look at again
// soon, not a calm one.
func burstRowWait(row metrics.BurstRow) time.Duration {
	if row.IntervalS <= 0 {
		return metrics.BurstFastInterval
	}
	return time.Duration(row.IntervalS * float64(time.Second))
}
