package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
// code did not take would move the row and fail the assertion.
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
			if samples > 2 {
				return metrics.BurstRow{}, metrics.BurstPath(env.Town), errors.New("the sampler ran a third sample")
			}
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
// sample leaves a zero row, and a host whose top failed is one to look at again
// soon. The next sample waits its own row's cadence.
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

// TestBurstSamplerLogsARepeatedFailureOnce pins the failure log's dedup: a host
// the sampler cannot read retries every 15 s, so the same line is one line
// however many ticks it repeats, and a failure that returns after a recovery is
// logged again.
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

// TestBurstSamplerOffWithTheMetricsSampler pins the gate: a town that switched
// its metrics sampler off takes no sample at all, and a town that never
// mentioned the switch samples.
func TestBurstSamplerOffWithTheMetricsSampler(t *testing.T) {
	t.Parallel()
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{ResourceSampler: &PatrolConfig{Enabled: false}}}
	tests := []struct {
		name    string
		patrols *DaemonPatrolConfig
		want    bool
	}{
		{name: "an explicit enabled=false samples nothing", patrols: off, want: false},
		{name: "the absent switch samples", patrols: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			d := burstTestDaemon(t, town, clk, io.Discard)
			d.patrolConfig = tt.patrols

			var calls atomic.Int32
			sampled := make(chan struct{}, 1)
			d.burstSeams = burstSamplerSeams{
				env: func(string) metrics.Env { return metrics.Env{} },
				run: func(context.Context, metrics.Env) (metrics.BurstRow, string, error) {
					if calls.Add(1) == 1 {
						sampled <- struct{}{}
					}
					return metrics.BurstRow{IntervalS: metrics.BurstSlowInterval.Seconds()}, metrics.BurstPath(town), nil
				},
			}
			stop := d.startBurstSamplerIfEnabled()
			if tt.want {
				waitForSignal(t, sampled, "a town that never switched its metrics sampler off took no sample")
			}
			stop()
			// stop joins the loop it started, so a sample the gate let through
			// has run by the time it returns.
			if got := calls.Load() > 0; got != tt.want {
				t.Errorf("the sampler sampled = %v, want %v", got, tt.want)
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
