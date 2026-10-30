// Package sling is the work-dispatch engine behind `gt sling`: it decides
// whether one bead may be dispatched to one rig and drives that dispatch to
// completion.
//
// internal/cmd keeps the cobra layer: it parses flags into Options and supplies
// everything the engine reaches outside the process (polecat spawn, bd writes,
// hooks, sessions) through Deps. An in-process caller such as the daemon calls
// Run with the same Options, so a dispatch that used to exec `gt sling` is a
// typed call a compiler checks.
//
// Admit is the admission step: it names the blockers that keep a bead off bd's
// ready board. The resolution rule stays in internal/beads, where
// UnresolvedBlockerIDs counts a deferred blocker as unresolved, so a bead
// waiting on a deferred blocker is refused (gt-o70a9).
package sling

import (
	"github.com/steveyegge/gastown/internal/beads"
)

// Options is one dispatch request: what to sling, where, and under which flags.
//
// Actor names the recorded dispatcher when the caller has no live agent role of
// its own (the daemon records "daemon/spec-dispatch"); empty leaves the choice
// to the caller's Actor dep.
//
// IgnoreBlockers is the only way past the admission check, and Force is not:
// the automated redispatch paths pass --force for the other guards they need,
// so a guard --force opened would be one none of them is actually held by
// (gt-e3hl2).
type Options struct {
	// What to sling
	BeadID      string // Base bead
	FormulaName string // Formula to apply ("mol-polecat-work", user formula, or "")
	RigName     string // Target rig (always a rig for this engine)

	// Flag passthrough
	Args           string   // --args
	Vars           []string // --var (key=value pairs)
	BaseBranch     string   // --base-branch
	ResumeBranch   string   // --branch / --pr (resume existing PR branch, gh#3602)
	Account        string   // --account
	Agent          string   // --agent
	NoMerge        bool     // --no-merge
	Force          bool     // --force
	IgnoreBlockers bool     // --ignore-blockers: dispatch over unresolved blockers and record who did
	HookRawBead    bool     // --hook-raw-bead
	NoBoot         bool     // --no-boot
	Mode           string   // --ralph: "" (normal) or "ralph"
	ReviewOnly     bool     // --review-only: review and report back only, no merge/commit/push
	Actor          string   // --actor: recorded dispatcher override
	// BlockerOverrideActor is who the --ignore-blockers comment names. A queued
	// sling is dispatched by the daemon, which is not the operator who passed
	// the flag, so the actor is carried from the sling that queued the bead to
	// the dispatch that runs it (gt-e3hl2). Empty leaves it to the Actor dep.
	BlockerOverrideActor string

	// Execution behavior (set by caller, not a flag)
	SkipCook         bool   // Batch optimization: formula already cooked
	FormulaFailFatal bool   // true=rollback+error (single/queue), false=hook raw bead (batch)
	CallerContext    string // Identifies the caller for shutdown messages (e.g., "queue-dispatch", "batch-sling", "daemon/spec-dispatch")
	TownRoot         string
	BeadsDir         string

	// Steps times the dispatch's stages, and is handed to the spawn path for its
	// own (gt-llg8, gt-7nctk). Nil is no timer: the engine names nothing and the
	// spawn falls back to this process's timer, which is what a hand-run
	// `gt sling` wants. The spec dispatcher passes its sling trace, so every
	// stage of a tick's sling lands on the line that tick logs.
	Steps func(name string)

	// Deliberate marks a hand dispatch, which clears the bead's respawn count
	// instead of spending it. Only the cobra layer sets it (slingDeliberate in
	// internal/cmd); every in-process dispatcher (spec, capacity queue, epic)
	// leaves it false and so spends the budget (gt-jogkx).
	Deliberate bool

	// SkipDuplicateCheck disables the pre-dispatch content duplicate check
	// (gt-mcq). No production dispatcher sets it: spec, epic and capacity-queue
	// dispatch are each a bead's first dispatch and run the check (gt-skk7,
	// gt-eisp2). Tests that target a later guard set it to reach that guard.
	SkipDuplicateCheck bool

	// AdmitOverlap lets the sling proceed past a content overlap the check
	// would refuse: a shared test against work in flight, or against unstarted
	// work the dispatcher would sling first (gt-e1ufo). Only the spec
	// dispatcher sets it, because its tick slings every ready bead it reaches
	// and an overlap is no reason to leave one unslung. The check still runs,
	// so the shared files and closed duplicates it finds still print as a
	// warning. A manual sling never sets it, so the refusal stands for a human.
	AdmitOverlap bool
}

// Result is a dispatch's outcome, for caller-level tracking.
type Result struct {
	BeadID           string
	PolecatName      string
	SpawnInfo        *Spawn
	Success          bool
	ErrMsg           string
	AttachedMolecule string
}

// Bead is a work bead reduced to the fields a dispatch decides on. The JSON
// tags are the shape bd prints it in.
type Bead struct {
	Title        string           `json:"title"`
	Status       string           `json:"status"`
	Assignee     string           `json:"assignee"`
	Description  string           `json:"description"`
	Design       string           `json:"design,omitempty"`
	Notes        string           `json:"notes,omitempty"`
	Labels       []string         `json:"labels,omitempty"`
	Dependencies []beads.IssueDep `json:"dependencies,omitempty"`
	IssueType    string           `json:"issue_type,omitempty"`
	// Acceptance is the acceptance field bd keeps beside the description, which
	// the shape lint reads when the description carries no section of its own
	// (gt-mknmb).
	Acceptance string `json:"acceptance_criteria,omitempty"`
	// Priority and CreatedAt are the bead's dispatch-order keys, read so the
	// content-overlap guard can rank an unstarted peer against it (gt-nlkba).
	Priority  int    `json:"priority"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Hold is a work bead's status and assignee before a dispatch touched it.
type Hold struct {
	Status   string
	Assignee string
}

// SpawnOptions are the spawn knobs a dispatch passes to the caller's spawner.
type SpawnOptions struct {
	TownRoot     string // Gas Town workspace root
	Force        bool   // Force spawn past uncommitted work and merge-queue backpressure
	Account      string // Claude Code account handle to use
	Create       bool   // Create the polecat if it does not exist
	HookBead     string // Bead ID to set as hook_bead at spawn time
	Agent        string // Agent override for this spawn
	BaseBranch   string // Override base branch for the polecat's worktree
	ResumeBranch string // Resume an existing branch instead of creating a fresh one
	Name         string // The exact polecat a named dispatch targets; empty lets the pool choose

	// Deliberate carries Options.Deliberate to the spawn, where the bead's
	// respawn count is charged.
	Deliberate bool

	// Steps times the spawn's stages; nil leaves the choice to the caller's
	// process-wide timer.
	Steps func(name string)
}

// Spawn is a polecat prepared for a dispatch, session start deferred.
//
// Ref carries the caller's own record through the dispatch untouched: the
// mechanisms a caller supplies need state this engine never reads, and
// rebuilding it from the fields below would drop it silently.
type Spawn struct {
	RigName     string
	PolecatName string
	ClonePath   string
	BaseBranch  string

	// OriginalHold is the work bead's status and assignee before this dispatch
	// touched it; nil when unknown. A rollback that finds the work surviving
	// hands the bead back to this holder instead of releasing it.
	OriginalHold *Hold

	Ref any
}

// AgentID returns the agent identifier (e.g., "gastown/polecats/Toast").
func (s *Spawn) AgentID() string {
	return s.RigName + "/polecats/" + s.PolecatName
}

// FieldUpdates are the bead fields one dispatch writes.
type FieldUpdates struct {
	Dispatcher       string   // Agent that dispatched the work
	Args             string   // Natural language instructions
	Vars             []string // Formula variables (key=value pairs)
	AttachedMolecule string   // Wisp root ID
	AttachedFormula  string   // Formula name for inline step display
	ClearAttachment  bool     // Clear stale workflow attachment fields first
	AttachedAt       string   // Assignment timestamp
	NoMerge          bool     // Skip merge queue on completion
	ReviewOnly       bool     // Review-only mode
	Mode             *string  // nil = unchanged, "" clears, "ralph" enables
	FormulaVars      string   // Newline-separated key=value pairs
}

// FormulaResult is what instantiating a formula on a bead produced.
type FormulaResult struct {
	WispRootID  string   // The wisp root ID (compound root after bonding)
	BeadToHook  string   // The bead to hook (the BASE bead, not the wisp)
	FormulaVars []string // Vars used to instantiate/render the formula
}
