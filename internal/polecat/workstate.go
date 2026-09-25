package polecat

import "strings"

const (
	WorkstateVerdictWorking       = "WORKING"
	WorkstateVerdictSafeToNuke    = "SAFE_TO_NUKE"
	WorkstateVerdictPendingMR     = "PENDING_MR"
	WorkstateVerdictNeedsRecovery = "NEEDS_RECOVERY"
	WorkstateVerdictNeedsMQSubmit = "NEEDS_MQ_SUBMIT"
)

// CleanupStatusSourceRecorded is the provenance tag for a cleanup_status
// value. Every cleanup_status in the system is a self-report written by
// `gt done` onto the polecat's agent bead (cmd/done.go selfReportCleanupStatus)
// — a recorded hint about a moment in the past, never a live measurement.
// claude-41j.1 D9 retires the "ZFC: trust polecat self-report" carve-out
// (docs/design/polecat-lifecycle-patrol.md) that treated it as authoritative;
// consumers must show which side of that line a field came from, and the tag
// exists so the demotion is visible at the API boundary instead of only in a
// doc comment.
const CleanupStatusSourceRecorded = "recorded"

// GitStateSource labels where a WorkstateInput's git facts came from. The
// reuse verdict re-derives from live git on every evaluation, so a consumer
// must be able to tell a measured answer from a recalled one.
const (
	// GitStateSourceLive means a live probe ran and answered: GitDirty,
	// StashCount and UnpushedCommits are that probe's measurement, and they
	// supersede the recorded cleanup_status for git-derived verdicts.
	GitStateSourceLive = "live"

	// GitStateSourceUnknown means a live probe was attempted and failed.
	// Git facts are unknown and the verdict fails closed.
	GitStateSourceUnknown = "unknown"

	// GitStateSourceRecorded means no live probe was attempted at all, so the
	// recorded cleanup_status stays authoritative for git-derived verdicts.
	// This is the zero value's meaning: a caller that has no worktree to
	// probe (a counts-only capacity projection, a bead-only pool check) keeps
	// failing closed rather than silently acquiring a cleaner verdict it
	// never measured.
	GitStateSourceRecorded = "recorded"
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
	GitStateSource                 string
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
// should present and count the polecat. Every refusing verdict names its
// predicate in Blockers, so no consumer has to report an unnamed guard
// (gt-3r1h).
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
	// CleanupStatusSource is the provenance of the recorded cleanup_status
	// hint this disposition was decided against — always "recorded", because
	// there is no live cleanup_status (see CleanupStatusSourceRecorded).
	CleanupStatusSource string `json:"cleanup_status_source,omitempty"`
	// GitStateSource says whether the verdict's git facts were measured
	// ("live"), failed to be measured ("unknown"), or never measured at all
	// ("recorded" — the recorded hint stood in).
	GitStateSource string `json:"git_state_source,omitempty"`
	// GitStateReason explains an "unknown" GitStateSource.
	GitStateReason string `json:"git_state_reason,omitempty"`
}

// DecideWorkstate returns the canonical disposition for a polecat.
func DecideWorkstate(in WorkstateInput) WorkstateDisposition {
	return labelFactSources(in, decideWorkstate(in))
}

// labelFactSources stamps each decided disposition with the provenance of the
// facts behind it, so every return path out of decideWorkstate reports where
// its verdict came from without repeating the labeling eight times.
func labelFactSources(in WorkstateInput, d WorkstateDisposition) WorkstateDisposition {
	d.CleanupStatusSource = CleanupStatusSourceRecorded
	switch in.GitStateSource {
	case GitStateSourceLive:
		d.GitStateSource = GitStateSourceLive
	case GitStateSourceUnknown:
		d.GitStateSource = GitStateSourceUnknown
		d.GitStateReason = in.GitCheckFailedReason
		if d.GitStateReason == "" {
			d.GitStateReason = "git_state=unknown"
		}
	default:
		d.GitStateSource = GitStateSourceRecorded
	}
	return d
}

// RecordedCleanupBlocks reports whether a recorded cleanup_status still
// contributes a blocker, given where the input's git facts came from.
//
// claude-41j.1 D9: reuse eligibility re-derives from live git on every
// evaluation, and the recorded self-report is demoted to a hint.
// has_uncommitted/has_stash/has_unpushed are recorded observations of exactly
// the three facts a live probe measures (GitDirty, StashCount, UnpushedCommits),
// so when such a probe answered (GitStateSourceLive) its answer supersedes the
// record: a stale has_stash cannot block a worktree that is demonstrably clean.
// On 2026-09-10 01:14 the recorded value reported has_stash / NEEDS_RECOVERY ten
// minutes after the stash had been dropped, with a verified 0 stashes and a
// clean tree — the self-report was simply wrong, and it was the only thing
// consulted.
//
// gt-14a fixed a fail-open regression here: missing/unknown must NOT clear
// just because a live probe ran, because a probe only measures
// GitDirty/StashCount/UnpushedCommits — it says nothing about hook_bead,
// push_failed, mr_failed or active_mr, which is exactly the information a
// missing or unreadable agent bead leaves unverified (see
// GetAgentBead(...) returning not-found as (nil, nil, nil) in
// workstateInputForPolecat and checkRecoveryForPolecat's no-agent-bead
// branch, both of which default CleanupStatus to CleanupUnknown on purpose
// for this reason). Demoting missing/unknown here on gitStateSource alone
// would clear those seats too, undoing gt-14a/gt-7kr.
//
// gt-ui2x gives missing/unknown a *narrower* way out instead:
// ResolveIgnoreCleanupStatus's agentBeadRead/liveGitProbeRan branch, gated
// on the agent bead having actually been read (so hook_bead/push_failed/
// mr_failed/active_mr are verified, not defaulted) in addition to the same
// hookSafe/activeMRSafe/gitSafe facts this function's git-derived case
// already trusts. That path only reaches this function's caller
// (decideWorkstate) via IgnoreCleanupStatus, so it is decided once, in one
// place, instead of here.
func RecordedCleanupBlocks(status CleanupStatus, gitStateSource string) bool {
	if status.IsSafe() {
		return false
	}
	if status.RequiresRecovery() && gitStateSource == GitStateSourceLive {
		return false
	}
	return true
}

func decideWorkstate(in WorkstateInput) WorkstateDisposition {
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
	if !in.State.IsReuseEligible() {
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
			// Name the state (gt-3r1h). Refusing on in.State while leaving
			// Blockers empty is what let check-recovery render a refusal whose
			// own text admitted it could not say what refused.
			Blockers: []string{"lifecycle_state=" + string(in.State)},
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
	if !in.IgnoreCleanupStatus && RecordedCleanupBlocks(in.CleanupStatus, in.GitStateSource) {
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
		} else if in.MRSubmitted || in.AssignedBeadTerminal {
			// A terminal assigned bead is submission evidence, exactly like a
			// found MR bead: the work is finished or intentionally superseded,
			// so there is nothing left to enqueue. This preserves the
			// applyMQCheck semantics (beadTerminal -> "submitted") that the
			// aa-55d8 zombie-restart fix established, on the unified
			// classifier path — a superseded branch is always "ahead", so
			// commits-ahead can never discriminate; source-issue terminality
			// is the signal (gt-nkyy).
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
//
// claude-41j.1 D9: for the reuse verdict this is no longer the reconciliation
// path — RecordedCleanupBlocks supersedes a git-derived recorded status on
// live-git evidence alone, without requiring a terminal work ref. What remains
// here is the *stricter* destructive-op gate (internal/cmd/polecat_helpers.go
// checkPolecatSafety, which sits in front of nuke), where demanding terminal
// work and a safe hook/active-MR before proceeding is exactly right.
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
// Since claude-41j.1 D9 the gate's live work is the missing/unknown case:
// git-derived statuses (uncommitted/stash/unpushed) are superseded outright by
// a live git probe through RecordedCleanupBlocks, so the CanIgnoreStaleCleanupStatus
// fallback below is retained for direct callers and for statuses the demotion
// does not reach, not because the classifier still needs it.
//
// It wraps CanIgnoreStaleCleanupStatus with three narrow extensions, each
// gated by the SAME hook/active-MR safety facts required for every other
// case — never granted unconditionally:
//
//  1. allowMissingForPartialSpawn: a polecat that never durably picked up
//     work (its hook_bead points at a bead it no longer owns, or never
//     owned) may ignore a missing/unknown CleanupStatus. Also requires
//     gitSafe, since the worktree is present and a live check is possible.
//  2. allowMissingForGoneWorktree (gt-2h6): a polecat whose worktree
//     directory has been structurally verified gone (see
//     IsStructuralWorktreeError/VerifyWorktreeExists) can never produce a
//     fresh CleanupStatus by self-report — there is no worktree left to
//     check. A missing/unknown status here means "never observable", not
//     "unknown risk", so it does not require gitSafe: gitSafe is derived
//     from a live git check that is impossible against a nonexistent
//     directory, and requiring it would permanently veto reclaiming the
//     slot. The structural proof of absence stands in for it instead.
//  3. agentBeadRead && liveGitProbeRan (gt-ui2x): a missing/unknown status
//     on an agent bead that WAS successfully read — as opposed to a bead
//     that could not be read at all, which RecordedCleanupBlocks keeps
//     blocking unconditionally — plus a live probe that verified
//     gitSafe/hookSafe/activeMRSafe. Requiring agentBeadRead is what makes
//     this narrower than gt-14a's regression: hook_bead, push_failed,
//     mr_failed and active_mr all came from that same successful read, so
//     they are verified facts here, not the unread defaults gt-14a's fix
//     protects against. Requiring liveGitProbeRan on top of gitSafe closes
//     the gap where a caller that never probed leaves GitDirty/StashCount/
//     UnpushedCommits at their zero value and gitSafe reads true by
//     omission rather than by measurement.
//
// gt-hsg: a prior version of this check (in cmd/polecat.go's check-recovery
// handler) set IgnoreCleanupStatus=true for the partial-spawn case without
// checking hookSafe/activeMRSafe/gitSafe at all — an ungated promotion of
// exactly the shape gt-7kr removed from workstateInputForPolecat, just
// reintroduced via a different precondition in a second, undiscovered copy
// of this policy. Route every caller through this one function instead.
func ResolveIgnoreCleanupStatus(status CleanupStatus, allowMissingForPartialSpawn, allowMissingForGoneWorktree, agentBeadRead, liveGitProbeRan, workTerminal, hookSafe, activeMRSafe, gitSafe bool) bool {
	if status == "" || status == CleanupUnknown {
		if allowMissingForPartialSpawn && hookSafe && activeMRSafe && gitSafe {
			return true
		}
		if allowMissingForGoneWorktree && hookSafe && activeMRSafe {
			return true
		}
		if agentBeadRead && liveGitProbeRan && hookSafe && activeMRSafe && gitSafe {
			return true
		}
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
//
// GitStateSource must be set by any caller that ran a live git probe
// (GitStateSourceLive, or GitStateSourceUnknown if the probe failed). Leaving
// it unset is a claim of its own — "no live probe was attempted" — and keeps
// the recorded cleanup_status authoritative for git-derived verdicts.
type WorkstateFacts struct {
	State                          State
	HookBead                       string
	HookBeadSafe                   bool
	HookBeadTerminal               bool
	PartialSpawnWithoutDurableHook bool
	WorktreeStructurallyMissing    bool
	// AgentBeadRead marks that the caller successfully read the polecat's
	// agent bead (as opposed to a not-found or error result, which
	// GetAgentBead-style lookups return as CleanupStatus staying at its
	// CleanupUnknown default). Only a caller that actually read the bead can
	// set this true — hook_bead, push_failed, mr_failed and active_mr came
	// from that same read, so they are verified facts, not unread defaults.
	// See ResolveIgnoreCleanupStatus's agentBeadRead/liveGitProbeRan branch.
	AgentBeadRead                  bool
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
	GitStateSource                 string
	ActiveWorkBlocker              string
	ActiveWorkCountsTowardCapacity bool
	ActiveMR                       string
	ActiveMRBlocker                string
	ActiveMRSourceTerminal         bool
	// AssignedBeadTerminal is the assigned bead's terminality alone. The MQ
	// verdict reads it as submission evidence of its own (aa-55d8), so it must
	// not be populated with a wider "some work ref is terminal" value — see
	// WorkTerminal for that (gt-pldt).
	AssignedBeadTerminal bool
	MQCheckRequired      bool
	HasSubmittableWork   bool
	MQNotRequired        bool
	MRSubmitted          bool
	MQLookupFailed       bool
}

// WorkTerminal reports whether any work ref the polecat can hold — its assigned
// bead, its hook bead, or the source issue of its active MR — is terminal: the
// cleanup-status ignore gate's precondition, deliberately wider than the MQ
// verdict's AssignedBeadTerminal (gt-pldt).
func (f WorkstateFacts) WorkTerminal() bool {
	return f.AssignedBeadTerminal || f.ActiveMRSourceTerminal || f.HookBeadTerminal
}

// NewWorkstateInput is the single production constructor for WorkstateInput.
// It derives gitSafe/activeMRSafe/workTerminal from the supplied facts and
// resolves IgnoreCleanupStatus through ResolveIgnoreCleanupStatus, so the
// fail-closed policy lives in exactly one place regardless of which caller
// (Manager, CLI check-recovery, list/inventory) is building the input.
func NewWorkstateInput(f WorkstateFacts) WorkstateInput {
	gitSafe := !f.GitCheckFailed && !f.GitDirty && f.StashCount == 0 && f.UnpushedCommits == 0
	activeMRSafe := f.ActiveMRBlocker == ""
	workTerminal := f.WorkTerminal()

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
		GitStateSource:                 f.GitStateSource,
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
		liveGitProbeRan := f.GitStateSource == GitStateSourceLive
		input.IgnoreCleanupStatus = ResolveIgnoreCleanupStatus(f.CleanupStatus, f.PartialSpawnWithoutDurableHook, f.WorktreeStructurallyMissing, f.AgentBeadRead, liveGitProbeRan, workTerminal, f.HookBeadSafe, activeMRSafe, gitSafe)
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
