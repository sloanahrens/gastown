package daemon

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
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
// timestamp, a bare clock time and a process id. Deliberately not every digit:
// an exit code or a count names which failure occurred, and masking it would
// fold two different failures into one line.
var (
	burstFailureStamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`)
	burstFailureClock = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	burstFailurePID   = regexp.MustCompile(`(?i)\bpid[=: ]?\s*\d+\b`)
)

// burstProcStatPath is the kernel's aggregate CPU counter. In the container it is
// the counter of the machine the daemon runs on, the control VM's kernel, which
// the container shares, so the idle share is the machine's and not the pod's
// quota's.
const burstProcStatPath = "/proc/stat"

// burstIdleWindowMax is the longest window a share is measured over: two slow
// cadences. A sampler at its own pace always measures inside it. A window that
// spans a failure streak does not, since no good read of the machine was taken in
// between, so its share would average the outage and not describe now.
const burstIdleWindowMax = 2 * metrics.BurstSlowInterval

// The row fields the pod's readings fill or leave absent, keyed as the row names
// them. A field the pod cannot read is absent with its reason under its key.
const (
	burstIdleKey      = "idle_pct"
	burstConsumersKey = "top_consumers"
	burstTaxKey       = "exec_tax_ms"
)

// Why the pod leaves the two host readings it cannot take absent: both are Mac
// process facts that the container cannot see.
const (
	burstConsumersPodReason = "the host's processes are not visible from the pod: a Mac process fact the container cannot see"
	burstTaxPodReason       = "the host's exec tax is not visible from the pod: a Mac process fact the container cannot see"
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
	// host takes a sample's host readings from the machine's CPU counter rather
	// than this process's own commands, reading the counter through the sample's
	// readFile (its Env's ReadFile). nil outside container mode, where top and ps
	// read the host the daemon runs on. A reading returned with an error names
	// each field it leaves absent in its Errors.
	host func(now time.Time, readFile func(string) ([]byte, error)) (metrics.BurstHostReading, error)
}

// burstSampler paces metrics.RunBurst for one town. started keeps the loop
// single: two samplers would write two rows per cadence and pay the sample's
// price twice.
type burstSampler struct {
	town string
	logf func(format string, args ...any)
	// board keeps the ready-to-land board counts the samples had to list, so a
	// rig whose landing worker publishes no count is read once a minute rather
	// than once a sample (metrics.BurstBoardCache).
	board   *metrics.BurstBoardCache
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
// one-second top window, one daemon-log tail and one bd read per rig whose
// landing-queue count is neither published nor kept from a read this minute.
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
	// The sampler owns the cache, so its samples reuse each other's landing
	// board reads and one line a minute names the rigs they listed.
	s.board = metrics.NewBurstBoardCache(s.logf)
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
	// measured in that same tree, so none of those readings describes the host.
	// The machine's aggregate CPU counter is the one host reading the container
	// can take, so the sampler's idle share comes from there.
	//
	// The answer is the daemon's (Daemon.containerMode), settled once in New
	// from the environment the container sets, and not read here: a test that
	// builds a daemon by hand is on the host whatever the process running the
	// tests says, so a gate run inside a container cannot move an existing
	// burst test onto the counter.
	if s.seams.host == nil && d.containerMode {
		s.seams.host = newBurstProcStatCPU(burstProcStatPath).read
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

// sampleEnv builds one sample's Env and, in container mode, the host readings
// for it, read through the Env's ReadFile.
//
// A reading the sampler cannot take leaves its field absent rather than failing
// the sample: the row is still written from the town's own readings, the field is
// null with the reason in the row's host_errors, and the returned error is
// logged. With no host reading to set its cadence, the row takes the fast one
// (metrics.BurstInterval).
func (s *burstSampler) sampleEnv() (metrics.Env, error) {
	env := s.seams.env(s.town)
	env.BurstBoard = s.board
	if s.seams.host == nil {
		return env, nil
	}
	now := s.clock.Now()
	if env.Now != nil {
		now = env.Now()
	}
	reading, err := s.seams.host(now, env.ReadFile)
	env.BurstHost = func(time.Time) metrics.BurstHostReading { return reading }
	return env, err
}

// burstProcStatCPU reads the host's idle share from the aggregate cpu line of
// /proc/stat. The counters only grow, so the share is the idle time between two
// reads over the total time between them.
type burstProcStatCPU struct {
	// path is the counter's file, read through the sample's readFile seam.
	path string
	// last is the previous good read, the baseline the next share is measured
	// from, and have says whether there is one.
	last burstProcStatRead
	have bool
}

// burstProcStatRead is one read of the counters and the time it was taken at.
type burstProcStatRead struct {
	idle, total uint64
	at          time.Time
}

// newBurstProcStatCPU reads the counter at path. The path is passed in so a test
// reads its own.
func newBurstProcStatCPU(path string) *burstProcStatCPU {
	return &burstProcStatCPU{path: path}
}

// read takes the host's readings as of now, reading the counter through readFile.
// The idle share is absent until a second read gives a window to measure, and
// whenever the window cannot give one: the counter went backwards or did not
// move, the clock did not advance, or the last good read is older than
// burstIdleWindowMax. The consumers and the exec tax are always absent, since the
// container cannot see the host's processes. A read that fails returns the error,
// and the reading names the idle share's failure too, so the sampler logs it and
// the row says why the share is missing.
func (c *burstProcStatCPU) read(now time.Time, readFile func(string) ([]byte, error)) (metrics.BurstHostReading, error) {
	r := metrics.BurstHostReading{Errors: map[string]string{
		burstConsumersKey: burstConsumersPodReason,
		burstTaxKey:       burstTaxPodReason,
	}}
	if readFile == nil {
		err := fmt.Errorf("%s: the sample's Env has no file reader", c.path)
		r.Errors[burstIdleKey] = err.Error()
		return r, err
	}
	idle, total, err := readBurstProcStat(c.path, readFile)
	if err != nil {
		r.Errors[burstIdleKey] = err.Error()
		return r, err
	}
	prev, had := c.last, c.have
	c.last, c.have = burstProcStatRead{idle: idle, total: total, at: now}, true
	switch {
	case !had:
		r.Errors[burstIdleKey] = "the first sample has no earlier reading of the host's CPU to measure against"
	case idle < prev.idle || total < prev.total:
		r.Errors[burstIdleKey] = "the host's CPU counter went backwards since the last sample: /proc/stat was reset"
	case total == prev.total:
		r.Errors[burstIdleKey] = "the host's CPU counter did not move since the last sample: no time to measure a share over"
	case !now.After(prev.at):
		r.Errors[burstIdleKey] = "the clock did not advance since the host's last CPU reading"
	case now.Sub(prev.at) > burstIdleWindowMax:
		r.Errors[burstIdleKey] = fmt.Sprintf("the host's last CPU reading was %v before this one: too old to measure a share over",
			now.Sub(prev.at).Round(time.Second))
	default:
		share := 100 * float64(idle-prev.idle) / float64(total-prev.total)
		r.IdlePct = &share
	}
	return r, nil
}

// readBurstProcStat returns the aggregate cpu line's idle counters, idle and
// iowait together, and the total of its time columns, both in clock ticks. The
// line lays out user nice system idle iowait irq softirq steal guest guest_nice.
// The guest columns are already counted in user and nice, so the total stops at
// steal, and a per-CPU line (cpu0, cpu1) is not the machine's counter.
func readBurstProcStat(path string, readFile func(string) ([]byte, error)) (idle, total uint64, err error) {
	data, err := readFile(path)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		if len(fields) < 6 {
			return 0, 0, fmt.Errorf("%s: the aggregate cpu line has no idle and iowait columns", path)
		}
		columns := fields[1:]
		if len(columns) > 8 {
			columns = columns[:8]
		}
		counts := make([]uint64, len(columns))
		for i, column := range columns {
			count, err := strconv.ParseUint(column, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("%s: the aggregate cpu line has a column that is not a count of ticks", path)
			}
			counts[i] = count
			total += count
		}
		return counts[3] + counts[4], total, nil
	}
	return 0, 0, fmt.Errorf("%s: no aggregate cpu line", path)
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
