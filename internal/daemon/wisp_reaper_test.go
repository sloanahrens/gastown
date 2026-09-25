package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/reaper"
)

func TestWispReaperInterval(t *testing.T) {
	// Default (now 1h after Dog-driven refactor)
	if got := wispReaperInterval(nil); got != defaultWispReaperInterval {
		t.Errorf("expected default %v, got %v", defaultWispReaperInterval, got)
	}

	// Custom
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:     true,
				IntervalStr: "2h",
			},
		},
	}
	if got := wispReaperInterval(config); got != 2*time.Hour {
		t.Errorf("expected 2h, got %v", got)
	}

	// Invalid falls back to default
	config.Patrols.WispReaper.IntervalStr = "nope"
	if got := wispReaperInterval(config); got != defaultWispReaperInterval {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

func TestWispReaperMaxAge(t *testing.T) {
	if got := wispReaperMaxAge(nil); got != defaultWispMaxAge {
		t.Errorf("expected default %v, got %v", defaultWispMaxAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:   true,
				MaxAgeStr: "48h",
			},
		},
	}
	if got := wispReaperMaxAge(config); got != 48*time.Hour {
		t.Errorf("expected 48h, got %v", got)
	}
}

func TestWispDeleteAge(t *testing.T) {
	if got := wispDeleteAge(nil); got != defaultWispDeleteAge {
		t.Errorf("expected default %v, got %v", defaultWispDeleteAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:      true,
				DeleteAgeStr: "336h",
			},
		},
	}
	if got := wispDeleteAge(config); got != 14*24*time.Hour {
		t.Errorf("expected 336h, got %v", got)
	}
}

func TestDefaultReaperIntervalIsOneHour(t *testing.T) {
	// Verify the default changed from 30m to 1h per issue gt-caf7.
	if defaultWispReaperInterval != 1*time.Hour {
		t.Errorf("expected default interval 1h, got %v", defaultWispReaperInterval)
	}
}

// TestDefaultStaleIssueAgeMatchesFormula is the regression guard for gt-2qzr.
// The daemon used to hardcode 7d and inject it on the dog path, overriding the
// mol-dog-reaper formula's 720h default; agent beads are idle by design and
// were 8d old, so the shorter threshold swept every agent bead in the town.
func TestDefaultStaleIssueAgeMatchesFormula(t *testing.T) {
	if defaultStaleIssueAge != 30*24*time.Hour {
		t.Fatalf("defaultStaleIssueAge = %v, want 720h to match the mol-dog-reaper formula default",
			defaultStaleIssueAge)
	}
	if got := wispReaperStaleIssueAge(nil); got != defaultStaleIssueAge {
		t.Fatalf("wispReaperStaleIssueAge(nil) = %v, want %v", got, defaultStaleIssueAge)
	}
}

func TestWispReaperStaleIssueAge(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{StaleIssueAgeStr: "45d"},
		},
	}
	// The formula renders the default as "30d"; time.ParseDuration rejects that
	// as "unknown unit d", so the day suffix has to be understood here.
	if got := wispReaperStaleIssueAge(config); got != 45*24*time.Hour {
		t.Errorf("expected 45d, got %v", got)
	}

	config.Patrols.WispReaper.StaleIssueAgeStr = "nope"
	if got := wispReaperStaleIssueAge(config); got != defaultStaleIssueAge {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

func TestParseAgeDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "720h", want: 720 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: " 7d ", want: 7 * 24 * time.Hour},
		{in: "d", wantErr: true},
		{in: "1.5d", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseAgeDuration(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseAgeDuration(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseAgeDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWispReaperAutoCloseKnob(t *testing.T) {
	yes, no := true, false

	// Unset is not disarmed: the knob exists to disarm, so absence must not
	// silently change behavior.
	if WispReaperAutoCloseDisarmed(nil) {
		t.Error("nil config should not be disarmed")
	}
	if WispReaperAutoCloseDisarmed(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}) {
		t.Error("config without a wisp_reaper section should not be disarmed")
	}

	unset := &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: &WispReaperConfig{}}}
	if WispReaperAutoCloseDisarmed(unset) {
		t.Error("unset auto_close should not be disarmed")
	}
	if !wispReaperAutoCloseEnabled(unset, false) {
		t.Error("unset auto_close should leave the dog path enabled")
	}
	// The inline fallback runs because Dog dispatch FAILED. Turning a dispatch
	// error into a sweep of the durable issue tracker is the gt-2qzr failure,
	// so it needs an explicit opt-in.
	if wispReaperAutoCloseEnabled(unset, true) {
		t.Error("unset auto_close should leave the inline fallback disarmed")
	}

	armed := &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: &WispReaperConfig{AutoClose: &yes}}}
	if WispReaperAutoCloseDisarmed(armed) {
		t.Error("auto_close=true should not be disarmed")
	}
	if !wispReaperAutoCloseEnabled(armed, false) || !wispReaperAutoCloseEnabled(armed, true) {
		t.Error("auto_close=true should enable both paths")
	}

	disarmed := &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: &WispReaperConfig{AutoClose: &no}}}
	if !WispReaperAutoCloseDisarmed(disarmed) {
		t.Error("auto_close=false should be disarmed")
	}
	if wispReaperAutoCloseEnabled(disarmed, false) || wispReaperAutoCloseEnabled(disarmed, true) {
		t.Error("auto_close=false should disarm both paths")
	}
}

// TestDispatchReaperDogReportsFailureCause covers the diagnostics gap from
// gt-2qzr: the log recorded only "exit status 1", which said nothing about why
// the sweep had moved to the inline fallback, so the cause went unnoticed.
func TestDispatchReaperDogReportsFailureCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock")
	}

	binDir := t.TempDir()
	fakeGT := filepath.Join(binDir, "gt")
	script := "#!/bin/sh\necho 'sling: no available dogs in deacon/dogs' >&2\nexit 1\n"
	if err := os.WriteFile(fakeGT, []byte(script), 0755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		gtPath: fakeGT,
		logger: log.New(io.Discard, "", 0),
	}
	err := d.dispatchReaperDog(map[string]string{"max_age": "1h"})
	if err == nil {
		t.Fatal("dispatchReaperDog() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "no available dogs") {
		t.Errorf("dispatchReaperDog() error = %q, want it to carry the command's stderr", err)
	}
}

func TestDispatchReaperDogUsesDogPoolSling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock")
	}

	townRoot := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gt-args.log")
	fakeGT := filepath.Join(binDir, "gt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", logPath)
	if err := os.WriteFile(fakeGT, []byte(script), 0755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		gtPath: fakeGT,
	}
	if err := d.dispatchReaperDog(map[string]string{"max_age": "1h"}); err != nil {
		t.Fatalf("dispatchReaperDog() error = %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gt args log: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	wantPrefix := []string{"sling", constants.MolDogReaper, "deacon/dogs"}
	if len(args) < len(wantPrefix) {
		t.Fatalf("gt args = %v, want prefix %v", args, wantPrefix)
	}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("gt arg %d = %q, want %q (all args: %v)", i, args[i], want, args)
		}
	}
}

func TestDoltServerHostIgnoresStaleBeadsHost(t *testing.T) {
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")

	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
	if got := d.doltServerHost(); got != "127.0.0.1" {
		t.Fatalf("doltServerHost() = %q, want default localhost", got)
	}
}

func TestDoltServerHostUsesConfiguredTownHost(t *testing.T) {
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	townRoot := t.TempDir()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{config: &Config{TownRoot: townRoot}}
	if got := d.doltServerHost(); got != "127.0.0.2" {
		t.Fatalf("doltServerHost() = %q, want configured host", got)
	}
}

// TestTriggerWispReaper_SkipsWhenNotDue is the regression test for
// gt-ima2/gt-gxpwc applied to wisp_reaper: with a recent last-run record on
// disk, the trigger must decline to start a cycle rather than firing on
// every tick (or every restart) regardless of the persisted schedule.
func TestTriggerWispReaper_SkipsWhenNotDue(t *testing.T) {
	townRoot := t.TempDir()
	if err := savePatrolLastRun(townRoot, "wisp_reaper", time.Now()); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf strings.Builder
	d := &Daemon{
		logger: log.New(&buf, "", 0),
		config: &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				WispReaper: &WispReaperConfig{Enabled: true},
			},
		},
	}

	d.triggerWispReaper()
	if d.wispReaperRunning.Load() {
		t.Error("a declined trigger must not set the running guard")
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Errorf("expected a not-due log line, got: %q", buf.String())
	}
}

// reaperSweepFake answers the one query the inline auto-close sweep issues —
// the stale-issue SELECT — with a fixed candidate set, so the daemon's handling
// of the sweep's outcomes can be driven without a live Dolt server. Any other
// statement is an error: the sweep may only reach the write path when it is
// meant to, and a fake that quietly accepted an UPDATE would hide a soft refusal
// leaking into a close.
type reaperSweepFake struct {
	mu      sync.Mutex
	writes  []string
	rows    [][]driver.Value
	nextID  uint64
	nextRow int
}

func (s *reaperSweepFake) rowsFor(cutoff time.Time) [][]driver.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stale [][]driver.Value
	for _, row := range s.rows {
		updatedAt, ok := row[2].(time.Time)
		if !ok || !updatedAt.Before(cutoff) {
			continue
		}
		stale = append(stale, row)
	}
	return stale
}

func (s *reaperSweepFake) recordWrite(query string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, query)
}

func (s *reaperSweepFake) recordedWrites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// openReaperSweepFake registers a driver over the fake and returns a handle to
// it, in the shape the daemon's autoCloseDB takes.
func openReaperSweepFake(t *testing.T, staleIssues [][]driver.Value) (*sql.DB, *reaperSweepFake) {
	t.Helper()
	fake := &reaperSweepFake{rows: staleIssues}
	driverName := fmt.Sprintf("fake_wisp_reaper_%d", atomic.AddUint64(&reaperSweepFakeDriverID, 1))
	sql.Register(driverName, &reaperSweepDriver{state: fake})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	return db, fake
}

var reaperSweepFakeDriverID uint64

type reaperSweepDriver struct{ state *reaperSweepFake }

func (d *reaperSweepDriver) Open(string) (driver.Conn, error) {
	return &reaperSweepConn{state: d.state}, nil
}

type reaperSweepConn struct{ state *reaperSweepFake }

func (c *reaperSweepConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not implemented")
}

func (c *reaperSweepConn) Close() error { return nil }

func (c *reaperSweepConn) Begin() (driver.Tx, error) { return reaperSweepTx{}, nil }

func (c *reaperSweepConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *reaperSweepConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "SELECT i.id, i.title, i.updated_at FROM issues i WHERE") {
		return nil, fmt.Errorf("unexpected query on the auto-close sweep: %s", query)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("stale-issue SELECT carried no cutoff argument")
	}
	cutoff, ok := args[0].Value.(time.Time)
	if !ok {
		return nil, fmt.Errorf("stale-issue SELECT cutoff = %#v, want a time", args[0].Value)
	}
	return &reaperSweepRows{rows: c.state.rowsFor(cutoff)}, nil
}

func (c *reaperSweepConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.recordWrite(query)
	return nil, fmt.Errorf("the below-floor sweep must not write, but issued: %s", query)
}

type reaperSweepTx struct{}

func (reaperSweepTx) Commit() error   { return nil }
func (reaperSweepTx) Rollback() error { return nil }

type reaperSweepRows struct {
	rows [][]driver.Value
	next int
}

func (r *reaperSweepRows) Columns() []string { return []string{"id", "title", "updated_at"} }
func (r *reaperSweepRows) Close() error      { return nil }

func (r *reaperSweepRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// TestAutoCloseDBSoftRefusalDoesNotFailTheStep covers the daemon half of
// gt-ecpj. The inline fallback is what a failed Dog dispatch lands on, so a
// below-floor stale-age treated as an error here fails the auto-close step and
// stops the reaper — the stop the soft refusal exists to remove. The sweep must
// instead log the notice, report nothing closed, and leave the issues alone.
func TestAutoCloseDBSoftRefusalDoesNotFailTheStep(t *testing.T) {
	// Two open issues, long past the floor: the below-floor threshold reaches
	// both, so the notice has a real set to name.
	db, fake := openReaperSweepFake(t, [][]driver.Value{
		{"hq-a", "abandoned hq-a", time.Now().UTC().Add(-60 * 24 * time.Hour)},
		{"hq-b", "abandoned hq-b", time.Now().UTC().Add(-60 * 24 * time.Hour)},
	})
	t.Cleanup(func() { _ = db.Close() })

	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0)}

	closed, err := d.autoCloseDB(db, "hq", time.Hour, false)
	if err != nil {
		t.Fatalf("autoCloseDB below the floor returned error %v, want the refusal to be soft", err)
	}
	if closed != 0 {
		t.Errorf("autoCloseDB below the floor closed %d issues, want 0", closed)
	}
	if writes := fake.recordedWrites(); len(writes) != 0 {
		t.Errorf("below-floor sweep issued %d write(s), want none: %v", len(writes), writes)
	}
	logged := buf.String()
	if !strings.Contains(logged, reaper.FloorNotice(time.Hour, 2)) {
		t.Errorf("cycle log = %q, want the below-floor notice naming the 2 candidates that threshold would take", logged)
	}
}

// TestAutoCloseDBCarriesTheRefusalIntoTheLog pins that the daemon reports the
// way the command does: the notice is the refusal, so it has to reach the log a
// patrol reader sees, with the threshold that was asked for and the size of the
// set it would take.
func TestAutoCloseDBCarriesTheRefusalIntoTheLog(t *testing.T) {
	db, _ := openReaperSweepFake(t, [][]driver.Value{
		{"hq-a", "abandoned hq-a", time.Now().UTC().Add(-60 * 24 * time.Hour)},
	})
	t.Cleanup(func() { _ = db.Close() })

	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0)}

	if _, err := d.autoCloseDB(db, "hq", 24*time.Hour, false); err != nil {
		t.Fatalf("autoCloseDB below the floor: %v", err)
	}
	logged := buf.String()
	for _, want := range []string{"wisp_reaper: hq:", "24h0m0s", reaper.MinStaleIssueAge.String(), "1 candidate(s)"} {
		if !strings.Contains(logged, want) {
			t.Errorf("cycle log = %q, want it to carry %q", logged, want)
		}
	}
}

// TestAutoCloseDBDryRunCycleCountsARefusalAsNothing covers the dry-run cycle:
// the sweep returns the set the mis-set threshold would take, and the cycle
// summary must not fold that count in as closed — a live run refuses the same
// threshold, so the number would be a close count for a write that cannot
// happen (gt-ecpj).
func TestAutoCloseDBDryRunCycleCountsARefusalAsNothing(t *testing.T) {
	db, _ := openReaperSweepFake(t, [][]driver.Value{
		{"hq-a", "abandoned hq-a", time.Now().UTC().Add(-60 * 24 * time.Hour)},
	})
	t.Cleanup(func() { _ = db.Close() })

	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0)}

	closed, err := d.autoCloseDB(db, "hq", time.Hour, true)
	if err != nil {
		t.Fatalf("dry-run autoCloseDB below the floor: %v", err)
	}
	if closed != 0 {
		t.Errorf("dry-run cycle counted %d auto-closed, want 0: the threshold is refused", closed)
	}
	if !strings.Contains(buf.String(), reaper.FloorNotice(time.Hour, 1)) {
		t.Errorf("cycle log = %q, want the below-floor notice", buf.String())
	}
}
