package daemon

import (
	"context"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hostprobe"
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
// timestamp, a bare clock time, a process id, and the age a refused probe file
// reports. Deliberately not every digit: an exit code or a count names which
// failure occurred, and masking it would fold two different failures into one
// line.
var (
	burstFailureStamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`)
	burstFailureClock = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	burstFailurePID   = regexp.MustCompile(`(?i)\bpid[=: ]?\s*\d+\b`)
	// The age grows with every sample a stale file sits unread: "4m0s old,
	// older than the 3m0s allowed".
	burstFailureAge = regexp.MustCompile(`\b\d[\dhms.ns]* old\b`)
)

// burstProbeMaxAge is how old the host agent's probe file may be and still
// describe the host now: three of the agent's sixty-second intervals, the age
// the attention side reads it at too, so one file answers both.
const burstProbeMaxAge = 3 * time.Minute

// burstProbeCPUPct is the unit of the probe file's consumer reading, in percent
// of one core, against the row's cores.
const burstProbeCPUPct = 100.0

// The row fields the probe file fills, keyed as the row names them, plus the
// key an error about the file itself is recorded under.
const (
	burstProbeIdleKey      = "idle_pct"
	burstProbeConsumersKey = "top_consumers"
	burstProbeTaxKey       = "exec_tax_ms"
	burstProbeFileKey      = "probes_file"
)

// containerModeFromEnv answers whether the process environment puts the daemon
// in the town's container: the rendered compose file sets
// config.ContainerModeEnv to config.SupervisorContainer beside it. The read is
// passed in rather than taken here so the daemon's New and a test ask the same
// question of their own environment.
func containerModeFromEnv(getenv func(string) string) bool {
	return getenv(config.ContainerModeEnv) == config.SupervisorContainer
}

// burstSamplerSeams replaces the sampler's sample, its wait, its Env and where
// its host readings come from in tests; the zero value is production.
type burstSamplerSeams struct {
	// run takes one sample and appends it; nil is metrics.RunBurst.
	run func(ctx context.Context, env metrics.Env) (metrics.BurstRow, string, error)
	// wait blocks before the next sample; nil waits on the daemon's clock.
	wait func(ctx context.Context, d time.Duration)
	// env builds a sample's Env; nil is metrics.OSEnv.
	env func(town string) metrics.Env
	// host takes a sample's host readings from the host agent's probe file
	// rather than this process's own commands. nil outside container mode,
	// where top and ps read the host the daemon runs on.
	host func(now time.Time) (metrics.BurstHostReading, error)
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
		// The town's own Env, plus the landing workers' published counts, so a
		// sample reads the ready-to-land column from memory while a worker's
		// count is fresh instead of listing every rig's board (gt-hw6ih).
		view := d.landingQueueView()
		s.seams.env = func(town string) metrics.Env {
			env := metrics.OSEnv(town)
			env.LandingQueue = view.Counts
			return env
		}
	}
	if s.seams.wait == nil {
		s.seams.wait = s.wait
	}
	// In container mode top and ps read this process's own process table, which
	// is the container's and not the Mac's, and the health file's exec tax is
	// measured in that same tree, so none of the three readings describes the
	// host: the host agent's probe file carries them.
	//
	// The answer is the daemon's (Daemon.containerMode), settled once in New
	// from the environment the container sets, and not read here: a test that
	// builds a daemon by hand is on the host whatever the process running the
	// tests says, so a gate run inside a container cannot move an existing
	// burst test onto the file.
	if s.seams.host == nil && d.containerMode {
		path := hostprobe.Path(s.town)
		s.seams.host = func(now time.Time) (metrics.BurstHostReading, error) {
			return burstHostFromProbeFile(path, now)
		}
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
	lastHostFailure := ""
	for {
		env, hostErr := s.sampleEnv()
		if hostErr != nil {
			s.reportFailure(hostErr, &lastHostFailure)
		} else {
			lastHostFailure = ""
		}
		row, _, err := s.seams.run(ctx, env)
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

// sampleEnv builds one sample's Env and, in container mode, the host reading it
// takes from the host agent's probe file.
//
// A file the sampler cannot use leaves the sample's host readings absent rather
// than failing the sample: the row is still written from the town's own
// readings, the three host fields are null with the reason in the row's
// host_errors, and the returned error is logged. The sampler keeps the row's
// cadence in that case and does not back off: the log, the queue and the
// session rows it still records are worth the fast sample a town whose agent
// stopped would otherwise thin out.
func (s *burstSampler) sampleEnv() (metrics.Env, error) {
	env := s.seams.env(s.town)
	if s.seams.host == nil {
		return env, nil
	}
	now := s.clock.Now()
	if env.Now != nil {
		now = env.Now()
	}
	reading, err := s.seams.host(now)
	if err != nil {
		reading = metrics.BurstHostReading{Errors: map[string]string{burstProbeFileKey: err.Error()}}
	}
	env.BurstHost = func(time.Time) metrics.BurstHostReading { return reading }
	return env, err
}

// burstHostFromProbeFile reads the host agent's probe file as of now.
// hostprobe.Read refuses a file that is missing, unreadable, from another
// schema, older than burstProbeMaxAge or stamped in the future, so a stale file
// is an error here and no reading it holds is carried into a row. A file Read
// accepts can still be up to burstProbeMaxAge old, so the reading names its age
// (burstProbeReading) and the row keeps it in host_age_s.
func burstHostFromProbeFile(path string, now time.Time) (metrics.BurstHostReading, error) {
	p, err := hostprobe.Read(path, now, burstProbeMaxAge)
	if err != nil {
		return metrics.BurstHostReading{}, err
	}
	return burstProbeReading(p, now), nil
}

// burstProbeReading turns one probe file into a row's host readings. A field's
// own Err makes that reading absent and its reason the row's host error, so one
// command the agent could not run costs the row that field and not the file's
// other readings: the shape a failed probe leaves when the sampler reads the
// host itself.
func burstProbeReading(p hostprobe.Probes, now time.Time) metrics.BurstHostReading {
	var r metrics.BurstHostReading
	// One stamp covers every reading the file holds, so the age is the file's
	// and not each field's: the idle share and the consumers are the host as
	// the agent saw it this long before the sample, and the row says so.
	age := now.Sub(p.At).Seconds()
	if age < 0 {
		// A stamp inside the drift Read tolerates: a current reading, not one
		// newer than fresh.
		age = 0
	}
	r.AgeS = &age
	errs := map[string]string{}
	if p.CPUIdlePct.Err != "" {
		errs[burstProbeIdleKey] = p.CPUIdlePct.Err
	} else {
		idle := p.CPUIdlePct.Value
		r.IdlePct = &idle
	}
	switch {
	case p.TopConsumers.Err != "":
		errs[burstProbeConsumersKey] = p.TopConsumers.Err
	case p.TopConsumers.Value == nil:
		// The agent writes a list for a sample it took, so a null list is a
		// file that says nothing about consumers rather than an idle host.
		errs[burstProbeConsumersKey] = "the probe file holds no consumer list"
	default:
		r.Consumers = make([]metrics.BurstConsumer, 0, len(p.TopConsumers.Value))
		for _, c := range p.TopConsumers.Value {
			r.Consumers = append(r.Consumers, metrics.BurstConsumer{Name: c.Name, Cores: c.CPU / burstProbeCPUPct})
		}
	}
	if p.ExecTaxMS.Err != "" {
		errs[burstProbeTaxKey] = p.ExecTaxMS.Err
	} else {
		// The tax was measured in the same pass the file is stamped for, so
		// its age is the file's. Read refuses a file older than
		// burstProbeMaxAge, so the tax is always current beside
		// burstExecTaxStaleAfter.
		tax := p.ExecTaxMS.Value
		r.ExecTaxMS = &tax
		taxAge := age
		r.ExecTaxAgeS = &taxAge
	}
	if len(errs) > 0 {
		r.Errors = errs
	}
	return r
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
// that names a pid, a timestamp or an age is one class rather than a new
// failure on every tick. An exit code or a count stays, because it says which
// failure occurred.
func burstFailureClass(err error) string {
	class := burstFailureStamp.ReplaceAllString(err.Error(), "<stamp>")
	class = burstFailureClock.ReplaceAllString(class, "<clock>")
	class = burstFailureAge.ReplaceAllString(class, "<age> old")
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
