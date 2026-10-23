package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

func stopEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// sandboxStopRun is one call of endSandboxSessionIfDone with its reads and kill
// recorded.
type sandboxStopRun struct {
	ended     bool
	scheduled []string
	delay     time.Duration
	logged    string
}

func runSandboxStop(backend string, fields *beads.AgentFields, readErr error) sandboxStopRun {
	var r sandboxStopRun
	var logged bytes.Buffer
	e := sandboxSessionEnd{
		fields: func(_, _, _ string) (*beads.AgentFields, error) { return fields, readErr },
		schedule: func(s string, d time.Duration) error {
			r.scheduled = append(r.scheduled, s)
			r.delay = d
			return nil
		},
		log: &logged,
	}
	r.ended = endSandboxSessionIfDone(e, stopEnv(map[string]string{"GT_SESSION_BACKEND": backend}), "/town", "gastown", "capable", "gt-capable")
	r.logged = logged.String()
	return r
}

// A sandbox polecat's session is ended only after a clean, pushed completion;
// everything else keeps it, because the sandbox clone holds the only copy of
// work that is not on origin. Every backend meets every agent bead state, and
// only openshell with a clean completion ends the session, once.
func TestEndSandboxSessionIfDone(t *testing.T) {
	t.Parallel()
	clean := beads.AgentFields{AgentState: "done", CleanupStatus: "clean", ExitType: "COMPLETED"}
	with := func(edit func(*beads.AgentFields)) *beads.AgentFields {
		f := clean
		edit(&f)
		return &f
	}
	states := []struct {
		name    string
		fields  *beads.AgentFields
		readErr error
		ends    bool
		logPart string // what the log says when the session is left
	}{
		{"done_clean", &clean, nil, true, ""},
		{"no_exit_type", with(func(f *beads.AgentFields) { f.ExitType = "" }), nil, true, ""},
		{"working", with(func(f *beads.AgentFields) { f.AgentState = "working" }), nil, false, `agent_state is "working"`},
		{"stuck_escalated", with(func(f *beads.AgentFields) { f.AgentState = "stuck"; f.ExitType = "ESCALATED" }), nil, false, `agent_state is "stuck"`},
		{"stuck_deferred", with(func(f *beads.AgentFields) { f.AgentState = "stuck"; f.ExitType = "DEFERRED" }), nil, false, `agent_state is "stuck"`},
		{"done_but_exit_escalated", with(func(f *beads.AgentFields) { f.ExitType = "ESCALATED" }), nil, false, "exit_type is ESCALATED"},
		{"has_unpushed", with(func(f *beads.AgentFields) { f.CleanupStatus = "has_unpushed" }), nil, false, `cleanup_status is "has_unpushed"`},
		{"has_uncommitted", with(func(f *beads.AgentFields) { f.CleanupStatus = "has_uncommitted" }), nil, false, `cleanup_status is "has_uncommitted"`},
		{"has_stash", with(func(f *beads.AgentFields) { f.CleanupStatus = "has_stash" }), nil, false, `cleanup_status is "has_stash"`},
		{"unknown_cleanup", with(func(f *beads.AgentFields) { f.CleanupStatus = "" }), nil, false, `cleanup_status is ""`},
		{"hook_still_set", with(func(f *beads.AgentFields) { f.HookBead = "gt-x" }), nil, false, "hook_bead gt-x is still set"},
		{"missing", nil, nil, false, "no agent bead"},
		{"unreadable", nil, errors.New("dolt unreachable"), false, "dolt unreachable"},
	}
	backends := []string{session.BackendOpenShell, session.BackendTmux, ""} // "" is a host session with no backend set
	for _, backend := range backends {
		label := backend
		if label == "" {
			label = "host"
		}
		for _, st := range states {
			t.Run(fmt.Sprintf("%s/%s", label, st.name), func(t *testing.T) {
				t.Parallel()
				want := backend == session.BackendOpenShell && st.ends
				r := runSandboxStop(backend, st.fields, st.readErr)
				if r.ended != want {
					t.Fatalf("endSandboxSessionIfDone = %v, want %v (log %q)", r.ended, want, r.logged)
				}
				switch {
				case want:
					if len(r.scheduled) != 1 || r.scheduled[0] != "gt-capable" || r.delay != sandboxEndDelay {
						t.Errorf("scheduled %v after %s, want [gt-capable] after %s", r.scheduled, r.delay, sandboxEndDelay)
					}
					if !strings.Contains(r.logged, "ending session gt-capable") {
						t.Errorf("no log line naming the session: %q", r.logged)
					}
				case len(r.scheduled) != 0:
					t.Errorf("a session end was scheduled: %v", r.scheduled)
				case backend == session.BackendOpenShell && !strings.Contains(r.logged, st.logPart):
					t.Errorf("a sandbox session was left without saying why (want %q): %q", st.logPart, r.logged)
				case backend != session.BackendOpenShell && r.logged != "":
					t.Errorf("a host session logged: %q", r.logged)
				}
			})
		}
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

// The Stop hook acts on a finished session only on the branch where the
// heartbeat says gt done already ran (exiting or idle), and then once; a polecat
// still working, or one with no heartbeat, is not done. runTapPolecatStop hands
// polecatStopGtDoneRan the sandbox end as its callback, so a dropped call or one
// moved off that branch fails here.
func TestPolecatStopGtDoneRan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hb   *polecat.SessionHeartbeat
		done bool
	}{
		{"exiting_after_gt_done", &polecat.SessionHeartbeat{State: polecat.HeartbeatExiting}, true},
		{"idle", &polecat.SessionHeartbeat{State: polecat.HeartbeatIdle}, true},
		{"working", &polecat.SessionHeartbeat{State: polecat.HeartbeatWorking}, false},
		{"v1_heartbeat_without_state", &polecat.SessionHeartbeat{}, false},
		{"no_heartbeat", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			got := polecatStopGtDoneRan(tc.hb, func() { calls++ })
			wantCalls := 0
			if tc.done {
				wantCalls = 1
			}
			if got != tc.done || calls != wantCalls {
				t.Errorf("polecatStopGtDoneRan = %v with %d callbacks, want %v with %d", got, calls, tc.done, wantCalls)
			}
		})
	}
}

// A fractional delay reaches sleep whole rather than truncated to zero.
func TestSandboxKillCommandKeepsAFractionalDelay(t *testing.T) {
	t.Parallel()
	cmd := sandboxKillCommand("gt-capable", 1500*time.Millisecond)
	if got := cmd.Args[len(cmd.Args)-1]; got != "1.5" {
		t.Errorf("delay argument = %q, want 1.5 (args %q)", got, cmd.Args)
	}
}
