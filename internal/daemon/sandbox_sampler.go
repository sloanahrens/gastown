package daemon

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// sandboxSeatSource is the one call a tick makes of the live seats: the seat
// source read over the sandbox registry (gt-a24bcc).
type sandboxSeatSource interface {
	Tick(ctx context.Context) (resourcesampler.SeatTick, error)
}

// sandboxRowWriter appends one row to the metrics directory, where the seat
// rows join the host rows in the day file the metrics snapshot reads.
// resourcesampler.AppendRow is the production writer (gt-6bmp5y).
type sandboxRowWriter func(metricsDir string, row resourcesampler.Row, now time.Time) error

// defaultSandboxSamplerTimeout bounds one tick at 15 seat reads' own bounds: the
// whole-tick backstop behind the source's per-seat DefaultSeatTimeout, since one
// tick pays one exec per seat (gt-psegop).
const defaultSandboxSamplerTimeout = 15 * resourcesampler.DefaultSeatTimeout

// sandboxSampler turns one tick of the seat source into rows in the town's
// metrics. The source, the writer, the clock and the logger are seams; the
// patrol supplies the live ones (gt-7d7jvn).
type sandboxSampler struct {
	metricsDir string
	source     sandboxSeatSource
	clock      resourcesampler.Clock
	write      sandboxRowWriter
	logf       func(format string, args ...any)
	// errLog takes the lines a nil logf would otherwise drop, writing them to
	// stderr in the standard logger's format, so a tick that could not read the
	// cluster is never invisible. It is the sampler's own logger so a test reads
	// those lines from a buffer instead of replacing the process's logger.
	errLog  *log.Logger
	timeout time.Duration
}

// newSandboxSampler returns the sampler a patrol ticks. A nil clock is the
// host's, a nil writer is resourcesampler.AppendRow, a nil logf writes to
// stderr in the standard logger's format, and a zero timeout is
// defaultSandboxSamplerTimeout.
func newSandboxSampler(metricsDir string, source sandboxSeatSource, clock resourcesampler.Clock, write sandboxRowWriter, logf func(format string, args ...any), timeout time.Duration) *sandboxSampler {
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
		metricsDir: metricsDir, source: source, clock: clock, write: write, logf: logf,
		errLog:  log.New(os.Stderr, "", log.LstdFlags),
		timeout: timeout,
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
