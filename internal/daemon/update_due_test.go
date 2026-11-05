package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

var testUpdateSettings = config.UpdateSettings{
	Enabled:     true,
	WindowHour:  3,
	MinInterval: 3 * time.Hour,
	MaxRuntime:  2 * time.Hour,
}

func TestUpdateDueNightlyWindowOncePerDay(t *testing.T) {
	t.Parallel()

	windowStart := time.Date(2026, 3, 10, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		now         time.Time
		lastStarted time.Time
		wantDue     bool
		wantReason  string
	}{
		{"inside window, never run", windowStart.Add(30 * time.Minute), time.Time{}, true, "nightly"},
		{"inside window, last run yesterday", windowStart.Add(30 * time.Minute), windowStart.AddDate(0, 0, -1).Add(10 * time.Minute), true, "nightly"},
		{"inside window, run started just after it opened", windowStart.Add(30 * time.Minute), windowStart.Add(5 * time.Minute), false, "nothing due"},
		{"inside window, run started exactly as it opened", windowStart.Add(30 * time.Minute), windowStart, false, "nothing due"},
		{"at window start, run yesterday", windowStart, windowStart.AddDate(0, 0, -1), true, "nightly"},
		{"at window end, never run", windowStart.Add(updateWindowLength), time.Time{}, false, "nothing due"},
		{"before window, never run", windowStart.Add(-30 * time.Minute), time.Time{}, false, "nothing due"},
		{"after window, never run", windowStart.Add(2 * time.Hour), time.Time{}, false, "nothing due"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			facts := UpdateFacts{LastStarted: tt.lastStarted}
			due, reason := updateDue(tt.now, testUpdateSettings, facts)
			if due != tt.wantDue {
				t.Fatalf("updateDue() due = %v, want %v (reason %q)", due, tt.wantDue, reason)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("updateDue() reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

// A window that opens late in the day stays open into the next morning, and
// still holds one run per window.
func TestUpdateDueNightlyWindowCarriesPastMidnight(t *testing.T) {
	t.Parallel()

	settings := testUpdateSettings
	settings.WindowHour, settings.WindowMinute = 23, 30
	opened := time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
	tests := []struct {
		name        string
		now         time.Time
		lastStarted time.Time
		wantDue     bool
	}{
		{"before midnight, no run since it opened", opened.Add(20 * time.Minute), opened.Add(-time.Hour), true},
		{"after midnight, no run since it opened", opened.Add(45 * time.Minute), opened.Add(-time.Hour), true},
		{"after midnight, run started inside the window", opened.Add(45 * time.Minute), opened.Add(10 * time.Minute), false},
		{"after the window closed", opened.Add(updateWindowLength), opened.Add(-time.Hour), false},
		{"the next evening before it opens", opened.Add(20 * time.Hour), opened.Add(-time.Hour), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			due, reason := updateDue(tt.now, settings, UpdateFacts{LastStarted: tt.lastStarted})
			if due != tt.wantDue {
				t.Fatalf("updateDue() due = %v, want %v (reason %q)", due, tt.wantDue, reason)
			}
		})
	}
}

func TestUpdateDueWhenMainMovedAfterMinInterval(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mainMoved  bool
		waiting    int
		lastEnded  time.Time
		wantDue    bool
		wantReason string
	}{
		{"moved, waiting, ended 4h ago", true, 2, now.Add(-4 * time.Hour), true, "main moved"},
		{"moved, waiting, ended exactly min interval ago", true, 1, now.Add(-testUpdateSettings.MinInterval), true, "main moved"},
		{"moved, waiting, ended 1h ago", true, 2, now.Add(-1 * time.Hour), false, "nothing due"},
		{"moved, nothing waiting", true, 0, now.Add(-4 * time.Hour), false, "nothing due"},
		{"moved, waiting, never run before", true, 1, time.Time{}, true, "main moved"},
		{"main unmoved, waiting, never run before", false, 2, time.Time{}, false, "nothing due"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			facts := UpdateFacts{MainMoved: tt.mainMoved, Waiting: tt.waiting, LastEnded: tt.lastEnded}
			due, reason := updateDue(now, testUpdateSettings, facts)
			if due != tt.wantDue {
				t.Fatalf("updateDue() due = %v, want %v (reason %q)", due, tt.wantDue, reason)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("updateDue() reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

// A run that failed or aborted on main's current target is not due under
// either rule until main moves; a run on an older target, or one that did not
// fail, keeps today's answer.
func TestUpdateDueHoldsAFailedRunOnMainsTarget(t *testing.T) {
	t.Parallel()

	const target, older = "a1b2c3", "0f0f0f"
	noon := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	nightly := time.Date(2026, 3, 10, 3, 30, 0, 0, time.UTC)
	lastNight := time.Date(2026, 3, 9, 3, 10, 0, 0, time.UTC)
	tests := []struct {
		name       string
		now        time.Time
		facts      UpdateFacts
		wantDue    bool
		wantReason string
	}{
		{
			name: "failed on main's target, min interval elapsed, main moved and waiting",
			now:  noon,
			facts: UpdateFacts{
				MainMoved: true, Waiting: 2,
				LastStarted: noon.Add(-5 * time.Hour), LastEnded: noon.Add(-4 * time.Hour),
				LastOutcome: update.OutcomeFailed, LastTarget: target, MainSHA: target,
			},
			wantDue:    false,
			wantReason: "gt update by hand",
		},
		{
			name: "aborted on main's target, nightly window open",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomeAborted, LastTarget: target, MainSHA: target,
			},
			wantDue:    false,
			wantReason: "gt update by hand",
		},
		{
			name: "failed on main's target, nightly window open, main unmoved",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomeFailed, LastTarget: target, MainSHA: target,
			},
			wantDue:    false,
			wantReason: "gt update by hand",
		},
		{
			name: "failed on main's target, main sha unknown",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomeFailed, LastTarget: target,
			},
			wantDue:    false,
			wantReason: "main's sha is unknown",
		},
		{
			name: "failed on an older target, main moved and waiting",
			now:  noon,
			facts: UpdateFacts{
				MainMoved: true, Waiting: 1,
				LastStarted: noon.Add(-5 * time.Hour), LastEnded: noon.Add(-4 * time.Hour),
				LastOutcome: update.OutcomeFailed, LastTarget: older, MainSHA: target,
			},
			wantDue:    true,
			wantReason: "main moved",
		},
		{
			name: "failed on an older target, nightly window open",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomeFailed, LastTarget: older, MainSHA: target,
			},
			wantDue:    true,
			wantReason: "nightly",
		},
		{
			name: "partial on main's target, nightly window open",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomePartial, LastTarget: target, MainSHA: target,
			},
			wantDue:    true,
			wantReason: "nightly",
		},
		{
			name: "interrupted on main's target, nightly window open",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomeInterrupted, LastTarget: target, MainSHA: target,
			},
			wantDue:    true,
			wantReason: "nightly",
		},
		{
			name: "ok on main's target, nightly window open",
			now:  nightly,
			facts: UpdateFacts{
				LastStarted: lastNight, LastEnded: lastNight.Add(time.Minute),
				LastOutcome: update.OutcomeOK, LastTarget: target, MainSHA: target,
			},
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

func TestUpdateNeverDueUnderStandbyRunningPendingOrDisabled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 3, 30, 0, 0, time.UTC)
	dueFacts := UpdateFacts{
		Standby:        "",
		Running:        false,
		InstallPending: false,
		MainMoved:      false,
		Waiting:        0,
		LastStarted:    time.Time{},
		LastEnded:      time.Time{},
	}

	if due, reason := updateDue(now, testUpdateSettings, dueFacts); !due {
		t.Fatalf("baseline updateDue() = false (%q), want the nightly rule to fire", reason)
	}

	tests := []struct {
		name       string
		settings   config.UpdateSettings
		facts      UpdateFacts
		wantReason string
	}{
		{"disabled", config.UpdateSettings{WindowHour: 3, MinInterval: 3 * time.Hour, MaxRuntime: 2 * time.Hour}, dueFacts, "disabled"},
		{"standby", testUpdateSettings, UpdateFacts{Standby: "operator hold", LastStarted: time.Time{}}, "operator hold"},
		{"running", testUpdateSettings, UpdateFacts{Running: true, LastStarted: time.Time{}}, "in progress"},
		{"install pending", testUpdateSettings, UpdateFacts{InstallPending: true, LastStarted: time.Time{}}, "install"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			due, reason := updateDue(now, tt.settings, tt.facts)
			if due {
				t.Fatalf("updateDue() = true, want false under %s", tt.name)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("updateDue() reason = %q, want it to name %q", reason, tt.wantReason)
			}
		})
	}
}
