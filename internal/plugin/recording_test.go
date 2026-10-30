package plugin

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// newFakeRecorder is a Recorder over an in-memory beads database whose clock
// reads now, so a receipt's created_at is the instant the test chose and a
// cooldown cutoff is judged against it. The clock is returned to move time
// between receipts.
func newFakeRecorder(t *testing.T, now time.Time) (*Recorder, *beadsfake.Fake, *clockwork.FakeClock) {
	t.Helper()
	clock := clockwork.NewFakeClockAt(now)
	fake := beadsfake.New(beadsfake.WithClock(clock))
	return &Recorder{bd: fake}, fake, clock
}

func runIDs(runs []*PluginRunBead) []string {
	out := make([]string, len(runs))
	for i, run := range runs {
		out[i] = run.ID
	}
	return out
}

func TestRecordRunCreatesAndClosesReceipt(t *testing.T) {
	t.Parallel()
	recorder, fake, _ := newFakeRecorder(t, time.Now())

	id, err := recorder.RecordRun(PluginRunRecord{
		PluginName:  "tool-updater",
		RigName:     "gastown",
		Result:      RunResult("warning"),
		Title:       "tool-updater: failed=brew",
		Body:        "brew failed",
		ExtraLabels: []string{"source:test"},
	})
	if err != nil {
		t.Fatalf("RecordRun failed: %v", err)
	}

	got, err := fake.Show(id)
	if err != nil {
		t.Fatalf("Show(%s): %v", id, err)
	}
	// A receipt is a wisp the cooldown gate reads back, so it must be
	// ephemeral and closed: an open one would linger on the board.
	if !got.Ephemeral {
		t.Errorf("receipt %s is not ephemeral", id)
	}
	if got.Status != "closed" || got.CloseReason != "plugin run recorded" {
		t.Errorf("receipt status %q reason %q, want closed/plugin run recorded", got.Status, got.CloseReason)
	}
	if got.Title != "tool-updater: failed=brew" || got.Description != "brew failed" {
		t.Errorf("receipt = title %q description %q", got.Title, got.Description)
	}
	for _, want := range []string{
		"type:plugin-run",
		"plugin:tool-updater",
		"result:warning",
		"rig:gastown",
		"source:test",
	} {
		if !slices.Contains(got.Labels, want) {
			t.Errorf("receipt labels %v missing %q", got.Labels, want)
		}
	}
}

// TestQueryRunsMatchesLabelsAndCutoff drives GetRunsSince against the receipts
// RecordRun wrote: a run of another plugin and a run older than the window
// are both left out, so the label AND and the created-after cutoff are doing
// the filtering, not an empty read.
func TestQueryRunsMatchesLabelsAndCutoff(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	recorder, _, clock := newFakeRecorder(t, now.Add(-3*time.Hour))

	old, err := recorder.RecordRun(PluginRunRecord{PluginName: "p", Result: ResultSuccess})
	if err != nil {
		t.Fatalf("RecordRun (old): %v", err)
	}
	clock.Advance(2 * time.Hour) // now-1h: outside a 1h window
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "other", Result: ResultSuccess}); err != nil {
		t.Fatalf("RecordRun (other plugin): %v", err)
	}
	clock.Advance(30 * time.Minute) // now-30m: inside the window
	recent, err := recorder.RecordRun(PluginRunRecord{PluginName: "p", Result: ResultFailure})
	if err != nil {
		t.Fatalf("RecordRun (recent): %v", err)
	}

	runs, err := recorder.GetRunsSince("p", "1h")
	if err != nil {
		t.Fatalf("GetRunsSince: %v", err)
	}
	if ids := runIDs(runs); !slices.Equal(ids, []string{recent}) {
		t.Errorf("GetRunsSince(p, 1h) = %v, want [%s]: the old run (%s) is past the cutoff",
			ids, recent, old)
	}
	if got := runs[0].Result; got != ResultFailure {
		t.Errorf("run result = %q, want failure (parsed from the receipt's labels)", got)
	}

	// Without a window every run of the plugin comes back, newest first.
	all, err := recorder.GetRunsSince("p", "")
	if err != nil {
		t.Fatalf("GetRunsSince(p, \"\"): %v", err)
	}
	if ids := runIDs(all); !slices.Equal(ids, []string{recent, old}) {
		t.Errorf("GetRunsSince(p, \"\") = %v, want [%s %s]", ids, recent, old)
	}
}

func TestRunResultConstants(t *testing.T) {
	t.Parallel()
	if ResultSuccess != "success" {
		t.Errorf("expected ResultSuccess to be 'success', got %q", ResultSuccess)
	}
	if ResultFailure != "failure" {
		t.Errorf("expected ResultFailure to be 'failure', got %q", ResultFailure)
	}
	if ResultSkipped != "skipped" {
		t.Errorf("expected ResultSkipped to be 'skipped', got %q", ResultSkipped)
	}
	// ResultWarning must stay distinct from ResultSuccess: it is what a
	// plugin records for a run that found something and escalated it, and
	// collapsing the two would report an escalated signal as a quiet run.
	if ResultWarning != "warning" {
		t.Errorf("expected ResultWarning to be 'warning', got %q", ResultWarning)
	}
	if ResultWarning == ResultSuccess {
		t.Error("ResultWarning must not equal ResultSuccess")
	}
	// ResultPrinted must stay distinct from ResultSuccess: it is what `gt
	// plugin run` records for a merely-printed, not-yet-executed run
	// (gt-o1z7). Collapsing the two back together is the fail-open bug.
	if ResultPrinted != "printed" {
		t.Errorf("expected ResultPrinted to be 'printed', got %q", ResultPrinted)
	}
	if ResultPrinted == ResultSuccess {
		t.Error("ResultPrinted must not equal ResultSuccess")
	}
}

func TestNewRecorder(t *testing.T) {
	t.Parallel()
	if recorder := NewRecorder("/tmp/test-town"); recorder == nil {
		t.Fatal("NewRecorder returned nil")
	}
}

// A ResultPrinted receipt records that `gt plugin run` printed a plugin's
// instructions without doing any work. It must not count toward the
// cooldown gate — otherwise printing instructions for a cooldown-gated
// plugin holds the daemon off for a full cooldown window even though
// nothing happened (gt-o1z7, finding f53b7b837c35).
func TestCountRunsSinceExcludesPrinted(t *testing.T) {
	t.Parallel()
	recorder, _, _ := newFakeRecorder(t, time.Now())
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "p", Result: ResultSuccess}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "p", Result: ResultPrinted}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	count, err := recorder.CountRunsSince("p", "1h")
	if err != nil {
		t.Fatalf("CountRunsSince failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountRunsSince = %d, want 1: a printed receipt must not satisfy the cooldown gate", count)
	}
}

// A receipt older than the cooldown window must not buy the plugin a fresh
// window: the gate reads the ledger's clock, not the file's.
func TestCountRunsSinceIgnoresRunsPastTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	recorder, _, clock := newFakeRecorder(t, now.Add(-48*time.Hour))
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "p", Result: ResultSuccess}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	clock.Advance(24 * time.Hour) // the receipt is now two days old

	count, err := recorder.CountRunsSince("p", "1h")
	if err != nil {
		t.Fatalf("CountRunsSince failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountRunsSince = %d, want 0: a run outside the window must not gate", count)
	}
}

// countingClient counts the List calls a Recorder makes against its store,
// so a test can price a read.
type countingClient struct {
	beads.Client
	lists int
}

func (c *countingClient) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.lists++
	return c.Client.List(opts)
}

// A tick that gates N plugins costs one list of the run wisps, not one per
// plugin: the ledger is read once and grouped in memory. Listing per plugin
// is what put 84 bd calls in 60 minutes on the daemon's hot path (gt-3bqfy).
func TestLoadRunLedgerCostsOneListForEveryPlugin(t *testing.T) {
	t.Parallel()
	recorder, fake, _ := newFakeRecorder(t, time.Now().UTC().Add(-30*time.Minute))
	counting := &countingClient{Client: fake}
	recorder.bd = counting

	plugins := []string{"alpha", "beta", "gamma"}
	for _, name := range plugins {
		if _, err := recorder.RecordRun(PluginRunRecord{PluginName: name, Result: ResultSuccess}); err != nil {
			t.Fatalf("RecordRun(%s): %v", name, err)
		}
	}

	ledger, err := recorder.LoadRunLedger(2 * time.Hour)
	if err != nil {
		t.Fatalf("LoadRunLedger: %v", err)
	}
	for _, name := range plugins {
		count, err := ledger.Count(name, time.Hour)
		if err != nil {
			t.Fatalf("Count(%s, 1h): %v", name, err)
		}
		if count != 1 {
			t.Errorf("Count(%s, 1h) = %d, want 1: the receipt is inside the window", name, count)
		}
	}
	if counting.lists != 1 {
		t.Errorf("%d list call(s) for %d plugins, want 1: one read answers every gate in the tick",
			counting.lists, len(plugins))
	}
}

// The ledger's grouped answers are the per-plugin queries' answers for the
// same receipts: same count, same last run, a printed receipt buying no
// cooldown in either.
func TestRunLedgerAnswersMatchPerPluginQueries(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	recorder, _, clock := newFakeRecorder(t, now.Add(-4*time.Hour))

	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "alpha", Result: ResultSuccess}); err != nil {
		t.Fatalf("RecordRun alpha (4h old): %v", err)
	}
	clock.Advance(2 * time.Hour) // now-2h
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "beta", Result: ResultFailure}); err != nil {
		t.Fatalf("RecordRun beta: %v", err)
	}
	clock.Advance(90 * time.Minute) // now-30m: a printed receipt, no work done
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "alpha", Result: ResultPrinted}); err != nil {
		t.Fatalf("RecordRun alpha (printed): %v", err)
	}
	clock.Advance(10 * time.Minute) // now-20m
	success, err := recorder.RecordRun(PluginRunRecord{PluginName: "alpha", Result: ResultSuccess})
	if err != nil {
		t.Fatalf("RecordRun alpha (20m old): %v", err)
	}

	ledger, err := recorder.LoadRunLedger(5 * time.Hour)
	if err != nil {
		t.Fatalf("LoadRunLedger: %v", err)
	}

	cases := []struct {
		plugin string
		window string
		count  int
	}{
		// The old success is outside a 1h window; the printed receipt inside
		// it is not work, so one run counts.
		{"alpha", "1h", 1},
		{"alpha", "3h", 1},
		// A 5h window reaches the old success too.
		{"alpha", "5h", 2},
		{"beta", "1h", 0},
		{"beta", "3h", 1},
		// A window of zero or less ends at or before now, so it holds no run.
		{"alpha", "0", 0},
		{"alpha", "-5m", 0},
		// A plugin with no runs counts 0 and has no last run.
		{"gamma", "1h", 0},
		{"gamma", "5h", 0},
	}
	for _, tc := range cases {
		window, err := time.ParseDuration(tc.window)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", tc.window, err)
		}
		want, err := recorder.CountRunsSince(tc.plugin, tc.window)
		if err != nil {
			t.Fatalf("CountRunsSince(%s, %s): %v", tc.plugin, tc.window, err)
		}
		if want != tc.count {
			t.Fatalf("CountRunsSince(%s, %s) = %d per the ledger, want %d: the table is built on it",
				tc.plugin, tc.window, want, tc.count)
		}
		got, err := ledger.Count(tc.plugin, window)
		if err != nil {
			t.Fatalf("ledger.Count(%s, %s): %v", tc.plugin, tc.window, err)
		}
		if got != want {
			t.Errorf("ledger.Count(%s, %s) = %d, CountRunsSince = %d", tc.plugin, tc.window, got, want)
		}
	}

	wantLast, err := recorder.GetLastRun("alpha")
	if err != nil {
		t.Fatalf("GetLastRun(alpha): %v", err)
	}
	if wantLast == nil || wantLast.ID != success {
		t.Fatalf("GetLastRun(alpha) = %+v, want the 20m-old run %s", wantLast, success)
	}
	if got := ledger.LastRun("alpha"); got == nil || got.ID != wantLast.ID {
		t.Errorf("ledger.LastRun(alpha) = %+v, want %+v", got, wantLast)
	}
	if got := ledger.LastRun("gamma"); got != nil {
		t.Errorf("ledger.LastRun(gamma) = %+v, want nil: the plugin never ran", got)
	}
}

// A window longer than the ledger was loaded with is refused: answering it
// from the shorter read would count a prefix of the runs and open a gate that
// should be closed.
func TestRunLedgerRefusesWindowItWasNotLoadedWith(t *testing.T) {
	t.Parallel()
	recorder, _, _ := newFakeRecorder(t, time.Now().UTC())

	ledger, err := recorder.LoadRunLedger(time.Hour)
	if err != nil {
		t.Fatalf("LoadRunLedger: %v", err)
	}
	if _, err := ledger.Count("alpha", 24*time.Hour); err == nil {
		t.Error("ledger.Count with a 24h window on a 1h ledger returned no error, want a refusal")
	}
	if _, err := ledger.Count("alpha", time.Hour); err != nil {
		t.Errorf("ledger.Count at its own window: %v", err)
	}
	if got, err := ledger.Count("alpha", 0); err != nil || got != 0 {
		t.Errorf("ledger.Count with a zero window = %d, %v; want 0, nil: a span ending now holds no run, so any ledger answers it", got, err)
	}
	if _, err := recorder.LoadRunLedger(-time.Minute); err == nil {
		t.Error("LoadRunLedger with a negative window returned no error")
	}
}

// A run wisp with no plugin label belongs to no plugin's gate: it is left out
// rather than grouped under an empty name.
func TestLoadRunLedgerSkipsUnlabelledRuns(t *testing.T) {
	t.Parallel()
	recorder, fake, _ := newFakeRecorder(t, time.Now().UTC().Add(-10*time.Minute))
	if _, err := fake.Create(beads.CreateOptions{
		Title:     "Plugin run: unlabelled",
		Labels:    []string{"type:plugin-run", "result:success"},
		Ephemeral: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := recorder.RecordRun(PluginRunRecord{PluginName: "alpha", Result: ResultSuccess}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	ledger, err := recorder.LoadRunLedger(time.Hour)
	if err != nil {
		t.Fatalf("LoadRunLedger: %v", err)
	}
	if got, err := ledger.Count("", time.Hour); err != nil || got != 0 {
		t.Errorf("Count(\"\", 1h) = %d, %v; want 0, nil: an unlabelled run belongs to no plugin", got, err)
	}
	if got, err := ledger.Count("alpha", time.Hour); err != nil || got != 1 {
		t.Errorf("Count(alpha, 1h) = %d, %v; want 1", got, err)
	}
}

// failingClient is a beads client whose reads fail.
type failingClient struct{ beads.Client }

func (failingClient) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("bd: read failed")
}

// A failed read of the wisp ledger is returned, never read as a ledger with
// no runs: an empty answer would open every cooldown gate it gates.
func TestLoadRunLedgerReportsListFailure(t *testing.T) {
	t.Parallel()
	recorder := &Recorder{bd: failingClient{}}
	if _, err := recorder.LoadRunLedger(time.Hour); err == nil {
		t.Fatal("LoadRunLedger over a failing store returned no error, want the read's error")
	}
}
