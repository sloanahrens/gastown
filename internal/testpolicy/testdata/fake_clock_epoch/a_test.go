package fakeclock

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

var epoch = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

func TestClock(t *testing.T) {
	t.Parallel()
	_ = clockwork.NewFakeClock()        // starts at time.Now(): flagged
	_ = clockwork.NewFakeClockAt(epoch) // fixed epoch: fine
	_ = clockwork.NewRealClock()        // not a fake: fine
}
