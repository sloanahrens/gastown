package metrics

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sweepLog is a daemon log line every snapshot's hour starts before, so a
// fixture that sums sweeps shows the read covering the hour.
const sweepLog = "2026/10/24 11:30:00 daemon: patrol tick complete\n"

// sweepFinished is one stage's finished line stamped at the hour the fixtures
// stand in.
func sweepFinished(stamp, duration string) string {
	return "2026/10/24 " + stamp + " tier_sweep: gastown: shell finished (exit 0) in " + duration + "; last lines:\n"
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
			log: sweepLog +
				sweepFinished("12:10:00", "20m0s") +
				"tier-sweep: shell GREEN passed=168 failed=0 skipped=0 (logs /tmp/tier-sweep.tXlJk) in 20m0s\n" +
				"\n" +
				sweepFinished("12:40:00", "1h0m0s") +
				"tier-sweep: integration GREEN passed=4 failed=0 skipped=0 (logs /tmp/tier-sweep.q8Zfx) in 1h0m0s\n",
			want: sweepMinutes(80),
		},
		{
			name: "a stage older than the hour is left out",
			log: "2026/10/24 11:59:59 tier_sweep: gastown: shell finished (exit 0) in 30m0s; last lines:\n" +
				sweepFinished("12:30:00", "5m0s"),
			want: sweepMinutes(5),
		},
		{
			// The cutoff is part of the hour, and this line's stamp is the
			// oldest the log holds, so it also shows the read covered the hour.
			name: "a stage exactly an hour old still counts",
			log:  sweepFinished("12:00:00", "12m30s"),
			want: sweepMinutes(12.5),
		},
		{
			// A RED stage swept as long as a green one, and the exit a killed
			// stage reports is negative: both are time spent sweeping.
			name: "a red stage and a killed one count their time",
			log: sweepLog +
				"2026/10/24 12:05:00 tier_sweep: gastown: shell RED: ./store\n" +
				sweepFinished("12:05:00", "3m0s") +
				"2026/10/24 12:50:00 tier_sweep: gastown: race finished (exit -1) in 2m0s; last lines:\n",
			want: sweepMinutes(5),
		},
		{
			name: "other lines are not sweep time",
			log: sweepLog +
				"2026/10/24 12:10:00 spec_dispatch: tick took 4.512s (roster 1.2ms, board 900.5ms)\n" +
				"2026/10/24 12:11:00 tier_sweep: gastown: sweep started 1a2b3c4 (shell, integration)\n" +
				"2026/10/24 12:12:00 tier_sweep: gastown: shell not offered by the rig's script\n" +
				"2026/10/24 12:13:00 daemon: patrol tick complete\n",
			want: sweepMinutes(0),
		},
		{
			// The log holds nothing, so it cannot show the hour started before
			// the sweep did: a daemon that restarted into an empty log must not
			// read as an hour with no test load.
			name: "an empty log cannot show it covers the hour",
			log:  "",
			want: nil,
		},
		{
			name:    "an unreadable log leaves the column nil",
			readErr: errors.New("lumberjack rotated it away"),
			want:    nil,
		},
		{
			name: "a finished line whose duration does not parse leaves the column nil",
			log: sweepLog +
				sweepFinished("12:10:00", "20m0s") +
				sweepFinished("12:40:00", "soon"),
			want: nil,
		},
		{
			// A line cut from the top of the read has no stamp to place it; the
			// stamped lines after it still show the read reached past the cutoff.
			name: "a line with no stamp above it is left out",
			log: "tier_sweep: gastown: shell finished (exit 0) in 20m0s; last lines:\n" +
				sweepLog +
				sweepFinished("12:30:00", "1m0s"),
			want: sweepMinutes(1),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
			log := &queueTestLog{data: []byte(tc.log), err: tc.readErr}
			env := queueTestEnv(t, nil, nil, log)
			env.Now = func() time.Time { return now }

			got, err := sweepSource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			sweepWantMinutes(t, got, tc.want)
			if log.max != daemonLogHourBytes {
				t.Errorf("Tail max = %d, want %d", log.max, daemonLogHourBytes)
			}
		})
	}
}

// The hour's sum is only the hour's when the read shows the hour's start.
// These cases run the production tail file over a real log, so what the source
// sees is the shape a byte-bounded read returns at either end of the file, not
// a fixture that assumes it.
func TestSourceSweepNeedsTheReadToCoverTheHour(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// content is the whole log; bound is what the source reads of its end.
		content string
		bound   int64
		want    *float64
	}{
		{
			// The bound cut the read off before the hour began: the sum is the
			// read's sweep time, not the hour's, so it must not be reported.
			name:    "a bounded read that starts inside the hour leaves the column nil",
			content: "2026/10/24 11:00:00 daemon: patrol tick complete\n" + strings.Repeat("2026/10/24 12:30:00 daemon: patrol tick complete\n", 20) + sweepFinished("12:40:00", "20m0s"),
			bound:   960,
			want:    nil,
		},
		{
			name:    "a bounded read that reaches past the cutoff still sums",
			content: "2026/10/24 11:00:00 daemon: patrol tick complete\n" + strings.Repeat("2026/10/24 11:50:00 daemon: patrol tick complete\n", 20) + sweepFinished("12:40:00", "20m0s"),
			bound:   960,
			want:    sweepMinutes(20),
		},
		{
			// A log shorter than the bound is read whole, and it still starts
			// inside the hour: a rotation or a restart left only part of it.
			name:    "a short log that begins inside the hour leaves the column nil",
			content: "2026/10/24 12:30:00 daemon: patrol tick complete\n" + sweepFinished("12:40:00", "20m0s"),
			bound:   1 << 20,
			want:    nil,
		},
		{
			name:    "a short log that reaches past the cutoff still sums",
			content: sweepLog + sweepFinished("12:40:00", "20m0s"),
			bound:   1 << 20,
			want:    sweepMinutes(20),
		},
		{
			name:    "a short log with no stamp leaves the column nil",
			content: "daemon: patrol tick complete\n" + sweepFinished("12:40:00", "20m0s"),
			bound:   1 << 20,
			want:    nil,
		},
		{
			// The bound lands after the last stamped line, so the read shows
			// nothing of where the hour starts.
			name:    "a bounded read with no stamp in it leaves the column nil",
			content: sweepFinished("12:40:00", "20m0s") + strings.Repeat("daemon: patrol tick complete\n", 40),
			bound:   500,
			want:    nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := sweepLogEnv(t, tc.content)
			got, err := sweepSource{tailBytes: tc.bound}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			sweepWantMinutes(t, got, tc.want)
		})
	}
}

// The columns come from the daemon's live log, the file the daemon is writing.
// The fake records the path and bound it was asked for.
func TestSourceSweepReadsTheDaemonLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  sweepSource
		want int64
	}{
		{name: "the hour the columns window", src: sweepSource{}, want: daemonLogHourBytes},
		{name: "the bound its test set", src: sweepSource{tailBytes: 1024}, want: 1024},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var path string
			env := queueTestEnv(t, nil, nil, &queueTestLog{})
			env.Tail = func(p string, maxBytes int64) ([]byte, error) {
				path = p
				if maxBytes != tc.want {
					t.Errorf("Tail(%q, %d), want max %d", p, maxBytes, tc.want)
				}
				return nil, nil
			}
			if _, err := tc.src.Collect(context.Background(), env); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if want := filepath.Join(queueTestTown, "daemon", "daemon.log"); path != want {
				t.Errorf("Tail(%q), want %q", path, want)
			}
		})
	}
}

// sweepLogEnv stages content as a town's daemon log and returns an Env that
// reads it through the production tail file.
func sweepLogEnv(t *testing.T, content string) Env {
	t.Helper()
	town := t.TempDir()
	path := daemonLogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("staging the daemon log: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("staging the daemon log: %v", err)
	}
	return Env{
		Town: town,
		Tail: tailFile,
		Now:  func() time.Time { return time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC) },
	}
}

// sweepMinutes is a wanted reading: a minute count, or nil for a column that
// must be left absent.
func sweepMinutes(v float64) *float64 { return &v }

// sweepWantMinutes checks a collected group against a wanted sweep_minutes_1h.
func sweepWantMinutes(t *testing.T, cols map[string]any, want *float64) {
	t.Helper()
	got, ok := cols["sweep_minutes_1h"]
	if !ok {
		t.Fatalf("sweep_minutes_1h is missing from the group: %v", cols)
	}
	if want == nil {
		if got != nil {
			t.Fatalf("sweep_minutes_1h = %v, want nil", got)
		}
		return
	}
	minutes, ok := got.(float64)
	if !ok {
		t.Fatalf("sweep_minutes_1h = %v (%T), want a float64", got, got)
	}
	if math.Abs(minutes-*want) > 1e-9 {
		t.Errorf("sweep_minutes_1h = %v, want %v", minutes, *want)
	}
}
