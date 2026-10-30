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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hostprobe"
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

// burstProbeFixture is a probe file a host agent writes at testEpoch: the host
// calm at 42.5 percent idle and a 12.5 ms exec tax, with four consumers written
// in no order, so a row that keeps the file's order is not the ranking the row
// promises.
func burstProbeFixture(at time.Time) hostprobe.Probes {
	return hostprobe.Probes{
		At:         at,
		CPUIdlePct: hostprobe.Field[float64]{Value: 42.5},
		TopConsumers: hostprobe.Field[[]hostprobe.Consumer]{Value: []hostprobe.Consumer{
			{Name: "gt", CPU: 40},
			{Name: "dolt", CPU: 250},
			{Name: "BDLDaemon", CPU: 90},
			{Name: "Google Chrome", CPU: 10},
		}},
		ExecTaxMS: hostprobe.Field[float64]{Value: 12.5},
	}
}

// writeBurstProbe stages the probe file under town the way the host agent
// writes it.
func writeBurstProbe(t *testing.T, town string, p hostprobe.Probes) {
	t.Helper()
	path := hostprobe.Path(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating the host agent's directory: %v", err)
	}
	if err := hostprobe.Write(path, p); err != nil {
		t.Fatalf("writing the probe file: %v", err)
	}
}

// burstContainerGetenv is the read containerModeFromEnv takes of a daemon in
// container mode: the variable the rendered compose file sets answers,
// everything else is the empty environment.
func burstContainerGetenv(key string) string {
	if key == config.ContainerModeEnv {
		return config.SupervisorContainer
	}
	return ""
}

// burstInContainer puts a test's daemon in the town's container. Container mode
// is settled once on the daemon (New asks the process environment in
// production), so a test says it here rather than letting the environment the
// tests run under decide.
func burstInContainer(d *Daemon) *Daemon {
	d.containerMode = true
	return d
}

// burstTestTownEnv is burstTestEnv plus a daemon log holding a dispatcher
// roster two minutes before testEpoch, so a row the probe file cannot fill
// still has the town's own reading.
func burstTestTownEnv(clk clockwork.Clock) func(string) metrics.Env {
	base := burstTestEnv(clk)
	return func(town string) metrics.Env {
		env := base(town)
		env.Tail = func(string, int64) ([]byte, error) {
			return []byte("2026/01/02 15:03:05 spec_dispatch: tick: 1 candidate(s), roster deepseek-flash 2/2, claude-sonnet 1/2, 1 dispatched, 1 skipped\n"), nil
		}
		return env
	}
}

// burstSampleOnce starts the sampler, waits for its first sample and stops it.
// The fake clock never advances, so the loop parks after that row and the stop
// joins a loop with nothing left to take.
func burstSampleOnce(t *testing.T, d *Daemon) {
	t.Helper()
	run := d.burstSeams.run
	if run == nil {
		run = metrics.RunBurst
	}
	sampled := make(chan struct{}, 1)
	d.burstSeams.run = func(ctx context.Context, env metrics.Env) (metrics.BurstRow, string, error) {
		row, path, err := run(ctx, env)
		select {
		case sampled <- struct{}{}:
		default:
		}
		return row, path, err
	}
	stop := d.startBurstSamplerIfEnabled()
	waitForSignal(t, sampled, "the sampler never took a sample")
	stop()
}

// TestBurstSamplerReadsTheProbeFileInContainerMode is the bead's first
// acceptance test: in container mode the production sample fills the row's
// existing fields from the host agent's probe file. The fake host's own commands
// would read 0.29 percent idle, so a row filled any other way fails here.
func TestBurstSamplerReadsTheProbeFileInContainerMode(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	d := burstInContainer(burstTestDaemon(t, town, clk, io.Discard))
	writeBurstProbe(t, town, burstProbeFixture(testEpoch))
	d.burstSeams = burstSamplerSeams{
		env: burstTestTownEnv(clk),
	}
	burstSampleOnce(t, d)

	rows := readBurstRows(t, town)
	if len(rows) != 1 {
		t.Fatalf("the burst log holds %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.IdlePct == nil || *row.IdlePct != 42.5 {
		t.Errorf("idle_pct = %v, want the probe file's 42.5", row.IdlePct)
	}
	if row.ExecTaxMS == nil || *row.ExecTaxMS != 12.5 {
		t.Errorf("exec_tax_ms = %v, want the probe file's 12.5", row.ExecTaxMS)
	}
	if row.ExecTaxAgeS == nil || *row.ExecTaxAgeS != 0 {
		t.Errorf("exec_tax_age_s = %v, want 0: the file was stamped at the sample's own instant", row.ExecTaxAgeS)
	}
	if row.HostAgeS == nil || *row.HostAgeS != 0 {
		t.Errorf("host_age_s = %v, want 0: the file was stamped at the sample's own instant", row.HostAgeS)
	}
	wantConsumers := []metrics.BurstConsumer{
		{Name: "dolt", Cores: 2.5},
		{Name: "BDLDaemon", Cores: 0.9},
		{Name: "gt", Cores: 0.4},
	}
	if !reflect.DeepEqual(row.TopConsumers, wantConsumers) {
		t.Errorf("top_consumers = %+v, want the file's three busiest as %+v", row.TopConsumers, wantConsumers)
	}
	if len(row.HostErrors) != 0 {
		t.Errorf("host_errors = %v, want none: the file answered every host reading", row.HostErrors)
	}
	if want := metrics.BurstSlowInterval.Seconds(); row.IntervalS != want {
		t.Errorf("interval_s = %v, want %v: 42.5 percent idle under a 12.5 ms tax is a calm host", row.IntervalS, want)
	}
}

// TestBurstSamplerRecordsTheProbeFilesAge pins the age a file's readings carry:
// a file the sampler accepts may be up to burstProbeMaxAge old, so idle_pct and
// top_consumers describe the host then rather than at the row's ts, and the row
// says how far before it they were read. The file is ninety seconds old and the
// only row must say so beside calm readings, so a row that drops the age fails
// here.
func TestBurstSamplerRecordsTheProbeFilesAge(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	d := burstInContainer(burstTestDaemon(t, town, clk, io.Discard))
	writeBurstProbe(t, town, burstProbeFixture(testEpoch.Add(-90*time.Second)))
	d.burstSeams = burstSamplerSeams{
		env: burstTestTownEnv(clk),
	}
	burstSampleOnce(t, d)

	rows := readBurstRows(t, town)
	if len(rows) != 1 {
		t.Fatalf("the burst log holds %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.HostAgeS == nil || *row.HostAgeS != 90 {
		t.Errorf("host_age_s = %v, want 90: the file was stamped ninety seconds before the sample", row.HostAgeS)
	}
	if row.ExecTaxAgeS == nil || *row.ExecTaxAgeS != 90 {
		t.Errorf("exec_tax_age_s = %v, want 90: the tax shares the file's stamp", row.ExecTaxAgeS)
	}
	if row.IdlePct == nil || *row.IdlePct != 42.5 {
		t.Errorf("idle_pct = %v, want the file's 42.5: an accepted file's reading still fills the row", row.IdlePct)
	}
	if want := metrics.BurstSlowInterval.Seconds(); row.IntervalS != want {
		t.Errorf("interval_s = %v, want %v: 42.5 percent idle is a calm host", row.IntervalS, want)
	}
}

// TestBurstSamplerUnusableProbeFileMarksTheRow is the bead's second acceptance
// test: a file the daemon cannot use leaves the three host fields null with the
// reason in the row's host_errors and no number invented for them, and the
// town's own reading still makes a row.
func TestBurstSamplerUnusableProbeFileMarksTheRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stage  func(t *testing.T, town string)
		reason string
	}{
		{
			name: "a stale file",
			stage: func(t *testing.T, town string) {
				writeBurstProbe(t, town, burstProbeFixture(testEpoch.Add(-4*time.Minute)))
			},
			reason: "older than",
		},
		{
			name:   "a missing file",
			stage:  func(*testing.T, string) {},
			reason: "no such file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			sink := newCensusLogSink()
			d := burstInContainer(burstTestDaemon(t, town, clk, sink))
			tt.stage(t, town)
			d.burstSeams = burstSamplerSeams{
				env: burstTestTownEnv(clk),
			}
			burstSampleOnce(t, d)

			rows := readBurstRows(t, town)
			if len(rows) != 1 {
				t.Fatalf("the burst log holds %d rows, want 1: the town's own reading still makes a row", len(rows))
			}
			row := rows[0]
			if row.IdlePct != nil || row.ExecTaxMS != nil || row.TopConsumers != nil {
				t.Errorf("idle_pct = %v, exec_tax_ms = %v, top_consumers = %+v, want all null: the file holds no reading of now",
					row.IdlePct, row.ExecTaxMS, row.TopConsumers)
			}
			if row.ExecTaxAgeS != nil {
				t.Errorf("exec_tax_age_s = %v, want null beside a tax that was never read", row.ExecTaxAgeS)
			}
			if row.HostAgeS != nil {
				t.Errorf("host_age_s = %v, want null: the file answered no reading to be an age for", row.HostAgeS)
			}
			reason, ok := row.HostErrors[burstProbeFileKey]
			if !ok || !strings.Contains(reason, tt.reason) {
				t.Errorf("host_errors = %v, want an entry under %q naming a file %s", row.HostErrors, burstProbeFileKey, tt.reason)
			}
			if want := metrics.BurstFastInterval.Seconds(); row.IntervalS != want {
				t.Errorf("interval_s = %v, want %v: a host the file cannot describe is not a calm one", row.IntervalS, want)
			}
			if got := sink.countContaining("burst_sampler: "); got != 1 {
				t.Errorf("the sampler logged the unusable file %d times, want once: %q", got, burstTestLogLines(sink))
			}
		})
	}
}

// TestBurstSamplerProbeFieldErrorsCostThatFieldOnly pins each of the file's
// per-field failures: a command the agent could not run, or a consumer list it
// wrote null, leaves that one reading null with the file's reason under that
// field's host_errors key, and the file's other readings still fill the row.
// Every case breaks one field, so a reading that costs another its value — or a
// broken field that writes a zero instead of leaving null — fails on the fields
// the case left whole.
func TestBurstSamplerProbeFieldErrorsCostThatFieldOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		broken string
		stage  func(*hostprobe.Probes)
		key    string
		reason string
	}{
		{
			name:   "an idle share the agent could not read",
			broken: "idle",
			stage: func(p *hostprobe.Probes) {
				p.CPUIdlePct = hostprobe.Field[float64]{Err: "top: the agent could not read the host's CPU"}
			},
			key:    burstProbeIdleKey,
			reason: "top: the agent could not read the host's CPU",
		},
		{
			name:   "a consumer list the agent could not read",
			broken: "consumers",
			stage: func(p *hostprobe.Probes) {
				p.TopConsumers = hostprobe.Field[[]hostprobe.Consumer]{Err: "ps: the agent could not read the process table"}
			},
			key:    burstProbeConsumersKey,
			reason: "ps: the agent could not read the process table",
		},
		{
			name:   "a consumer list written null",
			broken: "consumers",
			stage: func(p *hostprobe.Probes) {
				p.TopConsumers = hostprobe.Field[[]hostprobe.Consumer]{}
			},
			key:    burstProbeConsumersKey,
			reason: "no consumer list",
		},
		{
			name:   "an exec tax the agent could not measure",
			broken: "tax",
			stage: func(p *hostprobe.Probes) {
				p.ExecTaxMS = hostprobe.Field[float64]{Err: "exec: the agent could not measure the tax"}
			},
			key:    burstProbeTaxKey,
			reason: "exec: the agent could not measure the tax",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			d := burstInContainer(burstTestDaemon(t, town, clk, io.Discard))
			p := burstProbeFixture(testEpoch)
			tt.stage(&p)
			writeBurstProbe(t, town, p)
			d.burstSeams = burstSamplerSeams{
				env: burstTestTownEnv(clk),
			}
			burstSampleOnce(t, d)

			rows := readBurstRows(t, town)
			if len(rows) != 1 {
				t.Fatalf("the burst log holds %d rows, want 1", len(rows))
			}
			row := rows[0]
			if len(row.HostErrors) != 1 {
				t.Errorf("host_errors = %v, want one entry: only the broken field has a reason", row.HostErrors)
			}
			if got := row.HostErrors[tt.key]; !strings.Contains(got, tt.reason) {
				t.Errorf("host_errors[%q] = %q, want the field's reason naming %q", tt.key, got, tt.reason)
			}
			switch tt.broken {
			case "idle":
				if row.IdlePct != nil {
					t.Errorf("idle_pct = %v, want null: the agent could not read it", *row.IdlePct)
				}
			case "consumers":
				if row.TopConsumers != nil {
					t.Errorf("top_consumers = %+v, want null: the file gave no list", row.TopConsumers)
				}
			case "tax":
				if row.ExecTaxMS != nil || row.ExecTaxAgeS != nil {
					t.Errorf("exec_tax_ms = %v, exec_tax_age_s = %v, want both null: the tax was never measured",
						row.ExecTaxMS, row.ExecTaxAgeS)
				}
			}
			if tt.broken != "idle" {
				if row.IdlePct == nil || *row.IdlePct != 42.5 {
					t.Errorf("idle_pct = %v, want the file's 42.5: a broken %s took the idle share with it", row.IdlePct, tt.broken)
				}
			}
			if tt.broken != "consumers" && len(row.TopConsumers) != 3 {
				t.Errorf("top_consumers = %+v, want the file's three busiest: a broken %s took them with it",
					row.TopConsumers, tt.broken)
			}
			if tt.broken != "tax" {
				if row.ExecTaxMS == nil || *row.ExecTaxMS != 12.5 {
					t.Errorf("exec_tax_ms = %v, want the file's 12.5: a broken %s took the tax with it", row.ExecTaxMS, tt.broken)
				}
			}
			if row.HostAgeS == nil || *row.HostAgeS != 0 {
				t.Errorf("host_age_s = %v, want 0: the file was read, whatever one field of it could not answer", row.HostAgeS)
			}
		})
	}
}

// TestBurstSamplerOutsideContainerModeUsesTheHostsCommands is the bead's third
// acceptance test: the probe file is the container's source, so a daemon on the
// host samples top exactly as before even with a fresh file in the town.
func TestBurstSamplerOutsideContainerModeUsesTheHostsCommands(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	d := burstTestDaemon(t, town, clk, io.Discard)
	writeBurstProbe(t, town, burstProbeFixture(testEpoch))
	d.burstSeams = burstSamplerSeams{
		env: burstTestEnv(clk),
	}
	burstSampleOnce(t, d)

	rows := readBurstRows(t, town)
	if len(rows) != 1 {
		t.Fatalf("the burst log holds %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.IdlePct == nil || *row.IdlePct != 0.29 {
		t.Errorf("idle_pct = %v, want top's 0.29: the file answers the container and not this daemon", row.IdlePct)
	}
	if row.ExecTaxMS != nil {
		t.Errorf("exec_tax_ms = %v, want null: this host wrote no health file", row.ExecTaxMS)
	}
	if row.HostErrors != nil {
		t.Errorf("host_errors = %v, want none: no file was read", row.HostErrors)
	}
	if row.HostAgeS != nil {
		t.Errorf("host_age_s = %v, want none: top read the host at the sample's own instant", row.HostAgeS)
	}
}

// TestContainerModeFromEnv pins the one read that decides container mode: the
// value the rendered compose file sets answers yes, and every other value of
// the variable, the empty environment included, is the host.
func TestContainerModeFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		getenv func(string) string
		want   bool
	}{
		{name: "the compose file's container", getenv: burstContainerGetenv, want: true},
		{name: "launchd", getenv: func(string) string { return config.SupervisorLaunchd }, want: false},
		{name: "an empty environment", getenv: func(string) string { return "" }, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containerModeFromEnv(tt.getenv); got != tt.want {
				t.Errorf("containerModeFromEnv = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBurstSamplerLogsAStaleProbeFileOnce pins the log's dedup over a file that
// stays stale: the age it reports grows with every sample, and the failure is
// still one line rather than one per tick.
func TestBurstSamplerLogsAStaleProbeFileOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	sink := newCensusLogSink()
	d := burstInContainer(burstTestDaemon(t, town, clk, sink))
	writeBurstProbe(t, town, burstProbeFixture(testEpoch.Add(-10*time.Minute)))

	samples := 0
	third := make(chan struct{})
	d.burstSeams = burstSamplerSeams{
		env: burstTestTownEnv(clk),
		run: func(ctx context.Context, env metrics.Env) (metrics.BurstRow, string, error) {
			samples++
			if samples == 3 {
				close(third)
			}
			return metrics.RunBurst(ctx, env)
		},
	}
	// The sampler's one goroutine keeps the count, so neither it nor the
	// signal needs a lock.
	stop := d.startBurstSamplerIfEnabled()
	driveClock(t, clk, metrics.BurstFastInterval, third)
	stop()

	if samples != 3 {
		t.Fatalf("the sampler took %d samples, want 3", samples)
	}
	if got := sink.countContaining("burst_sampler: "); got != 1 {
		t.Errorf("the sampler logged the stale file %d times over three samples, want once: %q",
			got, burstTestLogLines(sink))
	}
}

// TestBurstSamplerLogsAProbeFailureAgainAfterRecovery pins the dedup's other
// half: a good sample forgets the failure's class, so a file that goes stale
// again is logged again rather than swallowed as the same failure. One stamp
// serves all three samples and the sample's own clock decides its age
// (testEpoch: ten minutes old, refused; eight minutes back: two minutes old,
// accepted; testEpoch again: refused), which changes freshness without the test
// rewriting the file from the sampler's goroutine.
func TestBurstSamplerLogsAProbeFailureAgainAfterRecovery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	sink := newCensusLogSink()
	d := burstInContainer(burstTestDaemon(t, town, clk, sink))
	writeBurstProbe(t, town, burstProbeFixture(testEpoch.Add(-10*time.Minute)))

	nows := []time.Time{
		testEpoch,
		testEpoch.Add(-8 * time.Minute),
		testEpoch,
	}
	samples := 0
	third := make(chan struct{})
	d.burstSeams = burstSamplerSeams{
		env: func(town string) metrics.Env {
			env := burstTestTownEnv(clk)(town)
			if samples < len(nows) {
				at := nows[samples]
				env.Now = func() time.Time { return at }
			}
			samples++
			return env
		},
		run: func(ctx context.Context, env metrics.Env) (metrics.BurstRow, string, error) {
			row, path, err := metrics.RunBurst(ctx, env)
			if samples == len(nows) {
				close(third)
			}
			return row, path, err
		},
	}
	// The sampler's one goroutine keeps the count, so neither it nor the
	// signal needs a lock.
	stop := d.startBurstSamplerIfEnabled()
	driveClock(t, clk, metrics.BurstFastInterval, third)
	stop()

	if got := sink.countContaining("burst_sampler: "); got != 2 {
		t.Fatalf("the sampler logged the file %d times, want 2: one line per stale stretch and none while it was fresh: %q",
			got, burstTestLogLines(sink))
	}
	rows := readBurstRows(t, town)
	if len(rows) != 3 {
		t.Fatalf("the burst log holds %d rows, want 3: a refused file still leaves the town's own reading", len(rows))
	}
	if rows[0].HostAgeS != nil || rows[2].HostAgeS != nil {
		t.Errorf("host_age_s = %v and %v, want null on both rows a refused file made", rows[0].HostAgeS, rows[2].HostAgeS)
	}
	if rows[1].IdlePct == nil || *rows[1].IdlePct != 42.5 {
		t.Errorf("idle_pct = %v on the sample after recovery, want the file's 42.5", rows[1].IdlePct)
	}
	if rows[1].HostAgeS == nil || *rows[1].HostAgeS != 120 {
		t.Errorf("host_age_s = %v on the sample after recovery, want 120: the file was two minutes old there", rows[1].HostAgeS)
	}
}

// A sample's Env carries the landing workers' published counts, and reads the
// view live rather than a copy: a count published after the Env was built still
// answers the sample, which is what lets a pass' own list reach the queue
// column (gt-hw6ih).
func TestBurstSamplerEnvReadsTheLandingQueueView(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	d := burstTestDaemon(t, t.TempDir(), clock, io.Discard)
	d.landingQueueView().Publish("gastown", 3)
	env, err := d.burstSampler().sampleEnv()
	if err != nil {
		t.Fatalf("sampleEnv: %v", err)
	}
	if env.LandingQueue == nil {
		t.Fatal("the sample's Env carries no landing queue; the burst row would list every rig's board")
	}
	if got := env.LandingQueue(clock.Now()); got["gastown"] != 3 {
		t.Fatalf("the sample read %v; want the published gastown 3", got)
	}
	d.landingQueueView().Publish("gastown", 5)
	if got := env.LandingQueue(clock.Now()); got["gastown"] != 5 {
		t.Fatalf("the sample read %v after a second publish; want gastown 5", got)
	}
}
