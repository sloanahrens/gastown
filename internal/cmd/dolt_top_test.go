package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

// fakeTopReader answers reads from a scripted list. With failRead set, the
// read at failAt returns errTopRead instead of a sample.
type fakeTopReader struct {
	samples  [][]doltTopSample
	failRead bool
	failAt   int
	reads    int
	closed   bool
}

var errTopRead = errors.New("server went away")

func (r *fakeTopReader) ReadProcessList(context.Context) ([]doltTopSample, error) {
	i := r.reads
	r.reads++
	if r.failRead && i == r.failAt {
		return nil, errTopRead
	}
	if i >= len(r.samples) {
		return nil, nil
	}
	return r.samples[i], nil
}

func (r *fakeTopReader) Close() error {
	r.closed = true
	return nil
}

// topDeps builds deps around reader with a sleep that records and does not
// wait, so a test runs the sampling loop without spending its interval.
func topDeps(out *bytes.Buffer, reader doltTopReader, sleeps *[]time.Duration) doltTopDeps {
	return doltTopDeps{
		open: func(context.Context) (doltTopReader, error) { return reader, nil },
		sleep: func(_ context.Context, d time.Duration) error {
			*sleeps = append(*sleeps, d)
			return nil
		},
		out: out,
	}
}

func TestDoltTopNormalizeStatement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "   ", ""},
		{"string literal", `SELECT * FROM issues WHERE id = 'gt-abc'`, `SELECT * FROM issues WHERE id = ?`},
		{"escaped quote", `SELECT * FROM issues WHERE title = 'it''s'`, `SELECT * FROM issues WHERE title = ?`},
		{"double-quoted literal", `SELECT * FROM issues WHERE title = "hi"`, `SELECT * FROM issues WHERE title = ?`},
		{"number literal", `SELECT * FROM issues LIMIT 50`, `SELECT * FROM issues LIMIT ?`},
		{"decimal and exponent", `SELECT * FROM issues WHERE weight > 1.5e3`, `SELECT * FROM issues WHERE weight > ?`},
		{"id list collapses", `SELECT * FROM issues WHERE id IN ('gt-a', 'gt-b', 'gt-c')`, `SELECT * FROM issues WHERE id IN (?)`},
		{"number list collapses", `SELECT * FROM issues WHERE id IN (1,2,3)`, `SELECT * FROM issues WHERE id IN (?)`},
		{"whitespace folds", "SELECT  *\n\tFROM   issues", `SELECT * FROM issues`},
		{"line comment drops", "SELECT * -- everything\nFROM issues", `SELECT * FROM issues`},
		{"hash comment drops", "SELECT *\n# everything\nFROM issues", `SELECT * FROM issues`},
		{"block comment drops", `SELECT /* everything */ * FROM issues`, `SELECT * FROM issues`},
		{"identifier digits kept", "SELECT t1.col1 FROM t1", "SELECT t1.col1 FROM t1"},
		{"backtick identifier kept", "SELECT `gt-abc`.id FROM `gt-abc`", "SELECT `gt-abc`.id FROM `gt-abc`"},
		{"backtick backslash literal", "SELECT `a\\` = 1", "SELECT `a\\` = ?"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeStatement(tc.in); got != tc.want {
				t.Errorf("normalizeStatement(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// One shape is one key: two calls that differ only in their literals must
// normalize to the same statement.
func TestDoltTopNormalizeStatementOneShape(t *testing.T) {
	t.Parallel()
	a := normalizeStatement(`SELECT * FROM issues WHERE id IN ('gt-a','gt-b')`)
	b := normalizeStatement(`SELECT  *  FROM issues WHERE id IN ('gt-c')`)
	if a != b {
		t.Fatalf("two statements of one shape normalized differently: %q and %q", a, b)
	}
}

func TestDoltTopAggregateRanksBySamples(t *testing.T) {
	t.Parallel()
	readings := [][]doltTopSample{
		{
			{User: "root", Host: "a:1", Statement: `SELECT * FROM issues WHERE id = 'gt-a'`},
			{User: "root", Host: "b:2", Statement: "SELECT COUNT(*) FROM issues"},
			{User: "root", Host: "a:1", Statement: ""}, // idle connection
		},
		{
			{User: "root", Host: "a:1", Statement: `SELECT * FROM issues WHERE id = 'gt-b'`},
			{User: "root", Host: "b:2", Statement: "SELECT COUNT(*) FROM issues"},
			{User: "root", Host: "d:4", Statement: "SELECT foo FROM x"},
			{User: "root", Host: "d:4", Statement: "SELECT bar FROM y"},
		},
	}

	got := aggregateSamples(readings)

	want := []doltTopRow{
		// Two reads each; the tie breaks on host, then statement.
		{User: "root", Host: "a:1", Statement: "SELECT * FROM issues WHERE id = ?", Samples: 2},
		{User: "root", Host: "b:2", Statement: "SELECT COUNT(*) FROM issues", Samples: 2},
		{User: "root", Host: "d:4", Statement: "SELECT bar FROM y", Samples: 1},
		{User: "root", Host: "d:4", Statement: "SELECT foo FROM x", Samples: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("aggregateSamples returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A read counts once for a shape even when one host runs it on several
// connections, so a row's count never exceeds the reads taken.
func TestDoltTopAggregateCountsEachReadOncePerShape(t *testing.T) {
	t.Parallel()
	readings := [][]doltTopSample{{
		{User: "root", Host: "a:1", Statement: "SELECT 1"},
		{User: "root", Host: "a:1", Statement: "SELECT 1"},
		{User: "root", Host: "a:1", Statement: "SELECT 2"},
	}}

	got := aggregateSamples(readings)
	if len(got) != 1 {
		t.Fatalf("aggregateSamples returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Statement != "SELECT ?" || got[0].Samples != 1 {
		t.Errorf("row = %+v, want SELECT ? seen in 1 read", got[0])
	}
}

// The command exists to find the caller that costs the most server time. A
// statement that runs in milliseconds and is seen in 40 of 60 reads must rank
// above one the server reports at TIME 900 that only 5 reads caught: ranking on
// TIME, or on the time it reports, hides exactly the hot caller (gt-tesr3).
func TestDoltTopRanksBySamplesNotStatementAge(t *testing.T) {
	t.Parallel()
	const reads = 60
	readings := make([][]doltTopSample, 0, reads)
	for i := 0; i < reads; i++ {
		var reading []doltTopSample
		if i < 40 {
			reading = append(reading, doltTopSample{
				User: "root", Host: "fast:1",
				Statement: `SELECT id FROM issues WHERE id = 'gt-a'`,
				Time:      0,
			})
		}
		if i%12 == 0 { // reads 0, 12, 24, 36, 48
			reading = append(reading, doltTopSample{
				User: "root", Host: "slow:2",
				Statement: "SELECT SLEEP(900)", Time: 900,
			})
		}
		readings = append(readings, reading)
	}

	var out bytes.Buffer
	var sleeps []time.Duration
	reader := &fakeTopReader{samples: readings}
	if err := runDoltTop(context.Background(), topDeps(&out, reader, &sleeps), reads, time.Second, 10); err != nil {
		t.Fatalf("runDoltTop: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("printed %d lines, want a summary, a header and 2 rows:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[2], "SELECT id FROM issues WHERE id = ?") || !strings.Contains(lines[2], "40/60") {
		t.Errorf("first row = %q, want the often-seen short statement at 40/60", lines[2])
	}
	if !strings.Contains(lines[3], "SELECT SLEEP(?)") || !strings.Contains(lines[3], "5/60") {
		t.Errorf("second row = %q, want the rarely-seen long statement at 5/60", lines[3])
	}
}

func TestDoltTopRenderCutsToWidth(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{
		{User: "root", Host: "127.0.0.1:54321", Statement: "SELECT * FROM issues WHERE id IN (?)", Samples: 40},
		{User: "reader", Host: "127.0.0.1:40001", Statement: "SELECT id FROM issues WHERE title LIKE ? ORDER BY updated_at", Samples: 12},
	}

	var out bytes.Buffer
	if err := renderTop(&out, rows, 60, 60, time.Second); err != nil {
		t.Fatalf("renderTop: %v", err)
	}

	want := "sampled 60 times at 1s; EST is reads seen x interval\n" +
		"USER    HOST              SEEN  EST  STATEMENT\n" +
		"root    127.0.0.1:54321  40/60  40s  SELECT * FROM issues W…\n" +
		"reader  127.0.0.1:40001  12/60  12s  SELECT id FROM issues …\n"
	if got := out.String(); got != want {
		t.Errorf("renderTop output =\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")[1:] {
		if n := len([]rune(line)); n > 60 {
			t.Errorf("line is %d runes wide, want at most 60: %q", n, line)
		}
	}
}

// A terminal too narrow for the fixed columns still gets a readable statement
// rather than one cut to nothing.
func TestDoltTopRenderAtNarrowWidthKeepsStatement(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{{
		User:      "root",
		Host:      "127.0.0.1:54321",
		Statement: "SELECT id FROM issues WHERE title LIKE ? ORDER BY updated_at",
		Samples:   1,
	}}

	var out bytes.Buffer
	if err := renderTop(&out, rows, 10, 1, time.Second); err != nil {
		t.Fatalf("renderTop: %v", err)
	}

	line := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")[2]
	start := strings.Index(line, "  SELECT ")
	if start < 0 {
		t.Fatalf("output has no statement column:\n%s", line)
	}
	statement := line[start+2:]
	if got := len([]rune(statement)); got != doltTopDefaultStatementWidth {
		t.Errorf("statement is %d runes at a narrow width, want %d: %q", got, doltTopDefaultStatementWidth, statement)
	}
	if !strings.HasSuffix(statement, "…") {
		t.Errorf("cut statement %q has no ellipsis", statement)
	}
}

// A user or host holding a non-ASCII rune must not move the columns: the
// table is padded by rune, so the statement starts at the same place on every
// line.
func TestDoltTopRenderAlignsColumnsByRune(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{
		{User: "ü", Host: "127.0.0.1:54321", Statement: "SELECT 1", Samples: 1},
		{User: "reader", Host: "127.0.0.1:54322", Statement: "SELECT 2", Samples: 2},
		{User: "root", Host: "127.0.0.1:54323", Statement: "SELECT 3", Samples: 12345},
	}

	var out bytes.Buffer
	if err := renderTop(&out, rows, 80, 60, time.Second); err != nil {
		t.Fatalf("renderTop: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	header := lines[1]
	want := len([]rune(header[:strings.Index(header, "STATEMENT")]))
	for _, line := range lines[2:] {
		start := strings.Index(line, "SELECT ")
		if start < 0 {
			t.Fatalf("row has no statement column: %q", line)
		}
		if got := len([]rune(line[:start])); got != want {
			t.Errorf("statement starts at rune %d, want %d: %q", got, want, line)
		}
	}
}

// The statement, user and host come from the server, so a control character
// in them must not reach the terminal as a live escape sequence.
func TestDoltTopRenderStripsControlCharacters(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{{
		User:      "ro\x1bot",
		Host:      "127.0.0.1:54321\x1b[31m",
		Statement: "SELECT \x1b[2J1 FROM t",
		Samples:   1,
	}}

	var out bytes.Buffer
	if err := renderTop(&out, rows, 120, 1, time.Second); err != nil {
		t.Fatalf("renderTop: %v", err)
	}

	for _, r := range out.String() {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Errorf("output holds control rune %U: %q", r, out.String())
		}
	}
	if !strings.Contains(out.String(), "ro ot") {
		t.Errorf("output does not hold the sanitized user: %q", out.String())
	}
}

func TestDoltTopRefusesBadFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		samples  int
		interval time.Duration
		limit    int
		flag     string
		value    string
	}{
		{"samples below one", 0, time.Second, 10, "--samples", "0"},
		{"negative samples", -3, time.Second, 10, "--samples", "-3"},
		{"interval below the minimum", 5, 50 * time.Millisecond, 10, "--interval", "50ms"},
		{"limit below one", 5, time.Second, 0, "--limit", "0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			var sleeps []time.Duration
			opened := false
			deps := topDeps(&out, &fakeTopReader{}, &sleeps)
			deps.open = func(context.Context) (doltTopReader, error) {
				opened = true
				return &fakeTopReader{}, nil
			}

			err := runDoltTop(context.Background(), deps, tc.samples, tc.interval, tc.limit)
			if err == nil {
				t.Fatalf("runDoltTop accepted %s=%s", tc.flag, tc.value)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error %q does not name %s", err, tc.flag)
			}
			if !strings.Contains(err.Error(), tc.value) {
				t.Errorf("error %q does not name the value %s it refused", err, tc.value)
			}
			if opened {
				t.Error("runDoltTop opened a connection before refusing the flags")
			}
			if out.Len() != 0 {
				t.Errorf("runDoltTop printed %q, want nothing", out.String())
			}
		})
	}
}

func TestDoltTopReadsSamplesTimesAndPrintsTopLimit(t *testing.T) {
	t.Parallel()
	var readings [][]doltTopSample
	for i := 0; i < 3; i++ {
		var reading []doltTopSample
		if i == 0 {
			reading = append(reading, doltTopSample{User: "root", Host: "a:1", Statement: `SELECT * FROM rigs WHERE name = 'a'`})
		}
		reading = append(reading, doltTopSample{User: "root", Host: "a:1", Statement: "SELECT COUNT(*) FROM issues"})
		if i > 0 {
			reading = append(reading, doltTopSample{User: "root", Host: "a:1", Statement: "SELECT MAX(id) FROM events"})
		}
		readings = append(readings, reading)
	}
	reader := &fakeTopReader{samples: readings}
	var out bytes.Buffer
	var sleeps []time.Duration

	err := runDoltTop(context.Background(), topDeps(&out, reader, &sleeps), 3, 500*time.Millisecond, 2)
	if err != nil {
		t.Fatalf("runDoltTop: %v", err)
	}
	if reader.reads != 3 {
		t.Errorf("read the process list %d times, want 3", reader.reads)
	}
	if !reader.closed {
		t.Error("the reader was not closed")
	}
	if len(sleeps) != 2 {
		t.Fatalf("waited %d times between 3 reads, want 2", len(sleeps))
	}
	for _, d := range sleeps {
		if d != 500*time.Millisecond {
			t.Errorf("waited %s between reads, want 500ms", d)
		}
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("printed %d lines, want a summary, a header and 2 rows:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[2], "SELECT COUNT(*) FROM issues") || !strings.Contains(lines[2], "3/3") {
		t.Errorf("row 1 = %q, want the statement seen in every read", lines[2])
	}
	if !strings.Contains(lines[3], "SELECT MAX(id) FROM events") || !strings.Contains(lines[3], "2/3") {
		t.Errorf("row 2 = %q, want the second busiest statement", lines[3])
	}
	if strings.Contains(out.String(), "SELECT * FROM rigs") {
		t.Errorf("output prints a statement outside the limit:\n%s", out.String())
	}
}

func TestDoltTopFailsWhenTheServerCannotBeRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		wantErr error
		deps    func(out *bytes.Buffer, sleeps *[]time.Duration) doltTopDeps
	}{
		{
			name:    "cannot open",
			wantErr: errTopRead,
			deps: func(out *bytes.Buffer, sleeps *[]time.Duration) doltTopDeps {
				return doltTopDeps{
					open:  func(context.Context) (doltTopReader, error) { return nil, errTopRead },
					sleep: func(context.Context, time.Duration) error { return nil },
					out:   out,
				}
			},
		},
		{
			name:    "a read fails",
			wantErr: errTopRead,
			deps: func(out *bytes.Buffer, sleeps *[]time.Duration) doltTopDeps {
				return topDeps(out, &fakeTopReader{failRead: true, failAt: 0}, sleeps)
			},
		},
		{
			name:    "the wait is canceled",
			wantErr: context.Canceled,
			deps: func(out *bytes.Buffer, sleeps *[]time.Duration) doltTopDeps {
				deps := topDeps(out, &fakeTopReader{}, sleeps)
				deps.sleep = func(context.Context, time.Duration) error { return context.Canceled }
				return deps
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			var sleeps []time.Duration
			err := runDoltTop(context.Background(), tc.deps(&out, &sleeps), 3, time.Second, 10)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("runDoltTop error = %v, want %v", err, tc.wantErr)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q after a failure, want nothing", out.String())
			}
		})
	}
}

// A server with nothing to rank is one clear line, never a table with no rows.
func TestDoltTopReportsNoStatements(t *testing.T) {
	t.Parallel()
	reader := &fakeTopReader{samples: [][]doltTopSample{
		{{User: "root", Host: "a:1", Statement: "", Time: 900}},
	}}
	var out bytes.Buffer
	var sleeps []time.Duration

	if err := runDoltTop(context.Background(), topDeps(&out, reader, &sleeps), 2, time.Second, 10); err != nil {
		t.Fatalf("runDoltTop: %v", err)
	}
	want := "no statements sampled in 2 process-list reads at 1s\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// failingTopWriter refuses every write, so renderTop's write-error branch runs.
type failingTopWriter struct{}

var errTopWrite = errors.New("broken pipe")

func (failingTopWriter) Write([]byte) (int, error) { return 0, errTopWrite }

func TestDoltTopRenderReportsWriteError(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{{User: "root", Host: "a:1", Statement: "SELECT 1", Samples: 1}}
	if err := renderTop(failingTopWriter{}, rows, 80, 1, time.Second); !errors.Is(err, errTopWrite) {
		t.Fatalf("renderTop error = %v, want %v", err, errTopWrite)
	}
}

// acceptWriter takes its first writes, then refuses every later one, so a
// failure lands on the header (accept 1) or the first row (accept 2).
type acceptWriter struct{ writes, accept int }

func (w *acceptWriter) Write(p []byte) (int, error) {
	if w.writes >= w.accept {
		return 0, errTopWrite
	}
	w.writes++
	return len(p), nil
}

func TestDoltTopRenderStopsOnAHeaderWriteError(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{{User: "root", Host: "a:1", Statement: "SELECT 1", Samples: 1}}
	if err := renderTop(&acceptWriter{accept: 1}, rows, 80, 1, time.Second); !errors.Is(err, errTopWrite) {
		t.Fatalf("renderTop error = %v, want %v", err, errTopWrite)
	}
}

func TestDoltTopRenderStopsOnARowWriteError(t *testing.T) {
	t.Parallel()
	rows := []doltTopRow{
		{User: "root", Host: "a:1", Statement: "SELECT 1", Samples: 1},
		{User: "root", Host: "a:1", Statement: "SELECT 2", Samples: 1},
	}
	if err := renderTop(&acceptWriter{accept: 2}, rows, 80, 2, time.Second); !errors.Is(err, errTopWrite) {
		t.Fatalf("renderTop error = %v, want %v", err, errTopWrite)
	}
}

// The SQL driver registry is global and keeps a driver forever, so the fake
// registers one driver for the whole test binary and finds its server by DSN.
// A registration per test would pile drivers up for the life of the process.
const topFakeDriverName = "gt_dolt_top_fake"

var (
	topFakeDriversMu sync.Mutex
	topFakeDrivers   = make(map[string]*topFakeServer)
	topFakeDriverSeq atomic.Uint64
	topFakeRegister  sync.Once
)

// topFakeServer answers the process-list read from memory. Its error fields
// make the connect, the query or the row shape fail, so each refusal of the
// reader runs without a server.
type topFakeServer struct {
	columns    []string
	rows       [][]driver.Value
	connectErr error
	queryErr   error
	closed     bool
}

// registerFakeTopServer records server under a DSN unique to this call, so
// tests that run in parallel never share a fake.
func registerFakeTopServer(t *testing.T, server *topFakeServer) string {
	t.Helper()
	topFakeRegister.Do(func() { sql.Register(topFakeDriverName, topFakeDriver{}) })
	dsn := fmt.Sprintf("fake-%d", topFakeDriverSeq.Add(1))
	topFakeDriversMu.Lock()
	topFakeDrivers[dsn] = server
	topFakeDriversMu.Unlock()
	t.Cleanup(func() {
		topFakeDriversMu.Lock()
		delete(topFakeDrivers, dsn)
		topFakeDriversMu.Unlock()
	})
	return dsn
}

func openFakeTopReader(t *testing.T, server *topFakeServer) *mysqlTopReader {
	t.Helper()
	db, err := sql.Open(topFakeDriverName, registerFakeTopServer(t, server))
	if err != nil {
		t.Fatalf("sql.Open on the fake driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &mysqlTopReader{db: db}
}

type topFakeDriver struct{}

func (topFakeDriver) Open(dsn string) (driver.Conn, error) {
	topFakeDriversMu.Lock()
	server, ok := topFakeDrivers[dsn]
	topFakeDriversMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no fake server registered for %q", dsn)
	}
	if server.connectErr != nil {
		return nil, server.connectErr
	}
	return &topFakeConn{server: server}, nil
}

type topFakeConn struct{ server *topFakeServer }

func (c *topFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by the reader")
}

func (c *topFakeConn) Close() error {
	c.server.closed = true
	return nil
}

func (c *topFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the reader")
}

func (c *topFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != doltTopProcessListQuery {
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
	if c.server.queryErr != nil {
		return nil, c.server.queryErr
	}
	columns := c.server.columns
	if columns == nil {
		columns = []string{"USER", "HOST", "TIME", "INFO"}
	}
	return &topFakeRows{columns: columns, rows: c.server.rows}, nil
}

type topFakeRows struct {
	columns []string
	rows    [][]driver.Value
	at      int
}

func (r *topFakeRows) Columns() []string { return r.columns }
func (r *topFakeRows) Close() error      { return nil }

func (r *topFakeRows) Next(dest []driver.Value) error {
	if r.at >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.at])
	r.at++
	return nil
}

func TestDoltTopReadsTheProcessList(t *testing.T) {
	t.Parallel()
	server := &topFakeServer{rows: [][]driver.Value{
		{"root", "127.0.0.1:54321", int64(2), "SELECT 1"},
		{"root", "127.0.0.1:54322", nil, ""},
	}}

	got, err := openFakeTopReader(t, server).ReadProcessList(context.Background())
	if err != nil {
		t.Fatalf("ReadProcessList: %v", err)
	}

	want := []doltTopSample{
		{User: "root", Host: "127.0.0.1:54321", Statement: "SELECT 1", Time: 2},
		{User: "root", Host: "127.0.0.1:54322", Statement: "", Time: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("ReadProcessList returned %d samples, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDoltTopReportsProcessListFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		server *topFakeServer
	}{
		{"the query fails", &topFakeServer{queryErr: errTopRead}},
		// Three columns cannot fill the reader's four destinations.
		{"a row does not scan", &topFakeServer{
			columns: []string{"USER", "HOST", "TIME"},
			rows:    [][]driver.Value{{"root", "a:1", int64(1)}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := openFakeTopReader(t, tc.server).ReadProcessList(context.Background()); err == nil {
				t.Fatal("ReadProcessList reported no error for a read that failed")
			}
		})
	}
}

func TestDoltTopOpensTheServer(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	findTown := func() (string, error) { return townRoot, nil }

	cases := []struct {
		name    string
		opener  doltTopOpener
		wantMsg string
	}{
		{
			name: "not in a workspace",
			opener: doltTopOpener{
				findTownRoot: func() (string, error) { return "", errTopRead },
			},
			wantMsg: "not in a Gas Town workspace",
		},
		{
			name: "the connection will not open",
			opener: doltTopOpener{
				findTownRoot: findTown,
				openDB:       func(string) (*sql.DB, error) { return nil, errTopRead },
			},
			wantMsg: "opening a connection",
		},
		{
			name: "the server does not answer",
			opener: doltTopOpener{
				findTownRoot: findTown,
				openDB:       fakeTopDB(t, &topFakeServer{connectErr: errTopRead}),
			},
			wantMsg: "connecting to the Dolt server",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader, err := tc.opener.open(context.Background())
			if err == nil {
				_ = reader.Close()
				t.Fatal("open accepted a server it could not reach")
			}
			if !errors.Is(err, errTopRead) {
				t.Errorf("open error = %v, want it to wrap %v", err, errTopRead)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("open error %q does not say %q", err, tc.wantMsg)
			}
		})
	}
}

// The path a healthy run takes: the opener reaches a server that answers, and
// the reader it returns serves the process list.
func TestDoltTopOpensAWorkingServer(t *testing.T) {
	t.Parallel()
	server := &topFakeServer{rows: [][]driver.Value{
		{"root", "127.0.0.1:54321", int64(2), "SELECT 1"},
	}}
	opener := doltTopOpener{
		findTownRoot: func() (string, error) { return t.TempDir(), nil },
		openDB:       fakeTopDB(t, server),
	}

	reader, err := opener.open(context.Background())
	if err != nil {
		t.Fatalf("open on a server that answers: %v", err)
	}

	got, err := reader.ReadProcessList(context.Background())
	if err != nil {
		t.Fatalf("ReadProcessList: %v", err)
	}
	want := []doltTopSample{
		{User: "root", Host: "127.0.0.1:54321", Statement: "SELECT 1", Time: 2},
	}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("ReadProcessList = %+v, want %+v", got, want)
	}
	if err := reader.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !server.closed {
		t.Error("Close did not close the server connection")
	}
}

// fakeTopDB answers a Dolt server with server, so the opener's ping and query
// run in process.
func fakeTopDB(t *testing.T, server *topFakeServer) func(string) (*sql.DB, error) {
	t.Helper()
	dsn := registerFakeTopServer(t, server)
	return func(string) (*sql.DB, error) { return sql.Open(topFakeDriverName, dsn) }
}
