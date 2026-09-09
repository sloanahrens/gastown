package polecat

import "strings"

const (
	WorkstateVerdictWorking       = "WORKING"
	WorkstateVerdictSafeToNuke    = "SAFE_TO_NUKE"
	WorkstateVerdictPendingMR     = "PENDING_MR"
	WorkstateVerdictNeedsRecovery = "NEEDS_RECOVERY"
	WorkstateVerdictNeedsMQSubmit = "NEEDS_MQ_SUBMIT"
)

// WorkstateInput contains the lifecycle, git, and merge-queue facts needed to
// classify a polecat consistently across list, recovery, witness, and capacity.
type WorkstateInput struct {
	State                          State
	HookBead                       string
	CleanupStatus                  CleanupStatus
	IgnoreCleanupStatus            bool
	PartialSpawnWithoutDurableHook bool
	PushFailed                     bool
	MRFailed                       bool
	Branch                         string
	GitDirty                       bool
	GitDirtyReason                 string
	StashCount                     int
	UnpushedCommits                int
	GitCheckFailed                 bool
	GitCheckFailedReason           string
	ActiveWorkBlocker              string
	ActiveWorkCountsTowardCapacity bool
	ActiveMR                       string
	ActiveMRBlocker                string
	MQCheckRequired                bool
	HasSubmittableWork             bool
	MQNotRequired                  bool
	AssignedBeadTerminal           bool
	MRSubmitted                    bool
	MQLookupFailed                 bool
}

// WorkstateDisposition is the canonical polecat lifecycle decision. It is pure
// policy: callers gather facts, this classifier decides how every subsystem
// should present and count the polecat.
type WorkstateDisposition struct {
	Verdict              string   `json:"verdict"`
	Reason               string   `json:"reason,omitempty"`
	Reusable             bool     `json:"reusable"`
	SafeToNuke           bool     `json:"safe_to_nuke"`
	NeedsRecovery        bool     `json:"needs_recovery"`
	NeedsMQSubmit        bool     `json:"needs_mq_submit"`
	MQStatus             string   `json:"mq_status,omitempty"`
	CountsTowardCapacity bool     `json:"counts_toward_capacity"`
	ReuseStatus          string   `json:"reuse_status,omitempty"`
	Blockers             []string `json:"blockers,omitempty"`
}

// DecideWorkstate returns the canonical disposition for a polecat.
func DecideWorkstate(in WorkstateInput) WorkstateDisposition {
	if in.ActiveMRBlocker != "" && !in.PushFailed && !in.MRFailed && in.State == StateDone {
		return WorkstateDisposition{
			Verdict:     WorkstateVerdictPendingMR,
			Reason:      "active-mr-open",
			ReuseStatus: "idle-pr-open",
			Blockers:    []string{in.ActiveMRBlocker},
		}
	}

	// StateDone (agent_state=done, seen before a polecat's own idle transition
	// lands) falls through to the real predicate checks below instead of
	// bailing out here — otherwise a merged/clean polecat gets NEEDS_RECOVERY
	// with no blockers, disagreeing with git-state for no reason (gt-check-recovery-bug).
	if in.State != StateIdle && in.State != StateDone {
		verdict := WorkstateVerdictNeedsRecovery
		needsRecovery := true
		if in.State == StateWorking {
			verdict = WorkstateVerdictWorking
			needsRecovery = false
		}
		d := WorkstateDisposition{
			Verdict:              verdict,
			Reason:               "not-idle",
			NeedsRecovery:        needsRecovery,
			CountsTowardCapacity: true,
		}
		if in.ActiveWorkBlocker != "" {
			d.Blockers = append(d.Blockers, in.ActiveWorkBlocker)
		}
		return d
	}

	d := WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke}
	capacityBlocked := false
	block := func(reason, blocker string, countsTowardCapacity bool) {
		if d.Reason == "" {
			d.Reason = reason
		}
		if blocker != "" {
			d.Blockers = append(d.Blockers, blocker)
		}
		capacityBlocked = capacityBlocked || countsTowardCapacity
	}

	if in.HookBead != "" && !in.PartialSpawnWithoutDurableHook {
		block("hook-still-set", "has work on hook ("+in.HookBead+")", true)
	}
	if in.PushFailed {
		block("push-failed", "push_failed=true", true)
	}
	if in.MRFailed {
		block("mr-failed", "mr_failed=true", true)
	}
	if in.ActiveWorkBlocker != "" {
		block("active-work", in.ActiveWorkBlocker, in.ActiveWorkCountsTowardCapacity)
	}
	if !in.IgnoreCleanupStatus && !in.CleanupStatus.IsSafe() {
		reason := "cleanup-" + string(in.CleanupStatus)
		blocker := "cleanup_status=" + string(in.CleanupStatus)
		if in.CleanupStatus == "" {
			reason = "cleanup-unknown"
			blocker = "cleanup_status=<missing>"
		} else if in.CleanupStatus == CleanupUnknown {
			reason = "cleanup-unknown"
		}
		block(reason, blocker, true)
	}
	if in.GitCheckFailed {
		blocker := in.GitCheckFailedReason
		if blocker == "" {
			blocker = "git_state=unknown"
		}
		block("git-check-failed", blocker, true)
	}
	if in.GitDirty {
		blocker := in.GitDirtyReason
		if blocker == "" {
			blocker = "git_state=has_uncommitted"
		}
		block("git-dirty", blocker, true)
	}
	if in.StashCount > 0 {
		block("git-stash", "git_state=has_stash stash_count="+itoa(in.StashCount), true)
	}
	if in.UnpushedCommits > 0 {
		block("git-unpushed", "git_state=has_unpushed unpushed_commits="+itoa(in.UnpushedCommits), true)
	}
	activeMRBlocks := in.ActiveMRBlocker != ""
	if activeMRBlocks {
		block("active-mr-open", in.ActiveMRBlocker, false)
	}

	if len(d.Blockers) > 0 {
		if activeMRBlocks && len(d.Blockers) == 1 {
			d.Verdict = WorkstateVerdictPendingMR
			d.ReuseStatus = "idle-pr-open"
			return d
		}
		d.Verdict = WorkstateVerdictNeedsRecovery
		d.NeedsRecovery = true
		d.CountsTowardCapacity = capacityBlocked
		d.ReuseStatus = "idle-recovery-needed"
		return d
	}

	if in.MQCheckRequired {
		if in.MQLookupFailed {
			d.Verdict = WorkstateVerdictNeedsRecovery
			d.Reason = "mq-lookup-failed"
			d.NeedsRecovery = true
			d.MQStatus = "unknown"
			d.CountsTowardCapacity = true
			d.ReuseStatus = "idle-recovery-needed"
			d.Blockers = append(d.Blockers, "mq_status=unknown")
			return d
		} else if !in.HasSubmittableWork || in.MQNotRequired {
			d.MQStatus = "not_required"
		} else if in.MRSubmitted {
			d.MQStatus = "submitted"
		} else {
			d.Verdict = WorkstateVerdictNeedsMQSubmit
			d.Reason = "mq-not-submitted"
			d.NeedsRecovery = true
			d.NeedsMQSubmit = true
			d.MQStatus = "not_submitted"
			d.CountsTowardCapacity = true
			d.ReuseStatus = "idle-recovery-needed"
			d.Blockers = append(d.Blockers, "mq_status=not_submitted")
			return d
		}
	}

	d.Reusable = true
	d.SafeToNuke = true
	d.Reason = "reusable"
	if strings.HasPrefix(in.Branch, "polecat/") {
		d.ReuseStatus = "idle-preserved"
	} else {
		d.ReuseStatus = "idle-clean"
	}
	return d
}

// CanIgnoreStaleCleanupStatus returns true when a dirty persisted
// cleanup_status is older than the direct predicates proving no work is at risk.
// The status remains unsafe globally; callers must opt into this reconciliation
// path only after gathering live git, hook, work, and active-MR facts.
func CanIgnoreStaleCleanupStatus(status CleanupStatus, workTerminal, hookSafe, activeMRSafe, gitSafe bool) bool {
	if !workTerminal || !hookSafe || !activeMRSafe || !gitSafe {
		return false
	}
	switch status {
	case CleanupUncommitted, CleanupStash, CleanupUnpushed:
		return true
	default:
		return false
	}
}

// ResolveIgnoreCleanupStatus is the single fail-closed gate for whether a
// non-safe CleanupStatus may be ignored when assembling a WorkstateInput.
//
// It wraps CanIgnoreStaleCleanupStatus with one narrow extension: a polecat
// that never durably picked up work (allowMissingForPartialSpawn — its
// hook_bead points at a bead it no longer owns, or never owned) may also
// ignore a missing/unknown CleanupStatus, which CanIgnoreStaleCleanupStatus
// always refuses. Even then this is gated by the SAME hook/active-MR/git
// safety facts required for every other case — never granted unconditionally.
//
// gt-hsg: a prior version of this check (in cmd/polecat.go's check-recovery
// handler) set IgnoreCleanupStatus=true for the partial-spawn case without
// checking hookSafe/activeMRSafe/gitSafe at all — an ungated promotion of
// exactly the shape gt-7kr removed from workstateInputForPolecat, just
// reintroduced via a different precondition in a second, undiscovered copy
// of this policy. Route every caller through this one function instead.
func ResolveIgnoreCleanupStatus(status CleanupStatus, allowMissingForPartialSpawn, workTerminal, hookSafe, activeMRSafe, gitSafe bool) bool {
	if allowMissingForPartialSpawn && (status == "" || status == CleanupUnknown) {
		return hookSafe && activeMRSafe && gitSafe
	}
	return CanIgnoreStaleCleanupStatus(status, workTerminal, hookSafe, activeMRSafe, gitSafe)
}

// WorkstateFacts carries the already-gathered lifecycle, git, and
// merge-queue signals a caller needs in order to build a WorkstateInput.
// Fact-gathering (beads reads, git checks) stays caller-specific — the
// Manager and the CLI check-recovery handler use different beads/git
// plumbing — but every production caller MUST assemble its final
// WorkstateInput through NewWorkstateInput. A WorkstateInput{} literal
// anywhere else in production code reintroduces the duplicated,
// independently-drifting input-building layer responsible for gt-7kr,
// gt-14a, and gt-hsg (see TestNoWorkstateInputLiteralsOutsideConstructor).
type WorkstateFacts struct {
	State                          State
	HookBead                       string
	HookBeadSafe                   bool
	HookBeadTerminal               bool
	PartialSpawnWithoutDurableHook bool
	CleanupStatus                  CleanupStatus
	PushFailed                     bool
	MRFailed                       bool
	Branch                         string
	GitDirty                       bool
	GitDirtyReason                 string
	StashCount                     int
	UnpushedCommits                int
	GitCheckFailed                 bool
	GitCheckFailedReason           string
	ActiveWorkBlocker              string
	ActiveWorkCountsTowardCapacity bool
	ActiveMR                       string
	ActiveMRBlocker                string
	ActiveMRSourceTerminal         bool
	AssignedBeadTerminal           bool
	MQCheckRequired                bool
	HasSubmittableWork             bool
	MQNotRequired                  bool
	MRSubmitted                    bool
	MQLookupFailed                 bool
}

// NewWorkstateInput is the single production constructor for WorkstateInput.
// It derives gitSafe/activeMRSafe/workTerminal from the supplied facts and
// resolves IgnoreCleanupStatus through ResolveIgnoreCleanupStatus, so the
// fail-closed policy lives in exactly one place regardless of which caller
// (Manager, CLI check-recovery, list/inventory) is building the input.
func NewWorkstateInput(f WorkstateFacts) WorkstateInput {
	gitSafe := !f.GitCheckFailed && !f.GitDirty && f.StashCount == 0 && f.UnpushedCommits == 0
	activeMRSafe := f.ActiveMRBlocker == ""
	workTerminal := f.AssignedBeadTerminal || f.ActiveMRSourceTerminal || f.HookBeadTerminal

	input := WorkstateInput{
		State:                          f.State,
		CleanupStatus:                  f.CleanupStatus,
		PartialSpawnWithoutDurableHook: f.PartialSpawnWithoutDurableHook,
		PushFailed:                     f.PushFailed,
		MRFailed:                       f.MRFailed,
		Branch:                         f.Branch,
		GitDirty:                       f.GitDirty,
		GitDirtyReason:                 f.GitDirtyReason,
		StashCount:                     f.StashCount,
		UnpushedCommits:                f.UnpushedCommits,
		GitCheckFailed:                 f.GitCheckFailed,
		GitCheckFailedReason:           f.GitCheckFailedReason,
		ActiveWorkBlocker:              f.ActiveWorkBlocker,
		ActiveWorkCountsTowardCapacity: f.ActiveWorkCountsTowardCapacity,
		ActiveMR:                       f.ActiveMR,
		ActiveMRBlocker:                f.ActiveMRBlocker,
		AssignedBeadTerminal:           f.AssignedBeadTerminal,
		MQCheckRequired:                f.MQCheckRequired,
		HasSubmittableWork:             f.HasSubmittableWork,
		MQNotRequired:                  f.MQNotRequired,
		MRSubmitted:                    f.MRSubmitted,
		MQLookupFailed:                 f.MQLookupFailed,
	}
	if !f.HookBeadSafe {
		input.HookBead = f.HookBead
	}
	if !input.CleanupStatus.IsSafe() {
		input.IgnoreCleanupStatus = ResolveIgnoreCleanupStatus(f.CleanupStatus, f.PartialSpawnWithoutDurableHook, workTerminal, f.HookBeadSafe, activeMRSafe, gitSafe)
	}
	return input
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
