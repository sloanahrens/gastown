package polecat

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// erroringProbe is a fake tmux whose routed probe fails, as it does when a
// sandbox has no pod: the exec into the sandbox errors instead of answering no
// session.
type erroringProbe struct {
	*fakeSessionTmux
	err error
}

func (e erroringProbe) HasSession(string) (bool, error) { return false, e.err }

// nukeStopCase is one way a sandboxed seat's session can be gone when nuke stops
// it: how the probe answers, the gateway's phase for the sandbox, and whether
// the gateway or the delete fails.
type nukeStopCase struct {
	probeErr  error
	phase     string
	statusErr error
	deleteErr error
	// fields is the seat's agent bead; nil with fieldsErr nil is a seat whose
	// gt done recorded a clean cleanup status, the usual finished seat.
	fields    *beads.AgentFields
	fieldsErr error
}

// nukeStopResult is what one nuke-style Stop did: its error, the sandboxes the
// deleter was asked to remove, whether the record survived, and the warnings
// logged.
type nukeStopResult struct {
	err      error
	deleted  []string
	recorded bool
	warnings []string
}

// runNukeStop drives SessionManager.Stop the way gt polecat nuke does for a
// sandboxed seat with no tmux session: a record for the seat's sandbox, a gateway
// that reports phase, and a probe that finds no session (or fails).
func runNukeStop(t *testing.T, c nukeStopCase) nukeStopResult {
	t.Helper()
	town := t.TempDir()
	base := newFakeSessionTmux()
	var tm sessionTmux = base
	if c.probeErr != nil {
		tm = erroringProbe{fakeSessionTmux: base, err: c.probeErr}
	}
	m := &SessionManager{tmux: tm, rig: &rig.Rig{Name: "test-rig", Path: filepath.Join(town, "test-rig"), Polecats: []string{"Toast"}}, gits: newWorld().opener()}
	sess := m.SessionName("Toast")
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sess, Rig: "test-rig", Polecat: "Toast", Gateway: "k8s"}); err != nil {
		t.Fatal(err)
	}

	var res nukeStopResult
	m.sandboxStatus = func(gateway, name string) (sandbox.Status, error) {
		if gateway != "k8s" || name != sess {
			t.Errorf("gateway status asked of (%q, %q), want (k8s, %s)", gateway, name, sess)
		}
		if c.statusErr != nil {
			return sandbox.Status{}, c.statusErr
		}
		return sandbox.Status{Name: sess, Phase: c.phase}, nil
	}
	m.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
		res.deleted = append(res.deleted, rec.Session)
		return c.deleteErr
	}
	m.sandboxPusher = func(string, session.SandboxRecord) error { return nil }
	fields := c.fields
	if fields == nil && c.fieldsErr == nil {
		fields = &beads.AgentFields{CleanupStatus: string(CleanupClean)}
	}
	m.agentFields = func(string) (*beads.AgentFields, error) {
		return fields, c.fieldsErr
	}
	m.warn = func(format string, args ...any) {
		res.warnings = append(res.warnings, fmt.Sprintf(format, args...))
	}

	res.err = m.Stop("Toast", true)
	_, recorded, err := session.ReadSandboxRecord(town, sess)
	if err != nil {
		t.Fatalf("reading the record after Stop: %v", err)
	}
	res.recorded = recorded
	return res
}

// TestNukeErroredSandboxDeletesRecord: nuking a seat whose sandbox the gateway
// reports in the Error phase deletes that sandbox and its record, so the seat
// name is free for the dispatcher (gt-jhoi1). The probe either answers no session
// or fails, as it does with no pod.
func TestNukeErroredSandboxDeletesRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		probe error
	}{
		{name: "probe_finds_no_session"},
		{name: "probe_fails_for_want_of_a_pod", probe: errors.New("exec into sandbox: pod not found")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := runNukeStop(t, nukeStopCase{probeErr: tt.probe, phase: "Error"})

			if len(res.deleted) != 1 {
				t.Fatalf("deleted %v, want the errored sandbox deleted once", res.deleted)
			}
			if res.recorded {
				t.Errorf("record survived; the seat name would stay occupied")
			}
		})
	}
}

// TestNukeErroredSandboxLeavesOtherPhases: a record whose sandbox is not in a
// terminal failure phase keeps its current handling: this path deletes nothing
// and the record survives. A Ready sandbox whose probe errors transiently is the
// case that must not be deleted.
func TestNukeErroredSandboxLeavesOtherPhases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		phase string
		probe error
	}{
		{name: "ready_no_session", phase: "Ready"},
		{name: "ready_probe_fails_transiently", phase: "Ready", probe: errors.New("exec into sandbox: timeout")},
		{name: "stopped", phase: "Stopped"},
		{name: "provisioning", phase: "Provisioning"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := runNukeStop(t, nukeStopCase{probeErr: tt.probe, phase: tt.phase})

			if len(res.deleted) != 0 {
				t.Errorf("deleted %v in phase %s, want nothing deleted", res.deleted, tt.phase)
			}
			if !res.recorded {
				t.Errorf("record was removed in phase %s, want it kept", tt.phase)
			}
		})
	}
}

// TestNukeErroredSandboxDeleteFailureDoesNotFailNuke: a failed delete is logged,
// and the stop still answers what it answered before: ErrSessionNotFound for a
// session that was not running, so nuke carries on. It is never reported as an
// unremoved sandbox that fails the nuke, and the record survives for a retry.
func TestNukeErroredSandboxDeleteFailureDoesNotFailNuke(t *testing.T) {
	t.Parallel()
	res := runNukeStop(t, nukeStopCase{phase: "Error", deleteErr: errors.New("gateway unreachable")})

	if !errors.Is(res.err, ErrSessionNotFound) {
		t.Fatalf("Stop = %v, want ErrSessionNotFound: the delete failure must not fail the stop", res.err)
	}
	if errors.Is(res.err, ErrSandboxNotRemoved) {
		t.Errorf("Stop = %v: a failed delete is logged, not reported as an unremoved sandbox", res.err)
	}
	if !res.recorded {
		t.Errorf("record was removed after a failed delete, want it kept for a retry")
	}
	if !anyContains(res.warnings, "gateway unreachable") {
		t.Errorf("warnings %q do not name the failed delete", res.warnings)
	}
}

// TestNukeErroredSandboxKeepsRecordOnUnansweredGateway: when the gateway cannot
// say what phase the sandbox is in, nothing is deleted and the failure is logged
// rather than read as a phase, so an outage never removes a live sandbox.
func TestNukeErroredSandboxKeepsRecordOnUnansweredGateway(t *testing.T) {
	t.Parallel()
	res := runNukeStop(t, nukeStopCase{phase: "Error", statusErr: errors.New("gateway timed out")})

	if len(res.deleted) != 0 {
		t.Errorf("deleted %v while the gateway could not answer, want nothing deleted", res.deleted)
	}
	if !res.recorded {
		t.Errorf("record was removed while the gateway could not answer, want it kept")
	}
	if !anyContains(res.warnings, "gateway timed out") {
		t.Errorf("warnings %q do not name the unanswered gateway", res.warnings)
	}
}

// TestNukeErroredSandboxKeepsUnprovenWork: an errored sandbox may hold commits
// that never reached origin, and with no pod nothing can push them. A stop
// deletes it only when the agent bead proves the work landed on origin, so every
// other recorded state keeps the sandbox and its record, and the stop still
// answers ErrSessionNotFound, as nuke expects.
func TestNukeErroredSandboxKeepsUnprovenWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fields *beads.AgentFields
		err    error
		reason string
	}{
		{name: "unpushed_commits", fields: &beads.AgentFields{CleanupStatus: string(CleanupUnpushed)}, reason: "cleanup_status=has_unpushed"},
		{name: "uncommitted_work", fields: &beads.AgentFields{CleanupStatus: string(CleanupUncommitted)}, reason: "cleanup_status=has_uncommitted"},
		{name: "status_unknown", fields: &beads.AgentFields{CleanupStatus: string(CleanupUnknown)}, reason: "cleanup_status=unknown"},
		{name: "status_never_recorded", fields: &beads.AgentFields{}, reason: "cleanup_status=<missing>"},
		{name: "push_refused", fields: &beads.AgentFields{CleanupStatus: string(CleanupClean), PushFailed: true}, reason: "push_failed=true"},
		{name: "agent_bead_unreadable", err: errors.New("bd show: timeout"), reason: "bd show: timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := runNukeStop(t, nukeStopCase{phase: "Error", fields: tt.fields, fieldsErr: tt.err})

			if len(res.deleted) != 0 {
				t.Errorf("deleted %v while its work is not proven on origin, want nothing deleted", res.deleted)
			}
			if !res.recorded {
				t.Errorf("record was removed while its work is not proven on origin, want it kept")
			}
			if !errors.Is(res.err, ErrSessionNotFound) {
				t.Errorf("Stop = %v, want ErrSessionNotFound: a kept sandbox must not fail the stop", res.err)
			}
			if !anyContains(res.warnings, tt.reason) {
				t.Errorf("warnings %q do not name why the sandbox is kept (%s)", res.warnings, tt.reason)
			}
		})
	}
}

func anyContains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
