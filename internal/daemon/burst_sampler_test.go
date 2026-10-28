package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// burstTestTop is top's two CPU lines, the last of which is the sample's idle
// share: a host at 0.29 percent idle is pressed, so a row sampled from it takes
// the fast cadence.
const burstTestTop = "CPU usage: 99.42% user, 0.29% sys, 0.29% idle\nCPU usage: 99.42% user, 0.29% sys, 0.29% idle\n"

// burstTestEnv answers a real metrics.RunBurst without a host: top reports the
// host pressed, and the town holds no health file, no daemon log and no landing
// queue, so the row's one reading is its idle share.
func burstTestEnv(clk clockwork.Clock) func(string) metrics.Env {
	return func(town string) metrics.Env {
		return metrics.Env{
			Town: town,
			Run: func(_ context.Context, argv []string) ([]byte, error) {
				if filepath.Base(argv[0]) != "top" {
					return nil, errors.New("the sample ran " + strings.Join(argv, " "))
				}
				return []byte(burstTestTop), nil
			},
			Now:      clk.Now,
			ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		}
	}
}

// burstTestDaemon is a daemon a sampler starts on: a town, a logger writing to
// logf, the daemon's own context and the clock the test drives.
func burstTestDaemon(t *testing.T, town string, clk clockwork.Clock, logf io.Writer) *Daemon {
	t.Helper()
	d := &Daemon{config: DefaultConfig(town), logger: log.New(logf, "", 0)}
	d.clock = clk
	d.ctx, d.cancel = context.WithCancel(context.Background())
	t.Cleanup(d.cancel)
	return d
}

// burstTestLogLines is the lines a sink has recorded, for a failure message.
func burstTestLogLines(s *censusLogSink) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// readBurstRows reads the town's burst log as rows.
func readBurstRows(t *testing.T, town string) []metrics.BurstRow {
	t.Helper()
	data, err := os.ReadFile(metrics.BurstPath(town))
	if err != nil {
		t.Fatalf("reading the burst log: %v", err)
	}
	var rows []metrics.BurstRow
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row metrics.BurstRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parsing the burst log's row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// TestBurstSamplerAppendsRowsOneCadenceApart is the bead's acceptance test: the
// production start, loop and wait, with a real metrics.RunBurst over a fake
// host, append two rows exactly one cadence apart while the host is pressed.
// The second sample's time comes from the daemon's clock, so a cadence the
// code did not take would move the row and fail the assertion. The sample
// count is asserted exactly: the test drives one cadence, so a third sample
// means the loop is not waiting it, and a short count means a sample it should
// have taken is missing.
func TestBurstSamplerAppendsRowsOneCadenceApart(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	d := burstTestDaemon(t, town, clk, io.Discard)

	samples := 0
	second := make(chan struct{})
	d.burstSeams = burstSamplerSeams{
		env: burstTestEnv(clk),
		run: func(ctx context.Context, env metrics.Env) (metrics.BurstRow, string, error) {
			samples++
			row, path, err := metrics.RunBurst(ctx, env)
			if samples == 2 {
				close(second)
			}
			return row, path, err
		},
	}
	stop := d.startBurstSamplerIfEnabled()
	driveClock(t, clk, metrics.BurstFastInterval, second)
	stop()

	if samples != 2 {
		t.Fatalf("the sampler took %d samples, want 2: the test drives one cadence, so a short count is a sample the loop skipped and a long one is a sample it took without waiting", samples)
	}
	rows := readBurstRows(t, town)
	if len(rows) != 2 {
		t.Fatalf("the burst log holds %d rows, want 2", len(rows))
	}
	first, then := rows[0], rows[1]
	if want := testEpoch.UTC().Format(time.RFC3339); first.TS != want {
		t.Errorf("the first row's ts = %q, want %q", first.TS, want)
	}
	if want := testEpoch.Add(metrics.BurstFastInterval).UTC().Format(time.RFC3339); then.TS != want {
		t.Errorf("the second row's ts = %q, want %q: the gap between rows is not the row's cadence", then.TS, want)
	}
	for i, row := range rows {
		if want := metrics.BurstFastInterval.Seconds(); row.IntervalS != want {
			t.Errorf("row %d's interval_s = %v, want %v on a host at 0.29 percent idle", i, row.IntervalS, want)
		}
	}
}

// TestBurstSamplerFailedSampleKeepsSamplingAtTheFastCadence pins what a failed
// sample leaves: nothing written, one log line, and the fast wait — the failed
// sample leaves a zero row, and the first failure in a run is a host to look at
// again soon. The next sample waits its own row's cadence.
func TestBurstSamplerFailedSampleKeepsSamplingAtTheFastCadence(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	sink := newCensusLogSink()
	d := burstTestDaemon(t, town, newFixedClock(), sink)

	// The loop runs one goroutine, so the waits it records need no lock.
	var waits []time.Duration
	twoWaits := make(chan struct{})
	d.burstSeams = burstSamplerSeams{
		env: func(string) metrics.Env { return metrics.Env{} },
		run: func(context.Context, metrics.Env) (metrics.BurstRow, string, error) {
			if len(waits) == 0 {
				return metrics.BurstRow{}, metrics.BurstPath(town), errors.New("no reading of the host could be taken")
			}
			return metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}, metrics.BurstPath(town), nil
		},
		wait: func(_ context.Context, dur time.Duration) {
			waits = append(waits, dur)
			if len(waits) == 2 {
				close(twoWaits)
				d.cancel()
			}
		},
	}
	stop := d.startBurstSamplerIfEnabled()
	waitForSignal(t, twoWaits, "the sampler never took two waits")
	stop()

	want := []time.Duration{metrics.BurstFastInterval, metrics.BurstSlowInterval}
	if len(waits) != len(want) || waits[0] != want[0] || waits[1] != want[1] {
		t.Errorf("the sampler waited %v, want %v: the fast cadence after a failed sample, then the row's own", waits, want)
	}
	if _, err := os.Stat(metrics.BurstPath(town)); !os.IsNotExist(err) {
		t.Errorf("the failed sample wrote the burst log (%v), want nothing written", err)
	}
	if got := sink.countContaining("burst_sampler: "); got != 1 {
		t.Errorf("the sampler logged the failure %d times, want once", got)
	}
}

// TestBurstSamplerBacksOffOnPersistentFailure is the bead's backoff acceptance
// test: the production loop and the production wait on the daemon's clock,
// with the samples scripted. Seven failures in a row double the wait from the
// fast cadence up to the ceiling, one good sample returns the row's own
// cadence, and a failure after it starts the fast cadence again. The clock
// advances one fast cadence at a time and only once the sampler is parked, so
// no advance moves a deadline the code has not set.
func TestBurstSamplerBacksOffOnPersistentFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	d := burstTestDaemon(t, town, clk, io.Discard)

	fail := errors.New("no reading of the host could be taken")
	// Seven failures, a good sample, a failure, a good sample: a nil is a
	// sample that read the host.
	script := []error{fail, fail, fail, fail, fail, fail, fail, nil, fail, nil}
	samples := 0
	var waits []time.Duration
	scriptWaited := make(chan struct{})

	d.burstSeams = burstSamplerSeams{
		env: func(string) metrics.Env { return metrics.Env{} },
		run: func(context.Context, metrics.Env) (metrics.BurstRow, string, error) {
			row := metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}
			if samples == len(script) {
				return row, metrics.BurstPath(town), errors.New("the sampler ran a sample the script does not cover")
			}
			err := script[samples]
			samples++
			return row, metrics.BurstPath(town), err
		},
	}
	// The recording wrapper runs the production wait, so the fake clock drives
	// the loop and the test reads the gap the code chose. The loop is one
	// goroutine, so the waits it records need no lock.
	s := d.burstSampler()
	clockWait := s.seams.wait
	s.seams.wait = func(ctx context.Context, gap time.Duration) {
		waits = append(waits, gap)
		if len(waits) == len(script) {
			close(scriptWaited)
		}
		clockWait(ctx, gap)
	}

	stop := d.startBurstSamplerIfEnabled()
	driveClock(t, clk, metrics.BurstFastInterval, scriptWaited)
	stop()

	want := []time.Duration{
		metrics.BurstFastInterval,      // failure 1
		2 * metrics.BurstFastInterval,  // failure 2
		4 * metrics.BurstFastInterval,  // failure 3
		8 * metrics.BurstFastInterval,  // failure 4
		16 * metrics.BurstFastInterval, // failure 5
		32 * metrics.BurstFastInterval, // failure 6
		burstFailureCeiling,            // failure 7, capped
		metrics.BurstSlowInterval,      // the good sample's own cadence
		metrics.BurstFastInterval,      // the failure after it starts over
		metrics.BurstSlowInterval,      // the second good sample
	}
	if len(waits) != len(want) {
		t.Fatalf("the sampler waited %d times, want %d: %v", len(waits), len(want), waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v: the whole run is %v", i+1, waits[i], want[i], waits)
		}
	}
}

// TestBurstFailureWait pins the backoff's shape as a table: the fast cadence on
// the first failure, doubling to the ceiling, and no overflow from a run long
// enough to leave it behind.
func TestBurstFailureWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		consecutive int
		want        time.Duration
	}{
		{name: "no failure yet takes the fast cadence", consecutive: 0, want: metrics.BurstFastInterval},
		{name: "the first failure takes the fast cadence", consecutive: 1, want: metrics.BurstFastInterval},
		{name: "the second failure doubles it", consecutive: 2, want: 2 * metrics.BurstFastInterval},
		{name: "the third failure doubles again", consecutive: 3, want: 4 * metrics.BurstFastInterval},
		{name: "the sixth failure is still under the ceiling", consecutive: 6, want: 32 * metrics.BurstFastInterval},
		{name: "the seventh failure reaches the ceiling", consecutive: 7, want: burstFailureCeiling},
		{name: "a failure past the ceiling stays at it", consecutive: 8, want: burstFailureCeiling},
		{name: "a very long run does not overflow", consecutive: 1000, want: burstFailureCeiling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := burstFailureWait(tt.consecutive); got != tt.want {
				t.Errorf("burstFailureWait(%d) = %v, want %v", tt.consecutive, got, tt.want)
			}
		})
	}
}

// TestBurstSamplerLogsARepeatedFailureOnce pins the failure log's dedup: a host
// the sampler cannot read retries on a timer, so the same failure is one line
// however many ticks it repeats — including a message that varies by a pid or
// a timestamp — and a failure that returns after a recovery is logged again.
// Two failures that differ in an exit code or a count are different failures
// and each log.
func TestBurstSamplerLogsARepeatedFailureOnce(t *testing.T) {
	t.Parallel()
	fail := errors.New("no reading of the host could be taken")
	other := errors.New("appending the burst row: the log is gone")
	tests := []struct {
		name   string
		script []error
		want   int
	}{
		{name: "a run of identical failures is one line", script: []error{fail, fail, fail}, want: 1},
		{name: "a failure after a recovery is logged again", script: []error{fail, fail, nil, fail}, want: 2},
		{name: "distinct failures each log", script: []error{fail, other}, want: 2},
		{name: "a good run logs nothing", script: []error{nil, nil}, want: 0},
		{name: "failures that differ only in a pid are one line", script: []error{pidFailure(4821), pidFailure(993)}, want: 1},
		{name: "failures that differ only in a timestamp are one line", script: []error{stampFailure(testEpoch), stampFailure(testEpoch.Add(time.Minute))}, want: 1},
		{name: "failures that differ in an exit code each log", script: []error{exitFailure(1), exitFailure(2)}, want: 2},
		{name: "failures that differ in a count each log", script: []error{countFailure(3), countFailure(4)}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			sink := newCensusLogSink()
			d := burstTestDaemon(t, town, newFixedClock(), sink)

			// The sampler's one goroutine keeps the script's place, so neither
			// the index nor the wait's check needs a lock.
			samples := 0
			scriptDone := make(chan struct{})
			d.burstSeams = burstSamplerSeams{
				env: func(string) metrics.Env { return metrics.Env{} },
				run: func(_ context.Context, _ metrics.Env) (metrics.BurstRow, string, error) {
					row := metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}
					if samples == len(tt.script) {
						return row, metrics.BurstPath(town), nil
					}
					err := tt.script[samples]
					samples++
					return row, metrics.BurstPath(town), err
				},
				wait: func(_ context.Context, _ time.Duration) {
					if samples == len(tt.script) {
						d.cancel()
						close(scriptDone)
					}
				},
			}
			stop := d.startBurstSamplerIfEnabled()
			waitForSignal(t, scriptDone, "the sampler never got through its script")
			stop()

			if got := sink.countContaining("burst_sampler: "); got != tt.want {
				t.Errorf("the sampler logged %d lines for the script %v, want %d: %q", got, tt.script, tt.want, burstTestLogLines(sink))
			}
		})
	}
}

// pidFailure is one failure reported against different processes: a message
// that names the pid differs on every tick, and the dedup still counts it as
// one failure.
func pidFailure(pid int) error {
	return fmt.Errorf("reading the process table at pid %d: the read failed", pid)
}

// stampFailure is one failure reported at different times.
func stampFailure(at time.Time) error {
	return fmt.Errorf("no reading of the host at %s could be taken", at.UTC().Format(time.RFC3339))
}

// exitFailure is a command failure whose exit code is what failed, so two codes
// are two failures rather than one class.
func exitFailure(code int) error {
	return fmt.Errorf("reading the host: top exited with status %d", code)
}

// countFailure is a failure that names how many rows it could not read, which
// is part of what failed rather than a value that varies per tick.
func countFailure(rows int) error {
	return fmt.Errorf("reading the landing queue: %d rows could not be read", rows)
}

// TestBurstSamplerOffWithTheMetricsSampler pins the gate: a town that switched
// its metrics sampler off takes no sample over a window of the row's cadences,
// and a town that never mentioned the switch samples once per cadence across
// that window.
func TestBurstSamplerOffWithTheMetricsSampler(t *testing.T) {
	t.Parallel()
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: false}}}
	// halfCadence is the step the window advances by, and window its length:
	// two and a half cadences, which ends between two samples so the count is
	// not a race with the stop. windowWaits is how many of the clock's waiters
	// the driver waits for before each advance: the sentinel, and the
	// sampler's loop when the switch is on.
	const halfCadence = metrics.BurstSlowInterval / 2
	const window = 5 * halfCadence
	const windowSamples = 3 // the start, then one per cadence across the window
	tests := []struct {
		name        string
		patrols     *DaemonPatrolConfig
		windowWaits int
		want        int32
	}{
		{name: "an explicit enabled=false samples nothing", patrols: off, windowWaits: 1, want: 0},
		{name: "the absent switch samples each cadence", patrols: nil, windowWaits: 2, want: windowSamples},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			d := burstTestDaemon(t, town, clk, io.Discard)
			d.patrolConfig = tt.patrols

			var calls atomic.Int32
			d.burstSeams = burstSamplerSeams{
				env: func(string) metrics.Env { return metrics.Env{} },
				run: func(context.Context, metrics.Env) (metrics.BurstRow, string, error) {
					calls.Add(1)
					return metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}, metrics.BurstPath(town), nil
				},
			}
			stop := d.startBurstSamplerIfEnabled()

			// A sentinel parks on the daemon's clock, so the window has a
			// waiter even when the gate started no loop, and each advance waits
			// until the clock's waiters are parked: a loop let through by
			// mistake has sampled by then, so the window catches it rather
			// than checking one instant.
			go func() { <-clk.After(window) }()
			for clk.Now().Before(testEpoch.Add(window)) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				err := clk.BlockUntilContext(ctx, tt.windowWaits)
				cancel()
				if err != nil {
					stop()
					t.Fatalf("the clock's waiters never parked for the window: %v", err)
				}
				clk.Advance(halfCadence)
			}
			stop()
			if got := calls.Load(); got != tt.want {
				t.Errorf("the sampler took %d samples over the window, want %d", got, tt.want)
			}
		})
	}
}

// TestBurstSamplerStartsOneLoop pins the single-flight guard: a second start
// runs no second loop, so the town pays one sample per cadence and not one per
// caller.
func TestBurstSamplerStartsOneLoop(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	d := burstTestDaemon(t, town, clk, io.Discard)

	var calls atomic.Int32
	sampled := make(chan struct{}, 2)
	d.burstSeams = burstSamplerSeams{
		env: func(string) metrics.Env { return metrics.Env{} },
		// The sample holds until the loop's context ends, so a second loop
		// started by the second start would enter one before its stop returns.
		run: func(ctx context.Context, _ metrics.Env) (metrics.BurstRow, string, error) {
			calls.Add(1)
			sampled <- struct{}{}
			<-ctx.Done()
			return metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}, metrics.BurstPath(town), nil
		},
	}
	stopFirst := d.startBurstSamplerIfEnabled()
	waitForSignal(t, sampled, "the first start never sampled")
	stopSecond := d.startBurstSamplerIfEnabled()
	stopSecond()
	if got := calls.Load(); got != 1 {
		t.Fatalf("the second start ran a sample, so it ran a second loop: %d samples, want 1", got)
	}
	stopFirst()
	if got := calls.Load(); got != 1 {
		t.Errorf("stopping the first loop ran %d samples, want 1", got)
	}
}

// TestBurstSamplerStopJoinsTheLoop pins the shutdown join: a sample still
// running when the daemon stops ends with the loop, before the stop returns, so
// a daemon that returned from Run leaves no sample behind it.
func TestBurstSamplerStopJoinsTheLoop(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := burstTestDaemon(t, town, newFixedClock(), io.Discard)

	inSample := make(chan struct{})
	d.burstSeams = burstSamplerSeams{
		env: func(string) metrics.Env { return metrics.Env{} },
		run: func(ctx context.Context, _ metrics.Env) (metrics.BurstRow, string, error) {
			close(inSample)
			<-ctx.Done()
			return metrics.BurstRow{}, "", errors.New("the sample ended with the town")
		},
	}
	stop := d.startBurstSamplerIfEnabled()
	waitForSignal(t, inSample, "the sampler never sampled")
	stop()
	select {
	case <-d.burst.done:
	default:
		t.Fatal("stop returned before the sampler's loop ended")
	}
}

// TestBurstRowWait pins the gap the sampler takes after a row: the row's own
// cadence, and the fast one for a row that names none, which is what a failed
// sample leaves behind.
func TestBurstRowWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		interval float64
		want     time.Duration
	}{
		{name: "a pressed row waits its fast cadence", interval: 15, want: 15 * time.Second},
		{name: "a calm row waits its slow cadence", interval: 60, want: time.Minute},
		{name: "a failed sample's zero row waits the fast cadence", interval: 0, want: metrics.BurstFastInterval},
		{name: "a negative interval waits the fast cadence", interval: -1, want: metrics.BurstFastInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := burstRowWait(metrics.BurstRow{IntervalS: tt.interval}); got != tt.want {
				t.Errorf("burstRowWait(%v) = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}

// TestRunStartsTheBurstSampler pins the call site that gives the sampler life:
// a Run start starts it, and Run's return stops and joins its loop. Run's other
// startup work is stubbed out the way the census test stubs it (heartbeat,
// legacy sockets, landing infra, dispatch); the resource sampler stays on
// because it is the burst sampler's switch, with its own sampler faked so no
// tick can reach the host's process list.
func TestRunStartsTheBurstSampler(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: DefaultConfig(town), logger: log.New(newCensusLogSink(), "", 0)}
	d.disabledPatrols = map[string]bool{
		"landing_infra": true,
		"spec_dispatch": true,
	}
	d.seams.heartbeatWork = func(*Daemon, *State, bool) {}
	d.seams.cleanupLegacySockets = func(string) (int, int) { return 0, 0 }
	d.newResourceSampler = func(string, resourcesampler.Clock, time.Duration) resourceTicker {
		return &fakeResourceTicker{}
	}

	started := make(chan struct{}, 1)
	waitEnded := make(chan struct{})
	d.burstSeams = burstSamplerSeams{
		env: func(string) metrics.Env { return metrics.Env{} },
		run: func(context.Context, metrics.Env) (metrics.BurstRow, string, error) {
			started <- struct{}{}
			return metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}, "", nil
		},
		wait: func(ctx context.Context, _ time.Duration) {
			<-ctx.Done()
			close(waitEnded)
		},
	}

	d.ctx, d.cancel = context.WithCancel(context.Background())
	t.Cleanup(d.cancel)
	done := make(chan error, 1)
	go func() { done <- d.Run() }()

	waitForSignal(t, started, "Run never started the burst sampler: the start path does not wire it in")
	d.cancel()
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after the daemon context ended = %v, want a clean shutdown", err)
		}
	case <-bounded.Done():
		t.Fatal("Run did not return after the daemon context ended")
	}
	waitForSignal(t, waitEnded, "Run returned without stopping the burst sampler's loop")
}
