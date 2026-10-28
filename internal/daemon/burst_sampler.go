package daemon

import (
	"context"
	"regexp"
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

// burstFailureCeiling is the longest gap between two samples while every
// sample fails: seven consecutive failures take the fast cadence to it, and
// the sampler then retries every ten minutes. The number is a choice, not a
// derivation. A host that cannot be read yields no row at any cadence, so the
// ceiling bounds what the retries cost rather than what the log holds. What it
// costs is the delay before a recovered host is sampled again, and ten minutes
// is well inside the hourly snapshot's hour.
const burstFailureCeiling = 10 * time.Minute

// The parts of an error's text that differ between two occurrences of the same
// failure, masked so the log's dedup sees one class (burstFailureClass): a
// timestamp, a bare clock time, and a process id. Deliberately not every
// digit: an exit code or a count names which failure occurred, and masking it
// would fold two different failures into one line.
var (
	burstFailureStamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`)
	burstFailureClock = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	burstFailurePID   = regexp.MustCompile(`(?i)\bpid[=: ]?\s*\d+\b`)
)

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

// run samples, waits the next sample's gap, and samples again until ctx ends.
// A run of consecutive failed samples backs the gap off (burstFailureWait);
// one good sample ends the run and returns the gap to the row's own cadence.
func (s *burstSampler) run(ctx context.Context) {
	defer close(s.done)
	failures := 0
	lastFailure := ""
	for {
		row, _, err := s.seams.run(ctx, s.seams.env(s.town))
		var next time.Duration
		if err != nil {
			failures++
			s.reportFailure(err, &lastFailure)
			next = burstFailureWait(failures)
		} else {
			failures = 0
			lastFailure = ""
			next = burstRowWait(row)
		}
		s.seams.wait(ctx, next)
		if ctx.Err() != nil {
			return
		}
	}
}

// burstFailureWait is the gap after the consecutive-th failed sample in a run:
// the fast cadence doubled once per further failure, held at
// burstFailureCeiling. A failed sample leaves no row to take a cadence from,
// and a host the sampler could not read is worth a fast look the first time;
// but a host that fails every time pays the sample's process-table and bd
// reads for nothing, so the retries thin out until one sample succeeds.
func burstFailureWait(consecutive int) time.Duration {
	wait := metrics.BurstFastInterval
	for i := 1; i < consecutive; i++ {
		if wait >= burstFailureCeiling {
			break
		}
		wait *= 2
	}
	if wait > burstFailureCeiling {
		return burstFailureCeiling
	}
	return wait
}

// burstFailureClass is the identity a failed sample logs under: the error's
// text with the parts that vary between two occurrences masked, so a message
// that names a pid or a timestamp is one class rather than a new failure on
// every tick. An exit code or a count stays, because it says which failure
// occurred.
func burstFailureClass(err error) string {
	class := burstFailureStamp.ReplaceAllString(err.Error(), "<stamp>")
	class = burstFailureClock.ReplaceAllString(class, "<clock>")
	return burstFailurePID.ReplaceAllString(class, "<pid>")
}

// reportFailure logs a failed sample once per class of failure (see
// burstFailureClass). A host the sampler cannot read retries on a timer, and
// the same failure every tick for hours buries the log's next real line; a
// good sample forgets the class, so a failure that returns after a recovery is
// logged again.
func (s *burstSampler) reportFailure(err error, last *string) {
	if class := burstFailureClass(err); class != *last {
		*last = class
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
