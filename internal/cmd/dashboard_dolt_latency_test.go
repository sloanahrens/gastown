package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// latencyMS is a step cost of n milliseconds.
func latencyMS(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// latencyTestTarget is the run the section tests probe: one round of the mix on
// a made-up address, so no read in a test asks a dialer to reach a server.
func latencyTestTarget(rounds int) doltLatencyTarget {
	return func(context.Context) (doltLatencyRun, error) {
		return doltLatencyRun{Host: "127.0.0.1", Port: 3307, Database: "gt", Rounds: rounds}, nil
	}
}

// latencyTestSection returns a section that runs each read through conn's dialer
// and reads its clock from the same fake, so every sample is exactly the cost
// the fake charged it.
func latencyTestSection(target doltLatencyTarget, conn doltLatencyConn, clock *fakeLatencyClock) *doltLatencySection {
	dial := func(context.Context, string, int, string) (doltLatencyConn, error) {
		return conn, nil
	}
	return newDoltLatencySection(target, dial, clock.now)
}

// latencyRead panels one section and returns its read, so the tests see the
// section the way the Dolt panel delivers it: keyed, isolated and with a failed
// read's reason scrubbed by dashWhy.
func latencyRead(t *testing.T, s *doltLatencySection) func() dashboard.DoltSection {
	t.Helper()
	panel := doltPanel([]doltSectionReader{{key: "latency", read: s.read}}, "/town", time.Minute)
	return func() dashboard.DoltSection {
		t.Helper()
		sections := panel().Sections
		if len(sections) != 1 {
			t.Fatalf("the panel has %d sections, want 1", len(sections))
		}
		if got := sections[0].Key; got != "latency" {
			t.Fatalf("the section's key is %q, want latency", got)
		}
		return sections[0]
	}
}

// latencyCell is the one row's cell under column, and it fails the test when the
// section has no row or no such column.
func latencyCell(t *testing.T, s dashboard.DoltSection, column string) string {
	t.Helper()
	if s.Err != "" {
		t.Fatalf("the section failed: %s", s.Err)
	}
	if len(s.Rows) != 1 {
		t.Fatalf("the section has %d rows, want 1", len(s.Rows))
	}
	for i, head := range s.Head {
		if head == column {
			return s.Rows[0][i]
		}
	}
	t.Fatalf("the section has no %q column: head %v", column, s.Head)
	return ""
}

// A read draws the probe's p50, p95 and max and the step whose sample was the
// run's slowest.
func TestDoltLatencySectionRowReportsSummaryAndSlowestStep(t *testing.T) {
	t.Parallel()

	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	// The mix runs list, lookup, count, so lookup costs the most here.
	conn := &fakeLatencyConn{clock: clock, failAt: -1, costs: []time.Duration{
		latencyMS(1), latencyMS(5), latencyMS(3),
	}}
	read := latencyRead(t, latencyTestSection(latencyTestTarget(1), conn, clock))

	got := read()
	if got.Title != "Dolt: query latency" {
		t.Errorf("the section's title is %q, want %q", got.Title, "Dolt: query latency")
	}
	// Sorted 1ms, 3ms, 5ms: p50 is rank 2 and p95 rank 3 by nearest rank.
	for column, want := range map[string]string{
		"p50": "3ms", "p95": "5ms", "max": "5ms", "slowest": "lookup",
	} {
		if cell := latencyCell(t, got, column); cell != want {
			t.Errorf("the row's %s is %q, want %q", column, cell, want)
		}
	}
}

// Every read adds its p95 to the trend, and the row shows only the most recent
// five.
func TestDoltLatencySectionTrendShowsTheLastFiveReads(t *testing.T) {
	t.Parallel()

	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	// Read i costs i ms on each of its three steps, so its p95 is i ms.
	costs := make([]time.Duration, 0, 7*len(doltLatencyMix))
	for read := 1; read <= 7; read++ {
		for range doltLatencyMix {
			costs = append(costs, latencyMS(read))
		}
	}
	conn := &fakeLatencyConn{clock: clock, failAt: -1, costs: costs}
	read := latencyRead(t, latencyTestSection(latencyTestTarget(1), conn, clock))

	// The window slides once five values are kept: from the sixth read on the
	// oldest is dropped.
	want := []string{
		"1ms",
		"1ms 2ms",
		"1ms 2ms 3ms",
		"1ms 2ms 3ms 4ms",
		"1ms 2ms 3ms 4ms 5ms",
		"2ms 3ms 4ms 5ms 6ms",
		"3ms 4ms 5ms 6ms 7ms",
	}
	for i, wantTrend := range want {
		if got := latencyCell(t, read(), "p95 trend"); got != wantTrend {
			t.Errorf("after %d reads the p95 trend is %q, want %q", i+1, got, wantTrend)
		}
	}
}

// A dial failure is the section's Err, naming the address, and the credential a
// driver error can carry never reaches the page.
func TestDoltLatencySectionRefusesDialError(t *testing.T) {
	t.Parallel()

	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	dialErr := errors.New("dial tcp 127.0.0.1:3307: mysql://root:hunter2@127.0.0.1:3307/gt: connection refused")
	dial := func(context.Context, string, int, string) (doltLatencyConn, error) {
		return nil, dialErr
	}
	section := newDoltLatencySection(latencyTestTarget(1), dial, clock.now)
	read := latencyRead(t, section)

	got := read()
	if got.Err == "" {
		t.Fatal("a failed dial left the section with no Err")
	}
	if !strings.Contains(got.Err, "127.0.0.1:3307") {
		t.Errorf("the Err %q does not name the address", got.Err)
	}
	if strings.Contains(got.Err, "hunter2") {
		t.Errorf("the Err carries the credential: %q", got.Err)
	}
	if got.Head != nil || got.Rows != nil {
		t.Errorf("the failed section carries a table: Head=%v Rows=%v", got.Head, got.Rows)
	}

	// A failed read adds nothing to the trend: the first successful read after
	// it is the trend's first value.
	conn := &fakeLatencyConn{clock: clock, failAt: -1, costs: []time.Duration{
		latencyMS(1), latencyMS(1), latencyMS(1),
	}}
	section.dial = func(context.Context, string, int, string) (doltLatencyConn, error) {
		return conn, nil
	}
	if trend := latencyCell(t, read(), "p95 trend"); trend != "1ms" {
		t.Errorf("the trend after a failed read and one good read is %q, want %q", trend, "1ms")
	}
}

// `gt dolt latency` prints the same six lines for the same fake it always did:
// the probe returns its summary, and the command's writer draws it.
func TestDoltLatencyCommandOutputIsUnchanged(t *testing.T) {
	t.Parallel()

	clock := &fakeLatencyClock{t: time.Unix(1_700_000_000, 0)}
	conn := &fakeLatencyConn{clock: clock, failAt: -1, costs: []time.Duration{
		latencyMS(1), latencyMS(2), latencyMS(3),
		latencyMS(1), latencyMS(2), latencyMS(3),
	}}
	run := doltLatencyRun{Host: "127.0.0.1", Port: 3307, Database: "beads", Rounds: 2}
	var dialed []string
	var out bytes.Buffer

	if err := doltLatencyTo(&out, fakeLatencyDialer(conn, nil, &dialed), clock.now, run); err != nil {
		t.Fatalf("doltLatencyTo: %v", err)
	}
	// Samples are 1,2,3,1,2,3 ms: p50 2ms, p95 3ms, max 3ms.
	want := "host 127.0.0.1\nport 3307\nsamples 6\np50 2ms\np95 3ms\nmax 3ms\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}

	// A failed run prints nothing at all.
	failDial := func(context.Context, string, int, string) (doltLatencyConn, error) {
		return nil, errors.New("connection refused")
	}
	out.Reset()
	if err := doltLatencyTo(&out, failDial, clock.now, run); err == nil {
		t.Fatal("doltLatencyTo succeeded on a failed dial; want an error")
	}
	if out.Len() != 0 {
		t.Errorf("printed output for a failed run: %q", out.String())
	}
}

// The section's target is the town's own bd store: its metadata names the
// server and the database, and the port file the server's port.
func TestDoltLatencySectionReadsTheTownStoreAddress(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	beadsDir := filepath.Join(root, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_server_host": "127.0.0.1",
  "dolt_database": "gt"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("4321"), 0o600); err != nil {
		t.Fatalf("write dolt-server.port: %v", err)
	}

	run, err := townDoltLatencyRun(withDoltTownRoot(context.Background(), root))
	if err != nil {
		t.Fatalf("townDoltLatencyRun: %v", err)
	}
	if run.Host != "127.0.0.1" || run.Port != 4321 || run.Database != "gt" {
		t.Errorf("the run targets %s:%d/%s, want 127.0.0.1:4321/gt", run.Host, run.Port, run.Database)
	}
	if run.Rounds != doltLatencyPanelRounds {
		t.Errorf("the run asks for %d rounds, want %d", run.Rounds, doltLatencyPanelRounds)
	}
}

// A target the section cannot read is an error naming the store it looked in,
// never a default address the section would dial instead.
func TestDoltLatencySectionRefusesAnUnknownTarget(t *testing.T) {
	t.Parallel()

	storeless := t.TempDir()
	dataless := t.TempDir()
	datalessBeads := filepath.Join(dataless, ".beads")
	if err := os.MkdirAll(datalessBeads, 0o755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	// Server-mode metadata that names no database.
	metadata := `{"backend": "dolt", "dolt_mode": "server"}`
	if err := os.WriteFile(filepath.Join(datalessBeads, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cases := []struct {
		name string
		ctx  context.Context
		want error
		says string
	}{
		{name: "no town root", ctx: context.Background(), want: errNoDoltLatencyTownRoot, says: "town root"},
		{name: "no store", ctx: withDoltTownRoot(context.Background(), storeless), says: ".beads"},
		{name: "store names no database", ctx: withDoltTownRoot(context.Background(), dataless), says: "database"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := townDoltLatencyRun(tc.ctx)
			if err == nil {
				t.Fatal("townDoltLatencyRun resolved a target; want an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error %v is not %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not say %q", err, tc.says)
			}
		})
	}
}

// The town's panel carries the latency section from its own init, and a town
// with no store draws the reason rather than dialling a guessed address.
func TestDoltLatencySectionIsRegistered(t *testing.T) {
	t.Parallel()

	var read doltSectionRead
	for _, r := range doltSections.snapshot() {
		if r.key == "latency" {
			read = r.read
		}
	}
	if read == nil {
		t.Fatal("no latency section is registered with the Dolt panel")
	}

	sections := doltPanel([]doltSectionReader{{key: "latency", read: read}}, t.TempDir(), time.Minute)().Sections
	if len(sections) != 1 {
		t.Fatalf("the panel has %d sections, want 1", len(sections))
	}
	if !strings.Contains(sections[0].Err, ".beads") {
		t.Errorf("the section's Err is %q, want it to name the store it looked in", sections[0].Err)
	}
}
