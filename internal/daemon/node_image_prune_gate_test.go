package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// TestNodeImagePruneDueTable drives every outcome of an update run through
// nodeImagePruneDue: only an ok run that ended with a new id selects the prune.
func TestNodeImagePruneDueTable(t *testing.T) {
	t.Parallel()

	ended := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	ok := update.State{RunID: "run-2", Outcome: update.OutcomeOK, EndedAt: ended}
	tests := []struct {
		name       string
		st         update.State
		lastPruned string
		jobs       []string
		wantOK     bool
		wantWhy    string
	}{
		{"ok run selects", ok, "run-1", nil, true, ""},
		{"running refuses", update.State{RunID: "run-2", Outcome: update.OutcomeRunning, EndedAt: ended}, "run-1", nil, false, "running"},
		{"partial refuses", update.State{RunID: "run-2", Outcome: update.OutcomePartial, EndedAt: ended}, "run-1", nil, false, "partial"},
		{"failed refuses", update.State{RunID: "run-2", Outcome: update.OutcomeFailed, EndedAt: ended}, "run-1", nil, false, "failed"},
		{"aborted refuses", update.State{RunID: "run-2", Outcome: update.OutcomeAborted, EndedAt: ended}, "run-1", nil, false, "aborted"},
		{"interrupted refuses", update.State{RunID: "run-2", Outcome: update.OutcomeInterrupted, EndedAt: ended}, "run-1", nil, false, "interrupted"},
		{"already pruned refuses", ok, "run-2", nil, false, "already pruned"},
		{"update Job active refuses", ok, "run-1", []string{"update-job-a"}, false, "update Job active"},
		{"no run recorded refuses", update.State{}, "", nil, false, "no run recorded"},
		{"ok run without end time refuses", update.State{RunID: "run-2", Outcome: update.OutcomeOK}, "run-1", nil, false, "no end time"},
		{"ok run without run id refuses", update.State{Outcome: update.OutcomeOK, EndedAt: ended}, "", nil, false, "no run id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID, got, why := nodeImagePruneDue(tt.st, tt.lastPruned, tt.jobs)
			if got != tt.wantOK {
				t.Fatalf("nodeImagePruneDue() ok = %v, want %v (why %q)", got, tt.wantOK, why)
			}
			if got && runID != tt.st.RunID {
				t.Errorf("nodeImagePruneDue() runID = %q, want %q", runID, tt.st.RunID)
			}
			if !got && !strings.Contains(why, tt.wantWhy) {
				t.Errorf("nodeImagePruneDue() why = %q, want it to contain %q", why, tt.wantWhy)
			}
		})
	}
}

// TestNodeImagePruneDueNeverDuringARoll holds an ok run beside an active update
// Job: the state reads as a finished run, but the roll is still in flight.
func TestNodeImagePruneDueNeverDuringARoll(t *testing.T) {
	t.Parallel()

	st := update.State{RunID: "run-2", Outcome: update.OutcomeOK, EndedAt: time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)}
	_, got, why := nodeImagePruneDue(st, "run-1", []string{"update-job-a", "update-job-b"})
	if got {
		t.Fatalf("nodeImagePruneDue() selected the prune during a roll (why %q)", why)
	}
}
