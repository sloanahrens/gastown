package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// slingOptions are gt sling's flags for one run. runSling copies them from
// the cobra flag globals once, so a run never reads or writes a package
// global and a test states its flags as a literal.
type slingOptions struct {
	subject       string
	message       string
	dryRun        bool
	on            string   // --on: the bead a formula is applied to
	vars          []string // --var
	argsText      string   // --args
	stdin         bool
	hookRawBead   bool
	create        bool
	force         bool
	account       string
	agent         string
	noConvoy      bool
	owned         bool
	noMerge       bool
	merge         string
	noBoot        bool
	baseBranch    string
	resumeBranch  string
	resumePR      int
	maxConcurrent int
	ralph         bool
	formula       string
	crew          string
	reviewOnly    bool
}

// slingOptionsFromFlags is the options the cobra flags hold.
func slingOptionsFromFlags() slingOptions {
	return slingOptions{
		subject:       slingSubject,
		message:       slingMessage,
		dryRun:        slingDryRun,
		on:            slingOnTarget,
		vars:          append([]string(nil), slingVars...),
		argsText:      slingArgs,
		stdin:         slingStdin,
		hookRawBead:   slingHookRawBead,
		create:        slingCreate,
		force:         slingForce,
		account:       slingAccount,
		agent:         slingAgent,
		noConvoy:      slingNoConvoy,
		owned:         slingOwned,
		noMerge:       slingNoMerge,
		merge:         slingMerge,
		noBoot:        slingNoBoot,
		baseBranch:    slingBaseBranch,
		resumeBranch:  slingResumeBranch,
		resumePR:      slingResumePR,
		maxConcurrent: slingMaxConcurrent,
		ralph:         slingRalph,
		formula:       slingFormula,
		crew:          slingCrew,
		reviewOnly:    slingReviewOnly,
	}
}

// slingDeps is everything a sling reaches outside the process: bd, tmux,
// git, mail, the town's files and locks, the environment and the terminal.
// realSlingDeps wires the real ones; unit tests build one from fakes, so
// runSling's and executeSling's decisions run with nothing spawned, no
// package global swapped and no PATH, cwd or environment change.
type slingDeps struct {
	getenv func(string) string
	out    io.Writer
	stdin  io.Reader
	steps  *sling.Timer

	// Process state.
	autoCommitOff func() (restore func())
	// releaseSeat drops the pool seat one spawn reserved; nil spawn means the
	// dispatch made none. See slingDeps.engineReleaseSeat.
	releaseSeat func(*SpawnedPolecatInfo)
	findTown    func() (string, error)
	townOrEnv   func() (string, error)

	// Routing and dispatch paths runSling hands a request to.
	resolvePRBranch        func(prNumber int) (string, error)
	workflowTargetOverride func(args []string) ([]string, error)
	shouldDefer            func() (bool, error)
	isRigName              func(target string) (string, bool)
	idType                 func(id string) (string, error)
	batchSchedule          func(opts slingOptions, beadIDs []string, rigName, townRoot string) error
	batchSling             func(opts slingOptions, beadIDs []string, rigName, townBeadsDir string) error
	rigFromBeadIDs         func(beadIDs []string, townRoot string) (string, error)
	scheduleBead           func(beadID, rigName string, opts ScheduleOptions) error
	rigForBead             func(townRoot, beadID string) string
	rigBeadsDir            func(townRoot, rigName string) (string, bool)
	resolveFormula         func(explicit string, hookRawBead bool, townRoot, rigName string) string
	slingFormula           func(ctx context.Context, args []string) error
	convoySchedule         func(convoyID string, opts convoyScheduleOpts) error
	convoySling            func(convoyID string, opts convoyScheduleOpts) error
	epicSchedule           func(epicID string, opts epicScheduleOpts) error
	epicSling              func(epicID string, opts epicScheduleOpts) error

	// The bead and its guards.
	verifyBead         func(beadID string) error
	verifyFormula      func(formulaName, workDir, townRoot string) error
	verifyInTargetRig  func(beadID, targetRig, townRoot string) error
	beadInfo           func(beadID string) (*beadInfo, error)
	beadInfoInTown     func(townRoot, beadID string) (*beadInfo, error)
	lockBead           func(townRoot, beadID string) (func(), error)
	lockAssignee       func(townRoot, targetAgent string) (func(), error)
	rigParked          func(townRoot, rigName string) (bool, string)
	estopOn            func(townRoot, rigName string) (bool, error)
	agentDead          func(assignee string) bool
	survivingWorkGuard func(townRoot, beadID, holder string) error
	// stewardReworkOwner reports why a rework bead is the steward patrol's to
	// settle rather than the sling's (daemon.StewardReworkOwner, gt-28ibg).
	stewardReworkOwner func(townRoot, rig string) string
	checkDuplicates    func(townRoot, beadID string, info *beadInfo) (*duplicateCandidate, []duplicateMatch, error)
	noteDispatched     func(townRoot string, candidate *duplicateCandidate)
	crossRigGuard      func(beadID, targetAgent, townRoot string) error

	// The target and its polecat.
	resolveSelf    func() (agentID, pane, hookRoot string, err error)
	resolveTarget  func(target string, opts ResolveTargetOptions) (*ResolvedTarget, error)
	spawnPolecat   func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error)
	admitPolecat   func(townRoot, rigName, beadID, operation string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error)
	startSession   func(spawn *SpawnedPolecatInfo) (string, error)
	cleanupSpawned func(spawn *SpawnedPolecatInfo, townRoot, rigName, convoyID string)
	resolveAgent   func(target string) (agentID, pane, hookRoot string, err error)
	cwdTown        func() string
	crewExists     func(townRoot, rigName, name string) bool
	peekPool       func(townRoot, requested string) (agent, reason string, err error)
	// peekNamed is the named-polecat half of the same preview: a refusal the
	// live sling would raise, or nil. See peekNamedPolecatSling (gt-yxc7m).
	peekNamed func(townRoot, rigName string, opts SlingSpawnOptions) error
	wakeRig   func(rigName string)

	// Reassignment away from a previous holder.
	requester          func() string
	clearReassigned    func(townRoot, assignee string)
	unhook             func(townRoot, beadID string) error
	recordReassignment func(townRoot, beadID, from, to, requester string)

	// Convoy, formula and hook writes. trackedByConvoy and createConvoy take
	// the town root: the daemon's convoy feeder reaches them in process, where
	// the cwd is not the town.
	trackedByConvoy    func(townRoot, beadID string) string
	createConvoy       func(townRoot, beadID, beadTitle string, owned bool, mergeStrategy, baseBranch, agent, formula string) (string, error)
	collectMolecules   func(info *beadInfo, beadID, townRoot string) ([]string, error)
	burnMolecules      func(molecules []string, beadID, townRoot string) error
	rigCommandVars     func(townRoot, rig string) []string
	priorAttempt       func(beadsDir, issueID string) []string
	cook               func(formulaName, workDir, townRoot string) error
	instantiateFormula func(ctx context.Context, formulaName, beadID, title, hookWorkDir, townRoot string, extraVars []string) (*FormulaOnBeadResult, error)
	actor              func() string
	hookDir            func(townRoot, beadID, workDir string) string
	storeFields        func(townRoot, beadID string, updates beadFieldUpdates) error
	hook               func(beadID, targetAgent, hookDir, townRoot string) error
	clearOrphanLabels  func(townRoot, beadID, workDir string)
	updateAgentHook    func(agentID, beadID, workDir, townBeadsDir string)
	updateAgentMode    func(agentID, mode, workDir, townBeadsDir string)
	logFeed            func(eventType, actor string, payload map[string]interface{}) error
	enqueueNudge       func(townRoot, session string, n nudge.QueuedNudge) error

	// A standalone formula sling's wisp.
	findHookedFormula func(workDir, targetAgent, formulaName string) (*beads.Issue, error)
	cookFormula       func(formulaName, workDir, townRoot string) error
	createWisp        func(formulaName, workDir, townRoot string, vars []string) ([]byte, error)
	hookWisp          func(beadID, targetAgent, hookDir string) error
	burnWisp          func(wispRootID, workDir string) error
	nudgePane         func(pane, message string) error

	// The sling context a scheduled bead waits in, in the target rig's beads.
	slingContexts func(rigBeadsDir string) slingContextStore

	// Undoing a partial sling.
	rollbackArtifacts func(spawn *SpawnedPolecatInfo, townRoot, beadID, hookWorkDir, convoyID string)
	restoreRawFields  func(beadID, townRoot, hookWorkDir string, originalInfo *beadInfo)
	restorePinned     func(townRoot, beadID, assignee string)

	// Nudging the target once the work is hooked.
	sessionFromPane   func(pane string) string
	ensureAgentReady  func(sessionName string) error
	injectStartPrompt func(pane, beadID, subject, args string) error
}

// realSlingDeps is the running gt's collaborators. The package seams some of
// them read are still replaced by tests of other commands.
func realSlingDeps() *slingDeps {
	d := &slingDeps{
		getenv:        os.Getenv,
		out:           os.Stdout,
		stdin:         os.Stdin,
		steps:         slingSteps,
		autoCommitOff: setBDAutoCommitOff,
		releaseSeat:   func(s *SpawnedPolecatInfo) { s.releaseSeatClaim() },
		findTown:      findTownRoot,
		townOrEnv:     workspace.FindFromCwdOrError,

		resolvePRBranch:        resolvePRBranch,
		workflowTargetOverride: applyWorkflowStepTargetOverride,
		shouldDefer:            shouldDeferDispatch,
		isRigName:              IsRigName,
		idType:                 detectSchedulerIDType,
		batchSchedule:          runBatchScheduleWith,
		batchSling:             runBatchSlingWith,
		rigFromBeadIDs:         resolveRigFromBeadIDs,
		rigForBead:             resolveRigForBead,
		rigBeadsDir:            beads.ResolveRepoAliasBeadsDir,
		resolveFormula:         resolveFormula,
		convoySchedule:         runConvoyScheduleByID,
		convoySling:            runConvoySlingByID,
		epicSchedule:           runEpicScheduleByID,
		epicSling:              runEpicSlingByID,

		verifyBead:         verifyBeadExists,
		verifyFormula:      verifyFormulaExists,
		verifyInTargetRig:  verifyBeadExistsInTargetRigDatabase,
		beadInfo:           getBeadInfo,
		beadInfoInTown:     getBeadInfoFromTownRoot,
		lockBead:           tryAcquireSlingBeadLock,
		lockAssignee:       tryAcquireSlingAssigneeLock,
		rigParked:          IsRigParkedOrDocked,
		estopOn:            estop.ActiveFor,
		agentDead:          isHookedAgentDeadFn,
		survivingWorkGuard: reslingSurvivingWorkGuard,
		stewardReworkOwner: daemon.StewardReworkOwner,
		checkDuplicates:    checkSlingDuplicates,
		noteDispatched:     noteSlingCandidateDispatched,
		crossRigGuard:      checkCrossRigGuard,

		resolveSelf:    resolveSelfTarget,
		spawnPolecat:   spawnPolecatForSling,
		admitPolecat:   acquirePolecatAdmissionFn,
		startSession:   func(s *SpawnedPolecatInfo) (string, error) { return s.StartSession() },
		cleanupSpawned: cleanupSpawnedPolecat,
		resolveAgent:   resolveTargetAgentFn,
		cwdTown:        townFromCwd,
		crewExists:     crewDirExists,
		peekPool:       peekPolecatPoolAgent,
		peekNamed:      peekNamedPolecatSling,
		wakeRig:        wakeRigAgents,

		requester:          reassignRequester,
		clearReassigned:    clearReassignedPolecatState,
		unhook:             unhookFromPreviousOwner,
		recordReassignment: recordReassignment,

		trackedByConvoy:    isTrackedByConvoy,
		createConvoy:       createAutoConvoy,
		collectMolecules:   collectExistingMoleculesForBead,
		burnMolecules:      burnExistingMolecules,
		rigCommandVars:     loadRigCommandVars,
		priorAttempt:       lookupPriorAttempt,
		cook:               CookFormula,
		instantiateFormula: InstantiateFormulaOnBead,
		actor:              resolveSlingActor,
		hookDir:            beads.ResolveHookDir,
		storeFields:        storeFieldsInBeadFromTownRoot,
		hook:               hookBeadWithRetryWithTownRoot,
		clearOrphanLabels:  clearOrphanEpisodeLabels,
		updateAgentHook:    updateAgentHookBead,
		updateAgentMode:    updateAgentMode,
		logFeed:            events.LogFeed,
		enqueueNudge:       nudge.Enqueue,

		findHookedFormula: findHookedFormulaSingletonFn,
		cookFormula:       CookFormula,
		createWisp:        createFormulaWisp,
		hookWisp:          hookBeadWithRetryFn,
		burnWisp:          burnSlingWispFn,
		nudgePane:         func(pane, msg string) error { return tmux.NewTmux().NudgePane(pane, msg) },

		slingContexts: func(rigBeadsDir string) slingContextStore {
			return beads.NewWithBeadsDir(filepath.Dir(rigBeadsDir), rigBeadsDir)
		},

		rollbackArtifacts: rollbackSlingArtifactsFn,
		restoreRawFields:  restoreRollbackRawWorkflowFieldsFromCurrent,
		restorePinned:     restorePinnedBead,

		sessionFromPane:   getSessionFromPane,
		ensureAgentReady:  ensureAgentReady,
		injectStartPrompt: injectStartPrompt,
	}
	d.resolveTarget = d.resolveSlingTarget
	d.scheduleBead = d.scheduleSlingBead
	return d
}

// slingRun is one gt sling invocation: its options, its town and its
// collaborators.
type slingRun struct {
	*slingDeps
	opts     slingOptions
	townRoot string
	townErr  error // set when the cwd is not in a town
}

// newSlingRun is a run of the real gt from the cwd's town.
func newSlingRun(opts slingOptions) *slingRun {
	townRoot, err := workspace.FindFromCwd()
	r := &slingRun{slingDeps: realSlingDeps(), opts: opts, townRoot: townRoot, townErr: err}
	r.slingFormula = r.runFormula
	return r
}

// orphanMolecule is isOrphanMolecule judged by this run's liveness check.
func (d *slingDeps) orphanMolecule(info *beadInfo) bool {
	return isOrphanMoleculeWith(info, d.agentDead)
}

// setBDAutoCommitOff turns Dolt auto-commit off for every bd the process runs
// until restore (gt-u6n6a). Under concurrent load (batch slinging),
// auto-commits from individual bd writes cause manifest contention and
// 'database is read only' errors; the Dolt server handles commits.
func setBDAutoCommitOff() (restore func()) {
	prev := os.Getenv("BD_DOLT_AUTO_COMMIT")
	//testpolicy:allow prod-no-setenv — every bd this process runs inherits its environment; scoped, and restored below; tests reach it only through slingDeps
	_ = os.Setenv("BD_DOLT_AUTO_COMMIT", "off")
	return func() {
		if prev == "" {
			//testpolicy:allow prod-no-setenv — removes the scoped override above, which the caller did not have
			_ = os.Unsetenv("BD_DOLT_AUTO_COMMIT")
		} else {
			//testpolicy:allow prod-no-setenv — restores the value the scoped override above replaced
			_ = os.Setenv("BD_DOLT_AUTO_COMMIT", prev)
		}
	}
}

// unhookFromPreviousOwner sets a force-reassigned bead back to open with no
// assignee.
func unhookFromPreviousOwner(townRoot, beadID string) error {
	open, unassigned := string(beads.StatusOpen), ""
	return pinnedBd(beads.ResolveHookDir(townRoot, beadID, "")).Update(beadID, beads.UpdateOptions{Status: &open, Assignee: &unassigned})
}
