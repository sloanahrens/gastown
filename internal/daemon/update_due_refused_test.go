package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
)

// TestPreRollGuardHoldsTheRefusedPair drives the heartbeat's update block with
// a real image ledger, written through the same FileImageLedger the update
// command writes: a record whose refused pair is the commit main is on holds
// the launch and names the check, while a record naming another commit, naming
// no refusal, or absent leaves the decision as it was.
func TestPreRollGuardHoldsTheRefusedPair(t *testing.T) {
	t.Parallel()

	// setUpdateSource reports this commit as the installed build, so it is the
	// target a run would install.
	const installed = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const otherCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const refusal = "preflight pair: no digest for the daemon image"

	tests := []struct {
		name         string
		record       *update.ImageRecord
		wantLaunches int
		wantLog      string
	}{
		{
			name:         "no record leaves the decision as it was",
			wantLaunches: 1,
			wantLog:      "due (nightly",
		},
		{
			name: "a record naming no refusal leaves the decision as it was",
			record: &update.ImageRecord{
				Current: update.ImagePair{Daemon: "daemon:old", Polecat: "polecat:old", Commit: otherCommit},
			},
			wantLaunches: 1,
			wantLog:      "due (nightly",
		},
		{
			name: "a different pair commit is due as before",
			record: &update.ImageRecord{
				Refused:       update.ImagePair{Commit: otherCommit},
				RefusedReason: refusal,
			},
			wantLaunches: 1,
			wantLog:      "due (nightly",
		},
		{
			name: "the refused pair is not due",
			record: &update.ImageRecord{
				Refused:       update.ImagePair{Commit: installed},
				RefusedReason: refusal,
			},
			wantLaunches: 0,
			wantLog:      "not due — pair aaaaaaaaaaaa was refused by " + refusal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
				Enabled: true,
				Window:  "03:00",
			})
			setUpdateSource(t, d)
			if tt.record != nil {
				ledger := update.FileImageLedger{Dir: update.Dir(d.config.TownRoot)}
				if err := ledger.Write(*tt.record); err != nil {
					t.Fatalf("write the image record: %v", err)
				}
			}

			d.triggerUpdate()

			if *launches != tt.wantLaunches {
				t.Fatalf("launches = %d, want %d (log %q)", *launches, tt.wantLaunches, logs.String())
			}
			if got := logs.String(); !strings.Contains(got, tt.wantLog) {
				t.Errorf("log = %q, want it to contain %q", got, tt.wantLog)
			}
		})
	}
}

// TestPreRollGuardDueDecisionForARefusedPair holds the due check's refusal
// rule: only the exact refused commit is held, under either rule; a different
// or unknown target, and a record naming no refusal, keeps today's answer. The
// reason names the twelve-character commit and the check the record names.
func TestPreRollGuardDueDecisionForARefusedPair(t *testing.T) {
	t.Parallel()

	const refused = "0123456789abcdef0123456789abcdef01234567"
	const other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const refusal = "preflight registry-tag: the dev tag names another digest"
	nightly := time.Date(2026, 3, 10, 3, 30, 0, 0, time.UTC)
	noon := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	endedLongAgo := noon.Add(-4 * time.Hour)

	tests := []struct {
		name       string
		now        time.Time
		facts      UpdateFacts
		wantDue    bool
		wantReason string
	}{
		{
			name:       "the refused commit is main's target",
			now:        nightly,
			facts:      UpdateFacts{MainSHA: refused, RefusedCommit: refused, RefusedReason: refusal},
			wantDue:    false,
			wantReason: "pair 0123456789ab was refused by " + refusal,
		},
		{
			name: "the refusal holds under the catch-up rule too",
			now:  noon,
			facts: UpdateFacts{
				MainSHA: refused, RefusedCommit: refused, RefusedReason: refusal,
				MainMoved: true, Waiting: 2, LastEnded: endedLongAgo,
			},
			wantDue:    false,
			wantReason: "was refused by",
		},
		{
			name:       "a record naming no check still names the pre-swap check",
			now:        nightly,
			facts:      UpdateFacts{MainSHA: refused, RefusedCommit: refused},
			wantDue:    false,
			wantReason: "pair 0123456789ab was refused by the pre-swap check",
		},
		{
			name:       "a different pair commit is due as before",
			now:        nightly,
			facts:      UpdateFacts{MainSHA: other, RefusedCommit: refused, RefusedReason: refusal},
			wantDue:    true,
			wantReason: "nightly",
		},
		{
			name: "a different pair commit with main moved is due as before",
			now:  noon,
			facts: UpdateFacts{
				MainSHA: other, RefusedCommit: refused, RefusedReason: refusal,
				MainMoved: true, Waiting: 1, LastEnded: endedLongAgo,
			},
			wantDue:    true,
			wantReason: "main moved",
		},
		{
			name:       "an unknown main sha holds nothing",
			now:        nightly,
			facts:      UpdateFacts{RefusedCommit: refused, RefusedReason: refusal},
			wantDue:    true,
			wantReason: "nightly",
		},
		{
			name:       "no refused pair is recorded",
			now:        nightly,
			facts:      UpdateFacts{MainSHA: refused},
			wantDue:    true,
			wantReason: "nightly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			due, reason := updateDue(tt.now, testUpdateSettings, tt.facts)
			if due != tt.wantDue {
				t.Fatalf("updateDue() due = %v, want %v (reason %q)", due, tt.wantDue, reason)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("updateDue() reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

// TestPreRollGuardAnUnreadableLedgerLeavesTheDecision holds a damaged image
// record to the decision the facts that read give: the refused-pair hold is
// skipped and its absence logged, a due update still launches, and a guard
// that was holding the update still holds. The run's own pre-swap check
// refuses a bad pair before any swap, so the launch rests on that check rather
// than a guess about pair safety.
func TestPreRollGuardAnUnreadableLedgerLeavesTheDecision(t *testing.T) {
	t.Parallel()

	const installed = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name    string
		content string
		// failedRun writes an update state failed on main's target, the
		// failed-run hold, so the decision under test is a held one.
		failedRun    bool
		wantLaunches int
		wantLog      []string
	}{
		{
			name:         "a record that does not parse still lets a due update launch",
			content:      "{",
			wantLaunches: 1,
			wantLog:      []string{"update: due (nightly", "update: the image ledger could not be read"},
		},
		{
			name:         "a record version this package does not read still lets a due update launch",
			content:      `{"v": 99}`,
			wantLaunches: 1,
			wantLog:      []string{"update: due (nightly", "update: the image ledger could not be read"},
		},
		{
			name:         "another guard still holds the update",
			content:      "{",
			failedRun:    true,
			wantLaunches: 0,
			wantLog:      []string{"update: not due — last run failed on this target", "update: the image ledger could not be read"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
				Enabled: true,
				Window:  "03:00",
			})
			createUpdateSource(t, d.config.TownRoot)
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
				return &version.StaleBinaryInfo{RepoCommit: installed}
			}
			if tt.failedRun {
				// The failed-run hold reads main's tip from origin, which this
				// stub answers with the installed commit.
				d.updateRemoteRun = (&updateRemoteFake{output: installed + "\trefs/heads/main\n"}).run
				store := update.Store{Dir: update.Dir(d.config.TownRoot)}
				ended := updateTriggerTestTime.Add(-24 * time.Hour)
				if err := store.Write(update.State{
					RunID:     "run-1",
					StartedAt: ended.Add(-time.Minute),
					EndedAt:   ended,
					Outcome:   update.OutcomeFailed,
					Target:    update.Target{SHA: installed},
				}); err != nil {
					t.Fatalf("write the failed run's state: %v", err)
				}
			}
			path := filepath.Join(update.Dir(d.config.TownRoot), "images.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("create the update state directory: %v", err)
			}
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write the damaged image record: %v", err)
			}

			d.triggerUpdate()

			if *launches != tt.wantLaunches {
				t.Fatalf("launches = %d, want %d (log %q)", *launches, tt.wantLaunches, logs.String())
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log = %q, want it to contain %q", logs.String(), want)
				}
			}
		})
	}
}

// newRefusedLedgerDaemon builds a trigger test daemon whose installed build is
// installed, so a run would install that commit, and returns the path of the
// image record the update command writes.
func newRefusedLedgerDaemon(t *testing.T, installed string) (*Daemon, *bytes.Buffer, *int, string) {
	t.Helper()
	d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
		Enabled: true,
		Window:  "03:00",
	})
	createUpdateSource(t, d.config.TownRoot)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		return &version.StaleBinaryInfo{RepoCommit: installed}
	}
	return d, logs, launches, filepath.Join(update.Dir(d.config.TownRoot), "images.json")
}

// writeImageRecordFile puts content at path as the image ledger's file.
func writeImageRecordFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create the update state directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the image record: %v", err)
	}
}

// TestPreRollGuardARepairedLedgerRestoresTheHold holds a record that reads
// again to the refused-pair hold it names: the damaged record let the update
// launch, and the repaired one holds the same pair.
func TestPreRollGuardARepairedLedgerRestoresTheHold(t *testing.T) {
	t.Parallel()

	const installed = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const refusal = "preflight pair: no digest for the daemon image"

	d, logs, launches, path := newRefusedLedgerDaemon(t, installed)
	writeImageRecordFile(t, path, "{")

	// The damaged record turns the hold off, so the nightly window launches.
	d.triggerUpdate()
	if *launches != 1 {
		t.Fatalf("launches after the damaged record = %d, want 1 (log %q)", *launches, logs.String())
	}

	if err := (update.FileImageLedger{Dir: update.Dir(d.config.TownRoot)}).Write(update.ImageRecord{
		Refused:       update.ImagePair{Commit: installed},
		RefusedReason: refusal,
	}); err != nil {
		t.Fatalf("write the repaired image record: %v", err)
	}
	d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime.Add(updateLaunchSuppression + time.Minute))
	d.triggerUpdate()
	if *launches != 1 {
		t.Fatalf("launches with the repaired record = %d, want 1: the refused pair is not due (log %q)", *launches, logs.String())
	}
	if got := logs.String(); !strings.Contains(got, "pair aaaaaaaaaaaa was refused by "+refusal) {
		t.Errorf("log = %q, want the repaired record's hold named", got)
	}
}

// TestPreRollGuardReportsEachLedgerOutage holds the damage log to one line per
// outage: one while the record stays damaged, and a new line after it reads
// again and is damaged once more.
func TestPreRollGuardReportsEachLedgerOutage(t *testing.T) {
	t.Parallel()

	const installed = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	d, logs, _, path := newRefusedLedgerDaemon(t, installed)
	writeImageRecordFile(t, path, "{")
	d.triggerUpdate()

	// A record that reads again is not an outage, even one naming nothing.
	writeImageRecordFile(t, path, `{"v": 1}`)
	d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime.Add(updateLaunchSuppression + time.Minute))
	d.triggerUpdate()
	if got := strings.Count(logs.String(), "update: the image ledger could not be read"); got != 1 {
		t.Fatalf("unreadable-ledger lines after the repair = %d, want 1 (log %q)", got, logs.String())
	}

	writeImageRecordFile(t, path, "{")
	d.clock = clockwork.NewFakeClockAt(updateTriggerTestTime.Add(2 * (updateLaunchSuppression + time.Minute)))
	d.triggerUpdate()
	if got := strings.Count(logs.String(), "update: the image ledger could not be read"); got != 2 {
		t.Errorf("unreadable-ledger lines after the second outage = %d, want 2 (log %q)", got, logs.String())
	}
}
