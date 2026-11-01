package hostload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
)

// Recorder appends one JSON line per sample until its context ends, so a
// slowdown can be attributed after the fact instead of argued about from a
// snapshot (gt-nu89c).
type Recorder struct {
	// Interval is how long the recorder waits between samples.
	Interval time.Duration
	// Sample takes one reading.
	Sample func(ctx context.Context) Sample
	// Append writes one line; production appends it to the --out file.
	Append func(line []byte) error
	// Clock schedules the intervals; nil means the real clock.
	Clock clockwork.Clock
}

// Run samples at once, then once per interval, until ctx is done. A canceled
// context is a clean stop and returns nil. A failed write ends the loop with
// its error: a record file that is not being written is the one thing a
// sampler exists to produce, so the loop does not carry on silently.
func (r *Recorder) Run(ctx context.Context) error {
	switch {
	case r.Interval <= 0:
		return fmt.Errorf("hostload: sample interval %s must be positive", r.Interval)
	case r.Sample == nil:
		return errors.New("hostload: recorder has no sample reader")
	case r.Append == nil:
		return errors.New("hostload: recorder has no line writer")
	}
	clk := r.Clock
	if clk == nil {
		clk = clockwork.NewRealClock()
	}
	for {
		s := r.Sample(ctx)
		if ctx.Err() != nil {
			return nil
		}
		line, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("hostload: encoding a sample: %w", err)
		}
		if err := r.Append(append(line, '\n')); err != nil {
			return fmt.Errorf("hostload: writing a sample: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-clk.After(r.Interval):
		}
	}
}
