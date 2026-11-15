package daemon

import (
	"context"
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

// sandboxSampler turns one tick of the seat source into rows in the town's
// metrics. The source, the writer, the clock and the logger are seams; the
// patrol supplies the live ones (gt-7d7jvn).
type sandboxSampler struct {
	metricsDir string
	source     sandboxSeatSource
	clock      resourcesampler.Clock
	write      sandboxRowWriter
	logf       func(format string, args ...any)
}

// newSandboxSampler returns the sampler a patrol ticks. A nil clock is the
// host's, a nil writer is resourcesampler.AppendRow, and a nil logf logs
// nothing.
func newSandboxSampler(metricsDir string, source sandboxSeatSource, clock resourcesampler.Clock, write sandboxRowWriter, logf func(format string, args ...any)) *sandboxSampler {
	if clock == nil {
		clock = resourcesampler.SystemClock{}
	}
	if write == nil {
		write = resourcesampler.AppendRow
	}
	return &sandboxSampler{metricsDir: metricsDir, source: source, clock: clock, write: write, logf: logf}
}

// Tick reads the live seats once and appends the rows that read to the metrics
// day file. A source that fails writes nothing and logs one line, so a tick
// that could not read the cluster cannot be read as a cluster with no seat. A
// seat the source skipped logs its reason while the other seats still write,
// and a row the writer refuses logs once and ends the tick: the rest of the
// rows share whatever refused that one.
func (s *sandboxSampler) Tick(ctx context.Context) {
	if s.source == nil {
		s.log("sandbox_sampler: no seat source")
		return
	}
	// The time is taken before the read so the rows of one tick land in the day
	// file of the day the tick began, even when a slow read crosses midnight.
	now := s.clock.Now()
	tick, err := s.source.Tick(ctx)
	if err != nil {
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

// log writes one line through the sampler's logger; a nil logger logs nothing.
func (s *sandboxSampler) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}
