package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func stopEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// A sandbox polecat's session is ended only after a clean, pushed completion;
// everything else keeps it, because the sandbox clone holds the only copy of
// work that is not on origin.
func TestEndSandboxSessionIfDone(t *testing.T) {
	t.Parallel()
	clean := beads.AgentFields{AgentState: "done", CleanupStatus: "clean", ExitType: "COMPLETED"}
	with := func(f beads.AgentFields, edit func(*beads.AgentFields)) *beads.AgentFields {
		edit(&f)
		return &f
	}
	tests := []struct {
		name    string
		backend string
		fields  *beads.AgentFields
		readErr error
		want    bool
	}{
		{"sandbox_clean_completion_ends", "openshell", &clean, nil, true},
		{"sandbox_completion_without_exit_type_ends", "openshell", with(clean, func(f *beads.AgentFields) { f.ExitType = "" }), nil, true},
		{"host_session_is_left", "", &clean, nil, false},
		{"tmux_backend_is_left", "tmux", &clean, nil, false},
		{"still_working_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.AgentState = "working" }), nil, false},
		{"escalated_exit_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.AgentState = "stuck"; f.ExitType = "ESCALATED" }), nil, false},
		{"deferred_exit_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.AgentState = "stuck"; f.ExitType = "DEFERRED" }), nil, false},
		{"unpushed_work_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.CleanupStatus = "has_unpushed" }), nil, false},
		{"uncommitted_work_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.CleanupStatus = "has_uncommitted" }), nil, false},
		{"unknown_cleanup_status_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.CleanupStatus = "" }), nil, false},
		{"hook_still_set_is_left", "openshell", with(clean, func(f *beads.AgentFields) { f.HookBead = "gt-x" }), nil, false},
		{"missing_agent_bead_is_left", "openshell", nil, nil, false},
		{"unreadable_agent_bead_is_left", "openshell", nil, errors.New("dolt unreachable"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var scheduled []string
			var delay time.Duration
			var logged bytes.Buffer
			e := sandboxSessionEnd{
				fields: func(_, _, _ string) (*beads.AgentFields, error) { return tt.fields, tt.readErr },
				schedule: func(s string, d time.Duration) error {
					scheduled = append(scheduled, s)
					delay = d
					return nil
				},
				log: &logged,
			}
			got := endSandboxSessionIfDone(e, stopEnv(map[string]string{"GT_SESSION_BACKEND": tt.backend}), "/town", "gastown", "capable", "gt-capable")
			if got != tt.want {
				t.Fatalf("endSandboxSessionIfDone = %v, want %v", got, tt.want)
			}
			if tt.want {
				if len(scheduled) != 1 || scheduled[0] != "gt-capable" || delay != sandboxEndDelay {
					t.Errorf("scheduled %v after %s, want [gt-capable] after %s", scheduled, delay, sandboxEndDelay)
				}
				if !strings.Contains(logged.String(), "ending session gt-capable") {
					t.Errorf("no log line naming the session: %q", logged.String())
				}
			} else if len(scheduled) != 0 {
				t.Errorf("a session end was scheduled: %v", scheduled)
			}
		})
	}
}

// A kill that cannot be scheduled is reported and does not count as ended.
func TestEndSandboxSessionIfDoneReportsAScheduleFailure(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	e := sandboxSessionEnd{
		fields: func(_, _, _ string) (*beads.AgentFields, error) {
			return &beads.AgentFields{AgentState: "done", CleanupStatus: "clean"}, nil
		},
		schedule: func(string, time.Duration) error { return errors.New("fork failed") },
		log:      &logged,
	}
	if endSandboxSessionIfDone(e, stopEnv(map[string]string{"GT_SESSION_BACKEND": "openshell"}), "/town", "gastown", "capable", "gt-capable") {
		t.Fatal("reported ended although the kill could not be scheduled")
	}
	if !strings.Contains(logged.String(), "could not schedule") || !strings.Contains(logged.String(), "fork failed") {
		t.Errorf("failure not reported: %q", logged.String())
	}
}
