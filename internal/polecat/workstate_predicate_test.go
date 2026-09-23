package polecat

import (
	"slices"
	"testing"
)

// TestDecideWorkstateNamesItsRefusalPredicate is the gt-3r1h regression: every
// refusing verdict must name the predicate behind it in Blockers, or the text
// built from that disposition can only admit that its guard refused for
// unknown reasons. Each mutation below is one refusal mode; the sweep runs
// them across every lifecycle state, because an empty Blockers list is a
// property of the refusal path, not of one caller.
func TestDecideWorkstateNamesItsRefusalPredicate(t *testing.T) {
	mutations := []struct {
		name        string
		apply       func(*WorkstateInput)
		wantBlocker string
	}{
		{"clean", func(*WorkstateInput) {}, ""},
		{"cleanup-uncommitted", func(in *WorkstateInput) { in.CleanupStatus = CleanupUncommitted }, "cleanup_status=has_uncommitted"},
		{"cleanup-stash", func(in *WorkstateInput) { in.CleanupStatus = CleanupStash }, "cleanup_status=has_stash"},
		{"cleanup-unpushed", func(in *WorkstateInput) { in.CleanupStatus = CleanupUnpushed }, "cleanup_status=has_unpushed"},
		{"cleanup-missing", func(in *WorkstateInput) { in.CleanupStatus = "" }, "cleanup_status=<missing>"},
		{"cleanup-unknown", func(in *WorkstateInput) { in.CleanupStatus = CleanupUnknown }, "cleanup_status=unknown"},
		{"hook-open", func(in *WorkstateInput) { in.HookBead = "gt-open" }, "has work on hook (gt-open)"},
		{"push-failed", func(in *WorkstateInput) { in.PushFailed = true }, "push_failed=true"},
		{"mr-failed", func(in *WorkstateInput) { in.MRFailed = true }, "mr_failed=true"},
		{"active-work", func(in *WorkstateInput) { in.ActiveWorkBlocker = "assigned_work=gt-open status=open" }, "assigned_work=gt-open status=open"},
		{"active-mr", func(in *WorkstateInput) {
			in.ActiveMR = "gt-mr"
			in.ActiveMRBlocker = "active_mr=gt-mr status=open"
		}, "active_mr=gt-mr status=open"},
		{"git-dirty", func(in *WorkstateInput) {
			in.GitDirty = true
			in.GitDirtyReason = "git_state=has_uncommitted uncommitted_files=1"
		}, "git_state=has_uncommitted uncommitted_files=1"},
		{"git-check-failed", func(in *WorkstateInput) {
			in.GitCheckFailed = true
			in.GitCheckFailedReason = "git_state=unknown preservation_check_failed"
		}, "git_state=unknown preservation_check_failed"},
		{"git-stash", func(in *WorkstateInput) { in.StashCount = 1 }, "git_state=has_stash stash_count=1"},
		{"git-unpushed", func(in *WorkstateInput) { in.UnpushedCommits = 2 }, "git_state=has_unpushed unpushed_commits=2"},
		{"mq-unsubmitted", func(in *WorkstateInput) {
			in.MQCheckRequired = true
			in.HasSubmittableWork = true
		}, "mq_status=not_submitted"},
		{"mq-lookup-failed", func(in *WorkstateInput) {
			in.MQCheckRequired = true
			in.MQLookupFailed = true
		}, "mq_status=unknown"},
	}

	states := []State{
		StateIdle, StateDone, StateWorking, StateReviewNeeded,
		StateStuck, StateStalled, StateSpawning, StateZombie, StateForeign,
	}

	for _, state := range states {
		for _, m := range mutations {
			in := WorkstateInput{
				State:         state,
				CleanupStatus: CleanupClean,
				Branch:        "polecat/flint",
				// Recorded, not live: a live probe supersedes the git-derived
				// cleanup statuses (RecordedCleanupBlocks), which would mask
				// the cleanup refusal modes this sweep needs to see.
				GitStateSource: GitStateSourceRecorded,
			}
			m.apply(&in)
			t.Run(string(state)+"/"+m.name, func(t *testing.T) {
				d := DecideWorkstate(in)
				if d.Verdict != WorkstateVerdictNeedsRecovery && d.Verdict != WorkstateVerdictNeedsMQSubmit && d.Verdict != WorkstateVerdictPendingMR {
					return
				}
				if len(d.Blockers) == 0 {
					t.Fatalf("DecideWorkstate(%+v) = %s with no Blockers — a refusal that names no predicate can only be escalated", in, d.Verdict)
				}
				if !state.IsReuseEligible() {
					// The lifecycle state is the only thing refusing this
					// seat; it must be the first predicate named.
					if d.Blockers[0] != "lifecycle_state="+string(state) {
						t.Fatalf("DecideWorkstate(%+v) = %s blockers %v, want it to lead with lifecycle_state=%s", in, d.Verdict, d.Blockers, state)
					}
					return
				}
				if m.wantBlocker != "" && !slices.Contains(d.Blockers, m.wantBlocker) {
					t.Fatalf("DecideWorkstate(%+v) = %s blockers %v, want %q named", in, d.Verdict, d.Blockers, m.wantBlocker)
				}
			})
		}
	}
}

// TestDecideWorkstateReviewNeededNamesLifecycleState pins the disposition
// shape behind the three reported instances (gt-3r1h): a finished polecat
// whose work landed and whose worktree a live probe measured clean, but whose
// session is still up with a recorded cleanup_status that is not safe, comes
// back from Get as StateReviewNeeded. The refusal must name that state — the
// classifier holds it in hand — and stay fail-closed.
func TestDecideWorkstateReviewNeededNamesLifecycleState(t *testing.T) {
	in := WorkstateInput{
		State:          StateReviewNeeded,
		CleanupStatus:  CleanupUnpushed,
		Branch:         "polecat/flint",
		GitStateSource: GitStateSourceLive,
	}
	d := DecideWorkstate(in)
	if d.Verdict != WorkstateVerdictNeedsRecovery || !d.NeedsRecovery {
		t.Fatalf("DecideWorkstate(%+v) = %+v, want NEEDS_RECOVERY", in, d)
	}
	if d.SafeToNuke || d.Reusable {
		t.Fatalf("DecideWorkstate(%+v) = %+v, want the verdict to stay fail-closed", in, d)
	}
	want := "lifecycle_state=review-needed"
	if len(d.Blockers) != 1 || d.Blockers[0] != want {
		t.Fatalf("Blockers = %v, want [%s]", d.Blockers, want)
	}
	if d.Reason != "not-idle" {
		t.Fatalf("Reason = %q, want not-idle", d.Reason)
	}
}
