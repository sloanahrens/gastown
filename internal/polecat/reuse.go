package polecat

import "errors"

// ErrPolecatNeedsRecovery marks an idle-looking polecat that must not be reset
// or advertised as reusable until its preserved work is recovered or submitted.
var ErrPolecatNeedsRecovery = errors.New("polecat needs recovery before reuse")

// SlotReuseInput is the shared input for deciding whether a polecat slot can be
// advertised as open and destructively reused for new work. It carries raw
// facts, not a pre-resolved IgnoreCleanupStatus — see DecideSlotReuse.
type SlotReuseInput struct {
	State                  State
	HookBead               string
	HookBeadSafe           bool
	HookBeadTerminal       bool
	CleanupStatus          CleanupStatus
	PushFailed             bool
	MRFailed               bool
	Branch                 string
	GitDirty               bool
	GitDirtyReason         string
	StashCount             int
	UnpushedCommits        int
	GitCheckFailed         bool
	GitCheckFailedReason   string
	ActiveMR               string
	ActiveMRBlocker        string
	ActiveMRSourceTerminal bool
	MQCheckRequired        bool
	HasSubmittableWork     bool
	MQNotRequired          bool
	AssignedBeadTerminal   bool
	MRSubmitted            bool
	MQLookupFailed         bool
}

// SlotReuseDecision explains whether a polecat can be reused and why not.
type SlotReuseDecision struct {
	Reusable bool
	Reason   string
}

// DecideSlotReuse is the single source of truth for reuse safety. It fails
// closed: unknown cleanup/git state means the slot needs recovery, not reuse.
//
// gt-hsg: this used to build a WorkstateInput directly, with callers
// resolving IgnoreCleanupStatus themselves (witness/handlers.go called
// CanIgnoreStaleCleanupStatus inline) — a fourth independent copy of the
// promotion policy the unification requires living in exactly one place.
// Routing through NewWorkstateInput closes that: callers supply raw facts,
// this function is the only place they turn into a decision.
func DecideSlotReuse(in SlotReuseInput) SlotReuseDecision {
	facts := WorkstateFacts{
		State:                  in.State,
		HookBead:               in.HookBead,
		HookBeadSafe:           in.HookBeadSafe,
		HookBeadTerminal:       in.HookBeadTerminal,
		CleanupStatus:          in.CleanupStatus,
		PushFailed:             in.PushFailed,
		MRFailed:               in.MRFailed,
		Branch:                 in.Branch,
		GitDirty:               in.GitDirty,
		GitDirtyReason:         in.GitDirtyReason,
		StashCount:             in.StashCount,
		UnpushedCommits:        in.UnpushedCommits,
		GitCheckFailed:         in.GitCheckFailed,
		GitCheckFailedReason:   in.GitCheckFailedReason,
		ActiveMR:               in.ActiveMR,
		ActiveMRBlocker:        in.ActiveMRBlocker,
		ActiveMRSourceTerminal: in.ActiveMRSourceTerminal,
		AssignedBeadTerminal:   in.AssignedBeadTerminal,
		MQCheckRequired:        in.MQCheckRequired,
		HasSubmittableWork:     in.HasSubmittableWork,
		MQNotRequired:          in.MQNotRequired,
		MRSubmitted:            in.MRSubmitted,
		MQLookupFailed:         in.MQLookupFailed,
	}
	d := DecideWorkstate(NewWorkstateInput(facts))
	return SlotReuseDecision{Reusable: d.Reusable, Reason: d.Reason}
}
