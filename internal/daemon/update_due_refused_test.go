package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
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

// TestPreRollGuardRefusesAnUnreadableLedger holds an image record the ledger
// cannot read to an unknown fact: the heartbeat makes no decision on it and
// launches nothing, rather than read the damaged file as no refusal.
func TestPreRollGuardRefusesAnUnreadableLedger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "the record does not parse", content: "{"},
		{name: "the record version is not one this package reads", content: `{"v": 99}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, logs, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{
				Enabled: true,
				Window:  "03:00",
			})
			setUpdateSource(t, d)
			path := filepath.Join(update.Dir(d.config.TownRoot), "images.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("create the update state directory: %v", err)
			}
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write the damaged image record: %v", err)
			}

			if _, err := d.updateFacts(updateTriggerTestTime); err == nil {
				t.Fatal("updateFacts() error = nil, want the unreadable record refused")
			}

			d.triggerUpdate()

			if *launches != 0 {
				t.Fatalf("launches = %d, want none: an unreadable record is unknown, not a reason to launch", *launches)
			}
			if got := logs.String(); !strings.Contains(got, "not launching") {
				t.Errorf("log = %q, want the refusal logged", got)
			}
		})
	}
}
