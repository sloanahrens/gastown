package done

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// A sandbox seat's DEFERRED or ESCALATED exit pushes the branch first, because
// the sandbox clone is the only copy of its commits and the Stop hook keeps a
// sandbox with unpushed commits alive (gt-oq8lc). Nothing else of a submission
// runs: no gate, no ready-to-land label. A host seat, an exit with nothing
// unpushed, and a failed push leave cleanup_status as they found it.
//
// Every way the push can go wrong without aborting it — a failed push, an
// unreadable source bead, an unresolvable target, a failed fetch — says so on
// the warning writer, so the exit names the base its push compared against.
func TestPushSandboxBranchBeforeDeferral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		backend     string
		cleanup     string
		pushErr     error
		fetchErr    error
		sourceErr   error
		sourceDesc  string
		wantPushed  bool
		wantCleanup string
		wantWarn    []string
	}{
		{name: "sandbox_unpushed", backend: backendOpenShell, cleanup: "unpushed", wantPushed: true, wantCleanup: cleanupStatusAfterSuccessfulPush("unpushed")},
		{name: "sandbox_has_unpushed", backend: backendOpenShell, cleanup: "has_unpushed", wantPushed: true, wantCleanup: cleanupStatusAfterSuccessfulPush("has_unpushed")},
		{name: "host_seat_pushes_nothing", backend: "", cleanup: "unpushed", wantCleanup: "unpushed"},
		{name: "sandbox_nothing_unpushed", backend: backendOpenShell, cleanup: "clean", wantCleanup: "clean"},
		{
			name: "sandbox_push_fails", backend: backendOpenShell, cleanup: "unpushed",
			pushErr: errors.New("connection reset"), wantCleanup: "unpushed",
			wantWarn: []string{"could not push " + doneTestBranch + " before deferring", "connection reset"},
		},
		{
			name: "sandbox_fetch_fails", backend: backendOpenShell, cleanup: "unpushed",
			fetchErr: errors.New("could not resolve host: origin"), wantPushed: true,
			wantCleanup: cleanupStatusAfterSuccessfulPush("unpushed"),
			wantWarn:    []string{"could not refresh origin before pushing " + doneTestBranch, "could not resolve host"},
		},
		{
			name: "sandbox_source_unreadable", backend: backendOpenShell, cleanup: "unpushed",
			sourceErr: errors.New("dolt: connection refused"), wantPushed: true,
			wantCleanup: cleanupStatusAfterSuccessfulPush("unpushed"),
			wantWarn:    []string{"could not read bd-source", "dolt: connection refused", "rig default main"},
		},
		{
			name: "sandbox_target_refused", backend: backendOpenShell, cleanup: "unpushed",
			sourceDesc: "formula_vars: base_branch=polecat/other", wantPushed: true,
			wantCleanup: cleanupStatusAfterSuccessfulPush("unpushed"),
			wantWarn:    []string{"could not resolve the landing target", "another polecat's branch", "rig default main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			h.r.sessionBackend = tt.backend
			h.r.cleanupStatus = tt.cleanup
			h.repo.fetchErr = tt.fetchErr
			if tt.pushErr != nil {
				h.repo.pushErrs = []error{tt.pushErr, tt.pushErr}
			}
			if tt.sourceDesc != "" {
				h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Status: string(beads.StatusHooked), Description: tt.sourceDesc})
			}
			if tt.sourceErr != nil {
				h.r.deps.source = func(string) (*beads.Issue, beads.Client, error) { return nil, nil, tt.sourceErr }
			}

			warn := new(bytes.Buffer)
			pushSandboxBranchBeforeDeferralTo(warn, h.r)

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
			if len(tt.wantWarn) == 0 && warn.Len() > 0 {
				t.Errorf("unexpected warning: %s", warn)
			}
			for _, want := range tt.wantWarn {
				if !strings.Contains(warn.String(), want) {
					t.Errorf("warning %q does not mention %q", warn, want)
				}
			}
		})
	}
}
