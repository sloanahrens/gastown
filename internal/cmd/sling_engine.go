package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/sling"
)

// engineDeps is internal/sling's view of this process's collaborators. The
// engine owns the dispatch policy; this file owns the mechanisms it reaches
// outside the process, so the same policy drives the cobra command and the
// daemon's convoy feeders.
//
// Most fields pass straight through: the shapes sling declares are the shapes
// this package already uses (beadInfo, beadFieldUpdates, duplicateCandidate and
// FormulaOnBeadResult are aliases for the engine's types). The exceptions are
// the polecat, whose record this package owns.
func (d *slingDeps) engineDeps() *sling.Deps {
	return &sling.Deps{
		Out:         d.out,
		FindTown:    d.findTown,
		Actor:       d.engineActor,
		Requester:   d.requester,
		ReleaseSeat: d.engineReleaseSeat,

		EstopOn:            d.estopOn,
		RigParked:          d.rigParked,
		BeadInfoInTown:     d.beadInfoInTown,
		AgentDead:          d.agentDead,
		SurvivingWorkGuard: d.survivingWorkGuard,
		StewardReworkOwner: d.stewardReworkOwner,
		CheckDuplicates:    d.checkDuplicates,
		VerifyInTargetRig:  d.verifyInTargetRig,
		DefaultFormula:     d.engineDefaultFormula,

		LockBead:     d.lockBead,
		LockAssignee: d.lockAssignee,

		SpawnPolecat:   d.engineSpawnPolecat,
		CleanupSpawned: d.engineCleanupSpawned,
		StartSession:   d.engineStartSession,

		ClearReassigned:    d.clearReassigned,
		RecordReassignment: d.recordReassignment,

		CollectMolecules:   d.collectMolecules,
		BurnMolecules:      d.burnMolecules,
		RigCommandVars:     d.rigCommandVars,
		PriorAttempt:       d.priorAttempt,
		Cook:               d.cook,
		InstantiateFormula: d.instantiateFormula,
		HookDir:            d.hookDir,
		Hook:               d.hook,
		StoreFields:        d.storeFields,
		ClearOrphanLabels:  d.clearOrphanLabels,
		UpdateAgentHook:    d.updateAgentHook,
		UpdateAgentMode:    d.updateAgentMode,
		LogFeed:            d.logFeed,

		TrackedByConvoy: d.trackedByConvoy,
		CreateConvoy:    d.createConvoy,

		RollbackArtifacts: d.engineRollbackArtifacts,
		RestoreRawFields:  d.restoreRawFields,
		RestorePinned:     d.restorePinned,
		NoteDispatched:    d.noteDispatched,
	}
}

// slingEngineDeps is the engine's collaborators for a caller that is not the
// cobra command — the daemon's convoy feeder and the convoy continuation feed.
// The daemon cannot import this package (it would close the cycle back to
// internal/daemon), so it is handed these at startup instead of exec'ing
// `gt sling`.
func slingEngineDeps() *sling.Deps {
	return realSlingDeps().engineDeps()
}

// engineActor names the dispatcher a dispatch is recorded under. An Options
// that names its own actor (the daemon's "daemon/convoy:<id>") wins: the
// auto-detected actor is the role of whoever happens to be running the process,
// which in a daemon is nobody.
func (d *slingDeps) engineActor(opts sling.Options) string {
	if opts.Actor != "" {
		return opts.Actor
	}
	return d.actor()
}

// engineReleaseSeat drops the pool seat the dispatch's spawn reserved. The
// claim lives in the spawn's own store, so this is the spawn's release and not
// a process-wide one: the daemon dispatches concurrently, and a release that
// dropped whichever claim the process held last would free a seat another
// in-flight dispatch is standing on (gt-t8q5).
func (d *slingDeps) engineReleaseSeat(spawn *sling.Spawn) {
	// Deliberately not cmdSpawn: that panics on a spawn this package did not
	// make, and dropping a claim is not a step worth crashing a dispatch over.
	// A spawn the engine made itself — or made none of — reserved no seat in
	// this process, so its release is the release of nothing.
	var cmdSpawnRecord *SpawnedPolecatInfo
	if spawn != nil {
		cmdSpawnRecord, _ = spawn.Ref.(*SpawnedPolecatInfo)
	}
	d.releaseSeat(cmdSpawnRecord)
}

// engineDefaultFormula is the formula the cobra layer would have applied to a
// dispatch that named none: resolveFormula's rig property layers, its rig
// settings file, and the system default. The engine asks for it instead of the
// caller so that a dispatch made from the daemon resolves the same formula the
// `gt sling` it replaced would have — the caller there holds a convoy's record,
// not the town's config.
func (d *slingDeps) engineDefaultFormula(townRoot, rigName string) string {
	return d.resolveFormula("", false, townRoot, rigName)
}

// engineSpawnPolecat is d.spawnPolecat in the engine's vocabulary.
func (d *slingDeps) engineSpawnPolecat(rigName string, opts sling.SpawnOptions) (*sling.Spawn, error) {
	spawnInfo, err := d.spawnPolecat(rigName, SlingSpawnOptions{
		TownRoot:     opts.TownRoot,
		Force:        opts.Force,
		Account:      opts.Account,
		Create:       opts.Create,
		HookBead:     opts.HookBead,
		Agent:        opts.Agent,
		BaseBranch:   opts.BaseBranch,
		ResumeBranch: opts.ResumeBranch,
		Name:         opts.Name,
		Steps:        opts.Steps,
	})
	if err != nil {
		return nil, err
	}
	return engineSpawn(spawnInfo), nil
}

// engineSpawn is the engine's view of a spawned polecat. Ref carries this
// package's record, so the mechanisms below reach the state only it holds (the
// account, the agent, the worktree it prepared).
func engineSpawn(s *SpawnedPolecatInfo) *sling.Spawn {
	spawn := &sling.Spawn{
		RigName:     s.RigName,
		PolecatName: s.PolecatName,
		ClonePath:   s.ClonePath,
		BaseBranch:  s.BaseBranch,
		Ref:         s,
	}
	if s.originalHold != nil {
		spawn.OriginalHold = &sling.Hold{Status: s.originalHold.Status, Assignee: s.originalHold.Assignee}
	}
	return spawn
}

// cmdSpawn is the engine's spawn back in this package's record, with the hold
// the engine recorded on it. It panics on a spawn the engine did not get from
// engineSpawn: only a spawn record this package made can be cleaned up or
// started, and a silent nil would fail further from the cause.
func cmdSpawn(s *sling.Spawn) *SpawnedPolecatInfo {
	spi, ok := s.Ref.(*SpawnedPolecatInfo)
	if !ok || spi == nil {
		panic(fmt.Sprintf("sling: spawn %q carries no polecat record (Ref %T)", s.PolecatName, s.Ref))
	}
	if s.OriginalHold != nil {
		spi.originalHold = &beadHold{Status: s.OriginalHold.Status, Assignee: s.OriginalHold.Assignee}
	}
	return spi
}

func (d *slingDeps) engineCleanupSpawned(spawn *sling.Spawn, townRoot, rigName, convoyID string) {
	d.cleanupSpawned(cmdSpawn(spawn), townRoot, rigName, convoyID)
}

func (d *slingDeps) engineStartSession(spawn *sling.Spawn) (string, error) {
	return d.startSession(cmdSpawn(spawn))
}

func (d *slingDeps) engineRollbackArtifacts(spawn *sling.Spawn, townRoot, beadID, hookWorkDir, convoyID string) {
	d.rollbackArtifacts(cmdSpawn(spawn), townRoot, beadID, hookWorkDir, convoyID)
}
