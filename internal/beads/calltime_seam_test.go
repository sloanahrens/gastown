package beads

import (
	"context"
	"testing"
	"time"
)

// fakeCallClock is the clock the fake bd runner advances by the time a call
// spends, so runBdOnce measures that time between its reads rather than the
// number of reads.
type fakeCallClock struct{ now time.Time }

func (c *fakeCallClock) read() time.Time { return c.now }

func (c *fakeCallClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// TestCallTimeSeam: each runBdOnce records the time its bd subprocess spends
// in the call, so the totals follow the subprocess and the --flat retry rather
// than the clock reads (gt-aibxst, gt-csgn6r, gt-1nk6ij).
//
//testpolicy:allow parallel — it swaps the package-level callNow clock, so no test may run beside it
func TestCallTimeSeam(t *testing.T) {
	tests := []struct {
		name      string
		args      []string        // the call runBdOnce is asked to run
		durations []time.Duration // time each subprocess spends, in order
		answer    func(args []string) reply
		wantErr   bool
		wantCalls int
		wantTotal time.Duration
		wantSlow  time.Duration
	}{
		{
			name:      "successful call of 300ms",
			args:      []string{"list", "--json"},
			durations: []time.Duration{300 * time.Millisecond},
			wantCalls: 1,
			wantTotal: 300 * time.Millisecond,
			wantSlow:  300 * time.Millisecond,
		},
		{
			name:      "failed call of 200ms",
			args:      []string{"list", "--json"},
			durations: []time.Duration{200 * time.Millisecond},
			answer: func([]string) reply {
				return reply{stderr: "Error: database is locked\n", err: exitError{code: 1}}
			},
			wantErr:   true,
			wantCalls: 1,
			wantTotal: 200 * time.Millisecond,
			wantSlow:  200 * time.Millisecond,
		},
		{
			name:      "flat rejection then a 100ms retry",
			args:      []string{"list", "--json", "--flat"},
			durations: []time.Duration{200 * time.Millisecond, 100 * time.Millisecond},
			answer: func(args []string) reply {
				if hasArg(args, "--flat") {
					return reply{stderr: "Error: unknown flag: --flat\n", err: exitError{code: 1}}
				}
				return reply{stdout: "[]"}
			},
			wantCalls: 2,
			wantTotal: 300 * time.Millisecond,
			wantSlow:  200 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCallTime(t)
			clock := &fakeCallClock{now: time.Unix(0, 0)}
			prior := callNow
			//testpolicy:allow no-global-swap — restores the clock seam swapped below
			t.Cleanup(func() { callNow = prior })
			//testpolicy:allow no-global-swap — the fake clock runBdOnce reads is the swap this test makes
			callNow = clock.read
			attempt := 0
			r := newRecorder(func(args []string) reply {
				if attempt >= len(tt.durations) {
					t.Fatalf("bd call %d: want at most %d subprocesses, got another for %v", attempt+1, len(tt.durations), args)
				}
				clock.advance(tt.durations[attempt])
				attempt++
				if tt.answer == nil {
					return reply{stdout: "[]"}
				}
				return tt.answer(args)
			})
			b := newRecordedBeads(t.TempDir(), r)
			_, err := b.runBdOnce(context.Background(), nil, nil, tt.args)
			if tt.wantErr && err == nil {
				t.Fatalf("runBdOnce: got nil error, want the runner's failure")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("runBdOnce: %v", err)
			}
			if calls, total, slowest := CallTimeSnapshotAndReset(); calls != tt.wantCalls || total != tt.wantTotal || slowest != tt.wantSlow {
				t.Fatalf("snapshot = %d calls, %v total, %v slowest; want %d, %v, %v", calls, total, slowest, tt.wantCalls, tt.wantTotal, tt.wantSlow)
			}
		})
	}
}
