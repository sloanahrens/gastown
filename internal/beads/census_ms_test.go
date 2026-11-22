package beads

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// censusCall is one bd call a test starts: its argv, the wall time the fake
// clock advances while it runs, and what the fake bd answers.
type censusCall struct {
	args  []string
	spend time.Duration
	reply reply
}

// TestTickEffCensusMs: each started bd subprocess adds the wall time it spent
// to its verb's row, read from the same injected clock as the call-time seam,
// and a call the OS never launched adds none (gt-y9wvm0).
//
//testpolicy:allow parallel — it swaps the package-level callNow clock, so no test may run beside it
func TestTickEffCensusMs(t *testing.T) {
	tests := []struct {
		name  string
		calls []censusCall
		// want is the one row the calls fold into: verb, count and summed
		// milliseconds.
		want CensusRow
	}{
		{
			name: "two calls of one verb and caller sum their milliseconds",
			calls: []censusCall{
				{args: []string{"list", "--json"}, spend: 100 * time.Millisecond, reply: reply{stdout: "[]"}},
				{args: []string{"list", "--json"}, spend: 250 * time.Millisecond, reply: reply{stdout: "[]"}},
			},
			want: CensusRow{Verb: "list", Count: 2, Ms: 350},
		},
		{
			name: "a subprocess that never started records no milliseconds",
			calls: []censusCall{
				{args: []string{"show", "gt-1"}, spend: 7 * time.Millisecond, reply: reply{err: &exec.Error{Name: "bd", Err: exec.ErrNotFound}}},
			},
			want: CensusRow{Verb: "show", Count: 1, Ms: 0},
		},
		{
			name: "a binary the OS could not fork records no milliseconds",
			calls: []censusCall{
				{args: []string{"show", "gt-1"}, spend: 7 * time.Millisecond, reply: reply{err: &os.PathError{Op: "fork/exec", Path: "bd", Err: errors.New("permission denied")}}},
			},
			want: CensusRow{Verb: "show", Count: 1, Ms: 0},
		},
		{
			name: "a subprocess that started and failed records its milliseconds",
			calls: []censusCall{
				{args: []string{"show", "gt-1"}, spend: 40 * time.Millisecond, reply: reply{stderr: "Error: database is locked\n", err: exitError{code: 1}}},
			},
			want: CensusRow{Verb: "show", Count: 1, Ms: 40},
		},
		{
			name: "a call under a millisecond records no whole milliseconds",
			calls: []censusCall{
				{args: []string{"show", "gt-1"}, spend: 500 * time.Microsecond, reply: reply{stdout: "{}"}},
			},
			want: CensusRow{Verb: "show", Count: 1, Ms: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCensus(t)
			clock := &fakeCallClock{now: time.Unix(0, 0)}
			prior := callNow
			//testpolicy:allow no-global-swap — restores the clock seam swapped below
			t.Cleanup(func() { callNow = prior })
			//testpolicy:allow no-global-swap — the fake clock runBdOnce reads is the swap this test makes
			callNow = clock.read
			next := 0
			r := newRecorder(func(args []string) reply {
				if next >= len(tt.calls) {
					t.Fatalf("bd call %d: want at most %d subprocesses, got another for %v", next+1, len(tt.calls), args)
				}
				call := tt.calls[next]
				next++
				clock.advance(call.spend)
				return call.reply
			})
			b := newRecordedBeads(t.TempDir(), r)
			for _, call := range tt.calls {
				if _, err := b.runBdOnce(context.Background(), nil, nil, call.args); (err != nil) != (call.reply.err != nil) {
					t.Fatalf("runBdOnce(%v) error = %v, want an error: %v", call.args, err, call.reply.err != nil)
				}
			}

			rows := CensusSnapshotAndReset()
			if len(rows) != 1 {
				t.Fatalf("rows = %+v, want the calls folded into one row", rows)
			}
			if got := rows[0]; got.Verb != tt.want.Verb || got.Count != tt.want.Count || got.Ms != tt.want.Ms {
				t.Errorf("row = %+v, want verb %q, count %d, %d ms", got, tt.want.Verb, tt.want.Count, tt.want.Ms)
			}
			if rows[0].Caller == "" || rows[0].Caller == censusUnknown {
				t.Errorf("row %+v did not name a caller outside the seam", rows[0])
			}
		})
	}
}

// TestCensusMsEndAfterResetAddsNoRow: a census window that ends while a call
// is in flight reports the call's count and clears its row, so the call's end
// adds no row that would name time for calls the window never counted.
//
//testpolicy:allow parallel — it swaps the package-level callNow clock, so no test may run beside it
func TestCensusMsEndAfterResetAddsNoRow(t *testing.T) {
	resetCensus(t)
	clock := &fakeCallClock{now: time.Unix(0, 0)}
	prior := callNow
	//testpolicy:allow no-global-swap — restores the clock seam swapped below
	t.Cleanup(func() { callNow = prior })
	//testpolicy:allow no-global-swap — the fake clock runBdOnce reads is the swap this test makes
	callNow = clock.read
	r := newRecorder(func([]string) reply {
		// The window boundary lands between the call's start and its end.
		if rows := CensusSnapshotAndReset(); len(rows) != 1 || rows[0].Count != 1 {
			t.Fatalf("rows = %+v, want the started call counted once", rows)
		}
		clock.advance(250 * time.Millisecond)
		return reply{stdout: "[]"}
	})
	b := newRecordedBeads(t.TempDir(), r)
	if _, err := b.runBdOnce(context.Background(), nil, nil, []string{"list", "--json"}); err != nil {
		t.Fatalf("runBdOnce: %v", err)
	}

	if rows := CensusSnapshotAndReset(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none after the reset", rows)
	}
}

// TestTickEffCensusMsStraddlesReset: a call that straddles a census reset is
// counted in the window it started in and its time is lost, so a same-key call
// that starts and ends in the next window reports its own time alone (the end of
// the first call must not be added to the row the second one created).
func TestTickEffCensusMsStraddlesReset(t *testing.T) {
	resetCensus(t)
	// Both calls start in this function, so they share a verb and a caller.
	a := censusRecord([]string{"list", "--json"})
	if rows := CensusSnapshotAndReset(); len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("window 1 rows = %+v, want call A counted once", rows)
	}
	b := censusRecord([]string{"list", "--json"})
	censusRecordEnd(b, 40*time.Millisecond)
	censusRecordEnd(a, 250*time.Millisecond)

	rows := CensusSnapshotAndReset()
	if len(rows) != 1 || rows[0].Count != 1 || rows[0].Ms != 40 {
		t.Fatalf("window 2 rows = %+v, want one row of count 1 and 40 ms (call B alone)", rows)
	}
}
