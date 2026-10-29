package cmd

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// fakeLatencyClock is the probe's clock. A fake conn advances it by each
// step's cost, so every sample is exactly that cost and no real time passes.
type fakeLatencyClock struct {
	t time.Time
}

func (c *fakeLatencyClock) now() time.Time { return c.t }

// fakeLatencyConn records the statements it runs and advances the clock by
// costs[i] for the i-th run. failAt, when it is a valid index, makes that run
// fail with errRun.
type fakeLatencyConn struct {
	clock  *fakeLatencyClock
	costs  []time.Duration
	failAt int
	ran    []string
	closed bool
}

var errLatencyRun = errors.New("connection reset")

func (c *fakeLatencyConn) Run(ctx context.Context, q beadsql.Query) error {
	i := len(c.ran)
	c.ran = append(c.ran, q.String())
	if i == c.failAt {
		return errLatencyRun
	}
	c.clock.t = c.clock.t.Add(c.costs[i])
	return nil
}

func (c *fakeLatencyConn) Close() error {
	c.closed = true
	return nil
}

// fakeLatencyDialer returns conn, or dialErr when it is set, and records the
// target it was asked to open.
func fakeLatencyDialer(conn doltLatencyConn, dialErr error, got *[]string) doltLatencyDialer {
	return func(ctx context.Context, host string, port int, database string) (doltLatencyConn, error) {
		*got = append(*got, host, strconv.Itoa(port), database)
		if dialErr != nil {
			return nil, dialErr
		}
		return conn, nil
	}
}

func TestSummarizeDoltLatencyNearestRank(t *testing.T) {
	t.Parallel()
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	// The series are given out of order: summarizing must sort them.
	hundred := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- {
		hundred = append(hundred, ms(i))
	}
	ten := make([]time.Duration, 0, 10)
	for i := 10; i >= 1; i-- {
		ten = append(ten, ms(i))
	}

	tests := []struct {
		name             string
		samples          []time.Duration
		wantP50, wantP95 time.Duration
		wantMax          time.Duration
		wantSamples      int
	}{
		{
			name:        "hundred known values read exact ranks",
			samples:     hundred,
			wantP50:     ms(50),
			wantP95:     ms(95),
			wantMax:     ms(100),
			wantSamples: 100,
		},
		{
			// Nearest rank of 95% of ten samples is rank 10, not an
			// interpolation between 9ms and 10ms.
			name:        "ten known values use nearest rank, not interpolation",
			samples:     ten,
			wantP50:     ms(5),
			wantP95:     ms(10),
			wantMax:     ms(10),
			wantSamples: 10,
		},
		{
			name:        "a series of one is its own p50, p95 and max",
			samples:     []time.Duration{ms(7)},
			wantP50:     ms(7),
			wantP95:     ms(7),
			wantMax:     ms(7),
			wantSamples: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := summarizeDoltLatency(tt.samples)
			if err != nil {
				t.Fatalf("summarizeDoltLatency: %v", err)
			}
			if got.Samples != tt.wantSamples || got.P50 != tt.wantP50 || got.P95 != tt.wantP95 || got.Max != tt.wantMax {
				t.Fatalf("got samples=%d p50=%s p95=%s max=%s; want samples=%d p50=%s p95=%s max=%s",
					got.Samples, got.P50, got.P95, got.Max,
					tt.wantSamples, tt.wantP50, tt.wantP95, tt.wantMax)
			}
		})
	}
}

func TestSummarizeDoltLatencyRefusesEmptySeries(t *testing.T) {
	t.Parallel()
	_, err := summarizeDoltLatency(nil)
	if !errors.Is(err, errNoDoltLatencySamples) {
		t.Fatalf("summarizeDoltLatency(nil) error = %v; want errNoDoltLatencySamples", err)
	}
}

func TestDoltLatencyMixIsReadOnly(t *testing.T) {
	t.Parallel()
	if len(doltLatencyMix) == 0 {
		t.Fatal("doltLatencyMix is empty")
	}
	for _, step := range doltLatencyMix {
		if _, err := step.query.Inline(); err != nil {
			t.Errorf("step %s is refused as not read-only: %v", step.name, err)
		}
	}
}

func TestRunDoltLatencyRunsFixedMixThroughInjectedConnection(t *testing.T) {
	t.Parallel()
	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	// Round 1 costs 1ms, 2ms, 3ms; round 2 the same.
	conn := &fakeLatencyConn{clock: clock, costs: []time.Duration{
		1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond,
		1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond,
	}, failAt: -1}
	var dialed []string
	var out bytes.Buffer

	err := runDoltLatencyProbe(context.Background(), &out, fakeLatencyDialer(conn, nil, &dialed), clock.now, doltLatencyRun{
		Host: "127.0.0.1", Port: 3307, Database: "beads", Rounds: 2,
	})
	if err != nil {
		t.Fatalf("runDoltLatencyProbe: %v", err)
	}

	var wantRan []string
	for round := 0; round < 2; round++ {
		for _, step := range doltLatencyMix {
			wantRan = append(wantRan, step.query.String())
		}
	}
	if strings.Join(conn.ran, "\n") != strings.Join(wantRan, "\n") {
		t.Fatalf("statements run:\n%s\nwant:\n%s", strings.Join(conn.ran, "\n"), strings.Join(wantRan, "\n"))
	}
	if !conn.closed {
		t.Error("connection was not closed")
	}
	if got := strings.Join(dialed, " "); got != "127.0.0.1 3307 beads" {
		t.Errorf("dialed %q; want host, port and database", got)
	}

	// Samples are 1,2,3,1,2,3 ms. Sorted: 1,1,2,2,3,3. p50 rank 3 = 2ms,
	// p95 rank 6 = 3ms, max = 3ms.
	want := "host 127.0.0.1\nport 3307\nsamples 6\np50 2ms\np95 3ms\nmax 3ms\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestRunDoltLatencyBoundsTheDial(t *testing.T) {
	t.Parallel()
	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	conn := &fakeLatencyConn{clock: clock, costs: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}, failAt: -1}
	var bounded bool
	dial := func(ctx context.Context, host string, port int, database string) (doltLatencyConn, error) {
		_, bounded = ctx.Deadline()
		return conn, nil
	}
	var out bytes.Buffer

	err := runDoltLatencyProbe(context.Background(), &out, dial, clock.now, doltLatencyRun{
		Host: "127.0.0.1", Port: 3307, Database: "beads", Rounds: 1,
	})
	if err != nil {
		t.Fatalf("runDoltLatencyProbe: %v", err)
	}
	if !bounded {
		t.Error("the dial ran under a context with no deadline; a stalled server would hang the probe")
	}
}

func TestRunDoltLatencyRefusesConnectionFailure(t *testing.T) {
	t.Parallel()
	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	var dialed []string
	var out bytes.Buffer
	dialErr := errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")

	err := runDoltLatencyProbe(context.Background(), &out, fakeLatencyDialer(nil, dialErr, &dialed), clock.now, doltLatencyRun{
		Host: "127.0.0.1", Port: 3307, Database: "beads", Rounds: 5,
	})
	if err == nil {
		t.Fatal("runDoltLatencyProbe succeeded on a failed connection; want an error")
	}
	if !errors.Is(err, dialErr) {
		t.Errorf("error %v does not wrap the dial error", err)
	}
	if !strings.Contains(err.Error(), "cannot connect to 127.0.0.1:3307") {
		t.Errorf("error %q does not name the host and port", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed output on a failed connection: %q", out.String())
	}
}

func TestRunDoltLatencyRefusesRunWithNoSamples(t *testing.T) {
	t.Parallel()
	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	conn := &fakeLatencyConn{clock: clock, failAt: -1}
	var dialed []string
	var out bytes.Buffer

	err := runDoltLatencyProbe(context.Background(), &out, fakeLatencyDialer(conn, nil, &dialed), clock.now, doltLatencyRun{
		Host: "127.0.0.1", Port: 3307, Database: "beads", Rounds: 0,
	})
	if !errors.Is(err, errNoDoltLatencySamples) {
		t.Fatalf("zero-round run error = %v; want errNoDoltLatencySamples", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed a summary for a run with no samples: %q", out.String())
	}
}

func TestRunDoltLatencyRefusesFailedStep(t *testing.T) {
	t.Parallel()
	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	// The second step (the lookup) fails on the first round.
	conn := &fakeLatencyConn{clock: clock, costs: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}, failAt: 1}
	var dialed []string
	var out bytes.Buffer

	err := runDoltLatencyProbe(context.Background(), &out, fakeLatencyDialer(conn, nil, &dialed), clock.now, doltLatencyRun{
		Host: "127.0.0.1", Port: 3307, Database: "beads", Rounds: 1,
	})
	if !errors.Is(err, errLatencyRun) {
		t.Fatalf("failed step error = %v; want it to wrap errLatencyRun", err)
	}
	if !strings.Contains(err.Error(), "query lookup failed") {
		t.Errorf("error %q does not name the failed step", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed a summary from a run with a failed step: %q", out.String())
	}
}

// fakeRows is a result of n rows that fails reading with err once the rows
// run out.
type fakeRows struct {
	n      int
	err    error
	closed bool
}

func (r *fakeRows) Next() bool {
	if r.n == 0 {
		return false
	}
	r.n--
	return true
}

func (r *fakeRows) Err() error { return r.err }

func (r *fakeRows) Close() error {
	r.closed = true
	return nil
}

func TestDrainRowsReadsEveryRow(t *testing.T) {
	t.Parallel()
	rows := &fakeRows{n: 3}
	err := drainRows(func() (rowsIter, error) { return rows, nil })
	if err != nil {
		t.Fatalf("drainRows: %v", err)
	}
	if rows.n != 0 {
		t.Errorf("%d rows left unread", rows.n)
	}
	if !rows.closed {
		t.Error("the result was not closed")
	}
}

func TestDrainRowsRefusesFailedOpen(t *testing.T) {
	t.Parallel()
	errOpen := errors.New("table not found")
	err := drainRows(func() (rowsIter, error) { return nil, errOpen })
	if !errors.Is(err, errOpen) {
		t.Fatalf("drainRows error = %v; want the open error", err)
	}
}

func TestDrainRowsRefusesFailedRead(t *testing.T) {
	t.Parallel()
	errRead := errors.New("connection lost mid-result")
	rows := &fakeRows{n: 2, err: errRead}
	err := drainRows(func() (rowsIter, error) { return rows, nil })
	if !errors.Is(err, errRead) {
		t.Fatalf("drainRows error = %v; want the read error, so a truncated result is not a fast sample", err)
	}
	if !rows.closed {
		t.Error("the result was not closed after a failed read")
	}
}

func TestWrapBeadsLatencyConnRefusesFailedOpen(t *testing.T) {
	t.Parallel()
	errOpen := errors.New("schema level mismatch")
	conn, err := wrapBeadsLatencyConn(nil, errOpen)
	if !errors.Is(err, errOpen) {
		t.Fatalf("wrapBeadsLatencyConn error = %v; want the open error", err)
	}
	if conn != nil {
		t.Errorf("wrapBeadsLatencyConn returned a connection %v with the open error", conn)
	}
}
