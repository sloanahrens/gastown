package polecat

import (
	"slices"
	"strings"
	"testing"
)

// TestDecideWorkstateSubmittedIsNotARecoveryCase is the gt-eqiid regression for
// the class-1 false positive: `gt polecat check-recovery-batch` reported
// NEEDS_RECOVERY / lifecycle_state=submitted for amber (gt-acdfp) and pearl
// (gt-tt8sg) — polecats whose bead is status=hooked with gt:ready-to-land,
// i.e. work gt done handed to the landing worker.
//
// StateSubmitted is deliberately not reuse-eligible, so the classifier's
// not-idle branch refused it. But a submitted seat has nothing to recover:
// mol-witness-patrol leaves it alone until the landing takes the bead, and
// StateSubmitted's own doc comment says it is 'not a stall'. The assert below
// pins all three consequences — no recovery, no nuke, no reuse — because
// getting two of them right still leaves a witness with work to do.
func TestDecideWorkstateSubmittedIsNotARecoveryCase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   WorkstateInput
	}{
		{
			// The shape the CLI builds today: the manager resolved the
			// lifecycle state from the same bead.
			name: "lifecycle state says submitted",
			in:   WorkstateInput{State: StateSubmitted, CleanupStatus: CleanupClean, Branch: "polecat/amber/gt-acdfp"},
		},
		{
			// The shape the hook classification builds: an idle-looking seat
			// whose hook bead is submitted for landing. The verdict must rest
			// on the bead's live state, not on mgr.Get's inference.
			name: "hook bead says submitted",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupUnknown, HookBeadSubmitted: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := DecideWorkstate(tt.in)
			if d.Verdict != WorkstateVerdictSubmitted {
				t.Fatalf("DecideWorkstate(%+v) = %+v, want %s", tt.in, d, WorkstateVerdictSubmitted)
			}
			if d.NeedsRecovery || d.NeedsMQSubmit {
				t.Fatalf("DecideWorkstate(%+v) = %+v, want no recovery and no MQ submit for submitted work", tt.in, d)
			}
			if d.SafeToNuke || d.Reusable {
				t.Fatalf("DecideWorkstate(%+v) = %+v, want the landing worker's seat left in place", tt.in, d)
			}
			if !d.CountsTowardCapacity {
				t.Fatalf("DecideWorkstate(%+v) = %+v, want the seat counted — the landing has not released it", tt.in, d)
			}
			if d.Reason != "submitted-for-landing" {
				t.Fatalf("Reason = %q, want submitted-for-landing", d.Reason)
			}
		})
	}
}

// TestDecideWorkstateSubmittedDoesNotOutrankAWorkingSession: a polecat still
// working on a bead that already carries gt:ready-to-land (a conflict-resolution
// pass, say) must keep reporting WORKING. Calling it SUBMITTED would advertise a
// live session as hands-off.
func TestDecideWorkstateSubmittedDoesNotOutrankAWorkingSession(t *testing.T) {
	t.Parallel()
	in := WorkstateInput{
		State:             StateWorking,
		CleanupStatus:     CleanupClean,
		HookBeadSubmitted: true,
		Branch:            "polecat/amber/gt-acdfp",
	}
	d := DecideWorkstate(in)
	if d.Verdict != WorkstateVerdictWorking || d.NeedsRecovery {
		t.Fatalf("DecideWorkstate(%+v) = %+v, want WORKING", in, d)
	}
}

// TestDecideWorkstateSubmittedReportsMeasuredGitRisk pins the report-only half,
// the same shape the not-idle branch uses (gt-d9z9z): landing reads the pushed
// branch, so a worktree a live probe measured dirty does not change the verdict
// — but the reader should see it without re-probing, and an unmeasured probe's
// zero values are not facts.
func TestDecideWorkstateSubmittedReportsMeasuredGitRisk(t *testing.T) {
	t.Parallel()
	base := WorkstateInput{
		State:           StateSubmitted,
		CleanupStatus:   CleanupClean,
		GitDirty:        true,
		GitDirtyReason:  "git_state=has_uncommitted uncommitted_files=2",
		StashCount:      1,
		UnpushedCommits: 3,
	}

	t.Run("live probe facts are reported", func(t *testing.T) {
		t.Parallel()
		in := base
		in.GitStateSource = GitStateSourceLive
		d := DecideWorkstate(in)
		want := []string{
			"git_state=has_uncommitted uncommitted_files=2",
			"git_state=has_stash stash_count=1",
			"git_state=has_unpushed unpushed_commits=3",
		}
		if !slices.Equal(d.Blockers, want) {
			t.Fatalf("Blockers = %v, want %v", d.Blockers, want)
		}
		if d.Verdict != WorkstateVerdictSubmitted || d.NeedsRecovery || d.SafeToNuke || !d.CountsTowardCapacity {
			t.Fatalf("disposition = %+v, want the SUBMITTED verdict unchanged by a report-only blocker", d)
		}
	})

	for _, source := range []string{GitStateSourceRecorded, GitStateSourceUnknown, ""} {
		t.Run("unmeasured source "+source+" reports nothing", func(t *testing.T) {
			t.Parallel()
			in := base
			in.GitStateSource = source
			if d := DecideWorkstate(in); len(d.Blockers) != 0 {
				t.Fatalf("Blockers = %v, want none: zero values from an unmeasured probe are not facts", d.Blockers)
			}
		})
	}
}

// TestDecideWorkstateSubmittedDoesNotHideAFailedSubmission: push_failed and
// mr_failed mean gt done never got the branch to the landing worker, so the
// seat is waiting on nothing. Reporting it hands-off would bury the one signal
// that says the submission did not happen — and because gt done sets those
// flags on the very seats this verdict was introduced for, "hands-off" is
// exactly the wrong answer there (gt-eqiid).
//
// The PENDING_MR arm above tests the same two facts for the same reason;
// these cases pin the SUBMITTED arm and its interaction with the not-idle
// branch, which is where a StateSubmitted seat carrying a failed push lands.
func TestDecideWorkstateSubmittedDoesNotHideAFailedSubmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		in         WorkstateInput
		wantNamed  string
		wantReason string
	}{
		{
			// The not-idle branch refuses on the state, so the failure has to
			// be named beside it or the refusal says nothing about the push.
			name: "a failed push on a submitted seat is named",
			in: WorkstateInput{
				State: StateSubmitted, CleanupStatus: CleanupClean,
				PushFailed: true, Branch: "polecat/amber/gt-acdfp",
			},
			wantNamed:  "push_failed=true",
			wantReason: "not-idle",
		},
		{
			name: "a failed MR on a submitted seat is named",
			in: WorkstateInput{
				State: StateSubmitted, CleanupStatus: CleanupClean,
				MRFailed: true, Branch: "polecat/amber/gt-acdfp",
			},
			wantNamed:  "mr_failed=true",
			wantReason: "not-idle",
		},
		{
			// The hook-classified shape: an idle, reuse-eligible seat whose
			// hook bead is ready-to-land — submission evidence — but whose push
			// failed. It falls through to the predicate checks, where the
			// submitted hook must NOT raise hook-still-set.
			name: "a failed push on a submitted hook is named, and the hook is not",
			in: WorkstateInput{
				State: StateIdle, CleanupStatus: CleanupClean,
				HookBead: "gt-acdfp", HookBeadSubmitted: true,
				PushFailed: true, Branch: "polecat/amber/gt-acdfp",
			},
			wantNamed:  "push_failed=true",
			wantReason: "push-failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := DecideWorkstate(tt.in)
			if d.Verdict != WorkstateVerdictSubmitted && d.Verdict != WorkstateVerdictNeedsRecovery {
				t.Fatalf("DecideWorkstate(%+v) = %+v, want a verdict that names the failure", tt.in, d)
			}
			if d.Verdict == WorkstateVerdictSubmitted || !d.NeedsRecovery || d.SafeToNuke {
				t.Fatalf("DecideWorkstate(%+v) = %+v, want NEEDS_RECOVERY: a seat whose submission failed is not hands-off", tt.in, d)
			}
			if !slices.Contains(d.Blockers, tt.wantNamed) {
				t.Fatalf("Blockers = %v, want %q among them", d.Blockers, tt.wantNamed)
			}
			for _, blocker := range d.Blockers {
				if strings.Contains(blocker, "hook-still-set") || strings.Contains(blocker, "has work on hook") {
					t.Fatalf("Blockers = %v: a submitted hook is set by design and is never hook-still-set", d.Blockers)
				}
			}
			if d.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", d.Reason, tt.wantReason)
			}
		})
	}
}

// TestDecideSlotReuseRefusesSubmittedSlots keeps the reuse gate on the same
// answer: a submitted seat is not a slot the allocator may hand to new work
// until the landing releases the assignment (manager.releaseAssignedWork keeps
// the bead assigned for exactly this reason).
func TestDecideSlotReuseRefusesSubmittedSlots(t *testing.T) {
	t.Parallel()
	d := DecideSlotReuse(SlotReuseInput{
		State:             StateIdle,
		HookBeadSubmitted: true,
		CleanupStatus:     CleanupClean,
	})
	if d.Reusable {
		t.Fatalf("DecideSlotReuse(submitted) = %+v, want not reusable", d)
	}
}
