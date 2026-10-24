package done

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// A sandbox seat's DEFERRED or ESCALATED exit pushes the branch first, because
// the sandbox clone is the only copy of its commits and the Stop hook keeps a
// sandbox with unpushed commits alive (gt-oq8lc). Nothing else of a submission
// runs: no gate, no ready-to-land label. A host seat, an exit with nothing
// unpushed, and a failed push leave cleanup_status as they found it.
func TestPushSandboxBranchBeforeDeferral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		exit        string
		backend     string
		cleanup     string
		pushErr     error
		wantPushed  bool
		wantCleanup string
	}{
		{name: "sandbox_deferred_unpushed", exit: ExitDeferred, backend: backendOpenShell, cleanup: "unpushed", wantPushed: true, wantCleanup: cleanupStatusAfterSuccessfulPush("unpushed")},
		{name: "sandbox_escalated_has_unpushed", exit: ExitEscalated, backend: backendOpenShell, cleanup: "has_unpushed", wantPushed: true, wantCleanup: cleanupStatusAfterSuccessfulPush("has_unpushed")},
		{name: "host_seat_pushes_nothing", exit: ExitDeferred, backend: "", cleanup: "unpushed", wantCleanup: "unpushed"},
		{name: "sandbox_nothing_unpushed", exit: ExitDeferred, backend: backendOpenShell, cleanup: "clean", wantCleanup: "clean"},
		{name: "sandbox_push_fails", exit: ExitDeferred, backend: backendOpenShell, cleanup: "unpushed", pushErr: errors.New("connection reset"), wantCleanup: "unpushed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			h.r.sessionBackend = tt.backend
			h.r.cleanupStatus = tt.cleanup
			if tt.pushErr != nil {
				h.repo.pushErrs = []error{tt.pushErr, tt.pushErr}
			}

			signalNonCompletedExit(h.r, tt.exit)

			pushed := h.repo.origin[doneTestBranch] == h.repo.head
			if pushed != tt.wantPushed {
				t.Errorf("origin/%s = %q (head %s): pushed = %v, want %v", doneTestBranch, h.repo.origin[doneTestBranch], h.repo.head, pushed, tt.wantPushed)
			}
			if h.r.cleanupStatus != tt.wantCleanup {
				t.Errorf("cleanup status = %q, want %q", h.r.cleanupStatus, tt.wantCleanup)
			}
			if len(h.gate.heads) != 0 || len(h.stages) != 0 {
				t.Errorf("a deferral ran submission stages: gate %v stages %v", h.gate.heads, h.stages)
			}
			if issue := h.source(t); beads.HasLabel(issue, land.LabelReadyToLand) {
				t.Errorf("a deferral marked %s ready to land", issue.ID)
			}
		})
	}
}
