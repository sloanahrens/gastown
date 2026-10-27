package metrics

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sweepMinutes is a wanted reading: a minute count, or nil for a column that
// must be left absent.
func sweepMinutes(v float64) *float64 { return &v }

// sweepStampedPad and sweepUnstampedPad fill a read past daemonLogHourBytes, so
// a test can put a stamp at the head and the tail around filler and hand
// Collect the shape a byte-bounded read returns: one that filled the bound.
var (
	sweepStampedPad   = strings.Repeat("2026/10/24 12:30:00 daemon: patrol tick complete\n", daemonLogHourBytes/48+1)
	sweepUnstampedPad = strings.Repeat("daemon: patrol tick complete\n", daemonLogHourBytes/26+1)
)

// sweepFullTail builds a log read that used up the whole byte bound, with the
// head and tail lines a test wants around the filler.
func sweepFullTail(t *testing.T, head, pad, tail string) string {
	t.Helper()
	log := head + pad + tail
	if len(log) < daemonLogHourBytes {
		t.Fatalf("fixture is %d bytes, want at least %d", len(log), daemonLogHourBytes)
	}
	return log
}

func TestSourceSweepSumsTheStagesFinishedInTheHour(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		log     string
		readErr error
		want    *float64
	}{
		{
			// The stage's output follows its line, so the lines after a
			// finished line are the run's own tail and are not sweep lines.
			name: "two stages inside the hour sum",
			log: "2026/10/24 12:10:00 tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n" +
				"tier-sweep: shell GREEN passed=168 failed=0 skipped=0 (logs /tmp/tier-sweep.tXlJk) in 20m0s\n" +
				"\n" +
				"2026/10/24 12:40:00 tier_sweep: gastown: integration finished (exit 0) in 1h0m0s; last lines:\n" +
				"tier-sweep: integration GREEN passed=4 failed=0 skipped=0 (logs /tmp/tier-sweep.q8Zfx) in 1h0m0s\n",
			want: sweepMinutes(80),
		},
		{
			name: "a stage older than the hour is left out",
			log: "2026/10/24 11:59:59 tier_sweep: gastown: shell finished (exit 0) in 30m0s; last lines:\n" +
				"2026/10/24 12:30:00 tier_sweep: gastown: shell finished (exit 0) in 5m0s; last lines:\n",
			want: sweepMinutes(5),
		},
		{
			name: "a stage exactly an hour old still counts",
			log:  "2026/10/24 12:00:00 tier_sweep: gastown: shell finished (exit 0) in 12m30s; last lines:\n",
			want: sweepMinutes(12.5),
		},
		{
			// A RED stage swept as long as a green one, and the exit a killed
			// stage reports is negative: both are time spent sweeping.
			name: "a red stage and a killed one count their time",
			log: "2026/10/24 12:05:00 tier_sweep: gastown: shell RED: ./store\n" +
				"2026/10/24 12:05:00 tier_sweep: gastown: shell finished (exit 1) in 3m0s; last lines:\n" +
				"2026/10/24 12:50:00 tier_sweep: gastown: race finished (exit -1) in 2m0s; last lines:\n",
			want: sweepMinutes(5),
		},
		{
			name: "other lines are not sweep time",
			log: "2026/10/24 12:10:00 spec_dispatch: tick took 4.512s (roster 1.2ms, board 900.5ms)\n" +
				"2026/10/24 12:11:00 tier_sweep: gastown: sweep started 1a2b3c4 (shell, integration)\n" +
				"2026/10/24 12:12:00 tier_sweep: gastown: shell not offered by the rig's script\n" +
				"2026/10/24 12:13:00 daemon: patrol tick complete\n",
			want: sweepMinutes(0),
		},
		{
			name: "an empty log is no sweep time, not a failed read",
			log:  "",
			want: sweepMinutes(0),
		},
		{
			name:    "an unreadable log leaves the column nil",
			readErr: errors.New("lumberjack rotated it away"),
			want:    nil,
		},
		{
			name:    "a log that is not there leaves the column nil",
			readErr: errors.New("no such file"),
			want:    nil,
		},
		{
			name: "a finished line whose duration does not parse leaves the column nil",
			log: "2026/10/24 12:10:00 tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n" +
				"2026/10/24 12:40:00 tier_sweep: gastown: integration finished (exit 0) in soon; last lines:\n",
			want: nil,
		},
		{
			name: "a line with no stamp above it is left out",
			log: "tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n" +
				"2026/10/24 12:30:00 tier_sweep: gastown: shell finished (exit 0) in 1m0s; last lines:\n",
			want: sweepMinutes(1),
		},
		{
			// The read filled its byte bound before it reached the start of
			// the hour, so the sum is the tail's sweep time, not the hour's.
			// The column is nil rather than a number that would read as a
			// quiet hour.
			name: "a full read that begins inside the hour leaves the column nil",
			log: sweepFullTail(t,
				"2026/10/24 12:00:30 daemon: patrol tick complete\n", sweepStampedPad,
				"2026/10/24 12:40:00 tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n"),
			want: nil,
		},
		{
			// A full read that still reaches past the cutoff saw the whole
			// hour, so its sum stands.
			name: "a full read that reaches back past the cutoff still sums",
			log: sweepFullTail(t,
				"2026/10/24 11:50:00 daemon: patrol tick complete\n", sweepStampedPad,
				"2026/10/24 12:40:00 tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n"),
			want: sweepMinutes(20),
		},
		{
			// A full read with no stamp at all can be placed nowhere, so it
			// cannot show that it covered the hour either.
			name: "a full read with no stamp leaves the column nil",
			log: sweepFullTail(t,
				"daemon: patrol tick complete\n", sweepUnstampedPad,
				"tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n"),
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
			log := &queueTestLog{data: []byte(tc.log), err: tc.readErr}
			env := queueTestEnv(t, nil, nil, log)
			env.Now = func() time.Time { return now }

			cols, err := sweepSource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			got, ok := cols["sweep_minutes_1h"]
			if !ok {
				t.Fatalf("sweep_minutes_1h is missing from the group: %v", cols)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("sweep_minutes_1h = %v, want nil", got)
				}
				return
			}
			minutes, ok := got.(float64)
			if !ok {
				t.Fatalf("sweep_minutes_1h = %v (%T), want a float64", got, got)
			}
			if math.Abs(minutes-*tc.want) > 1e-9 {
				t.Errorf("sweep_minutes_1h = %v, want %v", minutes, *tc.want)
			}
			if log.max != daemonLogHourBytes {
				t.Errorf("Tail max = %d, want %d", log.max, daemonLogHourBytes)
			}
		})
	}
}

// The columns come from the daemon's live log, the file the daemon is writing.
// The fake records the path it was asked for.
func TestSourceSweepReadsTheDaemonLog(t *testing.T) {
	t.Parallel()
	var path string
	env := queueTestEnv(t, nil, nil, &queueTestLog{})
	env.Tail = func(p string, _ int64) ([]byte, error) {
		path = p
		return nil, nil
	}
	if _, err := (sweepSource{}).Collect(context.Background(), env); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if want := filepath.Join(queueTestTown, "daemon", "daemon.log"); path != want {
		t.Errorf("Tail(%q), want %q", path, want)
	}
}
