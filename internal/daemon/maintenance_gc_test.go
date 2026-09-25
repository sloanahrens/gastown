package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// These tests drive the gc mode entirely through fakes: the size probe, the
// dolt_gc call, the quiet-window probe and the escalation sink are all seams.
// Nothing here opens a SQL connection, touches a real .dolt directory or
// needs Docker. The daemon's town root is always a t.TempDir().

const mib = int64(1024 * 1024)

// --- mode and config -------------------------------------------------------

func TestMaintenanceModeGC(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"gc", MaintenanceModeGC},
		{" GC ", MaintenanceModeGC},
		{"flatten", MaintenanceModeFlatten},
		{"", MaintenanceModeMonitor},
		{"monitor", MaintenanceModeMonitor},
		// Typos fail toward monitor, never toward an action.
		{"gcc", MaintenanceModeMonitor},
		{"gc-full", MaintenanceModeMonitor},
	}
	for _, tc := range cases {
		cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{Mode: tc.mode},
		}}
		if got := maintenanceMode(cfg); got != tc.want {
			t.Errorf("maintenanceMode(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestMaintenanceGCConfigDefaultsAndValidation(t *testing.T) {
	i64 := func(n int64) *int64 { return &n }
	f64 := func(f float64) *float64 { return &f }
	cfg := func(minBytes *int64, ratio *float64) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{GCMinBytes: minBytes, GCGrowthRatio: ratio},
		}}
	}

	cases := []struct {
		name      string
		cfg       *DaemonPatrolConfig
		wantMin   int64
		wantRatio float64
		wantWarn  bool
	}{
		{"nil config", nil, DefaultGCMinBytes, DefaultGCGrowthRatio, false},
		{"unset keys", cfg(nil, nil), DefaultGCMinBytes, DefaultGCGrowthRatio, false},
		{"valid keys", cfg(i64(64*mib), f64(1.5)), 64 * mib, 1.5, false},
		{"ratio exactly 1", cfg(nil, f64(1.0)), DefaultGCMinBytes, 1.0, false},
		{"zero min bytes", cfg(i64(0), nil), DefaultGCMinBytes, DefaultGCGrowthRatio, true},
		{"negative min bytes", cfg(i64(-1), nil), DefaultGCMinBytes, DefaultGCGrowthRatio, true},
		{"ratio below 1", cfg(nil, f64(0.5)), DefaultGCMinBytes, DefaultGCGrowthRatio, true},
		{"ratio NaN", cfg(nil, f64(math.NaN())), DefaultGCMinBytes, DefaultGCGrowthRatio, true},
		{"ratio Inf", cfg(nil, f64(math.Inf(1))), DefaultGCMinBytes, DefaultGCGrowthRatio, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, warnings := maintenanceGCPolicyFor(tc.cfg)
			if p.minBytes != tc.wantMin || p.growthRatio != tc.wantRatio {
				t.Errorf("policy = {min %d, ratio %v}, want {min %d, ratio %v}",
					p.minBytes, p.growthRatio, tc.wantMin, tc.wantRatio)
			}
			if (len(warnings) > 0) != tc.wantWarn {
				t.Errorf("warnings = %v, want warn=%v", warnings, tc.wantWarn)
			}
		})
	}
}

func TestShouldGCDatabase(t *testing.T) {
	p := maintenanceGCPolicy{minBytes: 256 * mib, growthRatio: 2.0}
	cases := []struct {
		name     string
		size     int64
		baseline int64
		want     bool
	}{
		{"below floor, no baseline", 100 * mib, 0, false},
		{"at floor, no baseline", 256 * mib, 0, true},
		{"above floor, no baseline", 600 * mib, 0, true},
		{"above floor, grown under ratio", 900 * mib, 480 * mib, false},
		{"above floor, grown exactly ratio", 960 * mib, 480 * mib, true},
		{"above floor, grown past ratio", 1200 * mib, 480 * mib, true},
		// A small baseline must not let a tiny database churn: the floor
		// still applies.
		{"ratio met but below floor", 200 * mib, 97 * mib, false},
		{"ratio met and floor met", 300 * mib, 97 * mib, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := shouldGCDatabase(tc.size, tc.baseline, p)
			if got != tc.want {
				t.Errorf("shouldGCDatabase(%d, %d) = %v (%s), want %v", tc.size, tc.baseline, got, reason, tc.want)
			}
			if reason == "" {
				t.Error("reason is empty; the log line needs it")
			}
		})
	}
}

// --- state file --------------------------------------------------------------

func TestMaintenanceGCStateRoundTrip(t *testing.T) {
	town := t.TempDir()

	st, err := loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load of a missing state file: %v", err)
	}
	if len(st.PostGCBytes) != 0 {
		t.Fatalf("missing state file yields baselines %v, want none", st.PostGCBytes)
	}

	at := time.Date(2026, 9, 25, 3, 5, 0, 0, time.UTC)
	if err := recordMaintenanceGCBaseline(town, "hq", 97*mib, at); err != nil {
		t.Fatalf("record hq: %v", err)
	}
	if err := recordMaintenanceGCBaseline(town, "gt", 480*mib, at); err != nil {
		t.Fatalf("record gt: %v", err)
	}

	st, err = loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.PostGCBytes["hq"] != 97*mib || st.PostGCBytes["gt"] != 480*mib {
		t.Errorf("baselines = %v, want hq=%d gt=%d", st.PostGCBytes, 97*mib, 480*mib)
	}
	if !st.LastGC["gt"].Equal(at) {
		t.Errorf("LastGC[gt] = %v, want %v", st.LastGC["gt"], at)
	}

	// The file lives in the daemon dir and nothing else is left behind (no
	// temp file from the atomic write).
	entries, err := os.ReadDir(filepath.Join(town, "daemon"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != maintenanceGCStateFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("daemon dir holds %v, want only %s", names, maintenanceGCStateFileName)
	}
}

func TestMaintenanceGCStateCorruptIsAnErrorAndRepairable(t *testing.T) {
	town := t.TempDir()
	path := maintenanceGCStatePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMaintenanceGCState(town); err == nil {
		t.Fatal("corrupt state file loaded without error")
	}
	// A record after corruption replaces the file rather than failing forever.
	if err := recordMaintenanceGCBaseline(town, "hq", mib, time.Now()); err != nil {
		t.Fatalf("record over corrupt file: %v", err)
	}
	st, err := loadMaintenanceGCState(town)
	if err != nil || st.PostGCBytes["hq"] != mib {
		t.Fatalf("after repair: state=%v err=%v", st, err)
	}
}

// --- size probe --------------------------------------------------------------

func TestMaintenanceDBSize(t *testing.T) {
	dataDir := t.TempDir()
	db := filepath.Join(dataDir, "hq", ".dolt", "noms")
	if err := os.MkdirAll(filepath.Join(db, "oldgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "a"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "oldgen", "b"), make([]byte, 234), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := maintenanceDBSize(dataDir, "hq")
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if got != 1234 {
		t.Errorf("size = %d, want 1234 (oldgen must count: gc --full is what empties it)", got)
	}

	if _, err := maintenanceDBSize(dataDir, "missing"); err == nil {
		t.Error("size of a missing database returned no error")
	}
	// A name that escapes the data dir is refused rather than walked.
	if _, err := maintenanceDBSize(dataDir, "../etc"); err == nil {
		t.Error("size of a path-escaping database name returned no error")
	}
}

// --- the cycle ----------------------------------------------------------------

// gcFakes records what a gc cycle did through its seams.
type gcFakes struct {
	sizes       map[string]int64 // current size per database
	shrinkTo    map[string]int64 // size after a successful gc
	gcErr       map[string]error
	gcCalls     []string
	quietCalls  int
	quietUntil  int // quiet for this many calls, then busy; -1 = always quiet
	escalations []string
	flattens    int
	pauses      int
	resumes     int
	pauseFails  bool
}

func withGCFakes(t *testing.T) *gcFakes {
	t.Helper()
	f := &gcFakes{sizes: map[string]int64{}, shrinkTo: map[string]int64{}, gcErr: map[string]error{}, quietUntil: -1}

	prevSize, prevGC, prevQuiet := maintenanceDBSizeFn, maintenanceGCExecFn, maintenanceQuietFn
	prevEsc, prevExec, prevDispatch := maintenanceEscalateFn, maintenanceExecFn, maintenanceGCDispatchFn
	prevDBs, prevExternal, prevPause := maintenanceGCDatabasesFn, maintenanceGCExternalFn, maintenanceConvoyPauseFn

	// Discovery returns the databases the fake sizes know about.
	maintenanceGCDatabasesFn = func(string) ([]string, error) {
		var dbs []string
		for db := range f.sizes {
			dbs = append(dbs, db)
		}
		sort.Strings(dbs)
		return dbs, nil
	}
	maintenanceGCExternalFn = func(*Daemon) (bool, string) { return false, "" }
	maintenanceConvoyPauseFn = func(*Daemon) (func(), bool) {
		f.pauses++
		if f.pauseFails {
			return nil, false
		}
		return func() { f.resumes++ }, true
	}

	maintenanceDBSizeFn = func(_ string, db string) (int64, error) {
		s, ok := f.sizes[db]
		if !ok {
			return 0, os.ErrNotExist
		}
		return s, nil
	}
	maintenanceGCExecFn = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		if err := f.gcErr[db]; err != nil {
			return err
		}
		if s, ok := f.shrinkTo[db]; ok {
			f.sizes[db] = s
		}
		return nil
	}
	maintenanceQuietFn = func(*Daemon) (bool, string) {
		f.quietCalls++
		if f.quietUntil >= 0 && f.quietCalls > f.quietUntil {
			return false, "refinery holds gate slot 0"
		}
		return true, ""
	}
	maintenanceEscalateFn = func(_ *Daemon, source, message string) {
		f.escalations = append(f.escalations, source+"|"+message)
	}
	maintenanceExecFn = func(context.Context, string, string, int) ([]byte, error) {
		f.flattens++
		return nil, nil
	}
	// Run the dispatched cycle inline so the test observes its result.
	maintenanceGCDispatchFn = func(fn func()) { fn() }

	t.Cleanup(func() {
		maintenanceDBSizeFn, maintenanceGCExecFn, maintenanceQuietFn = prevSize, prevGC, prevQuiet
		maintenanceEscalateFn, maintenanceExecFn, maintenanceGCDispatchFn = prevEsc, prevExec, prevDispatch
		maintenanceGCDatabasesFn, maintenanceGCExternalFn, maintenanceConvoyPauseFn = prevDBs, prevExternal, prevPause
	})
	return f
}

func gcTestDaemon(t *testing.T) (*Daemon, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&buf, "", 0),
	}, &buf
}

var testGCPolicy = maintenanceGCPolicy{minBytes: 256 * mib, growthRatio: 2.0}

func TestMaintenanceGCCycleCollectsEligibleSmallestFirst(t *testing.T) {
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"gt": 633 * mib, "hq": 631 * mib, "om": 185 * mib, "beads": 1 * mib}
	f.shrinkTo = map[string]int64{"gt": 480 * mib, "hq": 97 * mib}

	out := d.maintenanceGCCycle([]string{"gt", "hq", "om", "beads"}, "/unused", testGCPolicy)

	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq,gt" {
		t.Errorf("gc calls = %s, want hq,gt (eligible only, smallest first)", got)
	}
	if f.flattens != 0 {
		t.Errorf("gc mode ran gt maintain %d time(s); gc must never flatten", f.flattens)
	}
	if len(f.escalations) != 0 {
		t.Errorf("successful gc escalated: %v", f.escalations)
	}
	// Quiet is re-checked before each database gc'd.
	if f.quietCalls != 2 {
		t.Errorf("quiet probe ran %d time(s), want 2 (once before each gc)", f.quietCalls)
	}

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.PostGCBytes["hq"] != 97*mib || st.PostGCBytes["gt"] != 480*mib {
		t.Errorf("baselines = %v, want post-gc sizes hq=97MiB gt=480MiB", st.PostGCBytes)
	}
	if _, ok := st.PostGCBytes["om"]; ok {
		t.Error("om was not gc'd but got a baseline")
	}

	// Diagnostics: before/after size and duration per database.
	for _, want := range []string{"gc hq: 631.0MiB -> 97.0MiB", "gc gt: 633.0MiB -> 480.0MiB", "om: 185.0MiB"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestMaintenanceGCCycleRespectsBaseline(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	// gt was gc'd to 480MiB last time; 900MiB is under 2x, hq has doubled.
	if err := recordMaintenanceGCBaseline(d.config.TownRoot, "gt", 480*mib, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := recordMaintenanceGCBaseline(d.config.TownRoot, "hq", 150*mib, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.sizes = map[string]int64{"gt": 900 * mib, "hq": 300 * mib}

	out := d.maintenanceGCCycle([]string{"gt", "hq"}, "/unused", testGCPolicy)

	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq only (gt has not doubled since its last gc)", got)
	}
}

func TestMaintenanceGCCycleNothingEligibleSkipsQuietProbe(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"om": 42 * mib}

	if out := d.maintenanceGCCycle([]string{"om"}, "/unused", testGCPolicy); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if len(f.gcCalls) != 0 || f.quietCalls != 0 {
		t.Errorf("nothing eligible, yet gc=%v quietCalls=%d", f.gcCalls, f.quietCalls)
	}
}

func TestMaintenanceGCCycleDefersWhenNotQuiet(t *testing.T) {
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib, "gt": 600 * mib, "om": 700 * mib}
	f.quietUntil = 1 // quiet before hq, busy before gt

	out := d.maintenanceGCCycle([]string{"hq", "gt", "om"}, "/unused", testGCPolicy)

	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq only — the run must stop when the town gets busy", got)
	}
	if len(f.escalations) != 0 {
		t.Errorf("a deferral escalated: %v", f.escalations)
	}
	if !strings.Contains(logs.String(), "refinery holds gate slot 0") {
		t.Errorf("deferral log does not say why:\n%s", logs.String())
	}
}

func TestMaintenanceGCCycleStopsAndEscalatesOnceOnError(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib, "gt": 600 * mib, "om": 700 * mib}
	f.gcErr["gt"] = errors.New("Error 1105: gc failed: boom")

	out := d.maintenanceGCCycle([]string{"hq", "gt", "om"}, "/unused", testGCPolicy)

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq,gt" {
		t.Errorf("gc calls = %s, want hq,gt — a failure must stop the run", got)
	}
	if len(f.escalations) != 1 {
		t.Fatalf("escalations = %d, want exactly 1: %v", len(f.escalations), f.escalations)
	}
	msg := f.escalations[0]
	for _, want := range []string{"scheduled_maintenance|", "gt", "boom", "600.0MiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("escalation missing %q:\n%s", want, msg)
		}
	}
	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if _, ok := st.PostGCBytes["gt"]; ok {
		t.Error("a failed gc recorded a baseline")
	}
	if st.PostGCBytes["hq"] != 300*mib {
		t.Errorf("hq baseline = %d, want its post-gc size", st.PostGCBytes["hq"])
	}
}

func TestMaintenanceGCCycleSizeErrorSkipsDatabase(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib} // "ghost" has no directory

	out := d.maintenanceGCCycle([]string{"ghost", "hq"}, "/unused", testGCPolicy)
	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
}

// --- wiring through runScheduledMaintenance ---------------------------------

func gcModeConfig(dbs []string, mode string) *DaemonPatrolConfig {
	now := time.Now()
	window := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location()).Format("15:04")
	minBytes := 256 * mib
	return &DaemonPatrolConfig{
		Type: "daemon-patrol-config", Version: 1,
		Patrols: &PatrolsConfig{
			CompactorDog: &CompactorDogConfig{Databases: dbs},
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled: true, Window: window, Interval: "daily", Mode: mode, GCMinBytes: &minBytes,
			},
		},
	}
}

func TestScheduledMaintenanceGCModeNeverFlattensAndMarksRun(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 631 * mib}
	f.shrinkTo = map[string]int64{"hq": 97 * mib}
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)

	d.runScheduledMaintenance()

	if f.flattens != 0 {
		t.Errorf("gc mode ran gt maintain %d time(s)", f.flattens)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
	if d.maintenanceGCRunning.Load() {
		t.Error("gc cycle still marked running after it returned")
	}

	// The completion is folded into lastMaintenanceRun on the next tick, so a
	// second tick in the same window does nothing.
	f.sizes["hq"] = 900 * mib
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 1 {
		t.Errorf("second tick in the same window ran gc again: %v", f.gcCalls)
	}
}

func TestScheduledMaintenanceGCModeDeferralRetriesNextTick(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 631 * mib}
	f.quietUntil = 0 // busy
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Fatalf("gc ran while the town was busy: %v", f.gcCalls)
	}

	f.quietUntil = -1 // town goes quiet
	d.runScheduledMaintenance()
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("after a deferral the next tick ran gc on %q, want hq", got)
	}
}

func TestScheduledMaintenanceGCModeFailureDoesNotRetryInWindow(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 631 * mib}
	f.gcErr["hq"] = errors.New("boom")
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)

	d.runScheduledMaintenance()
	d.runScheduledMaintenance()

	if len(f.gcCalls) != 1 || len(f.escalations) != 1 {
		t.Errorf("after a failure: gc calls %v, escalations %d — want one of each per interval",
			f.gcCalls, len(f.escalations))
	}
}

func TestScheduledMaintenanceGCModeSkipsWhileCycleRunning(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 631 * mib}
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)
	d.maintenanceGCRunning.Store(true)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Errorf("a second cycle started while one was running: %v", f.gcCalls)
	}
}

// --- the quiet-window probe ---------------------------------------------------

func TestMaintenanceQuiet(t *testing.T) {
	prevSlots, prevPolecats := maintenanceSlotHoldersFn, maintenanceWorkingPolecatsFn
	t.Cleanup(func() { maintenanceSlotHoldersFn, maintenanceWorkingPolecatsFn = prevSlots, prevPolecats })

	cases := []struct {
		name     string
		set      func(d *Daemon)
		slots    func(string) ([]string, error)
		polecats func(*Daemon) ([]string, error)
		want     bool
		reason   string
	}{
		{"all clear", func(*Daemon) {}, nil, nil, true, ""},
		{"compactor running", func(d *Daemon) { d.compactorDogRunning = true }, nil, nil, false, "daemon"},
		{"main branch test waiting on a slot", func(d *Daemon) {
			d.mainBranchTestRunning.Store(true)
			d.mainBranchTestWaitingSlot.Store(true)
		}, nil, nil, false, "main_branch_test"},
		{"gate slot held", func(*Daemon) {},
			func(string) ([]string, error) { return []string{"gastown/refinery"}, nil }, nil, false, "gastown/refinery"},
		{"slot probe fails", func(*Daemon) {},
			func(string) ([]string, error) { return nil, errors.New("flock: permission denied") }, nil, false, "slot"},
		{"polecat working", func(*Daemon) {}, nil,
			func(*Daemon) ([]string, error) { return []string{"gastown/opal"}, nil }, false, "gastown/opal"},
		{"polecat probe fails", func(*Daemon) {}, nil,
			func(*Daemon) ([]string, error) { return nil, errors.New("rig gastown: permission denied") }, false, "cannot list polecats"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maintenanceSlotHoldersFn = func(string) ([]string, error) { return nil, nil }
			if tc.slots != nil {
				maintenanceSlotHoldersFn = tc.slots
			}
			maintenanceWorkingPolecatsFn = func(*Daemon) ([]string, error) { return nil, nil }
			if tc.polecats != nil {
				maintenanceWorkingPolecatsFn = tc.polecats
			}
			d := idleTestDaemon(t)
			tc.set(d)
			quiet, reason := d.maintenanceQuiet()
			if quiet != tc.want {
				t.Fatalf("maintenanceQuiet() = %v (%s), want %v", quiet, reason, tc.want)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", reason, tc.reason)
			}
		})
	}
}

// The gc cycle holds off a daemon upgrade-restart, but must not block its own
// quiet probe (which would make every run defer forever).
func TestMaintenanceGCRunningBlocksUpgradeButNotItsOwnQuietProbe(t *testing.T) {
	prevSlots, prevPolecats := maintenanceSlotHoldersFn, maintenanceWorkingPolecatsFn
	t.Cleanup(func() { maintenanceSlotHoldersFn, maintenanceWorkingPolecatsFn = prevSlots, prevPolecats })
	maintenanceSlotHoldersFn = func(string) ([]string, error) { return nil, nil }
	maintenanceWorkingPolecatsFn = func(*Daemon) ([]string, error) { return nil, nil }

	d := idleTestDaemon(t)
	d.maintenanceGCRunning.Store(true)
	if d.isIdleForUpgrade() {
		t.Error("isIdleForUpgrade() = true while a gc cycle runs")
	}
	if quiet, reason := d.maintenanceQuiet(); !quiet {
		t.Errorf("maintenanceQuiet() = false (%s) — the cycle is blocking itself", reason)
	}
}

// writeTestRigsJSON registers rigs in <town>/mayor/rigs.json.
func writeTestRigsJSON(t *testing.T, town string, rigNames ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := map[string]any{}
	for _, r := range rigNames {
		entries[r] = map[string]any{}
	}
	data, _ := json.Marshal(map[string]any{"rigs": entries})
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A rig whose polecats directory cannot be listed (here: it is a file, so
// ReadDir fails with ENOTDIR rather than not-exist) must make the town read
// busy: the guard cannot tell whether a polecat is working there. A rig with
// no polecats directory at all is simply a rig without polecats.
func TestMaintenanceQuietFailsClosedWhenPolecatListingFails(t *testing.T) {
	prevSlots := maintenanceSlotHoldersFn
	t.Cleanup(func() { maintenanceSlotHoldersFn = prevSlots })
	maintenanceSlotHoldersFn = func(string) ([]string, error) { return nil, nil }

	d := idleTestDaemon(t)
	town := d.config.TownRoot
	writeTestRigsJSON(t, town, "broken", "nopolecats")
	if err := os.MkdirAll(filepath.Join(town, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "broken", "polecats"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	quiet, reason := d.maintenanceQuiet()
	if quiet {
		t.Fatal("maintenanceQuiet() = true although a rig's polecats could not be listed; want fail closed")
	}
	if !strings.Contains(reason, "broken") {
		t.Errorf("reason %q does not name the rig that could not be listed", reason)
	}

	// The not-exist case alone stays quiet.
	d2 := idleTestDaemon(t)
	writeTestRigsJSON(t, d2.config.TownRoot, "nopolecats")
	if quiet, reason := d2.maintenanceQuiet(); !quiet {
		t.Errorf("maintenanceQuiet() = false (%s) for a rig with no polecats directory", reason)
	}
}

func TestWorkingPolecatsFromHeartbeats(t *testing.T) {
	town := t.TempDir()
	rig := "testrig"
	writeTestRigsJSON(t, town, rig)
	for _, p := range []string{"fresh", "stale", "idle", "nobeat"} {
		if err := os.MkdirAll(filepath.Join(town, rig, "polecats", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sess := func(p string) string { return session.PolecatSessionName(session.PrefixFor(rig), p) }
	polecat.TouchSessionHeartbeatWithState(town, sess("fresh"), polecat.HeartbeatWorking, "", "")
	polecat.TouchSessionHeartbeatWithState(town, sess("idle"), polecat.HeartbeatIdle, "", "")
	stale := polecat.SessionHeartbeat{Timestamp: time.Now().Add(-2 * maintenancePolecatFreshness), State: polecat.HeartbeatWorking}
	staleData, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(town, ".runtime", "heartbeats", sess("stale")+".json"), staleData, 0o644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	got, err := d.workingPolecats()
	if err != nil {
		t.Fatalf("workingPolecats() error: %v", err)
	}
	if strings.Join(got, ",") != rig+"/fresh" {
		t.Errorf("workingPolecats() = %v, want [%s/fresh]", got, rig)
	}
}
