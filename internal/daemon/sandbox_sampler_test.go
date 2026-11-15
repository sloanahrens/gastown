package daemon

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// fakeSeatSource is one tick's seat source outcome.
type fakeSeatSource struct {
	tick resourcesampler.SeatTick
	err  error
}

func (f *fakeSeatSource) Tick(context.Context) (resourcesampler.SeatTick, error) {
	return f.tick, f.err
}

// fixedClock is the tick's injected clock: every row of a test lands in one
// day file.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// sandboxSeatRow is a row the seat source could have returned, in the shape
// the design's row table gives a seat.
func sandboxSeatRow(name string) resourcesampler.Row {
	rig, polecat, cluster := "gastown", "toast", "gt-sbx"
	cpu := 12.5
	return resourcesampler.Row{
		V:         resourcesampler.SchemaVersion,
		TS:        "2026-11-14T09:30:05Z",
		IntervalS: 60,
		Source:    "sandbox",
		Class:     "sandbox",
		Component: "seat",
		Name:      name,
		Rig:       &rig,
		Polecat:   &polecat,
		CPUPct:    &cpu,
		MemBytes:  1 << 30,
		Workspace: "acme",
		Cluster:   &cluster,
	}
}

// sandboxSamplerDayRows decodes the rows the tick appended to the day file the
// production writer names. The layout is resourcesampler.AppendRow's alone: the
// helper has it file a probe row into a fresh directory and reads the one file
// it leaves there, so a change to the layout moves this read with it. A metrics
// directory holding no file is a tick that wrote no row, which reads as no rows.
func sandboxSamplerDayRows(t *testing.T, metricsDir string, now time.Time) []resourcesampler.Row {
	t.Helper()
	want := sandboxSamplerDayFile(t, now)
	files, err := sandboxSamplerFiles(metricsDir)
	if err != nil {
		t.Fatalf("list the metrics directory: %v", err)
	}
	if len(files) == 0 {
		return nil
	}
	if len(files) != 1 || files[0] != want {
		t.Fatalf("the tick wrote %v, want only the day file %s", files, want)
	}
	data, err := os.ReadFile(filepath.Join(metricsDir, want))
	if err != nil {
		t.Fatalf("read the day file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	rows := make([]resourcesampler.Row, 0, len(lines))
	for _, line := range lines {
		var row resourcesampler.Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode the day file's line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// sandboxSamplerDayFile is where the production writer files the day of now,
// relative to the metrics directory, as the writer itself names it.
func sandboxSamplerDayFile(t *testing.T, now time.Time) string {
	t.Helper()
	probe := t.TempDir()
	if err := resourcesampler.AppendRow(probe, sandboxSeatRow("probe"), now); err != nil {
		t.Fatalf("file a probe row through the production writer: %v", err)
	}
	files, err := sandboxSamplerFiles(probe)
	if err != nil {
		t.Fatalf("list the probe directory: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("the production writer left %v under the probe directory, want one file", files)
	}
	return files[0]
}

// sandboxSamplerFiles lists every file under dir, relative to it, in walk order.
// A directory that does not exist holds no file.
func sandboxSamplerFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// TestSandboxSamplerDefaultsToTheRowWriter builds the sampler with no writer
// seam, the way the patrol will, and checks the rows reach the day file
// resourcesampler.AppendRow keeps (gt-6bmp5y).
func TestSandboxSamplerDefaultsToTheRowWriter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	metricsDir := t.TempDir()
	row := sandboxSeatRow("mango-toast")
	sampler := newSandboxSampler(metricsDir,
		&fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{row}}},
		nil, fixedClock{now: now}, nil, nil, 0)

	sampler.Tick(context.Background())

	if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, []resourcesampler.Row{row}) {
		t.Errorf("the day file holds\n%+v\nwant\n%+v", got, []resourcesampler.Row{row})
	}
}

// TestSandboxSamplerTick is the bead's acceptance, and the refusal branches
// beside it: one tick writes the rows its source returns, and a tick whose
// source fails writes none and logs once (gt-6e3crf).
func TestSandboxSamplerTick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	toast, nux := sandboxSeatRow("mango-toast"), sandboxSeatRow("mango-nux")
	for _, test := range []struct {
		name string
		// ctx is the tick's caller context; nil is context.Background.
		ctx context.Context
		// source is the fake the tick reads; nil stands for a sampler built
		// without one.
		source sandboxSeatSource
		// write replaces the row writer; nil writes through
		// resourcesampler.AppendRow, the production writer.
		write sandboxRowWriter
		// wantRows is what the day file must hold, in order.
		wantRows []resourcesampler.Row
		// wantWrites counts the writer's calls, so a failed write is told from
		// a write that never happened.
		wantWrites int
		// wantLogs counts the lines the tick logs.
		wantLogs int
		// wantLogHas, when set, must appear in every logged line.
		wantLogHas string
	}{
		{
			name:       "writes the rows the source returns",
			source:     &fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{toast, nux}}},
			wantRows:   []resourcesampler.Row{toast, nux},
			wantWrites: 2,
		},
		{
			// The rows are there so the tick's early return is what keeps them
			// out of the file: a source that failed read the cluster
			// incompletely, so no row it returned is a sample.
			name: "a failed source writes nothing and logs once",
			source: &fakeSeatSource{
				tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{toast, nux}},
				err:  errors.New("the sandbox records cannot be listed"),
			},
			wantLogs:   1,
			wantLogHas: "the sandbox records cannot be listed",
		},
		{
			name: "a skipped seat is logged and the rest write",
			source: &fakeSeatSource{tick: resourcesampler.SeatTick{
				Rows:    []resourcesampler.Row{toast},
				Skipped: []resourcesampler.SeatSkip{{Seat: "mango-nux", Err: errors.New("the exec failed")}},
			}},
			wantRows:   []resourcesampler.Row{toast},
			wantWrites: 1,
			wantLogs:   1,
			wantLogHas: "mango-nux",
		},
		{
			name:   "a failed write logs once and writes no later row",
			source: &fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{toast, nux}}},
			write: func(string, resourcesampler.Row, time.Time) error {
				return errors.New("the day file is read-only")
			},
			wantWrites: 1,
			wantLogs:   1,
			wantLogHas: "the day file is read-only",
		},
		{
			// The fake answers anyway, so the tick refusing the rows is what
			// keeps them out of the file: a caller that gave up on the read is
			// not a cluster sample. The line names the caller, not the
			// sampler's bound, which is what a read the caller ended is.
			name:       "a caller that ended the read writes nothing and logs once",
			ctx:        canceledContext(),
			source:     &fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{toast}}},
			wantLogs:   1,
			wantLogHas: "the caller ended the read",
		},
		{
			name:       "no source logs and writes nothing",
			wantLogs:   1,
			wantLogHas: "no seat source",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			metricsDir := t.TempDir()
			writes := 0
			var logs []string
			write := test.write
			if write == nil {
				write = resourcesampler.AppendRow
			}
			sampler := newSandboxSampler(metricsDir, test.source, nil, fixedClock{now: now},
				func(dir string, row resourcesampler.Row, at time.Time) error {
					writes++
					return write(dir, row, at)
				},
				func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
				0,
			)
			ctx := test.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			sampler.Tick(ctx)

			if writes != test.wantWrites {
				t.Errorf("the tick wrote %d rows, want %d", writes, test.wantWrites)
			}
			if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, test.wantRows) {
				t.Errorf("the day file holds\n%+v\nwant\n%+v", got, test.wantRows)
			}
			if len(logs) != test.wantLogs {
				t.Errorf("the tick logged %d lines, want %d: %q", len(logs), test.wantLogs, logs)
			}
			if test.wantLogHas != "" {
				for _, line := range logs {
					if !strings.Contains(line, test.wantLogHas) {
						t.Errorf("log line %q does not name %q", line, test.wantLogHas)
					}
				}
			}
		})
	}
}

// TestSandboxSamplerNilLoggerIsNotSilent is the nil-logger finding's acceptance
// (cea0ceaa8e9b): a tick built with no logf still records a failed read, on the
// sampler's own logger over stderr, so an unset seam cannot turn a failure into
// a cluster with no seat. The test buffers that logger's output rather than
// replacing the process's standard logger.
func TestSandboxSamplerNilLoggerIsNotSilent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	metricsDir := t.TempDir()
	sampler := newSandboxSampler(metricsDir,
		&fakeSeatSource{err: errors.New("the sandbox records cannot be listed")},
		nil, fixedClock{now: now}, nil, nil, 0)
	var buf bytes.Buffer
	sampler.errLog = log.New(&buf, "", 0)

	sampler.Tick(context.Background())

	if !strings.Contains(buf.String(), "the sandbox records cannot be listed") {
		t.Errorf("a tick with no logger wrote %q to the standard logger, want it to name the source's failure", buf.String())
	}
	if got := sandboxSamplerDayRows(t, metricsDir, now); got != nil {
		t.Errorf("the failed tick wrote\n%+v\nwant no rows", got)
	}
}

// canceledContext is a caller context that ended before the tick read the
// seats.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// hangingFirstTickSource is a seat source whose first tick hangs on its later
// seats until the read's context ends, then reports them the way the production
// source reports a tick its bound cut: the seats read before it as rows, the
// rest skipped, and a nil Tick error. Every later tick answers with the row,
// the recovered cluster.
type hangingFirstTickSource struct {
	row   resourcesampler.Row
	ticks int
}

func (h *hangingFirstTickSource) Tick(ctx context.Context) (resourcesampler.SeatTick, error) {
	h.ticks++
	if h.ticks > 1 {
		return resourcesampler.SeatTick{Rows: []resourcesampler.Row{h.row}}, nil
	}
	if _, bounded := ctx.Deadline(); !bounded {
		// A tick with no bound answers at once, so a sampler that never bounded
		// the read fails the test instead of hanging it.
		return resourcesampler.SeatTick{Rows: []resourcesampler.Row{h.row}}, nil
	}
	<-ctx.Done()
	return resourcesampler.SeatTick{
		Rows:    []resourcesampler.Row{h.row},
		Skipped: []resourcesampler.SeatSkip{{Seat: "mango-nux", Err: ctx.Err()}},
	}, nil
}

// TestSandboxSamplerTickEndsAtItsBound is the bead's acceptance (gt-psegop): the
// tick ends at its own bound, writes nothing it read before the bound, and the
// next tick reads the recovered source and writes its row.
func TestSandboxSamplerTickEndsAtItsBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	row := sandboxSeatRow("mango-toast")
	metricsDir := t.TempDir()
	var logs []string
	timeout := 10 * time.Millisecond
	sampler := newSandboxSampler(metricsDir, &hangingFirstTickSource{row: row}, nil, fixedClock{now: now}, nil,
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		timeout,
	)

	sampler.Tick(context.Background())

	if got := sandboxSamplerDayRows(t, metricsDir, now); got != nil {
		t.Errorf("the bounded tick wrote\n%+v\nwant no rows", got)
	}
	if len(logs) != 1 {
		t.Fatalf("the bounded tick logged %d lines, want 1 naming the bound: %q", len(logs), logs)
	}
	for _, want := range []string{"bound", timeout.String()} {
		if !strings.Contains(logs[0], want) {
			t.Errorf("the log line %q does not name %q", logs[0], want)
		}
	}

	sampler.Tick(context.Background())

	if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, []resourcesampler.Row{row}) {
		t.Errorf("the next tick's day file holds\n%+v\nwant\n%+v", got, []resourcesampler.Row{row})
	}
}

// deadlineRecordingSource records the deadline of the context it is read with,
// and answers no rows, so a tick that reads it writes nothing either way.
type deadlineRecordingSource struct {
	bounded   bool
	remaining time.Duration
}

func (d *deadlineRecordingSource) Tick(ctx context.Context) (resourcesampler.SeatTick, error) {
	deadline, bounded := ctx.Deadline()
	d.bounded, d.remaining = bounded, time.Until(deadline)
	return resourcesampler.SeatTick{}, nil
}

// TestSandboxSamplerDefaultTimeout pins the zero timeout's default at the seam
// the source sees: the read is bounded, and the bound is the default (gt-psegop).
func TestSandboxSamplerDefaultTimeout(t *testing.T) {
	t.Parallel()
	source := &deadlineRecordingSource{}
	sampler := newSandboxSampler(t.TempDir(), source, nil, nil, nil, nil, 0)

	sampler.Tick(context.Background())

	if !source.bounded {
		t.Fatalf("the seat read had no deadline, want the %s default", defaultSandboxSamplerTimeout)
	}
	if source.remaining > defaultSandboxSamplerTimeout || source.remaining < defaultSandboxSamplerTimeout-time.Second {
		t.Errorf("the seat read was bounded %s away, want the %s default", source.remaining, defaultSandboxSamplerTimeout)
	}
}

// TestSandboxSamplerDefaultsOff is the patrol's registration test (gt-7d7jvn):
// a town that does not name the key samples no seat, so an upgrade never
// starts one gateway exec per live seat on its own.
func TestSandboxSamplerDefaultsOff(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "sandbox_sampler") {
		t.Error("sandbox_sampler runs with no daemon config, want it off")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "sandbox_sampler") {
		t.Error("sandbox_sampler runs with its key absent, want it off")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SandboxSampler: &PatrolConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "sandbox_sampler") {
		t.Error("an explicit enabled=false left sandbox_sampler on")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SandboxSampler: &PatrolConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "sandbox_sampler") {
		t.Error("an explicit enabled=true did not turn sandbox_sampler on")
	}
}

// TestSandboxSamplerInterval pins the patrol's own cadence: 60 seconds unless
// configured, and a configured value cannot make it pay for a seat read more
// often than the floor (gt-7d7jvn).
func TestSandboxSamplerInterval(t *testing.T) {
	t.Parallel()
	if got := sandboxSamplerInterval(nil); got != 60*time.Second {
		t.Errorf("the default interval = %v, want 60s", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SandboxSampler: &PatrolConfig{Enabled: true, Interval: "90s"}}}
	if got := sandboxSamplerInterval(cfg); got != 90*time.Second {
		t.Errorf("the configured interval = %v, want 90s", got)
	}
	cfg.Patrols.SandboxSampler.Interval = "soon"
	if got := sandboxSamplerInterval(cfg); got != 60*time.Second {
		t.Errorf("an interval that does not parse = %v, want the 60s default", got)
	}
	cfg.Patrols.SandboxSampler.Interval = "1s"
	if got := sandboxSamplerInterval(cfg); got != minSandboxSamplerInterval {
		t.Errorf("a 1s interval = %v, want the %v floor", got, minSandboxSamplerInterval)
	}
}

// countingSeatSource counts the patrol's ticks and answers with one recorded
// outcome.
type countingSeatSource struct {
	ticks int
	tick  resourcesampler.SeatTick
	err   error
}

func (c *countingSeatSource) Tick(context.Context) (resourcesampler.SeatTick, error) {
	c.ticks++
	return c.tick, c.err
}

// seatSourceSpec is what the daemon handed the seat-source seam when it built
// the patrol's sampler.
type seatSourceSpec struct {
	townRoot string
	clock    resourcesampler.Clock
	timeout  time.Duration
}

// newSandboxPatrolDaemon is a daemon whose seat source is a fake the test
// counts, and whose clock a test moves by hand.
func newSandboxPatrolDaemon(t *testing.T, town string, clock clockwork.Clock, config *DaemonPatrolConfig) (*Daemon, *countingSeatSource, *seatSourceSpec) {
	t.Helper()
	source := &countingSeatSource{}
	spec := &seatSourceSpec{}
	d := &Daemon{
		config:       &Config{TownRoot: town},
		patrolConfig: config,
		logger:       log.New(io.Discard, "", 0),
		clock:        clock,
	}
	d.newSandboxSeatSource = func(townRoot string, clock resourcesampler.Clock, timeout time.Duration) sandboxSeatSource {
		spec.townRoot, spec.clock, spec.timeout = townRoot, clock, timeout
		return source
	}
	return d, source, spec
}

// TestTriggerSandboxSamplerTicksThroughTheLiveSeams is the patrol's wiring
// test (gt-7d7jvn): one trigger reads the seat source the seam builds for the
// town at the source's per-seat bound, and the row lands in the host rows' day
// file.
func TestTriggerSandboxSamplerTicksThroughTheLiveSeams(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	config := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SandboxSampler: &PatrolConfig{Enabled: true}}}
	d, source, spec := newSandboxPatrolDaemon(t, town, clock, config)
	source.tick = resourcesampler.SeatTick{Rows: []resourcesampler.Row{sandboxSeatRow("mango-aaa")}}

	d.triggerSandboxSampler()

	if source.ticks != 1 {
		t.Fatalf("the trigger read the seats %d times, want 1", source.ticks)
	}
	if spec.townRoot != town {
		t.Errorf("the seat source read %q, want the town root %q", spec.townRoot, town)
	}
	if spec.clock != clock {
		t.Errorf("the seat source read clock %v, want the daemon's %v", spec.clock, clock)
	}
	if spec.timeout != resourcesampler.DefaultSeatTimeout {
		t.Errorf("the seat source read timeout = %v, want the %v per-seat bound", spec.timeout, resourcesampler.DefaultSeatTimeout)
	}
	metricsDir := filepath.Join(town, ".runtime", "metrics")
	if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, []resourcesampler.Row{sandboxSeatRow("mango-aaa")}) {
		t.Errorf("the day file holds\n%+v\nwant\n%+v", got, []resourcesampler.Row{sandboxSeatRow("mango-aaa")})
	}
}

// TestTriggerSandboxSamplerOffTicksNothing: a switched-off patrol builds no
// source and reads no seat, whether the key is off or the daemon holds no
// config at all.
func TestTriggerSandboxSamplerOffTicksNothing(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	config := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SandboxSampler: &PatrolConfig{Enabled: false}}}
	d, source, spec := newSandboxPatrolDaemon(t, t.TempDir(), clock, config)

	d.triggerSandboxSampler()

	if source.ticks != 0 {
		t.Errorf("a disabled patrol read the seats %d times, want none", source.ticks)
	}
	if spec.townRoot != "" {
		t.Error("a disabled patrol built a seat source")
	}
	if d.sandboxSamplerPatrol != nil {
		t.Error("a disabled patrol built a sampler")
	}

	noConfig := &Daemon{config: nil, logger: log.New(io.Discard, "", 0), clock: clock}
	noConfig.newSandboxSeatSource = func(string, resourcesampler.Clock, time.Duration) sandboxSeatSource {
		t.Error("a daemon with no config built a seat source")
		return source
	}
	noConfig.triggerSandboxSampler()
}

// TestSandboxSamplerBuildsTheGatewaySourceByDefault: with no seam set, the
// patrol's sampler reads the source through the gateway's own openshell CLI
// and writes beside the host rows (gt-7d7jvn).
func TestSandboxSamplerBuildsTheGatewaySourceByDefault(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}

	sampler := d.sandboxSampler()

	if _, ok := sampler.source.(*resourcesampler.SeatSource); !ok {
		t.Fatalf("the sampler's source is %T, want the gateway seat source", sampler.source)
	}
	if want := filepath.Join(d.config.TownRoot, ".runtime", "metrics"); sampler.metricsDir != want {
		t.Errorf("the sampler writes %q, want the host rows' directory %q", sampler.metricsDir, want)
	}
}

// TestStartSandboxSamplerTicker: the patrol gets its own ticker when it is on,
// and none when the town does not name it.
func TestStartSandboxSamplerTicker(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)),
	}

	if ticker := d.startSandboxSamplerTicker(); ticker != nil {
		ticker.Stop()
		t.Fatal("a town that does not name sandbox_sampler started its ticker")
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{SandboxSampler: &PatrolConfig{Enabled: true, Interval: "90s"}}}
	ticker := d.startSandboxSamplerTicker()
	if ticker == nil {
		t.Fatal("a town that names sandbox_sampler started no ticker")
	}
	ticker.Stop()
}

// fakeVMSource is one tick's VM source outcome, and records the read so a test
// can place it in the writer's event order.
type fakeVMSource struct {
	tick   resourcesampler.VMTick
	err    error
	ticks  int
	onTick func(ctx context.Context)
}

func (f *fakeVMSource) Tick(ctx context.Context) (resourcesampler.VMTick, error) {
	f.ticks++
	if f.onTick != nil {
		f.onTick(ctx)
	}
	return f.tick, f.err
}

// hangingVMSource is a VM read that answers only when its bound ends it, the
// way a wedged gateway call does.
type hangingVMSource struct {
	bounded bool
}

func (h *hangingVMSource) Tick(ctx context.Context) (resourcesampler.VMTick, error) {
	_, h.bounded = ctx.Deadline()
	<-ctx.Done()
	return resourcesampler.VMTick{}, ctx.Err()
}

// sandboxVMRow is a row the VM source could have returned, in the shape the
// design's row table gives the VM.
func sandboxVMRow() resourcesampler.Row {
	cluster, load := "gt-sbx", 0.52
	return resourcesampler.Row{
		V:         resourcesampler.SchemaVersion,
		TS:        "2026-11-14T09:30:05Z",
		Source:    "sandbox",
		Class:     "sandbox",
		Component: "vm",
		Name:      "gt-sbx",
		MemBytes:  1 << 30,
		Workspace: "acme",
		Cluster:   &cluster,
		Load1:     &load,
	}
}

// TestSandboxSamplerWritesTheVMRow is the bead's acceptance: the VM row goes
// through the same writer as the seat rows, in the same day file, and the VM
// read starts only after the seat rows are written (gt-2qe2m6).
func TestSandboxSamplerWritesTheVMRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	metricsDir := t.TempDir()
	seat, vmRow := sandboxSeatRow("mango-toast"), sandboxVMRow()
	var events []string
	vm := &fakeVMSource{tick: resourcesampler.VMTick{Row: &vmRow}, onTick: func(context.Context) {
		events = append(events, "vm read")
	}}
	sampler := newSandboxSampler(metricsDir,
		&fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{seat}}},
		vm, fixedClock{now: now},
		func(dir string, row resourcesampler.Row, at time.Time) error {
			events = append(events, "write "+row.Component)
			return resourcesampler.AppendRow(dir, row, at)
		}, nil, 0)

	sampler.Tick(context.Background())

	want := []resourcesampler.Row{seat, vmRow}
	if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, want) {
		t.Errorf("the day file holds\n%+v\nwant the seat row then the VM row\n%+v", got, want)
	}
	if wantEvents := []string{"write seat", "vm read", "write vm"}; !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("the tick did %v, want %v: the VM read starts after the seat rows write", events, wantEvents)
	}
	if vm.ticks != 1 {
		t.Errorf("the tick read the VM %d times, want 1", vm.ticks)
	}
}

// TestSandboxSamplerVMWritesNoRowOnFailure is the bead's acceptance: a VM read
// that fails, or that finds no seat to read, writes no VM row and never zeros,
// and the seat rows the tick already wrote stay (gt-2qe2m6).
func TestSandboxSamplerVMWritesNoRowOnFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	seat := sandboxSeatRow("mango-toast")
	for _, test := range []struct {
		name string
		vm   sandboxVMSource
		// wantLogs counts the lines the tick logs.
		wantLogs int
		// wantLogHas must each appear in the logged lines.
		wantLogHas []string
	}{
		{
			name:       "a vm read that fails",
			vm:         &fakeVMSource{err: errors.New("the workspace map cannot be read")},
			wantLogs:   1,
			wantLogHas: []string{"vm read", "the workspace map cannot be read"},
		},
		{
			name: "every live seat skipped",
			vm: &fakeVMSource{tick: resourcesampler.VMTick{Skipped: []resourcesampler.SeatSkip{
				{Seat: "mango-aaa", Err: errors.New("the read was cut short")},
				{Seat: "mango-bbb", Err: errors.New("the read was cut short")},
			}}},
			wantLogs:   2,
			wantLogHas: []string{"vm read", "mango-aaa", "mango-bbb"},
		},
		{
			name:     "no live seat",
			vm:       &fakeVMSource{},
			wantLogs: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			metricsDir := t.TempDir()
			var logs []string
			sampler := newSandboxSampler(metricsDir,
				&fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{seat}}},
				test.vm, fixedClock{now: now}, nil,
				func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
				0)

			sampler.Tick(context.Background())

			if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, []resourcesampler.Row{seat}) {
				t.Errorf("the day file holds\n%+v\nwant only the seat row: a VM read that wrote no row must not cost it", got)
			}
			if len(logs) != test.wantLogs {
				t.Errorf("the tick logged %d lines, want %d: %q", len(logs), test.wantLogs, logs)
			}
			for _, want := range test.wantLogHas {
				if !strings.Contains(strings.Join(logs, "\n"), want) {
					t.Errorf("the logged lines %q do not name %q", logs, want)
				}
			}
		})
	}
}

// TestSandboxSamplerVMSlowStillWritesTheSeatRows is the attempt-2 finding's
// acceptance: the VM read has a bound of its own, so a read that hangs costs
// the VM row and logs one line, and the seat rows the tick wrote stay
// (gt-2qe2m6).
func TestSandboxSamplerVMSlowStillWritesTheSeatRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	metricsDir := t.TempDir()
	seat := sandboxSeatRow("mango-toast")
	vm := &hangingVMSource{}
	var logs []string
	sampler := newSandboxSampler(metricsDir,
		&fakeSeatSource{tick: resourcesampler.SeatTick{Rows: []resourcesampler.Row{seat}}},
		vm, fixedClock{now: now}, nil,
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		0)
	sampler.vmTimeout = 10 * time.Millisecond

	sampler.Tick(context.Background())

	if got := sandboxSamplerDayRows(t, metricsDir, now); !reflect.DeepEqual(got, []resourcesampler.Row{seat}) {
		t.Errorf("the day file holds\n%+v\nwant the seat row only: a VM read that hangs must not cost it", got)
	}
	if !vm.bounded {
		t.Error("the VM read was called with no deadline")
	}
	if len(logs) != 1 {
		t.Fatalf("the tick logged %d lines, want 1 naming the VM read's bound: %q", len(logs), logs)
	}
	for _, want := range []string{"vm read", "bound", sampler.vmTimeout.String()} {
		if !strings.Contains(logs[0], want) {
			t.Errorf("the log line %q does not name %q", logs[0], want)
		}
	}
}

// TestSandboxSamplerRegistersTheVMSource is the bead's acceptance: the patrol's
// sampler carries the gateway VM source beside the seat source, bounded by its
// own default (gt-2qe2m6).
func TestSandboxSamplerRegistersTheVMSource(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}

	sampler := d.sandboxSampler()

	if _, ok := sampler.vm.(*resourcesampler.VMSource); !ok {
		t.Fatalf("the sampler's VM source is %T, want the gateway VM source", sampler.vm)
	}
	if sampler.vmTimeout != defaultVMReadTimeout {
		t.Errorf("the VM read is bounded %v, want its own %v default", sampler.vmTimeout, defaultVMReadTimeout)
	}
	if sampler.metricsDir != filepath.Join(d.config.TownRoot, ".runtime", "metrics") {
		t.Errorf("the VM row writes %q, want the seat rows' directory %q", sampler.metricsDir, filepath.Join(d.config.TownRoot, ".runtime", "metrics"))
	}
}

// TestVMReadBudgetLeavesRoomForTheNextSeat pins the production shape the
// next-seat fallback depends on: the VM read's whole bound covers more than one
// seat's own bound, so a first seat that hangs for all of its bound does not use
// up the budget before the fallback tries the next seat (gt-2qe2m6).
func TestVMReadBudgetLeavesRoomForTheNextSeat(t *testing.T) {
	t.Parallel()
	if vmSeatAttempts < 2 {
		t.Fatalf("vmSeatAttempts = %d, want at least 2 so a fallback can run", vmSeatAttempts)
	}
	if defaultVMReadTimeout < 2*resourcesampler.DefaultSeatTimeout {
		t.Errorf("the VM read budget %v is under two per-seat bounds (%v): a hung first seat would exhaust it",
			defaultVMReadTimeout, resourcesampler.DefaultSeatTimeout)
	}
	if want := vmSeatAttempts * resourcesampler.DefaultSeatTimeout; defaultVMReadTimeout != want {
		t.Errorf("the VM read budget is %v, want %d per-seat bounds = %v", defaultVMReadTimeout, vmSeatAttempts, want)
	}
}
