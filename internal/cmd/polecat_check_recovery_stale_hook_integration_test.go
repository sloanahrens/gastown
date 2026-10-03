//go:build integration

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestIntegrationCheckRecoveryResolvesHookFromLiveBeadState is the gt-eqiid
// regression, driven through checkRecoveryForPolecat itself — the function
// every consumer of the verdict reads (`gt polecat check-recovery`,
// `gt polecat check-recovery-batch`, the witness's sweep).
//
// The unit tests pin the two policies in isolation (ClassifyHookBead,
// DecideWorkstate's SUBMITTED verdict). What they cannot catch is the wiring
// between them, which is where the reported false positives actually came
// from: check-recovery read the agent bead's hook_bead reference and asked
// only whether the bead it named was closed.
//
// The fixtures are the live ones, transcribed from
// `gt polecat check-recovery-batch gastown` on 2026-10-01:
//
//   - flint and garnet, hook_bead=gt-2xqtj, and granite, hook_bead=gt-gyw5w.
//     Both referenced beads are status=deferred with no assignee, and `gt hook`
//     run in either worktree renders "Nothing on hook - no work slung". The
//     checker reported "has work on hook" anyway.
//   - amber (gt-acdfp) and pearl (gt-tt8sg): status=hooked with
//     gt:ready-to-land, assignee the polecat. gt done submitted both. The
//     checker reported NEEDS_RECOVERY / lifecycle_state=submitted.
//
// The third case is the control the first two must not swallow: a hook bead
// that really is live work still refuses.
//
// The worktree is a real repository per case (the verdict re-derives from live
// git, and setupGitStateRemoteRepo is the shared integration-tier fixture), but
// the database is a beadsfake — the same seam the missing-cleanup-status
// integration test drives, so this file costs no Docker slot of its own.
func TestIntegrationCheckRecoveryResolvesHookFromLiveBeadState(t *testing.T) {
	t.Parallel()

	const (
		rigName     = "gastown"
		polecatName = "flint"
		branch      = "polecat/flint/gt-2xqtj"
	)
	assignee := rigName + "/polecats/" + polecatName

	tests := []struct {
		name         string
		state        polecat.State
		cleanup      string
		hookBead     string
		hookStatus   string
		hookLabels   []string
		wantVerdict  string
		wantSafe     bool
		wantRecovery bool
		// wantHookBlocker asserts the hook-still-set predicate is (or is not)
		// among the named blockers, which is the reported symptom either way.
		wantHookBlocker bool
	}{
		{
			// flint/garnet/granite: the reference outlived the work it named.
			name:         "a deferred and unassigned hook bead is a stale reference",
			state:        polecat.StateIdle,
			cleanup:      "clean",
			hookBead:     "gt-2xqtj",
			hookStatus:   string(beads.StatusDeferred),
			wantVerdict:  polecat.WorkstateVerdictSafeToNuke,
			wantSafe:     true,
			wantRecovery: false,
		},
		{
			// The fail-closed control for the case above: an unmodeled status is
			// not evidence of staleness, and this classifier gates a nuke.
			name:            "a hook bead in an unrecognized status still blocks",
			state:           polecat.StateIdle,
			cleanup:         "clean",
			hookBead:        "gt-mystery",
			hookStatus:      "quarantined",
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery:    true,
			wantHookBlocker: true,
		},
		{
			// amber/pearl: gt done submitted the work; the landing worker owns
			// the bead and the hook stays set on purpose.
			name:         "a hook bead submitted for landing is left to the landing worker",
			state:        polecat.StateSubmitted,
			cleanup:      "clean",
			hookBead:     "gt-acdfp",
			hookStatus:   string(beads.IssueStatusHooked),
			hookLabels:   []string{"gt:ready-to-land"},
			wantVerdict:  polecat.WorkstateVerdictSubmitted,
			wantSafe:     false,
			wantRecovery: false,
		},
		{
			// The control: this one really is live work, and a hook-still-set
			// refusal is correct.
			name:            "a hooked bead with no submission is still live work",
			state:           polecat.StateIdle,
			cleanup:         "clean",
			hookBead:        "gt-work",
			hookStatus:      string(beads.IssueStatusHooked),
			wantVerdict:     polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery:    true,
			wantHookBlocker: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &rig.Rig{Name: rigName, Path: filepath.Join(t.TempDir(), rigName)}
			agentBeadID := polecatBeadIDForRig(r, rigName, polecatName)

			repo := setupGitStateRemoteRepo(t)
			runGitCmd(t, repo, "switch", "-c", branch, "main")

			// The worktree is clean, so the ONLY thing that can refuse these
			// fixtures is the hook reference — which is the point.
			db := &recoveryDB{Fake: beadsfake.New()}
			db.Seed(beads.Issue{
				ID: agentBeadID, Title: "Polecat " + polecatName, Status: "open",
				Labels:      []string{"gt:agent"},
				Description: "role_type: polecat\nrig: " + rigName + "\nagent_state: idle\nhook_bead: " + tt.hookBead + "\ncleanup_status: " + tt.cleanup + "\nactive_mr: null\n",
			})
			db.Seed(beads.Issue{
				ID: tt.hookBead, Title: "hooked work", Status: tt.hookStatus,
				Labels: tt.hookLabels, Assignee: assignee,
			})

			p := &polecat.Polecat{
				Name: polecatName, Rig: rigName, State: tt.state,
				ClonePath: repo, Branch: branch, Issue: tt.hookBead,
			}
			status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)

			if status.Verdict != tt.wantVerdict || status.SafeToNuke != tt.wantSafe || status.NeedsRecovery != tt.wantRecovery {
				t.Fatalf("checkRecoveryForPolecat() = verdict %s safe_to_nuke=%v needs_recovery=%v, want %s/%v/%v (status %+v)",
					status.Verdict, status.SafeToNuke, status.NeedsRecovery,
					tt.wantVerdict, tt.wantSafe, tt.wantRecovery, status)
			}

			gotHookBlocker := false
			for _, blocker := range status.Blockers {
				if strings.Contains(blocker, "hook") {
					gotHookBlocker = true
				}
			}
			if gotHookBlocker != tt.wantHookBlocker {
				t.Fatalf("hook blocker present = %v, want %v (blockers %v)", gotHookBlocker, tt.wantHookBlocker, status.Blockers)
			}
		})
	}
}
