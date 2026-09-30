package cmd

import (
	"context"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/workspace"
)

// slingOptions are gt sling's flags for one run. runSling copies them from
// the cobra flag globals once, so a run never reads or writes a package
// global and a test states its flags as a literal.
type slingOptions struct {
	subject      string
	message      string
	dryRun       bool
	on           string   // --on: the bead a formula is applied to
	vars         []string // --var
	argsText     string   // --args
	stdin        bool
	hookRawBead  bool
	create       bool
	force        bool
	account      string
	agent        string
	noConvoy     bool
	owned        bool
	noMerge      bool
	merge        string
	noBoot       bool
	baseBranch   string
	resumeBranch string
	resumePR     int
	ralph        bool
	formula      string
	crew         string
	reviewOnly   bool
}

// slingOptionsFromFlags is the options the cobra flags hold.
func slingOptionsFromFlags() slingOptions {
	return slingOptions{
		subject:      slingSubject,
		message:      slingMessage,
		dryRun:       slingDryRun,
		on:           slingOnTarget,
		vars:         append([]string(nil), slingVars...),
		argsText:     slingArgs,
		stdin:        slingStdin,
		hookRawBead:  slingHookRawBead,
		create:       slingCreate,
		force:        slingForce,
		account:      slingAccount,
		agent:        slingAgent,
		noConvoy:     slingNoConvoy,
		owned:        slingOwned,
		noMerge:      slingNoMerge,
		merge:        slingMerge,
		noBoot:       slingNoBoot,
		baseBranch:   slingBaseBranch,
		resumeBranch: slingResumeBranch,
		resumePR:     slingResumePR,
		ralph:        slingRalph,
		formula:      slingFormula,
		crew:         slingCrew,
		reviewOnly:   slingReviewOnly,
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
	steps  *slingTimer

	// Process state.
	autoCommitOff func() (restore func())
	releaseSeat   func()
	findTown      func() (string, error)
	townOrEnv     func() (string, error)

	// Routing and dispatch paths runSling hands a request to.
	resolvePRBranch        func(prNumber int) (string, error)
	workflowTargetOverride func(args []string) ([]string, error)
	shouldDefer            func() (bool, error)
	isRigName              func(target string) (string, bool)
	idType                 func(id string) (string, error)
	batchSchedule          func(beadIDs []string, rigName, townRoot string) error
	batchSling             func(beadIDs []string, rigName, townBeadsDir string) error
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
	agentDead          func(assignee string) bool
	survivingWorkGuard func(townRoot, beadID, holder string) error
	checkDuplicates    func(townRoot, beadID string, info *beadInfo) (*duplicateCandidate, []duplicateMatch, error)
	noteDispatched     func(townRoot string, candidate *duplicateCandidate)
	crossRigGuard      func(beadID, targetAgent, townRoot string) error

	// The target and its polecat.
	resolveSelf     func() (agentID, pane, hookRoot string, err error)
	resolveTarget   func(target string, opts ResolveTargetOptions) (*ResolvedTarget, error)
	spawnPolecat    func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error)
	admitPolecat    func(townRoot, rigName, beadID, operation string) (*polecatAdmissionHandle, polecatCapacitySnapshot, error)
	startSession    func(spawn *SpawnedPolecatInfo) (string, error)
	startDelayedDog func(dog *DogDispatchInfo) (string, error)
	cleanupSpawned  func(spawn *SpawnedPolecatInfo, rigName, convoyID string)

	// Reassignment away from a previous holder.
	requester          func() string
	notifyWitness      func(townRoot string, msg *mail.Message) error
	clearReassigned    func(townRoot, assignee string)
	unhook             func(townRoot, beadID string) error
	recordReassignment func(townRoot, beadID, from, to, requester string)

	// Convoy, formula and hook writes.
	trackedByConvoy    func(beadID string) string
	createConvoy       func(beadID, beadTitle string, owned bool, mergeStrategy, baseBranch, agent, formula string) (string, error)
	collectMolecules   func(info *beadInfo, beadID, townRoot string) ([]string, error)
	burnMolecules      func(molecules []string, beadID, townRoot string) error
	rigCommandVars     func(townRoot, rig string) []string
	priorAttempt       func(beadsDir, issueID string) []string
	cook               func(formulaName, workDir, townRoot string) error
	instantiateFormula func(ctx context.Context, formulaName, beadID, title, hookWorkDir, townRoot string, skipCook bool, extraVars []string) (*FormulaOnBeadResult, error)
	actor              func() string
	hookDir            func(townRoot, beadID, workDir string) string
	storeFields        func(townRoot, beadID string, updates beadFieldUpdates) error
	hook               func(beadID, targetAgent, hookDir, townRoot string) error
	clearOrphanLabels  func(townRoot, beadID, workDir string)
	updateAgentHook    func(agentID, beadID, workDir, townBeadsDir string)
	updateAgentMode    func(agentID, mode, workDir, townBeadsDir string)
	logFeed            func(eventType, actor string, payload map[string]interface{}) error
	enqueueNudge       func(townRoot, session string, n nudge.QueuedNudge) error

	// Undoing a partial sling.
	rollbackArtifacts func(spawn *SpawnedPolecatInfo, beadID, hookWorkDir, convoyID string)
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
	return &slingDeps{
		getenv:        os.Getenv,
		out:           os.Stdout,
		stdin:         os.Stdin,
		steps:         slingSteps,
		autoCommitOff: setBDAutoCommitOff,
		releaseSeat:   releasePoolSeatClaim,
		findTown:      findTownRoot,
		townOrEnv:     workspace.FindFromCwdOrError,

		resolvePRBranch:        resolvePRBranch,
		workflowTargetOverride: applyWorkflowStepTargetOverride,
		shouldDefer:            shouldDeferDispatch,
		isRigName:              IsRigName,
		idType:                 detectSchedulerIDType,
		batchSchedule:          runBatchSchedule,
		batchSling:             runBatchSling,
		rigFromBeadIDs:         resolveRigFromBeadIDs,
		scheduleBead:           scheduleBead,
		rigForBead:             resolveRigForBead,
		rigBeadsDir:            beads.ResolveRepoAliasBeadsDir,
		resolveFormula:         resolveFormula,
		slingFormula:           runSlingFormula,
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
		agentDead:          isHookedAgentDeadFn,
		survivingWorkGuard: reslingSurvivingWorkGuard,
		checkDuplicates:    checkSlingDuplicates,
		noteDispatched:     noteSlingCandidateDispatched,
		crossRigGuard:      checkCrossRigGuard,

		resolveSelf:     resolveSelfTarget,
		resolveTarget:   resolveTarget,
		spawnPolecat:    spawnPolecatForSling,
		admitPolecat:    acquirePolecatAdmissionFn,
		startSession:    func(s *SpawnedPolecatInfo) (string, error) { return s.StartSession() },
		startDelayedDog: func(d *DogDispatchInfo) (string, error) { return d.StartDelayedSession() },
		cleanupSpawned:  cleanupSpawnedPolecat,

		requester:          reassignRequester,
		notifyWitness:      sendWitnessShutdown,
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

		rollbackArtifacts: rollbackSlingArtifacts,
		restoreRawFields:  restoreRollbackRawWorkflowFieldsFromCurrent,
		restorePinned:     restorePinnedBead,

		sessionFromPane:   getSessionFromPane,
		ensureAgentReady:  ensureAgentReady,
		injectStartPrompt: injectStartPrompt,
	}
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
	return &slingRun{slingDeps: realSlingDeps(), opts: opts, townRoot: townRoot, townErr: err}
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
	_ = os.Setenv("BD_DOLT_AUTO_COMMIT", "off")
	return func() {
		if prev == "" {
			_ = os.Unsetenv("BD_DOLT_AUTO_COMMIT")
		} else {
			_ = os.Setenv("BD_DOLT_AUTO_COMMIT", prev)
		}
	}
}

// sendWitnessShutdown mails msg and waits for its notifications.
func sendWitnessShutdown(townRoot string, msg *mail.Message) error {
	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()
	return router.Send(msg)
}

// unhookFromPreviousOwner sets a force-reassigned bead back to open with no
// assignee.
func unhookFromPreviousOwner(townRoot, beadID string) error {
	return BdCmd("update", beadID, "--status=open", "--assignee=").
		Dir(beads.ResolveHookDir(townRoot, beadID, "")).
		WithAutoCommit().
		Run()
}
