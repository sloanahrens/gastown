package plugin

import (
	"slices"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
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
