package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// burstNow is the clock every burst fixture is sampled at: the reference burst
// of 2026-10-26 is the shape the row exists for, and the fixtures stand a
// minute after their log lines.
var burstNow = time.Date(2026, 10, 26, 17, 7, 0, 0, time.UTC)

// burstHealth is a health file the daemon wrote a minute before burstNow, with
// the reference burst's exec tax.
const burstHealth = `{"at":"2026-10-26T17:06:00Z","verdict":"red","fields":[],"exec_tax_ms":243}`

// burstRigs is a one-rig registry, so the landing-queue read asks one store.
const burstRigs = `{"version":1,"rigs":{"gastown":{"git_url":"git@example.com:gastown.git","added_at":"2026-01-01T00:00:00Z"}}}`

// burstLog is a daemon log with a dispatcher roster, a sweep in progress and a
// post-land suite in progress.
const burstLog = "2026/10/26 17:05:00 spec_dispatch: tick: 2 candidate(s), roster deepseek-flash 2/2, claude-sonnet 1/2, 1 dispatched, 1 skipped\n" +
	"2026/10/26 17:05:30 tier_sweep: gastown: sweep started 1a2b3c4d (shell, integration, race)\n" +
	"2026/10/26 17:06:30 landing_worker: gastown: post-land: running \"make gate\" at 5e6f7a8b (bullet)\n"

// burstTop is top's two CPU lines, the last of which is the sample's idle
// share.
const burstTop = "CPU usage: 99.42% user, 0.29% sys, 0.29% idle\nCPU usage: 99.42% user, 0.29% sys, 0.29% idle\n"

// burstFake answers a burst sample's reads: top, the two process tables, the
// daemon log, the health file and the landing-queue read.
type burstFake struct {
	at         time.Time
	top        string
	topErr     error
	names      []string
	args       []string
	psErr      error
	log        string
	logErr     error
	health     string
	rigs       string
	rigsErr    error
	landing    int
	landingErr error
	ran        []string
	storeDirs  []string
	readsAt    map[string]int
}

// burstFakeEnv is the fake every fixture starts from: the host pressed as the
// reference burst pressed it, both test kinds running, two beads waiting to
// land.
func burstFakeEnv() *burstFake {
	return &burstFake{
		at:      burstNow,
		top:     burstTop,
		names:   []string{hostCPUSampleOne.names, hostCPUSampleTwo.names},
		args:    []string{hostCPUSampleOne.args, hostCPUSampleTwo.args},
		log:     burstLog,
		health:  burstHealth,
		rigs:    burstRigs,
		landing: 2,
		readsAt: map[string]int{},
	}
}

func (f *burstFake) env(town string) Env {
	return Env{
		Town: town,
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			key := strings.Join(argv, " ")
			f.ran = append(f.ran, key)
			switch key {
			case strings.Join(hostIdleProbe.argv, " "):
				if f.topErr != nil {
					return nil, f.topErr
				}
				return []byte(f.top), nil
			case strings.Join(hostCPUNameArgv, " "):
				return f.table(f.names)
			case strings.Join(hostCPUArgsArgv, " "):
				return f.table(f.args)
			}
			return nil, errors.New("the sample ran unexpected command " + key)
		},
		Beads: func(_ context.Context, dir string) BeadStore {
			f.storeDirs = append(f.storeDirs, dir)
			return burstStore{f: f}
		},
		Now:  func() time.Time { return f.at },
		Wait: func(d time.Duration) { f.at = f.at.Add(d) },
		ReadFile: func(path string) ([]byte, error) {
			switch filepath.Base(path) {
			case "townhealth.json":
				if f.health == "" {
					return nil, fs.ErrNotExist
				}
				return []byte(f.health), nil
			case "rigs.json":
				if f.rigsErr != nil {
					return nil, f.rigsErr
				}
				return []byte(f.rigs), nil
			}
			return nil, errors.New("the sample read unexpected file " + path)
		},
		Tail: func(string, int64) ([]byte, error) {
			if f.logErr != nil {
				return nil, f.logErr
			}
			return []byte(f.log), nil
		},
	}
}

// burstStore answers the one board a burst row reads: the beads waiting to
// land. The other stores' reads are another source's business, so a burst
// sample that made one is a failure the test should see.
type burstStore struct{ f *burstFake }

func (s burstStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Label != "gt:ready-to-land" || opts.Limit != 0 {
		return nil, errors.New("the burst row asked the landing board for other options than the queue group's")
	}
	if s.f.landingErr != nil {
		return nil, s.f.landingErr
	}
	issues := make([]*beads.Issue, s.f.landing)
	for i := range issues {
		issues[i] = &beads.Issue{}
	}
	return issues, nil
}

func (s burstStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the burst row read the ready board")
}

func (s burstStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the burst row read the blocked board")
}

// table answers one process table read per sample.
func (f *burstFake) table(tables []string) ([]byte, error) {
	if f.psErr != nil {
		return nil, f.psErr
	}
	key := strings.Join(tables, "\x00")
	i := f.readsAt[key]
	f.readsAt[key]++
	if i >= len(tables) {
		return nil, errors.New("the sample read a process table more times than the fake answers")
	}
	return []byte(tables[i]), nil
}

// The cadence is 15 seconds while either reading says the host is pressed and a
// minute otherwise, with the thresholds' own edges on the slow side: "under 15
// percent" and "over 50 ms" are the pressures the bead names.
func TestBurstInterval(t *testing.T) {
	t.Parallel()
	pct := func(v float64) *float64 { return &v }
	tests := []struct {
		name string
		idle *float64
		tax  *float64
		want time.Duration
	}{
		{name: "an idle host on a fast tax", idle: pct(97), tax: pct(9), want: BurstSlowInterval},
		{name: "an idle share under 15", idle: pct(14.9), tax: pct(9), want: BurstFastInterval},
		{name: "an idle share of exactly 15 is not under it", idle: pct(15), tax: pct(9), want: BurstSlowInterval},
		{name: "an exec tax over 50", idle: pct(97), tax: pct(50.1), want: BurstFastInterval},
		{name: "an exec tax of exactly 50 is not over it", idle: pct(97), tax: pct(50), want: BurstSlowInterval},
		{name: "the reference burst", idle: pct(0), tax: pct(243), want: BurstFastInterval},
		{name: "an idle share that could not be read", idle: nil, tax: pct(9), want: BurstFastInterval},
		{name: "no tax the daemon has measured", idle: pct(97), tax: nil, want: BurstSlowInterval},
		{name: "neither reading", idle: nil, tax: nil, want: BurstFastInterval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BurstInterval(tc.idle, tc.tax); got != tc.want {
				t.Errorf("BurstInterval(%v, %v) = %v, want %v", tc.idle, tc.tax, got, tc.want)
			}
		})
	}
}

// A burst row names what the bead asks it to name: the host's pressure, the
// consumers that burned the cores, the live polecats, the landing queue and the
// test load running at the time.
func TestBurstRowNamesTheLoad(t *testing.T) {
	t.Parallel()
	fake := burstFakeEnv()

	town := "/town"
	row, err := SampleBurst(context.Background(), fake.env(town))
	if err != nil {
		t.Fatalf("SampleBurst: %v", err)
	}

	if row.V != BurstVersion {
		t.Errorf("v = %d, want %d", row.V, BurstVersion)
	}
	if want := "2026-10-26T17:07:00Z"; row.TS != want {
		t.Errorf("ts = %q, want %q", row.TS, want)
	}
	if row.IdlePct == nil || *row.IdlePct != 0.29 {
		t.Errorf("idle_pct = %v, want 0.29", row.IdlePct)
	}
	if row.ExecTaxMS == nil || *row.ExecTaxMS != 243 {
		t.Errorf("exec_tax_ms = %v, want 243", row.ExecTaxMS)
	}
	if row.ExecTaxAgeS == nil || *row.ExecTaxAgeS != 60 {
		t.Errorf("exec_tax_age_s = %v, want 60", row.ExecTaxAgeS)
	}
	if want := 15.0; row.IntervalS != want {
		t.Errorf("interval_s = %v, want %v (a 0 percent idle host and 243 ms per exec)", row.IntervalS, want)
	}
	wantConsumers := []BurstConsumer{
		{Name: "dolt", Cores: 1.5},
		{Name: "Google Chrome", Cores: 0.5},
		{Name: "Google Chrome Helper", Cores: 0.5},
	}
	if !reflect.DeepEqual(row.TopConsumers, wantConsumers) {
		t.Errorf("top_consumers = %+v, want %+v", row.TopConsumers, wantConsumers)
	}
	if row.Polecats == nil || *row.Polecats != 3 {
		t.Errorf("polecats = %v, want 3", row.Polecats)
	}
	if row.LandingQueue == nil || *row.LandingQueue != 2 {
		t.Errorf("landing_queue = %v, want 2", row.LandingQueue)
	}
	wantRunning := []BurstRun{
		{Kind: burstSweep, Rig: "gastown", Commit: "1a2b3c4d", StartedS: 90},
		{Kind: burstPostLand, Rig: "gastown", Commit: "5e6f7a8b", StartedS: 30},
	}
	if !reflect.DeepEqual(row.Running, wantRunning) {
		t.Errorf("running = %+v, want %+v", row.Running, wantRunning)
	}
	// The sample runs the host reads the snapshot's own groups run, and asks
	// the landing board's store for the rig it is registered under.
	wantReads := []string{
		strings.Join(hostIdleProbe.argv, " "),
		strings.Join(hostCPUNameArgv, " "),
		strings.Join(hostCPUArgsArgv, " "),
		strings.Join(hostCPUNameArgv, " "),
		strings.Join(hostCPUArgsArgv, " "),
	}
	if !reflect.DeepEqual(fake.ran, wantReads) {
		t.Errorf("the sample ran %q, want %q", fake.ran, wantReads)
	}
	if wantDir := "/town/gastown"; !reflect.DeepEqual(fake.storeDirs, []string{wantDir}) {
		t.Errorf("the sample opened stores %q, want one for %q", fake.storeDirs, wantDir)
	}
}

// Each reading is independent: one that fails is null in the row and the rest
// still fill, so a row explains what it can instead of being lost whole.
func TestBurstRowLeavesAFailedReadingNull(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		spoil func(f *burstFake)
		check func(t *testing.T, row BurstRow)
	}{
		{
			name:  "top fails, so the idle share is null and the rest fills",
			spoil: func(f *burstFake) { f.topErr = errors.New("top timed out") },
			check: func(t *testing.T, row BurstRow) {
				if row.IdlePct != nil {
					t.Errorf("idle_pct = %v, want null", row.IdlePct)
				}
				if row.IntervalS != BurstFastInterval.Seconds() {
					t.Errorf("interval_s = %v, want the fast cadence: an unread idle share is not a calm host", row.IntervalS)
				}
				if row.ExecTaxMS == nil || row.LandingQueue == nil {
					t.Errorf("a failed top took the other readings with it: %+v", row)
				}
			},
		},
		{
			name:  "a missing health file leaves the tax and its age null",
			spoil: func(f *burstFake) { f.health = "" },
			check: func(t *testing.T, row BurstRow) {
				if row.ExecTaxMS != nil || row.ExecTaxAgeS != nil {
					t.Errorf("exec_tax_ms = %v, exec_tax_age_s = %v, want both null", row.ExecTaxMS, row.ExecTaxAgeS)
				}
				if row.IntervalS != BurstFastInterval.Seconds() {
					t.Errorf("interval_s = %v, want the fast cadence: top still read 0.29 percent idle", row.IntervalS)
				}
			},
		},
		{
			name:  "a health file the daemon stopped writing leaves the tax null and its age stated",
			spoil: func(f *burstFake) { f.health = `{"at":"2026-10-26T16:00:00Z","fields":[],"exec_tax_ms":243}` },
			check: func(t *testing.T, row BurstRow) {
				if row.ExecTaxMS != nil {
					t.Errorf("exec_tax_ms = %v, want null: an hour-old measurement is not a reading of now", row.ExecTaxMS)
				}
				if row.ExecTaxAgeS == nil || *row.ExecTaxAgeS != 4020 {
					t.Errorf("exec_tax_age_s = %v, want 4020, so a null tax says how old the reason was", row.ExecTaxAgeS)
				}
			},
		},
		{
			name:  "an unreadable daemon log leaves the polecats and the running runs null",
			spoil: func(f *burstFake) { f.logErr = errors.New("lumberjack rotated it away") },
			check: func(t *testing.T, row BurstRow) {
				if row.Polecats != nil || row.Running != nil {
					t.Errorf("polecats = %v, running = %v, want both null", row.Polecats, row.Running)
				}
			},
		},
		{
			name:  "a store that cannot answer leaves the landing queue null",
			spoil: func(f *burstFake) { f.landingErr = errors.New("bd: connection refused") },
			check: func(t *testing.T, row BurstRow) {
				if row.LandingQueue != nil {
					t.Errorf("landing_queue = %v, want null: an unread queue is not an empty one", row.LandingQueue)
				}
			},
		},
		{
			name:  "a process table that cannot be read leaves the consumers null",
			spoil: func(f *burstFake) { f.psErr = errors.New("ps failed") },
			check: func(t *testing.T, row BurstRow) {
				if row.TopConsumers != nil {
					t.Errorf("top_consumers = %+v, want null", row.TopConsumers)
				}
				if row.IdlePct == nil || row.Polecats == nil {
					t.Errorf("a failed ps took the other readings with it: %+v", row)
				}
			},
		},
		{
			name:  "a registry that cannot be read leaves the landing queue null",
			spoil: func(f *burstFake) { f.rigsErr = errors.New("rigs.json: permission denied") },
			check: func(t *testing.T, row BurstRow) {
				if row.LandingQueue != nil {
					t.Errorf("landing_queue = %v, want null: no rigs read is not an empty queue", row.LandingQueue)
				}
			},
		},
		{
			name:  "a registry naming no rig leaves the landing queue null",
			spoil: func(f *burstFake) { f.rigs = `{"version":1,"rigs":{}}` },
			check: func(t *testing.T, row BurstRow) {
				if row.LandingQueue != nil {
					t.Errorf("landing_queue = %v, want null: a town with no registered rig is not a town with nothing waiting", row.LandingQueue)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := burstFakeEnv()
			tc.spoil(fake)
			row, err := SampleBurst(context.Background(), fake.env("/town"))
			if err != nil {
				t.Fatalf("SampleBurst: %v", err)
			}
			tc.check(t, row)
		})
	}
}

// A row that names a running test is a reading of its own: the log answered
// the one question a wedged host still allows, and the row is recorded rather
// than thrown away with the readings that failed.
func TestBurstSampleNamingARunningRunIsARow(t *testing.T) {
	t.Parallel()
	fake := burstFakeEnv()
	fake.topErr = errors.New("top timed out")
	fake.psErr = errors.New("ps timed out")
	fake.landingErr = errors.New("bd timed out")
	fake.health = ""

	row, err := SampleBurst(context.Background(), fake.env("/town"))
	if err != nil {
		t.Fatalf("SampleBurst: %v", err)
	}
	if len(row.Running) != 2 {
		t.Errorf("running = %+v, want the sweep and the post-land suite", row.Running)
	}
	if row.IdlePct != nil || row.TopConsumers != nil || row.LandingQueue != nil || row.ExecTaxMS != nil {
		t.Errorf("row = %+v, want the failed readings null", row)
	}
}

// A sample that could not read anything is an error, and RunBurst writes no
// row for it: a sampler whose seams are all gone must not look like a town
// with nothing to report.
func TestBurstSampleWithNoReadingIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := Env{Town: town, Now: func() time.Time { return burstNow }}

	row, err := SampleBurst(context.Background(), env)
	if err == nil {
		t.Fatalf("SampleBurst = %+v, want an error", row)
	}
	if row.IdlePct != nil || row.TopConsumers != nil || row.Polecats != nil || row.LandingQueue != nil {
		t.Errorf("the row beside the error = %+v, want no readings", row)
	}

	if _, path, err := RunBurst(context.Background(), env); err == nil {
		t.Fatal("RunBurst wrote a row for a sample with no readings")
	} else if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("%s exists after a failed sample, want no row written", path)
	}
}

// The row's JSON column names are the ones a reader parses by, so a rename is a
// broken series rather than a new one.
func TestRunBurstAppendsOneRowUnderItsColumns(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	row, path, err := RunBurst(context.Background(), burstFakeEnv().env(town))
	if err != nil {
		t.Fatalf("RunBurst: %v", err)
	}
	if want := BurstPath(town); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the burst log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("the burst log holds %d lines, want 1", len(lines))
	}
	var written BurstRow
	if err := json.Unmarshal([]byte(lines[0]), &written); err != nil {
		t.Fatalf("parsing the appended row: %v", err)
	}
	if !reflect.DeepEqual(written, row) {
		t.Errorf("the appended row = %+v, want %+v", written, row)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &fields); err != nil {
		t.Fatalf("parsing the appended row's columns: %v", err)
	}
	got := make([]string, 0, len(fields))
	for name := range fields {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"exec_tax_age_s", "exec_tax_ms", "idle_pct", "interval_s", "landing_queue",
		"polecats", "running", "top_consumers", "ts", "v",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the row's columns = %v, want %v", got, want)
	}
}

// A burst log that cannot be appended to is an error naming the append, not a
// sample reported as recorded.
func TestRunBurstReportsAFailedAppend(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setting up the blocked path: %v", err)
	}

	if _, _, err := RunBurst(context.Background(), burstFakeEnv().env(town)); err == nil {
		t.Fatal("RunBurst = nil error, want the append failure")
	} else if !strings.Contains(err.Error(), "appending the burst row") {
		t.Errorf("error = %v, want it to name the append", err)
	}
}

// The ranking names the busiest consumers, leaves out one that burned nothing
// or could not be read, and breaks a tie by name so the same host ranks the
// same way every sample.
func TestBurstTopConsumers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cols map[string]any
		n    int
		want []BurstConsumer
	}{
		{
			name: "named consumers and ranked names together",
			cols: map[string]any{
				"dolt_cores": 1.5, "scanner_cores": nil, "gt_cores": 0.0,
				"other_1_name": "WindowServer", "other_1_cores": 0.25,
				"other_2_name": "Google Chrome", "other_2_cores": 0.5,
			},
			n: 3,
			want: []BurstConsumer{
				{Name: "dolt", Cores: 1.5},
				{Name: "Google Chrome", Cores: 0.5},
				{Name: "WindowServer", Cores: 0.25},
			},
		},
		{
			name: "a tie ranks by name",
			cols: map[string]any{
				"dolt_cores": 1.5, "gt_cores": 1.5, "scanner_cores": 1.5,
				"docker_vm_cores": 1.5, "polecat_vm_cores": 1.5,
			},
			n: 3,
			want: []BurstConsumer{
				{Name: "docker_vm", Cores: 1.5},
				{Name: "dolt", Cores: 1.5},
				{Name: "gt", Cores: 1.5},
			},
		},
		{
			name: "an idle host ranks none",
			cols: map[string]any{"dolt_cores": 0.0, "other_1_name": "WindowServer", "other_1_cores": 0.0},
			n:    3,
			want: []BurstConsumer{},
		},
		{
			name: "a name with no cores is not a consumer",
			cols: map[string]any{"other_1_name": "WindowServer", "other_1_cores": nil},
			n:    3,
			want: []BurstConsumer{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := burstTopConsumers(tc.cols, tc.n)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("burstTopConsumers = %+v, want %+v", got, tc.want)
			}
			if len(got) > tc.n {
				t.Errorf("burstTopConsumers returned %d consumers, more than the %d asked for", len(got), tc.n)
			}
		})
	}
}

// A run is named only while its log shows it started and not finished, and the
// polecat count comes from the newest roster, kept only while it answers for
// now.
func TestBurstLogReadings(t *testing.T) {
	t.Parallel()
	stamp := "2026/10/26 "
	tests := []struct {
		name     string
		log      string
		polecats *int
		running  []BurstRun
	}{
		{
			name: "a sweep and a post-land suite in progress",
			log: stamp + "17:05:30 tier_sweep: gastown: sweep started 1a2b3c4d (shell)\n" +
				stamp + "17:06:30 landing_worker: gastown: post-land: running \"make gate\" at 5e6f7a8b (bullet)\n",
			running: []BurstRun{
				{Kind: burstSweep, Rig: "gastown", Commit: "1a2b3c4d", StartedS: 90},
				{Kind: burstPostLand, Rig: "gastown", Commit: "5e6f7a8b", StartedS: 30},
			},
		},
		{
			name: "a swept commit is over",
			log: stamp + "17:05:30 tier_sweep: gastown: sweep started 1a2b3c4d (shell)\n" +
				stamp + "17:06:30 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in 1m0s\n",
		},
		{
			name: "an aborted sweep is over",
			log: stamp + "17:05:30 tier_sweep: gastown: sweep started 1a2b3c4d (shell)\n" +
				stamp + "17:06:00 tier_sweep: gastown: sweep aborted 1a2b3c4d (no stage ran)\n",
		},
		{
			name: "a green post-land suite is over",
			log: stamp + "17:05:30 landing_worker: gastown: post-land: running \"make gate\" at 5e6f7a8b (bullet)\n" +
				stamp + "17:06:00 landing_worker: gastown: post-land: green at 5e6f7a8b (bullet)\n",
		},
		{
			name: "a red post-land suite is over",
			log: stamp + "17:05:30 landing_worker: gastown: post-land: running \"make gate\" at 5e6f7a8b (bullet)\n" +
				stamp + "17:06:00 landing_worker: gastown: post-land: RED: \"make gate\" exited 1 at 5e6f7a8b (bullet); failing: ./a\n",
		},
		{
			name: "a superseded post-land suite is over",
			log: stamp + "17:05:30 landing_worker: gastown: post-land: running \"make gate\" at 5e6f7a8b (bullet)\n" +
				stamp + "17:06:00 landing_worker: gastown: post-land: 5e6f7a8b (bullet) superseded: the daemon stopped before the verdict\n",
		},
		{
			// The line names the newest commit first and the queued run it
			// replaces second; the run already going at that commit keeps going.
			name: "a newer trigger that drops a queued run does not end the one running",
			log: stamp + "17:05:30 landing_worker: gastown: post-land: running \"make gate\" at 5e6f7a8b (bullet)\n" +
				stamp + "17:06:00 landing_worker: gastown: post-land: 5e6f7a8b (bullet) supersedes queued 9a8b7c6d (bullet); one run at the newest commit\n",
			running: []BurstRun{{Kind: burstPostLand, Rig: "gastown", Commit: "5e6f7a8b", StartedS: 90}},
		},
		{
			name: "a run started longer ago than the limit is not the one in progress",
			log:  stamp + "16:00:00 tier_sweep: gastown: sweep started 1a2b3c4d (shell)\n",
		},
		{
			name: "a run at the same commit started again is the new one",
			log: stamp + "16:40:00 tier_sweep: gastown: sweep started 1a2b3c4d (shell)\n" +
				stamp + "16:50:00 tier_sweep: gastown: swept 1a2b3c4d (shell RED) in 1m0s\n" +
				stamp + "17:06:00 tier_sweep: gastown: sweep started 1a2b3c4d (shell)\n",
			running: []BurstRun{{Kind: burstSweep, Rig: "gastown", Commit: "1a2b3c4d", StartedS: 60}},
		},
		{
			name: "an end line with no start line in the read names no run",
			log:  stamp + "17:06:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in 1m0s\n",
		},
		{
			name: "the newest roster counts the live sessions",
			log: stamp + "17:03:00 spec_dispatch: tick: 9 candidate(s), roster deepseek-flash 4/4, 3 dispatched, 0 skipped\n" +
				stamp + "17:06:00 spec_dispatch: tick: 2 candidate(s), roster deepseek-flash 2/2, claude-sonnet 1/2, 5 dispatched, 0 skipped\n",
			polecats: intPtr(3),
		},
		{
			name:     "a roster older than the limit answers for another time",
			log:      stamp + "16:30:00 spec_dispatch: tick: 2 candidate(s), roster deepseek-flash 2/2, 1 dispatched, 0 skipped\n",
			polecats: nil,
		},
		{
			name:     "a roster naming no seat is no count",
			log:      stamp + "17:06:00 spec_dispatch: tick: 2 candidate(s), roster no seats, 1 dispatched, 0 skipped\n",
			polecats: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := Env{Town: "/town", Tail: func(string, int64) ([]byte, error) { return []byte(tc.log), nil }}
			polecats, running, read := burstLogReadings(env, burstNow)
			if !read {
				t.Fatal("burstLogReadings reported the log unreadable")
			}
			if !intPtrEqual(polecats, tc.polecats) {
				t.Errorf("polecats = %v, want %v", polecats, tc.polecats)
			}
			want := tc.running
			if want == nil {
				want = []BurstRun{}
			}
			if !reflect.DeepEqual(running, want) {
				t.Errorf("running = %+v, want %+v", running, want)
			}
		})
	}
}

// An unreadable log is a stated gap: running is null, not an empty list.
func TestBurstLogReadingsWithNoTail(t *testing.T) {
	t.Parallel()
	polecats, running, read := burstLogReadings(Env{Town: "/town"}, burstNow)
	if read || polecats != nil || running != nil {
		t.Errorf("burstLogReadings = (%v, %v, %v), want (nil, nil, false)", polecats, running, read)
	}
}

func intPtr(v int) *int { return &v }

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
