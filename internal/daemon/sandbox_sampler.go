package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// sandboxSeatSource is the one call a tick makes of the live seats: the seat
// source read over the sandbox registry (gt-a24bcc).
type sandboxSeatSource interface {
	Tick(ctx context.Context) (resourcesampler.SeatTick, error)
}

// sandboxVMSource is the one call a tick makes of the VM's proc files: the VM
// source read through one live seat's exec seam (gt-2qe2m6).
type sandboxVMSource interface {
	Tick(ctx context.Context) (resourcesampler.VMTick, error)
}

// sandboxRowWriter appends one row to the metrics directory, where the seat
// rows join the host rows in the day file the metrics snapshot reads.
// resourcesampler.AppendRow is the production writer (gt-6bmp5y).
type sandboxRowWriter func(metricsDir string, row resourcesampler.Row, now time.Time) error

// defaultSandboxSamplerTimeout bounds one tick's seat read at 15 seat reads'
// own bounds: the backstop behind the source's per-seat DefaultSeatTimeout,
// since one tick pays one exec per seat (gt-psegop). The VM read is bounded
// separately, after the seat rows are written (gt-2qe2m6).
const defaultSandboxSamplerTimeout = 15 * resourcesampler.DefaultSeatTimeout

// vmSeatAttempts is how many live seats the VM read may try in one tick: each
// attempt is bounded by the source's per-seat DefaultSeatTimeout, so a seat that
// hangs for all of its bound still leaves budget for the next seat (gt-2qe2m6).
const vmSeatAttempts = 3

// defaultVMReadTimeout bounds one tick's whole VM read at vmSeatAttempts per-seat
// bounds, and is a bound of its own so a VM read that hangs costs the VM row only
// (gt-2qe2m6). It is longer than one seat's bound on purpose: with equal bounds a
// hung first seat would use up the whole budget and the next-seat fallback would
// never run.
const defaultVMReadTimeout = vmSeatAttempts * resourcesampler.DefaultSeatTimeout

// defaultSandboxSamplerInterval is the period between two seat samples: the
// seat rows are a 60 s series beside the host's, so the patrol runs a ticker
// of its own rather than the heartbeat (gt-7d7jvn).
const defaultSandboxSamplerInterval = 60 * time.Second

// minSandboxSamplerInterval bounds a configured interval up: one tick pays one
// exec per live seat, so a shorter one only loads the gateway.
const minSandboxSamplerInterval = 5 * time.Second

// sandboxSamplerInterval is the period between two samples: the configured
// patrols.sandbox_sampler.interval, clamped up to minSandboxSamplerInterval,
// or the default when the key is absent or does not parse.
func sandboxSamplerInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.SandboxSampler != nil {
		if d, err := time.ParseDuration(config.Patrols.SandboxSampler.Interval); err == nil && d > 0 {
			return max(d, minSandboxSamplerInterval)
		}
	}
	return defaultSandboxSamplerInterval
}

// newSandboxSeatSourceFn builds the seat source the patrol ticks. Nil means the
// live one: the town's sandbox records read through the gateway's openshell
// CLI, at the daemon's clock and the source's own per-seat bound.
type newSandboxSeatSourceFn func(townRoot string, clock resourcesampler.Clock, timeout time.Duration) sandboxSeatSource

// gatewaySeatSource is the live seat source: *sandbox.CLI is the exec seam
// resourcesampler.SeatExec names, and the empty Gateway reads openshell's
// active one.
func gatewaySeatSource(townRoot string, clock resourcesampler.Clock, timeout time.Duration) sandboxSeatSource {
	return resourcesampler.NewSeatSource(townRoot, &sandbox.CLI{}, clock, timeout)
}

// gatewayVMSource is the live VM source, read through the same exec seam and
// the same live seats the seat source reads.
func gatewayVMSource(townRoot string, clock resourcesampler.Clock, timeout time.Duration) sandboxVMSource {
	return resourcesampler.NewVMSource(townRoot, &sandbox.CLI{}, clock, timeout)
}

// sandboxSampler turns one tick of the seat and VM sources into rows in the
// town's metrics. Both sources, the writer, the clock and the logger are seams;
// the patrol supplies the live ones (gt-7d7jvn, gt-2qe2m6).
type sandboxSampler struct {
	metricsDir string
	source     sandboxSeatSource
	vm         sandboxVMSource
	clock      resourcesampler.Clock
	write      sandboxRowWriter
	logf       func(format string, args ...any)
	// errLog takes the lines a nil logf would otherwise drop, writing them to
	// stderr in the standard logger's format, so a tick that could not read the
	// cluster is never invisible. It is the sampler's own logger so a test reads
	// those lines from a buffer instead of replacing the process's logger.
	errLog  *log.Logger
	timeout time.Duration
	// vmTimeout bounds the VM read, which starts only after the seat rows are
	// written, so the two bounds cannot swallow each other.
	vmTimeout time.Duration
}

// newSandboxSampler returns the sampler a patrol ticks. A nil clock is the
// host's, a nil writer is resourcesampler.AppendRow, a nil logf writes to
// stderr in the standard logger's format, and a zero timeout is
// defaultSandboxSamplerTimeout. The VM read's bound is defaultVMReadTimeout.
func newSandboxSampler(metricsDir string, source sandboxSeatSource, vm sandboxVMSource, clock resourcesampler.Clock, write sandboxRowWriter, logf func(format string, args ...any), timeout time.Duration) *sandboxSampler {
	if clock == nil {
		clock = resourcesampler.SystemClock{}
	}
	if write == nil {
		write = resourcesampler.AppendRow
	}
	if timeout <= 0 {
		timeout = defaultSandboxSamplerTimeout
	}
	return &sandboxSampler{
		metricsDir: metricsDir, source: source, vm: vm, clock: clock, write: write, logf: logf,
		errLog:    log.New(os.Stderr, "", log.LstdFlags),
		timeout:   timeout,
		vmTimeout: defaultVMReadTimeout,
	}
}

// Tick reads the live seats once and appends the rows that read to the metrics
// day file. A read that fails, or that ends before the source returns because
// the caller's context or the tick's own bound ended it, writes nothing and
// logs one line, so a tick that could not read the cluster cannot be read as a
// cluster with no seat. A seat the source skipped logs its reason while the
// other seats still write, and a row the writer refuses logs once and ends the
// tick: the rest of the rows share whatever refused that one.
//
// The bound ends the read through the context the source honors, not by
// outlasting the source: the seat source kills each seat's exec with it
// (gt-psegop).
//
// The VM row is read after the seat rows are written and under a bound of its
// own: a VM read that fails or ends at its bound logs one line and writes no VM
// row, and the seat rows are already the tick's (gt-2qe2m6).
func (s *sandboxSampler) Tick(ctx context.Context) {
	if s.source == nil {
		s.log("sandbox_sampler: no seat source")
		return
	}
	// The time is taken before the read so the rows of one tick land in the day
	// file of the day the tick began, even when a slow read crosses midnight.
	now := s.clock.Now()
	tickCtx, cancel := context.WithTimeout(ctx, s.timeout)
	tick, err := s.source.Tick(tickCtx)
	// Read before cancel: canceling the context marks it canceled, which would
	// read as a tick the bound ended.
	ended := tickCtx.Err()
	cancel()
	switch {
	case ctx.Err() != nil:
		// The caller's own deadline or cancel ended the read, so naming the
		// sampler's bound would charge it for a bound it did not apply.
		s.log("sandbox_sampler: the caller ended the read: %v", ctx.Err())
		return
	case ended != nil:
		s.log("sandbox_sampler: the read ends at its %s bound: %v", s.timeout, ended)
		return
	case err != nil:
		s.log("sandbox_sampler: %v", err)
		return
	}
	for _, skip := range tick.Skipped {
		s.log("sandbox_sampler: seat %s skipped: %v", skip.Seat, skip.Err)
	}
	for _, row := range tick.Rows {
		if err := s.write(s.metricsDir, row, now); err != nil {
			s.log("sandbox_sampler: %v", err)
			return
		}
	}
	s.tickVM(ctx, now)
}

// tickVM reads the VM's proc files once and appends its row, after the seat
// rows are written: a VM read that hangs loses only the VM row, never the seat
// rows the tick already wrote. A read that fails or ends at its bound logs one
// line and writes no row; a seat the source skipped logs its reason, and a tick
// with no live seat logs nothing and writes no row. A VM that could not be read
// is never a VM of zeros (gt-2qe2m6).
func (s *sandboxSampler) tickVM(ctx context.Context, now time.Time) {
	if s.vm == nil {
		return
	}
	vmCtx, cancel := context.WithTimeout(ctx, s.vmTimeout)
	vm, err := s.vm.Tick(vmCtx)
	// Read before cancel, as the seat read does, so canceling does not read as
	// the bound ending the read.
	ended := vmCtx.Err()
	cancel()
	switch {
	case ctx.Err() != nil:
		s.log("sandbox_sampler: the caller ended the vm read: %v", ctx.Err())
		return
	case ended != nil:
		s.log("sandbox_sampler: the vm read ends at its %s bound: %v", s.vmTimeout, ended)
		return
	case err != nil:
		s.log("sandbox_sampler: vm read: %v", err)
		return
	}
	for _, skip := range vm.Skipped {
		s.log("sandbox_sampler: vm read: seat %s skipped: %v", skip.Seat, skip.Err)
	}
	if vm.Row == nil {
		return
	}
	if err := s.write(s.metricsDir, *vm.Row, now); err != nil {
		s.log("sandbox_sampler: vm read: %v", err)
	}
}

// log writes one line through the sampler's logger, or through errLog when the
// patrol wired none: a line a nil logger dropped would make a failed tick look
// like a cluster with no seat.
func (s *sandboxSampler) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
		return
	}
	s.errLog.Printf(format, args...)
}

// triggerSandboxSampler runs one seat sample. The patrol's own ticker calls it
// (daemon.go): the heartbeat is minutes apart, and the seat rows are a 60 s
// series (gt-7d7jvn).
func (d *Daemon) triggerSandboxSampler() {
	if d.config == nil || !d.isPatrolActive("sandbox_sampler") {
		return
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	d.sandboxSampler().Tick(ctx)
}

// sandboxSampler returns the patrol's sampler, building it on first use. It
// writes into the host rows' metrics directory, so the two series share a day
// file, and the sampler's own bound ends a tick the gateway hangs on.
func (d *Daemon) sandboxSampler() *sandboxSampler {
	d.sandboxSamplerMu.Lock()
	defer d.sandboxSamplerMu.Unlock()
	if d.sandboxSamplerPatrol == nil {
		build := d.newSandboxSeatSource
		if build == nil {
			build = gatewaySeatSource
		}
		clock := d.clk()
		d.sandboxSamplerPatrol = newSandboxSampler(
			filepath.Join(d.config.TownRoot, ".runtime", "metrics"),
			build(d.config.TownRoot, clock, resourcesampler.DefaultSeatTimeout),
			gatewayVMSource(d.config.TownRoot, clock, resourcesampler.DefaultSeatTimeout),
			clock, nil, d.logger.Printf, 0,
		)
	}
	return d.sandboxSamplerPatrol
}
