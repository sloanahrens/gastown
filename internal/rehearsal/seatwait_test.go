package rehearsal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRehearsalAwaitSeat(t *testing.T) {
	t.Parallel()
	boom := errors.New("the pool read failed")
	reads := 0
	rising := func(context.Context) (int, error) { reads++; return reads + 1, nil }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	later, cancelLater := context.WithCancel(context.Background())
	cancels := func(context.Context) (int, error) { cancelLater(); return 1, nil }
	late := make(chan time.Time, 1)
	fires := func(context.Context) (int, error) { late <- time.Time{}; return 1, nil }
	three := func(context.Context) (int, error) { return 3, nil }
	tests := []struct {
		name           string
		ctx            context.Context
		free           func(context.Context) (int, error)
		running        int
		tick, deadline chan time.Time
		want           error
	}{
		{"free seats return at once", context.Background(), three, 2, waitTicks(0), waitDeadline(false), nil},
		{"a seat comes free after a tick", context.Background(), rising, 2, waitTicks(1), waitDeadline(false), nil},
		{"a full pool is not a free seat", context.Background(), three, 3, waitTicks(0), waitDeadline(false), errTickClosed},
		{"a failed read is wrapped and not retried", context.Background(), func(context.Context) (int, error) { return 0, boom }, 2, waitTicks(1), waitDeadline(false), boom},
		{"a canceled ctx wins before the read", canceled, three, 2, waitTicks(0), waitDeadline(false), context.Canceled},
		{"a canceled ctx ends the wait", later, cancels, 2, make(chan time.Time), waitDeadline(false), context.Canceled},
		{"a fired deadline wins before the read", context.Background(), three, 2, waitTicks(0), waitDeadline(true), errSeatDeadline},
		{"a fired deadline ends the wait", context.Background(), fires, 2, make(chan time.Time), late, errSeatDeadline},
		{"a closed tick returns its error", context.Background(), func(context.Context) (int, error) { return 1, nil }, 2, waitTicks(0), waitDeadline(false), errTickClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := awaitSeat(tt.ctx, tt.free, func() int { return tt.running }, tt.tick, tt.deadline)
			if !errors.Is(got, tt.want) || (tt.want == boom && got == boom) {
				t.Errorf("awaitSeat() = %v; want %v", got, tt.want)
			}
		})
	}
}
