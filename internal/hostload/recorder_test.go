package hostload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// recorderWait bounds every wait the loop tests make. A loop that neither
// writes nor returns within it fails the test instead of running until go
// test's own timeout ends the package.
const recorderWait = 10 * time.Second

// TestHostLoadSampleLoopAppends drives the sampling loop on a fake clock: one JSON
// line appended per interval, in order, and a clean stop on the cancel.
func TestHostLoadSampleLoopAppends(t *testing.T) {
	t.Parallel()
	const interval = 10 * time.Second
	clk := clockwork.NewFakeClockAt(sampleEpoch)
	lines := make(chan []byte, 8)
	r := &Recorder{
		Interval: interval,
		Sample:   func(context.Context) Sample { return Sample{At: clk.Now()} },
		Append:   func(line []byte) error { lines <- line; return nil },
		Clock:    clk,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	wait, stopWait := context.WithTimeout(context.Background(), recorderWait)
	defer stopWait()
	var got []Sample
	for i := 0; i < 3; i++ {
		// The loop appends before it waits, so a registered interval means
		// the next line is already on its way.
		if err := clk.BlockUntilContext(wait, 1); err != nil {
			t.Fatalf("waiting for the loop's next interval: %v", err)
		}
		select {
		case line := <-lines:
			if !bytes.HasSuffix(line, []byte("\n")) {
				t.Fatalf("line %d is not newline-terminated: %q", i, line)
			}
			var s Sample
			if err := json.Unmarshal(line, &s); err != nil {
				t.Fatalf("line %d is not JSON: %v (%q)", i, err, line)
			}
			got = append(got, s)
		case <-wait.Done():
			t.Fatalf("no line %d within the test's bound", i)
		}
		if i < 2 {
			clk.Advance(interval)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v, want a clean stop on the cancel", err)
		}
	case <-wait.Done():
		t.Fatal("the loop did not stop after the cancel")
	}
	if extra := len(lines); extra != 0 {
		t.Errorf("the loop appended %d line(s) past the three intervals", extra)
	}
	for i, s := range got {
		if want := sampleEpoch.Add(time.Duration(i) * interval); !s.At.Equal(want) {
			t.Errorf("sample %d stamped %s, want %s", i, s.At, want)
		}
	}
}

// TestHostLoadRecorderRefusesUnusableSettings drives each way the recorder refuses to
// start or carry on: a bad interval, a missing seam, a sample it cannot
// encode and a write that failed all end the loop with an error.
func TestHostLoadRecorderRefusesUnusableSettings(t *testing.T) {
	t.Parallel()
	okSample := func(context.Context) Sample { return Sample{} }
	okAppend := func([]byte) error { return nil }
	cases := []struct {
		name string
		r    *Recorder
		want string
	}{
		{
			name: "zero interval",
			r:    &Recorder{Interval: 0, Sample: okSample, Append: okAppend},
			want: "must be positive",
		},
		{
			name: "no sample reader",
			r:    &Recorder{Interval: time.Second, Append: okAppend},
			want: "no sample reader",
		},
		{
			name: "no line writer",
			r:    &Recorder{Interval: time.Second, Sample: okSample},
			want: "no line writer",
		},
		{
			name: "a sample JSON cannot hold",
			r: &Recorder{Interval: time.Second, Append: okAppend, Sample: func(context.Context) Sample {
				return Sample{Owners: hostprobe.Field[[]Row]{Value: []Row{{Owner: "x", CPU: math.NaN()}}}}
			}},
			want: "encoding a sample",
		},
		{
			name: "a write that failed",
			r: &Recorder{Interval: time.Second, Sample: okSample, Append: func([]byte) error {
				return errors.New("disk full")
			}},
			want: "disk full",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Run(context.Background())
			if err == nil {
				t.Fatalf("the recorder carried on with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
