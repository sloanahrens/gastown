package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// sandboxSamplerDayRows decodes the day file the tick appends to. A tick that
// wrote nothing leaves no file, which reads as no rows.
func sandboxSamplerDayRows(t *testing.T, metricsDir string, now time.Time) []resourcesampler.Row {
	t.Helper()
	path := filepath.Join(metricsDir, "resources", now.Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
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
		fixedClock{now: now}, nil, nil, 0)

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
			sampler := newSandboxSampler(metricsDir, test.source, fixedClock{now: now},
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
	sampler := newSandboxSampler(metricsDir, &hangingFirstTickSource{row: row}, fixedClock{now: now}, nil,
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
	sampler := newSandboxSampler(t.TempDir(), source, nil, nil, nil, 0)

	sampler.Tick(context.Background())

	if !source.bounded {
		t.Fatalf("the seat read had no deadline, want the %s default", defaultSandboxSamplerTimeout)
	}
	if source.remaining > defaultSandboxSamplerTimeout || source.remaining < defaultSandboxSamplerTimeout-time.Second {
		t.Errorf("the seat read was bounded %s away, want the %s default", source.remaining, defaultSandboxSamplerTimeout)
	}
}
