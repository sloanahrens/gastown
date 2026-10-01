package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// These tests drive the gc entirely through fakes: the size probe, the
// dolt_gc call, the clock, the pause marker, the quiet-window probe and the
// escalation sink are all seams. Nothing here opens a SQL connection, touches
// a real .dolt directory or needs Docker. The daemon's town root is always a
// t.TempDir().

const mib = int64(1024 * 1024)

// gcTestNow is the fake clock's reading.
var gcTestNow = time.Date(2026, 10, 1, 3, 10, 0, 0, time.UTC)

// --- the triggers -------------------------------------------------------------

func TestShouldGCDatabase(t *testing.T) {
	t.Parallel()
	now := gcTestNow
	days := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	cases := []struct {
		name     string
		lastGC   time.Time
		oldGen   int64
		baseline int64
		want     bool
	}{
		{"never gc'd", time.Time{}, 10 * mib, 0, true},
		{"weekly: 7 days since last gc", days(7), 100 * mib, 100 * mib, true},
		{"weekly: in the window a day short of a week ahead", now.Add(-maintenanceGCWeekly), 100 * mib, 100 * mib, true},
		{"6 days, old-gen unchanged", days(6), 100 * mib, 100 * mib, false},
		{"2 days, old-gen grew 10%", days(2), 110 * mib, 100 * mib, false},
		{"2 days, old-gen grew exactly 20%", days(2), 120 * mib, 100 * mib, false},
		{"2 days, old-gen grew 25%", days(2), 125 * mib, 100 * mib, true},
		{"1 day, old-gen shrank", days(1), 80 * mib, 100 * mib, false},
		{"1 day, no old-gen before or now", days(1), 0, 0, false},
		{"1 day, old-gen appeared since an empty baseline", days(1), mib, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := shouldGCDatabase(now, tc.lastGC, tc.oldGen, tc.baseline)
			if got != tc.want {
				t.Errorf("shouldGCDatabase = %v (%s), want %v", got, reason, tc.want)
			}
			if reason == "" {
				t.Error("reason is empty; the log line needs it")
			}
		})
	}
}

// --- state file --------------------------------------------------------------

func TestMaintenanceGCStateRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	st, err := loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load of a missing state file: %v", err)
	}
	if len(st.PostGCBytes) != 0 || len(st.LastGC) != 0 {
		t.Fatalf("missing state file yields %v / %v, want none", st.PostGCBytes, st.LastGC)
	}

	at := gcTestNow
	if err := recordMaintenanceGCRun(town, "hq", gcMeasure{631 * mib, 500 * mib}, gcMeasure{97 * mib, 90 * mib}, at); err != nil {
		t.Fatalf("record hq: %v", err)
	}
	if err := recordMaintenanceGCRun(town, "gt", gcMeasure{633 * mib, 300 * mib}, gcMeasure{480 * mib, 307 * mib}, at); err != nil {
		t.Fatalf("record gt: %v", err)
	}

	st, err = loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.PostGCBytes["hq"] != 97*mib || st.PostGCBytes["gt"] != 480*mib {
		t.Errorf("post_gc_bytes = %v, want hq=97MiB gt=480MiB", st.PostGCBytes)
	}
	if st.PostGCOldGenBytes["hq"] != 90*mib || st.PostGCOldGenBytes["gt"] != 307*mib {
		t.Errorf("post_gc_oldgen_bytes = %v, want hq=90MiB gt=307MiB", st.PostGCOldGenBytes)
	}
	if st.ReclaimedBytes["hq"] != 534*mib || st.ReclaimedBytes["gt"] != 153*mib {
		t.Errorf("reclaimed_bytes = %v, want hq=534MiB gt=153MiB", st.ReclaimedBytes)
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
	t.Parallel()
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
	if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
		t.Fatalf("record over corrupt file: %v", err)
	}
	st, err := loadMaintenanceGCState(town)
	if err != nil || st.PostGCBytes["hq"] != mib {
		t.Fatalf("after repair: state=%v err=%v", st, err)
	}
}

// --- size probe --------------------------------------------------------------

func TestMaintenanceMeasure(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	noms := filepath.Join(dataDir, "hq", ".dolt", "noms")
	if err := os.MkdirAll(filepath.Join(noms, "oldgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noms, "a"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noms, "oldgen", "b"), make([]byte, 234), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "fresh", ".dolt", "noms"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := maintenanceMeasure(dataDir, "hq")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if got.total != 1234 || got.oldGen != 234 {
		t.Errorf("measure = %+v, want total 1234 (oldgen counts) and oldGen 234", got)
	}

	// A database that has never had a gc --full has no oldgen directory.
	if got, err := maintenanceMeasure(dataDir, "fresh"); err != nil || got.oldGen != 0 {
		t.Errorf("measure of a database without oldgen = %+v, %v; want oldGen 0, no error", got, err)
	}
	if _, err := maintenanceMeasure(dataDir, "missing"); err == nil {
		t.Error("measure of a missing database returned no error")
	}
	// A name that escapes the data dir is refused rather than walked.
	if _, err := maintenanceMeasure(dataDir, "../etc"); err == nil {
		t.Error("measure of a path-escaping database name returned no error")
	}
}

// --- the cycle ----------------------------------------------------------------

// gcFakes records what a gc cycle did through its seams.
type gcFakes struct {
	sizes       map[string]gcMeasure // current sizes per database
	shrinkTo    map[string]gcMeasure // sizes after a successful gc
	gcErr       map[string]error
	gcCalls     []string
	quietCalls  int
	quietUntil  int // quiet for this many calls, then busy; -1 = always quiet
	escalations []string
	pauses      int
	resumes     int
	pauseFails  bool

	// The nightly backup: backupRoot is a temp dir, backupCalls the
	// databases copied, backupErr a failure per database.
	backupRoot  string
	backupCalls []string
	backupErr   map[string]error

	// The pause marker: events records "write <db>", "gc <db>" and
	// "remove" in order; marker is the one on "disk".
	events       []string
	marker       *doltpause.Marker
	written      []doltpause.Marker
	foreignPause *doltpause.Marker
	writeErr     error
	removeErr    error
}

func withGCFakes(t *testing.T, d *Daemon) *gcFakes {
	t.Helper()
	f := &gcFakes{sizes: map[string]gcMeasure{}, shrinkTo: map[string]gcMeasure{}, gcErr: map[string]error{}, quietUntil: -1,
		backupRoot: t.TempDir(), backupErr: map[string]error{}}

	// Discovery returns the databases the fake sizes know about.
	d.maint.gcDatabases = func(string) ([]string, error) {
		var dbs []string
		for db := range f.sizes {
			dbs = append(dbs, db)
		}
		sort.Strings(dbs)
		return dbs, nil
	}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return false, "" }
	d.maint.convoyPause = func(*Daemon) (func(), bool) {
		f.pauses++
		if f.pauseFails {
			return nil, false
		}
		return func() { f.resumes++ }, true
	}
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.pauseCurrent = func(string, time.Time) *doltpause.Marker { return f.foreignPause }
	d.maint.pauseWrite = func(_ string, m doltpause.Marker) error {
		if f.writeErr != nil {
			return f.writeErr
		}
		f.events = append(f.events, "write "+strings.TrimPrefix(m.Reason, "dolt_gc --full on "))
		f.marker = &m
		f.written = append(f.written, m)
		return nil
	}
	d.maint.pauseRemove = func(string) (bool, error) {
		f.events = append(f.events, "remove")
		if f.removeErr != nil {
			return false, f.removeErr
		}
		had := f.marker != nil
		f.marker = nil
		return had, nil
	}

	d.maint.measure = func(_ string, db string) (gcMeasure, error) {
		s, ok := f.sizes[db]
		if !ok {
			return gcMeasure{}, os.ErrNotExist
		}
		return s, nil
	}
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		f.events = append(f.events, "gc "+db)
		if f.marker == nil {
			t.Errorf("gc of %s ran with no pause marker written", db)
		}
		if err := f.gcErr[db]; err != nil {
			return err
		}
		if s, ok := f.shrinkTo[db]; ok {
			f.sizes[db] = s
		}
		return nil
	}
	d.maint.backupRoot = func() (string, error) { return f.backupRoot, nil }
	d.maint.backupExec = func(_ context.Context, _ *Daemon, db, dest string) error {
		f.backupCalls = append(f.backupCalls, db)
		f.events = append(f.events, "backup "+db)
		if f.marker == nil {
			t.Errorf("backup of %s ran with no pause marker written", db)
		}
		if err := f.backupErr[db]; err != nil {
			return err
		}
		// What the server's sync-url leaves behind.
		return os.MkdirAll(dest, 0o755)
	}
	d.maint.quiet = func(*Daemon) (bool, string) {
		f.quietCalls++
		if f.quietUntil >= 0 && f.quietCalls > f.quietUntil {
			return false, "refinery holds gate slot 0"
		}
		return true, ""
	}
	d.maint.escalate = func(_ *Daemon, source, message string) {
		f.escalations = append(f.escalations, source+"|"+message)
	}
	// Run the dispatched cycle inline so the test observes its result.
	d.maint.dispatch = func(fn func()) { fn() }

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

// recordRun seeds db's last patrol gc: daysAgo before the fake clock, with
// oldGen as its post-gc old-gen size.
func recordRun(t *testing.T, d *Daemon, db string, daysAgo int, oldGen int64) {
	t.Helper()
	at := gcTestNow.Add(-time.Duration(daysAgo) * 24 * time.Hour)
	if err := recordMaintenanceGCRun(d.config.TownRoot, db, gcMeasure{}, gcMeasure{total: oldGen, oldGen: oldGen}, at); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceGCCycleCollectsDueSmallestFirstUnderPause(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 300 * mib}, "hq": {631 * mib, 500 * mib}}
	f.shrinkTo = map[string]gcMeasure{"gt": {480 * mib, 307 * mib}, "hq": {97 * mib, 90 * mib}}

	out := d.maintenanceGCCycle([]string{"gt", "hq"}, "/unused")

	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	// Smallest first, each gc between its own marker write and remove.
	want := "write hq,gc hq,remove,write gt,gc gt,remove"
	if got := strings.Join(f.events, ","); got != want {
		t.Errorf("events = %s, want %s", got, want)
	}
	if f.marker != nil {
		t.Errorf("pause marker left behind: %+v", f.marker)
	}
	for _, m := range f.written {
		if m.Actor != maintenancePauseActor {
			t.Errorf("marker actor = %q, want %q", m.Actor, maintenancePauseActor)
		}
		if !m.Since.Equal(gcTestNow) || !m.Until.Equal(gcTestNow.Add(maintenanceGCTimeout+maintenancePauseSlack)) {
			t.Errorf("marker since/until = %v/%v, want now and now+gc timeout+slack", m.Since, m.Until)
		}
		if m.Until.Sub(m.Since) > doltpause.MaxDuration {
			t.Errorf("marker runs %v, over the doltpause cap", m.Until.Sub(m.Since))
		}
	}
	if len(f.escalations) != 0 {
		t.Errorf("successful gc escalated: %v", f.escalations)
	}
	if f.quietCalls != 2 {
		t.Errorf("quiet probe ran %d time(s), want 2 (once before each gc)", f.quietCalls)
	}

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.PostGCBytes["hq"] != 97*mib || st.PostGCOldGenBytes["gt"] != 307*mib || st.ReclaimedBytes["hq"] != 534*mib {
		t.Errorf("state = %+v, want hq post 97MiB reclaimed 534MiB, gt old-gen 307MiB", st)
	}
	if !st.LastGC["gt"].Equal(gcTestNow) {
		t.Errorf("LastGC[gt] = %v, want the fake clock", st.LastGC["gt"])
	}
	for _, want := range []string{"gc hq: 631.0MiB -> 97.0MiB (old-gen 500.0MiB -> 90.0MiB)", "no patrol gc recorded yet"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestMaintenanceGCCycleWeeklyAndOldGenTriggers(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	recordRun(t, d, "weekly", 7, 100*mib) // a week old, old-gen flat
	recordRun(t, d, "grown", 2, 100*mib)  // two days old, old-gen +30%
	recordRun(t, d, "steady", 2, 100*mib) // two days old, old-gen +5%
	f.sizes = map[string]gcMeasure{
		"weekly": {300 * mib, 100 * mib},
		"grown":  {200 * mib, 130 * mib},
		"steady": {150 * mib, 105 * mib},
	}

	if out := d.maintenanceGCCycle([]string{"grown", "steady", "weekly"}, "/unused"); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "grown,weekly" {
		t.Errorf("gc calls = %s, want grown,weekly (steady is neither a week old nor >20%% grown)", got)
	}
}

func TestMaintenanceGCCycleNothingDueSkipsQuietProbeAndPause(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	recordRun(t, d, "om", 1, 42*mib)
	f.sizes = map[string]gcMeasure{"om": {50 * mib, 42 * mib}}

	if out := d.maintenanceGCCycle([]string{"om"}, "/unused"); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if len(f.gcCalls) != 0 || f.quietCalls != 0 || len(f.events) != 0 {
		t.Errorf("nothing due, yet gc=%v quietCalls=%d events=%v", f.gcCalls, f.quietCalls, f.events)
	}
}

func TestMaintenanceGCCycleDefersWhenNotQuiet(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}, "om": {700 * mib, 0}}
	f.quietUntil = 1 // quiet before hq, busy before gt

	out := d.maintenanceGCCycle([]string{"hq", "gt", "om"}, "/unused")

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

// Someone else's pause (the nightly backup, an operator) defers the gc and is
// left exactly as it was: the gc neither overwrites nor removes it.
func TestMaintenanceGCCycleDefersToAnotherActorsPause(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	f.foreignPause = &doltpause.Marker{Actor: "sloan", Reason: "restore drill", Until: gcTestNow.Add(time.Hour)}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused")

	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred", out.outcome)
	}
	if len(f.gcCalls) != 0 || len(f.events) != 0 {
		t.Errorf("gc touched a pause it does not own: gc=%v events=%v", f.gcCalls, f.events)
	}
	if !strings.Contains(out.reason, "Dolt paused by sloan") {
		t.Errorf("deferral reason %q does not name the pause", out.reason)
	}
}

func TestMaintenanceGCCycleStopsEscalatesAndUnpausesOnError(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}, "om": {700 * mib, 0}}
	f.gcErr["gt"] = errors.New("Error 1105: gc failed: boom")

	out := d.maintenanceGCCycle([]string{"hq", "gt", "om"}, "/unused")

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq,gt" {
		t.Errorf("gc calls = %s, want hq,gt — a failure must stop the run", got)
	}
	if f.marker != nil || f.events[len(f.events)-1] != "remove" {
		t.Errorf("failed gc left the pause marker: events=%v", f.events)
	}
	if len(f.escalations) != 1 {
		t.Fatalf("escalations = %d, want exactly 1: %v", len(f.escalations), f.escalations)
	}
	for _, want := range []string{"scheduled_maintenance|", "gt", "boom", "600.0MiB"} {
		if !strings.Contains(f.escalations[0], want) {
			t.Errorf("escalation missing %q:\n%s", want, f.escalations[0])
		}
	}
	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if _, ok := st.LastGC["gt"]; ok {
		t.Error("a failed gc recorded a run")
	}
	if _, ok := st.LastGC["hq"]; !ok {
		t.Error("hq's successful gc was not recorded")
	}
}

// No marker, no gc: a gc the clients were not told about is the outage the
// marker exists to prevent.
func TestMaintenanceGCCycleRefusesGCWhenPauseCannotBeWritten(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	f.writeErr = errors.New("read-only file system")

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused")

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran without a pause marker: %v", f.gcCalls)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "pause marker") {
		t.Errorf("escalations = %v, want one naming the pause marker", f.escalations)
	}
}

// A marker the daemon cannot remove is logged and left to lapse at its until,
// which the marker sets within the gc bound: the gc itself still counts.
func TestMaintenanceGCCycleUnremovablePauseLapses(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	f.removeErr = errors.New("permission denied")

	if out := d.maintenanceGCCycle([]string{"hq"}, "/unused"); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if !strings.Contains(logs.String(), "cannot remove pause marker") {
		t.Errorf("log does not report the stuck marker:\n%s", logs.String())
	}
	if f.marker == nil || f.marker.Active(gcTestNow.Add(maintenanceGCTimeout+maintenancePauseSlack)) {
		t.Errorf("stuck marker %+v must lapse by the gc timeout plus slack", f.marker)
	}
}

func TestMaintenanceGCCycleSizeErrorSkipsDatabase(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}} // "ghost" has no directory

	out := d.maintenanceGCCycle([]string{"ghost", "hq"}, "/unused")
	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
}

// --- wiring through runScheduledMaintenance ---------------------------------

// gcModeConfig is a config whose window is open at the fake clock's reading.
func gcModeConfig() *DaemonPatrolConfig {
	window := gcTestNow.Truncate(time.Hour).Format("15:04")
	return &DaemonPatrolConfig{
		Type: "daemon-patrol-config", Version: 1,
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{Enabled: true, Window: window},
		},
	}
}

func TestScheduledMaintenanceRunsGCAndMarksRun(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 500 * mib}}
	f.shrinkTo = map[string]gcMeasure{"hq": {97 * mib, 90 * mib}}
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
	if d.maintenanceGCRunning.Load() {
		t.Error("gc cycle still marked running after it returned")
	}

	// The completion is folded into lastMaintenanceRun, so a second tick in
	// the same window does nothing even though old-gen grew.
	f.sizes["hq"] = gcMeasure{900 * mib, 800 * mib}
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 1 {
		t.Errorf("second tick in the same window ran gc again: %v", f.gcCalls)
	}
}

// A live daemon.json still carries the retired mode keys: they decode, change
// nothing, and are named in the log.
func TestScheduledMaintenanceIgnoresAndNamesRetiredKeys(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	recordRun(t, d, "hq", 1, 100*mib)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 105 * mib}}
	cfg := gcModeConfig()
	threshold, minBytes, ratio := 1, int64(1), 1.0
	sm := cfg.Patrols.ScheduledMaintenance
	sm.Interval, sm.Threshold, sm.Mode, sm.GCMinBytes, sm.GCGrowthRatio = "daily", &threshold, "flatten", &minBytes, &ratio
	d.patrolConfig = cfg

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("retired keys changed the schedule: gc ran on %v", f.gcCalls)
	}
	if !strings.Contains(logs.String(), "deprecated config ignored — patrols.scheduled_maintenance.{interval,threshold,mode,gc_min_bytes,gc_growth_ratio}") {
		t.Errorf("log does not name the retired keys:\n%s", logs.String())
	}
}

func TestScheduledMaintenanceDeferralRetriesNextTick(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	f.quietUntil = 0 // busy
	d.patrolConfig = gcModeConfig()

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

func TestScheduledMaintenanceFailureDoesNotRetryInWindow(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	f.gcErr["hq"] = errors.New("boom")
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	d.runScheduledMaintenance()

	if len(f.gcCalls) != 1 || len(f.escalations) != 1 {
		t.Errorf("after a failure: gc calls %v, escalations %d — want one of each per window",
			f.gcCalls, len(f.escalations))
	}
}

func TestScheduledMaintenanceSkipsWhileCycleRunning(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	d.patrolConfig = gcModeConfig()
	d.maintenanceGCRunning.Store(true)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Errorf("a second cycle started while one was running: %v", f.gcCalls)
	}
}

// --- the quiet-window probe ---------------------------------------------------

func TestMaintenanceQuiet(t *testing.T) {
	t.Parallel()

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
			t.Parallel()
			d := idleTestDaemon(t)
			d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
			if tc.slots != nil {
				d.maint.slotHolders = tc.slots
			}
			d.maint.workingPolecats = func(*Daemon) ([]string, error) { return nil, nil }
			if tc.polecats != nil {
				d.maint.workingPolecats = tc.polecats
			}
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
	t.Parallel()
	d := idleTestDaemon(t)
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	d.maint.workingPolecats = func(*Daemon) ([]string, error) { return nil, nil }
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
	t.Parallel()
	d := idleTestDaemon(t)
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
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
	d2.maint.slotHolders = d.maint.slotHolders
	writeTestRigsJSON(t, d2.config.TownRoot, "nopolecats")
	if quiet, reason := d2.maintenanceQuiet(); !quiet {
		t.Errorf("maintenanceQuiet() = false (%s) for a rig with no polecats directory", reason)
	}
}

func TestWorkingPolecatsFromHeartbeats(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := "testrig"
	writeTestRigsJSON(t, town, rig)
	for _, p := range []string{"fresh", "stale", "idle", "nobeat"} {
		if err := os.MkdirAll(filepath.Join(town, rig, "polecats", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sess := func(p string) string { return session.PolecatSessionName(session.DefaultPrefix, p) }
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
