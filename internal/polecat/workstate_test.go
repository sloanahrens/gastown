package polecat

import "testing"

func TestDecideWorkstateCanonicalFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   WorkstateInput
		want WorkstateDisposition
	}{
		{
			name: "clean idle is reusable and safe",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "main"},
			want: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke, Reason: "reusable", Reusable: true, SafeToNuke: true, ReuseStatus: "idle-clean"},
		},
		{
			name: "dirty idle needs recovery and capacity",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupUnpushed},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "cleanup-has_unpushed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed"},
		},
		{
			name: "protected active work fails closed without capacity",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, ActiveWorkBlocker: "assigned_work=gt-blocked status=blocked"},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "active-work", NeedsRecovery: true, CountsTowardCapacity: false, ReuseStatus: "idle-recovery-needed"},
		},
		{
			name: "active work blocker consumes capacity when requested",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, ActiveWorkBlocker: "assigned_work=gt-open status=open", ActiveWorkCountsTowardCapacity: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "active-work", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed"},
		},
		{
			name: "unsubmitted branch needs mq submit",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", MQCheckRequired: true, HasSubmittableWork: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsMQSubmit, Reason: "mq-not-submitted", NeedsRecovery: true, NeedsMQSubmit: true, MQStatus: "not_submitted", CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed"},
		},
		{
			name: "mq lookup uncertainty blocks cleanup",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", MQCheckRequired: true, MQLookupFailed: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "mq-lookup-failed", NeedsRecovery: true, MQStatus: "unknown", CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"mq_status=unknown"}},
		},
		{
			name: "open work with unpushed commits needs recovery",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", UnpushedCommits: 1},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-unpushed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_unpushed unpushed_commits=1"}},
		},
		{
			name: "mr submission makes mq submitted",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", MQCheckRequired: true, HasSubmittableWork: true, MRSubmitted: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke, Reason: "reusable", Reusable: true, SafeToNuke: true, MQStatus: "submitted", ReuseStatus: "idle-preserved"},
		},
		{
			// gt-nkyy: a CLOSED source issue with superseded commits ahead of
			// main used to classify NEEDS_MQ_SUBMIT/not_submitted here,
			// holding done polecats out of reuse and counting them toward
			// capacity. Source-issue terminality IS the submission signal
			// (same semantics as the aa-xtee/aa-55d8 applyMQCheck case):
			// the work landed or was intentionally superseded, so there is
			// nothing left to submit.
			name: "terminal source with submittable work resolves submitted",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke, Reason: "reusable", Reusable: true, SafeToNuke: true, MQStatus: "submitted", ReuseStatus: "idle-preserved"},
		},
		{
			name: "dirty worktree blocks terminal source",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", GitDirty: true, GitDirtyReason: "git_state=has_uncommitted uncommitted_files=1", MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-dirty", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_uncommitted uncommitted_files=1"}},
		},
		{
			name: "stash blocks terminal source",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", StashCount: 1, MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-stash", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_stash stash_count=1"}},
		},
		{
			// gt-b9ud0: the unpushed-commits refusal is kept, so the work is
			// still reported and the seat still needs recovery — but a
			// terminal assigned bead means nothing is in flight, and nuke
			// preserves the branch on origin, so the seat stops holding a
			// dispatch seat. Only the capacity accounting and the extra
			// preservations line change.
			name: "terminal source keeps the unpushed blocker without its capacity",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", UnpushedCommits: 1, MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-unpushed", NeedsRecovery: true, CountsTowardCapacity: false, CommitsPreservedOnBranch: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_unpushed unpushed_commits=1", "unpushed commits preserved on branch polecat/test only"}},
		},
		{
			// The exemption is for the measured refusal. A recorded
			// cleanup_status is a hint about a moment in the past, not a
			// measurement, and nothing here proves the commits are on the
			// branch: the seat keeps counting until a probe says otherwise.
			name: "terminal source with only a recorded unpushed cleanup still counts",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupUnpushed, Branch: "polecat/test", AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "cleanup-has_unpushed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"cleanup_status=has_unpushed"}},
		},
		{
			// The exemption is for commits alone. A dirty tree is work no nuke
			// preserves, so a terminal bead does not excuse it.
			name: "terminal source with a dirty tree still counts",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", GitDirty: true, GitDirtyReason: "git_state=has_uncommitted uncommitted_files=1", UnpushedCommits: 1, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-dirty", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_uncommitted uncommitted_files=1", "git_state=has_unpushed unpushed_commits=1"}},
		},
		{
			// The realistic sibling of the case above: a push that never reached
			// the remote is not something nuke preserves — the branch is only
			// preserved because the push failed, and nothing proves the commits
			// are on origin. A terminal bead does not excuse it.
			name: "terminal source with a failed push still counts",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", PushFailed: true, UnpushedCommits: 1, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "push-failed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"push_failed=true", "git_state=has_unpushed unpushed_commits=1"}},
		},
		{
			name: "terminal source with a stash still counts",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", StashCount: 1, UnpushedCommits: 1, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-stash", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_stash stash_count=1", "git_state=has_unpushed unpushed_commits=1"}},
		},
		{
			// Liveness, not terminality, decides a hook: a seat whose bead is
			// terminal but whose hook is still set is holding work.
			name: "terminal source with a hook still set still counts",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", UnpushedCommits: 1, HookBead: "gt-hooked", AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "hook-still-set", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"has work on hook (gt-hooked)", "git_state=has_unpushed unpushed_commits=1"}},
		},
		{
			// A live bead keeps the refusal and the seat (gt-nkyy's case was
			// the closed-source half; this is the half it did not move).
			name: "nonterminal source with unpushed commits still counts",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", UnpushedCommits: 1, AssignedBeadTerminal: false},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "git-unpushed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"git_state=has_unpushed unpushed_commits=1"}},
		},
		{
			name: "terminal source with no commits and no other blocker is reusable",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke, Reason: "reusable", Reusable: true, SafeToNuke: true, ReuseStatus: "idle-preserved"},
		},
		{
			name: "push failure blocks terminal source",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", PushFailed: true, MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "push-failed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"push_failed=true"}},
		},
		{
			name: "mr failure blocks terminal source",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", MRFailed: true, MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "mr-failed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"mr_failed=true"}},
		},
		{
			name: "open active mr blocks terminal source",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, Branch: "polecat/test", ActiveMR: "gt-mr-open", ActiveMRBlocker: "active_mr=gt-mr-open status=open", MQCheckRequired: true, HasSubmittableWork: true, AssignedBeadTerminal: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictPendingMR, Reason: "active-mr-open", ReuseStatus: "idle-pr-open", Blockers: []string{"active_mr=gt-mr-open status=open"}},
		},
		{
			name: "terminal active mr does not block when gatherer omits blocker",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, ActiveMR: "gt-mr-closed"},
			want: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke, Reason: "reusable", Reusable: true, SafeToNuke: true, ReuseStatus: "idle-clean"},
		},
		{
			name: "open active mr is preserved pending mr",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupClean, ActiveMR: "gt-mr-open", ActiveMRBlocker: "active_mr=gt-mr-open status=open"},
			want: WorkstateDisposition{Verdict: WorkstateVerdictPendingMR, Reason: "active-mr-open", ReuseStatus: "idle-pr-open"},
		},
		{
			name: "open active mr does not hide cleanup blocker",
			in:   WorkstateInput{State: StateIdle, CleanupStatus: CleanupUnpushed, ActiveMR: "gt-mr-open", ActiveMRBlocker: "active_mr=gt-mr-open status=open"},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "cleanup-has_unpushed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"cleanup_status=has_unpushed", "active_mr=gt-mr-open status=open"}},
		},
		{
			name: "done active mr remains pending mr",
			in:   WorkstateInput{State: StateDone, CleanupStatus: CleanupClean, ActiveMR: "gt-mr-open", ActiveMRBlocker: "active_mr=gt-mr-open status=open"},
			want: WorkstateDisposition{Verdict: WorkstateVerdictPendingMR, Reason: "active-mr-open", ReuseStatus: "idle-pr-open", Blockers: []string{"active_mr=gt-mr-open status=open"}},
		},
		{
			name: "done without mr and clean cleanup is reusable and safe",
			in:   WorkstateInput{State: StateDone, CleanupStatus: CleanupClean},
			want: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke, Reason: "reusable", Reusable: true, SafeToNuke: true, ReuseStatus: "idle-clean"},
		},
		{
			name: "done without mr blocks reuse when cleanup is dirty",
			in:   WorkstateInput{State: StateDone, CleanupStatus: CleanupUnpushed},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "cleanup-has_unpushed", NeedsRecovery: true, CountsTowardCapacity: true, ReuseStatus: "idle-recovery-needed", Blockers: []string{"cleanup_status=has_unpushed"}},
		},
		{
			name: "working counts as working capacity",
			in:   WorkstateInput{State: StateWorking, CleanupStatus: CleanupClean},
			want: WorkstateDisposition{Verdict: WorkstateVerdictWorking, Reason: "not-idle", NeedsRecovery: false, CountsTowardCapacity: true},
		},
		{
			// The state blocker comes first: the lifecycle state is this
			// branch's own predicate, and the active-work blocker is the
			// caller's supplementary one (gt-3r1h).
			name: "stalled active work preserves both blockers",
			in:   WorkstateInput{State: StateStalled, CleanupStatus: CleanupClean, ActiveWorkBlocker: "assigned_work=gt-open status=open", ActiveWorkCountsTowardCapacity: true},
			want: WorkstateDisposition{Verdict: WorkstateVerdictNeedsRecovery, Reason: "not-idle", NeedsRecovery: true, CountsTowardCapacity: true, Blockers: []string{"lifecycle_state=stalled", "assigned_work=gt-open status=open"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideWorkstate(tt.in)
			if got.Verdict != tt.want.Verdict || got.Reason != tt.want.Reason || got.Reusable != tt.want.Reusable || got.SafeToNuke != tt.want.SafeToNuke || got.NeedsRecovery != tt.want.NeedsRecovery || got.NeedsMQSubmit != tt.want.NeedsMQSubmit || got.MQStatus != tt.want.MQStatus || got.CountsTowardCapacity != tt.want.CountsTowardCapacity || got.ReuseStatus != tt.want.ReuseStatus {
				t.Fatalf("DecideWorkstate() = %+v, want fields %+v", got, tt.want)
			}
			if tt.want.Blockers != nil {
				if len(got.Blockers) != len(tt.want.Blockers) {
					t.Fatalf("DecideWorkstate() blockers = %v, want %v", got.Blockers, tt.want.Blockers)
				}
				for i := range tt.want.Blockers {
					if got.Blockers[i] != tt.want.Blockers[i] {
						t.Fatalf("DecideWorkstate() blockers = %v, want %v", got.Blockers, tt.want.Blockers)
					}
				}
			}
		})
	}
}

// TestResolveIgnoreCleanupStatusPartialSpawnStillGated is the gt-hsg
// regression test. A prior version of the check-recovery CLI's own
// input-building code set IgnoreCleanupStatus=true for a "partial spawn
// without a durable hook" polecat with a missing/unknown CleanupStatus
// WITHOUT checking hookSafe/activeMRSafe/gitSafe at all — an ungated
// fail-open promotion of exactly the shape gt-7kr removed from
// workstateInputForPolecat, reintroduced via a different precondition.
// allowMissingForPartialSpawn must only waive a missing/unknown status
// alongside the same live safety facts every other case requires.
func TestResolveIgnoreCleanupStatusPartialSpawnStillGated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		status       CleanupStatus
		allowPartial bool
		workTerminal bool
		hookSafe     bool
		activeMRSafe bool
		gitSafe      bool
		want         bool
	}{
		{
			name:         "partial spawn with all facts safe may ignore missing status",
			status:       "",
			allowPartial: true,
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      true,
			want:         true,
		},
		{
			name:         "partial spawn does NOT waive a still-open hook bead",
			status:       "",
			allowPartial: true,
			hookSafe:     false,
			activeMRSafe: true,
			gitSafe:      true,
			want:         false,
		},
		{
			name:         "partial spawn does NOT waive a pending active MR",
			status:       "",
			allowPartial: true,
			hookSafe:     true,
			activeMRSafe: false,
			gitSafe:      true,
			want:         false,
		},
		{
			name:         "partial spawn does NOT waive dirty/unpushed git state",
			status:       CleanupUnknown,
			allowPartial: true,
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      false,
			want:         false,
		},
		{
			name:   "non-partial-spawn missing status still fails closed unconditionally",
			status: "",
			// allowPartial false, and workTerminal/hookSafe/activeMRSafe/gitSafe
			// all true: CanIgnoreStaleCleanupStatus never waives ""/Unknown.
			workTerminal: true,
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      true,
			want:         false,
		},
		{
			name:         "non-partial-spawn stale unpushed status still uses the general gate",
			status:       CleanupUnpushed,
			workTerminal: true,
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      true,
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveIgnoreCleanupStatus(tt.status, tt.allowPartial, false, false, false, tt.workTerminal, tt.hookSafe, tt.activeMRSafe, tt.gitSafe)
			if got != tt.want {
				t.Fatalf("ResolveIgnoreCleanupStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResolveIgnoreCleanupStatusGoneWorktreeStillGated is the gt-2h6
// regression test. A polecat whose worktree directory has been structurally
// verified gone can never self-report a fresh CleanupStatus again — there is
// nothing left to check — so a missing/unknown status there must not
// permanently veto reclaiming the slot. allowMissingForGoneWorktree grants
// that waiver WITHOUT requiring gitSafe (a live git check is impossible
// against a nonexistent directory, so requiring it would make the waiver
// unreachable), but still requires the same hook/active-MR safety facts as
// every other exception — it must never bypass those.
func TestResolveIgnoreCleanupStatusGoneWorktreeStillGated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		status       CleanupStatus
		allowGone    bool
		hookSafe     bool
		activeMRSafe bool
		gitSafe      bool
		want         bool
	}{
		{
			name:         "gone worktree with missing status and safe hook/MR may ignore, gitSafe false",
			status:       "",
			allowGone:    true,
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      false,
			want:         true,
		},
		{
			name:         "gone worktree with unknown status and safe hook/MR may ignore",
			status:       CleanupUnknown,
			allowGone:    true,
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      false,
			want:         true,
		},
		{
			name:         "gone worktree does NOT waive a still-open hook bead",
			status:       "",
			allowGone:    true,
			hookSafe:     false,
			activeMRSafe: true,
			want:         false,
		},
		{
			name:         "gone worktree does NOT waive a pending active MR",
			status:       "",
			allowGone:    true,
			hookSafe:     true,
			activeMRSafe: false,
			want:         false,
		},
		{
			name:         "gone worktree does NOT waive a recorded dirty status",
			status:       CleanupUncommitted,
			allowGone:    true,
			hookSafe:     true,
			activeMRSafe: true,
			want:         false,
		},
		{
			name:         "missing status without allowGone still fails closed",
			status:       "",
			hookSafe:     true,
			activeMRSafe: true,
			gitSafe:      true,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveIgnoreCleanupStatus(tt.status, false, tt.allowGone, false, false, false, tt.hookSafe, tt.activeMRSafe, tt.gitSafe)
			if got != tt.want {
				t.Fatalf("ResolveIgnoreCleanupStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNewWorkstateInputGoneWorktreeReclaimableOnlyWhenNoOtherRisk exercises
// the full NewWorkstateInput/DecideWorkstate path for the gt-2h6 scenario: a
// structurally gone worktree (GitCheckFailed=true, CleanupStatus missing)
// must resolve to disposition Reason="git-check-failed" with exactly the
// blocker "git_state=unknown" — the shape brokenIdleReclaimDispositionBlocker
// treats as reclaimable — as long as nothing else is at risk. A hooked bead
// or a pending active MR must still block, proving the new exception does
// not blanket-waive real risk signals.
func TestNewWorkstateInputGoneWorktreeReclaimableOnlyWhenNoOtherRisk(t *testing.T) {
	t.Parallel()
	base := WorkstateFacts{
		State:                       StateIdle,
		CleanupStatus:               "",
		HookBeadSafe:                true,
		WorktreeStructurallyMissing: true,
		GitCheckFailed:              true,
	}

	t.Run("no other risk resolves to git-check-failed only", func(t *testing.T) {
		d := DecideWorkstate(NewWorkstateInput(base))
		if d.Reason != "git-check-failed" {
			t.Fatalf("Reason = %q, want git-check-failed (got disposition %+v)", d.Reason, d)
		}
		if len(d.Blockers) != 1 || d.Blockers[0] != "git_state=unknown" {
			t.Fatalf("Blockers = %v, want exactly [git_state=unknown]", d.Blockers)
		}
		if brokenIdleReclaimDispositionBlocker(d) != "" {
			t.Fatalf("brokenIdleReclaimDispositionBlocker() = %q, want empty (reclaimable)", brokenIdleReclaimDispositionBlocker(d))
		}
	})

	t.Run("hooked bead still blocks despite gone worktree", func(t *testing.T) {
		f := base
		f.HookBead = "gt-x8y"
		f.HookBeadSafe = false
		d := DecideWorkstate(NewWorkstateInput(f))
		if brokenIdleReclaimDispositionBlocker(d) == "" {
			t.Fatalf("expected hooked bead to block reclaim, got disposition %+v", d)
		}
	})

	t.Run("pending active MR still blocks despite gone worktree", func(t *testing.T) {
		f := base
		f.ActiveMRBlocker = "active_mr=gt-wisp-rxnx status=open"
		d := DecideWorkstate(NewWorkstateInput(f))
		if brokenIdleReclaimDispositionBlocker(d) == "" {
			t.Fatalf("expected pending active MR to block reclaim, got disposition %+v", d)
		}
	})
}

// TestNewWorkstateInputWorkAtRiskSignalsBlockIndependentlyWithEmptyCleanupStatus
// exercises NewWorkstateInput (the single production constructor) against
// the gt-swq/gt-hsg fixture: cleanup_status empty (the state that produces
// the bug — a populated status would pass for the wrong reason), and each
// work-at-risk signal (hooked bead, active MR, unpushed/dirty git) present
// on its own. Each must block SAFE_TO_NUKE independently.
func TestNewWorkstateInputWorkAtRiskSignalsBlockIndependentlyWithEmptyCleanupStatus(t *testing.T) {
	t.Parallel()
	base := WorkstateFacts{State: StateIdle, CleanupStatus: "", HookBeadSafe: true}

	tests := []struct {
		name  string
		facts WorkstateFacts
	}{
		{
			name: "hooked bead blocks alone",
			facts: func() WorkstateFacts {
				f := base
				f.HookBead = "gt-x8y"
				f.HookBeadSafe = false
				return f
			}(),
		},
		{
			name: "pending active MR blocks alone",
			facts: func() WorkstateFacts {
				f := base
				f.ActiveMRBlocker = "active_mr=gt-wisp-rxnx status=open"
				return f
			}(),
		},
		{
			name: "unpushed commit blocks alone",
			facts: func() WorkstateFacts {
				f := base
				f.UnpushedCommits = 1
				return f
			}(),
		},
		{
			name: "dirty worktree blocks alone",
			facts: func() WorkstateFacts {
				f := base
				f.GitDirty = true
				f.GitDirtyReason = "git_state=has_uncommitted uncommitted_files=1"
				return f
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DecideWorkstate(NewWorkstateInput(tt.facts))
			if d.SafeToNuke || d.Verdict == WorkstateVerdictSafeToNuke {
				t.Fatalf("DecideWorkstate(NewWorkstateInput(%+v)) = %+v, want not SAFE_TO_NUKE", tt.facts, d)
			}
		})
	}
}

// TestNewWorkstateInputRealisticCleanPolecatStillClears is the adversarial
// control required alongside the above: a genuinely clean, done polecat
// (real work, merged, closed out, cleanup_status=clean, no live signals at
// risk) must still report SAFE_TO_NUKE through the shared constructor.
// Without this, a blanket fail-closed change would pass every regression
// test above while stranding the rig at its polecat cap.
func TestNewWorkstateInputRealisticCleanPolecatStillClears(t *testing.T) {
	t.Parallel()
	facts := WorkstateFacts{
		State:           StateDone,
		CleanupStatus:   CleanupClean,
		HookBeadSafe:    true,
		Branch:          "main",
		MQCheckRequired: false,
	}
	d := DecideWorkstate(NewWorkstateInput(facts))
	if !d.SafeToNuke || d.Verdict != WorkstateVerdictSafeToNuke {
		t.Fatalf("DecideWorkstate(NewWorkstateInput(%+v)) = %+v, want SAFE_TO_NUKE", facts, d)
	}
}

// TestNewWorkstateInputMissingCleanupStatusClearsOnLiveCleanProbe is the
// gt-ui2x acceptance case: a seat whose cleanup_status was never recorded
// (gt done crashed before self-reporting, or predates the field) must not be
// stuck in NEEDS_RECOVERY forever just because nothing ever answers for it.
// Once the agent bead has actually been read (AgentBeadRead — so hook_bead,
// push_failed, mr_failed and active_mr are verified facts, not unread
// defaults) and a live git probe measures the worktree clean, that
// combination is the verified path CanIgnoreStaleCleanupStatus's narrow
// hatches could not reach — see ResolveIgnoreCleanupStatus.
func TestNewWorkstateInputMissingCleanupStatusClearsOnLiveCleanProbe(t *testing.T) {
	t.Parallel()
	facts := WorkstateFacts{
		State:          StateIdle,
		CleanupStatus:  "",
		HookBeadSafe:   true,
		AgentBeadRead:  true,
		Branch:         "polecat/opal",
		GitStateSource: GitStateSourceLive,
		// GitDirty/StashCount/UnpushedCommits all zero: the live probe found
		// nothing at risk.
	}
	d := DecideWorkstate(NewWorkstateInput(facts))
	if !d.SafeToNuke || d.Verdict != WorkstateVerdictSafeToNuke {
		t.Fatalf("DecideWorkstate(NewWorkstateInput(%+v)) = %+v, want SAFE_TO_NUKE", facts, d)
	}
	if len(d.Blockers) != 0 {
		t.Fatalf("Blockers = %v, want none", d.Blockers)
	}
}

// TestNewWorkstateInputMissingCleanupStatusStillBlocksWithoutAgentBeadRead is
// the gt-14a regression case at the NewWorkstateInput level: the same
// clean-live-probe facts as the acceptance case above, but WITHOUT
// AgentBeadRead, must still fail closed. This is what an unreadable or
// not-found agent bead looks like (workstateInputForPolecat and
// checkRecoveryForPolecat both leave AgentBeadRead at its zero value, false,
// in that case) — hook_bead/push_failed/mr_failed/active_mr are unverified,
// so a clean local git check alone must not promote the missing status.
func TestNewWorkstateInputMissingCleanupStatusStillBlocksWithoutAgentBeadRead(t *testing.T) {
	t.Parallel()
	facts := WorkstateFacts{
		State:          StateIdle,
		CleanupStatus:  "",
		HookBeadSafe:   true,
		Branch:         "polecat/opal",
		GitStateSource: GitStateSourceLive,
		// AgentBeadRead left false: the agent bead was never actually read.
	}
	d := DecideWorkstate(NewWorkstateInput(facts))
	if d.SafeToNuke || d.Verdict != WorkstateVerdictNeedsRecovery {
		t.Fatalf("DecideWorkstate(NewWorkstateInput(%+v)) = %+v, want NEEDS_RECOVERY — an unread agent bead must not clear a missing cleanup_status", facts, d)
	}
	if len(d.Blockers) != 1 || d.Blockers[0] != "cleanup_status=<missing>" {
		t.Fatalf("Blockers = %v, want [cleanup_status=<missing>]", d.Blockers)
	}
}

// TestNewWorkstateInputMissingCleanupStatusStillBlocksOnLiveDirtyProbe proves
// the gt-ui2x fix does not weaken protection: a missing cleanup_status paired
// with a live probe that actually finds risk (real uncommitted work) must
// still block, exactly like every other status — even with AgentBeadRead
// true, which would otherwise satisfy every other precondition for clearing.
// A genuinely dirty seat must always block.
func TestNewWorkstateInputMissingCleanupStatusStillBlocksOnLiveDirtyProbe(t *testing.T) {
	t.Parallel()
	facts := WorkstateFacts{
		State:          StateIdle,
		CleanupStatus:  "",
		HookBeadSafe:   true,
		AgentBeadRead:  true,
		Branch:         "polecat/jade",
		GitStateSource: GitStateSourceLive,
		GitDirty:       true,
		GitDirtyReason: "git_state=has_uncommitted uncommitted_files=1",
	}
	d := DecideWorkstate(NewWorkstateInput(facts))
	if d.SafeToNuke || d.Verdict != WorkstateVerdictNeedsRecovery {
		t.Fatalf("DecideWorkstate(NewWorkstateInput(%+v)) = %+v, want NEEDS_RECOVERY", facts, d)
	}
	found := false
	for _, b := range d.Blockers {
		if b == facts.GitDirtyReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("Blockers = %v, want it to include %q", d.Blockers, facts.GitDirtyReason)
	}
}

// TestNewWorkstateInputMissingCleanupStatusStillBlocksWithoutLiveProbe proves
// the demotion requires an actual live measurement: a missing cleanup_status
// with no probe attempted (the caller never measured git at all) must still
// fail closed, exactly as before gt-ui2x.
func TestNewWorkstateInputMissingCleanupStatusStillBlocksWithoutLiveProbe(t *testing.T) {
	t.Parallel()
	facts := WorkstateFacts{
		State:         StateIdle,
		CleanupStatus: "",
		HookBeadSafe:  true,
		Branch:        "polecat/pyrite",
		// GitStateSource left unset: GitStateSourceRecorded, no probe ran.
	}
	d := DecideWorkstate(NewWorkstateInput(facts))
	if d.SafeToNuke || d.Verdict != WorkstateVerdictNeedsRecovery {
		t.Fatalf("DecideWorkstate(NewWorkstateInput(%+v)) = %+v, want NEEDS_RECOVERY", facts, d)
	}
	if len(d.Blockers) != 1 || d.Blockers[0] != "cleanup_status=<missing>" {
		t.Fatalf("Blockers = %v, want [cleanup_status=<missing>]", d.Blockers)
	}
}

func TestWorkstateFactsCanIgnoreStaleCleanupStatusForNuke(t *testing.T) {
	t.Parallel()
	safe := WorkstateFacts{CleanupStatus: CleanupUnpushed, HookBeadSafe: true, AssignedBeadTerminal: true}
	tests := []struct {
		name    string
		mutate  func(f *WorkstateFacts)
		gitSafe bool
		want    bool
	}{
		{"terminal work, safe hook, no MR, safe git", func(*WorkstateFacts) {}, true, true},
		{"git not safe", func(*WorkstateFacts) {}, false, false},
		{"hook unsafe", func(f *WorkstateFacts) { f.HookBeadSafe = false }, true, false},
		{"active MR pending", func(f *WorkstateFacts) { f.ActiveMRBlocker = "active_mr=x status=open" }, true, false},
		{"no terminal work ref", func(f *WorkstateFacts) { f.AssignedBeadTerminal = false }, true, false},
		{"hook bead terminal counts as terminal work", func(f *WorkstateFacts) { f.AssignedBeadTerminal, f.HookBeadTerminal = false, true }, true, true},
		{"active MR source terminal counts as terminal work", func(f *WorkstateFacts) { f.AssignedBeadTerminal, f.ActiveMRSourceTerminal = false, true }, true, true},
		{"unknown status is never waived", func(f *WorkstateFacts) { f.CleanupStatus = CleanupUnknown }, true, false},
		{"missing status is never waived", func(f *WorkstateFacts) { f.CleanupStatus = "" }, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := safe
			tt.mutate(&f)
			if got := f.CanIgnoreStaleCleanupStatusForNuke(tt.gitSafe); got != tt.want {
				t.Fatalf("CanIgnoreStaleCleanupStatusForNuke(%v) = %v, want %v", tt.gitSafe, got, tt.want)
			}
		})
	}
}
