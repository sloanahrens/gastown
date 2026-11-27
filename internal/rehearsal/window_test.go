package rehearsal

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// waitOpen and waitDone report every id as still open or as done.
func waitOpen(context.Context, string) (bool, error) { return false, nil }
func waitDone(context.Context, string) (bool, error) { return true, nil }

// waitTicks returns a tick channel with n ticks queued and then closed, so a Wait
// that reads past its window ends in the closed-channel error.
func waitTicks(n int) chan time.Time {
	ch := make(chan time.Time, n)
	for i := 0; i < n; i++ {
		ch <- time.Time{}
	}
	close(ch)
	return ch
}

// waitDeadline returns a deadline channel that has fired when fired is true.
func waitDeadline(fired bool) chan time.Time {
	ch := make(chan time.Time, 1)
	if fired {
		ch <- time.Time{}
		close(ch)
	}
	return ch
}

func TestRehearsalWait(t *testing.T) {
	t.Parallel()
	first, last := errors.New("no dolt"), errors.New("still no dolt")
	calls := 0
	lastRead := func(context.Context, string) (bool, error) { // fails, fails, then last
		calls++
		if calls < 3 {
			return false, first
		}
		return false, last
	}
	reads := 0
	resetRead := func(context.Context, string) (bool, error) { // fail fail ok fail fail done
		reads++
		switch reads {
		case 3:
			return false, nil
		case 6:
			return true, nil
		}
		return false, first
	}
	doneB := 0
	partialRead := func(_ context.Context, id string) (bool, error) { // "a" fails, "b" done 2nd tick
		if id == "a" {
			return false, first
		}
		doneB++
		return doneB > 1, nil
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name     string
		ctx      context.Context
		ids      []string
		state    func(context.Context, string) (bool, error)
		tick     chan time.Time
		deadline chan time.Time
		wantOpen []string
		wantErr  error
	}{
		{"all done returns no open id", context.Background(), []string{"a", "b"}, waitDone, waitTicks(1), waitDeadline(false), nil, nil},
		{"deadline returns the open ids", context.Background(), []string{"a", "b"}, waitOpen, waitTicks(0), waitDeadline(true), []string{"a", "b"}, nil},
		{"cancel returns ctx's error with a tick ready", canceled, []string{"a"}, waitOpen, waitTicks(1), waitDeadline(false), []string{"a"}, context.Canceled},
		{"three failed ticks return the last error", context.Background(), []string{"a"}, lastRead, waitTicks(3), waitDeadline(false), []string{"a"}, last},
		{"a readable tick resets the failed count", context.Background(), []string{"a"}, resetRead, waitTicks(6), waitDeadline(false), nil, nil},
		{"a failed read keeps its id open", context.Background(), []string{"a", "b"}, partialRead, waitTicks(2), waitDeadline(false), []string{"a"}, errTickClosed},
		{"a closed tick channel returns its error", context.Background(), []string{"a"}, func(context.Context, string) (bool, error) { return false, first }, waitTicks(0), waitDeadline(false), []string{"a"}, errTickClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotOpen, gotErr := Wait(tt.ctx, tt.ids, tt.state, tt.tick, tt.deadline)
			if !slices.Equal(gotOpen, tt.wantOpen) || gotErr != tt.wantErr {
				t.Errorf("Wait(%v) = %v, %v; want %v, %v", tt.ids, gotOpen, gotErr, tt.wantOpen, tt.wantErr)
			}
		})
	}
}
