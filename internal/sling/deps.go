package sling

import (
	"context"
	"io"
)

// Deps is everything a dispatch reaches outside this process: bd, tmux, git,
// the town's files and locks, the environment and the terminal.
//
// The engine owns the dispatch policy — which guards run, in what order, and
// what is rolled back when one fails. Its callers own the mechanisms, so the
// same policy runs behind the cobra command, behind the daemon's convoy
// feeder, and behind a unit test's fakes without a process boundary between
// them.
type Deps struct {
	// Output and identity.
	Out         io.Writer
	FindTown    func() (string, error)
	Actor       func(opts Options) string
	Requester   func() string
	ReleaseSeat func()

	// Rig and bead guards.
	EstopOn            func(townRoot, rigName string) (bool, error)
	RigParked          func(townRoot, rigName string) (bool, string)
	BeadInfoInTown     func(townRoot, beadID string) (*Bead, error)
	AgentDead          func(assignee string) bool
	SurvivingWorkGuard func(townRoot, beadID, holder string) error
	CheckDuplicates    func(townRoot, beadID string, info *Bead) (*Duplicate, []DuplicateMatch, error)
	VerifyInTargetRig  func(beadID, targetRig, townRoot string) error

	// Serializing concurrent writes.
	LockBead     func(townRoot, beadID string) (func(), error)
	LockAssignee func(townRoot, targetAgent string) (func(), error)

	// The polecat the dispatch runs on. CleanupSpawned and StartSession take
	// the town root explicitly: the exec boundary used to supply it as the
	// subprocess's working directory, and a mechanism that reads the cwd
	// instead is a mechanism the daemon cannot call.
	SpawnPolecat   func(rigName string, opts SpawnOptions) (*Spawn, error)
	CleanupSpawned func(spawn *Spawn, townRoot, rigName, convoyID string)
	StartSession   func(spawn *Spawn) (string, error)

	// Reassignment away from a previous holder.
	ClearReassigned    func(townRoot, assignee string)
	RecordReassignment func(townRoot, beadID, from, to, requester string)

	// Formula, hook and bead writes.
	CollectMolecules   func(info *Bead, beadID, townRoot string) ([]string, error)
	BurnMolecules      func(molecules []string, beadID, townRoot string) error
	RigCommandVars     func(townRoot, rig string) []string
	PriorAttempt       func(beadsDir, issueID string) []string
	Cook               func(formulaName, workDir, townRoot string) error
	InstantiateFormula func(ctx context.Context, formulaName, beadID, title, hookWorkDir, townRoot string, extraVars []string) (*FormulaResult, error)
	HookDir            func(townRoot, beadID, workDir string) string
	Hook               func(beadID, targetAgent, hookDir, townRoot string) error
	StoreFields        func(townRoot, beadID string, updates FieldUpdates) error
	ClearOrphanLabels  func(townRoot, beadID, workDir string)
	UpdateAgentHook    func(agentID, beadID, workDir, townBeadsDir string)
	UpdateAgentMode    func(agentID, mode, workDir, townBeadsDir string)
	LogFeed            func(eventType, actor string, payload map[string]interface{}) error

	// Convoy tracking.
	TrackedByConvoy func(townRoot, beadID string) string
	CreateConvoy    func(townRoot, beadID, beadTitle string, owned bool, mergeStrategy, baseBranch, agent, formula string) (string, error)

	// Undoing a partial dispatch.
	RollbackArtifacts func(spawn *Spawn, townRoot, beadID, hookWorkDir, convoyID string)
	RestoreRawFields  func(beadID, townRoot, hookWorkDir string, originalInfo *Bead)
	RestorePinned     func(townRoot, beadID, assignee string)
	NoteDispatched    func(townRoot string, candidate *Duplicate)
}

// out is the dispatch's output sink; a nil one discards.
func (d *Deps) out() io.Writer {
	if d.Out == nil {
		return io.Discard
	}
	return d.Out
}
