package witness

import (
	"context"
	"time"

	"github.com/steveyegge/gastown/internal/guard"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// handlers carries the collaborators the witness handlers reach outside the
// process through: the refinery and mayor sessions, the gt CLI, git, and tmux.
//
// Every field is nil in production, meaning "use the real one", so
// newHandlers() is the production value and the exported handler functions
// run on it. A test builds its own *handlers with the fields it needs faked
// and calls the handler method on that value. Nothing here is package state:
// two parallel tests faking the same collaborator hold two instances and
// cannot see each other's fakes. These used to be package-level function
// variables that tests swapped and restored, which raced against every
// parallel test that read them (TestProcessDiscoveredCompletion_
// NudgeFailureDoesNotBlockMetadataClear swapped nudgeRefinery under
// t.Parallel while its neighbors called it).
type handlers struct {
	// nudgeRefineryFn wakes the refinery to check the merge queue.
	nudgeRefineryFn func(townRoot, rigName string) error
	// slotOpenRecoveryCheckFn runs `gt polecat check-recovery` for a polecat.
	slotOpenRecoveryCheckFn func(workDir, rigName, polecatName string) (string, error)
	// runSchedulerForSlotOpenFn gives the scheduler a chance to fill a freed slot.
	runSchedulerForSlotOpenFn func(townRoot string) (slotOpenSchedulerResult, error)
	// slotOpenDecisionFn decides whether a freed polecat slot can be reused.
	slotOpenDecisionFn func(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision
	// restartSessionExecFn runs the real `gt session restart`. Its default
	// panics under a test binary (gt-5itbt).
	restartSessionExecFn func(workDir, address string) error
	// hookHoldReasonFn reports whether a polecat's hooked work is held.
	hookHoldReasonFn func(bd *BdCli, workDir, rigName, polecatName string) (string, bool)
	// nukePolecatFn is the whole nuke the zombie archive path performs.
	nukePolecatFn func(bd *BdCli, workDir, rigName, polecatName string) error
	// nukeKillSessionFn kills a polecat's tmux session. Its default panics
	// under a test binary (gt-5itbt).
	nukeKillSessionFn func(sessionName string)
	// nukePolecatWorktreeFn runs `gt polecat nuke`. Its default panics under a
	// test binary (gt-5itbt).
	nukePolecatWorktreeFn func(workDir, address string) error
	// verifyCommitOnMainFn reports whether a polecat's commit is on the
	// default branch.
	verifyCommitOnMainFn func(workDir, rigName, polecatName string) guard.Result
	// verifyBranchAlreadyMergedFn reports whether a polecat's branch work has
	// landed, squash merges included.
	verifyBranchAlreadyMergedFn func(workDir, rigName, polecatName, hookBead string) (bool, error)
	// observeDoneIntentActivityFn is the stuck-in-done gate's activity probe.
	observeDoneIntentActivityFn func(t *tmux.Tmux, polecatName, sessionName, workDir string) RealActivity
	// restartStuckSessionFn is the restart the stuck-in-done gate performs.
	restartStuckSessionFn func(workDir, rigName, polecatName string) error
	// neverHeartbeatedLivenessFn gathers the never-heartbeated rule's evidence.
	neverHeartbeatedLivenessFn func(t *tmux.Tmux, townRoot, rigName, polecatName, sessionName string, graceDeadline, now time.Time) neverHeartbeatedEvidence
	// survivingWorkForBeadFn is the shared surviving-work predicate.
	survivingWorkForBeadFn func(workDir, rigName, beadID string) (string, error)
	// sleepFn waits out a settle delay (the composer recheck).
	sleepFn func(time.Duration)
	// notifier sends the handlers' mail and escalations; nil means gt run
	// from the town root.
	notifier notify.Notifier
}

// notify returns the handlers' Notifier, or gt run from townRoot.
func (h *handlers) notify(townRoot string) notify.Notifier {
	if h.notifier != nil {
		return h.notifier
	}
	return &notify.CLI{Dir: townRoot}
}

// newHandlers returns the production handlers: every collaborator real.
func newHandlers() *handlers { return &handlers{} }

func (h *handlers) nudgeRefinery(townRoot, rigName string) error {
	if h.nudgeRefineryFn != nil {
		return h.nudgeRefineryFn(townRoot, rigName)
	}
	return _nudgeRefinery(townRoot, rigName)
}

func (h *handlers) slotOpenRecoveryCheck(workDir, rigName, polecatName string) (string, error) {
	if h.slotOpenRecoveryCheckFn != nil {
		return h.slotOpenRecoveryCheckFn(workDir, rigName, polecatName)
	}
	return defaultSlotOpenRecoveryCheck(workDir, rigName, polecatName)
}

func (h *handlers) runSchedulerForSlotOpen(townRoot string) (slotOpenSchedulerResult, error) {
	if h.runSchedulerForSlotOpenFn != nil {
		return h.runSchedulerForSlotOpenFn(townRoot)
	}
	return defaultRunSchedulerForSlotOpen(townRoot)
}

func (h *handlers) slotOpenDecisionForNotify(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
	if h.slotOpenDecisionFn != nil {
		return h.slotOpenDecisionFn(workDir, townRoot, rigName, polecatName, exitType)
	}
	return slotOpenDecision(workDir, townRoot, rigName, polecatName, exitType)
}

func (h *handlers) restartSessionExec(workDir, address string) error {
	if h.restartSessionExecFn != nil {
		return h.restartSessionExecFn(workDir, address)
	}
	return defaultRestartSessionExec(workDir, address)
}

func (h *handlers) hookHoldReason(bd *BdCli, workDir, rigName, polecatName string) (string, bool) {
	if h.hookHoldReasonFn != nil {
		return h.hookHoldReasonFn(bd, workDir, rigName, polecatName)
	}
	return readHookHold(bd, workDir, rigName, polecatName)
}

func (h *handlers) nukePolecat(bd *BdCli, workDir, rigName, polecatName string) error {
	if h.nukePolecatFn != nil {
		return h.nukePolecatFn(bd, workDir, rigName, polecatName)
	}
	return h.nukePolecatImpl(bd, workDir, rigName, polecatName)
}

func (h *handlers) nukeKillSessionExec(sessionName string) {
	if h.nukeKillSessionFn != nil {
		h.nukeKillSessionFn(sessionName)
		return
	}
	defaultNukeKillSession(sessionName)
}

func (h *handlers) nukePolecatWorktreeExec(workDir, address string) error {
	if h.nukePolecatWorktreeFn != nil {
		return h.nukePolecatWorktreeFn(workDir, address)
	}
	return defaultNukePolecatWorktree(workDir, address)
}

func (h *handlers) verifyCommitOnMain(workDir, rigName, polecatName string) guard.Result {
	if h.verifyCommitOnMainFn != nil {
		return h.verifyCommitOnMainFn(workDir, rigName, polecatName)
	}
	return _verifyCommitOnMain(workDir, rigName, polecatName)
}

func (h *handlers) verifyBranchAlreadyMerged(workDir, rigName, polecatName, hookBead string) (bool, error) {
	if h.verifyBranchAlreadyMergedFn != nil {
		return h.verifyBranchAlreadyMergedFn(workDir, rigName, polecatName, hookBead)
	}
	return h._verifyBranchAlreadyMerged(workDir, rigName, polecatName, hookBead)
}

func (h *handlers) observeDoneIntentActivity(t *tmux.Tmux, polecatName, sessionName, workDir string) RealActivity {
	if h.observeDoneIntentActivityFn != nil {
		return h.observeDoneIntentActivityFn(t, polecatName, sessionName, workDir)
	}
	return ObserveRealActivity(t, polecatName, sessionName, workDir)
}

func (h *handlers) restartStuckSession(workDir, rigName, polecatName string) error {
	if h.restartStuckSessionFn != nil {
		return h.restartStuckSessionFn(workDir, rigName, polecatName)
	}
	return h.restartPolecatSession(workDir, rigName, polecatName)
}

func (h *handlers) neverHeartbeatedLiveness(t *tmux.Tmux, townRoot, rigName, polecatName, sessionName string, graceDeadline, now time.Time) neverHeartbeatedEvidence {
	if h.neverHeartbeatedLivenessFn != nil {
		return h.neverHeartbeatedLivenessFn(t, townRoot, rigName, polecatName, sessionName, graceDeadline, now)
	}
	return assessNeverHeartbeatedLiveness(t, townRoot, rigName, polecatName, sessionName, graceDeadline, now)
}

func (h *handlers) survivingWorkForBead(workDir, rigName, beadID string) (string, error) {
	if h.survivingWorkForBeadFn != nil {
		return h.survivingWorkForBeadFn(workDir, rigName, beadID)
	}
	return defaultSurvivingWorkForBead(workDir, rigName, beadID)
}

func (h *handlers) sleep(d time.Duration) {
	if h.sleepFn != nil {
		h.sleepFn(d)
		return
	}
	time.Sleep(d)
}

// supervisor returns the lifecycle supervisor the witness's restarts and
// nuke kills go through (gt-4k3fj.3). The patrol scan still runs in its own
// process until the witness becomes a daemon tick (gt-4k3fj.6); the
// supervisor's guards are files, so they hold here as in the daemon.
func (h *handlers) supervisor(townRoot, workDir string) *supervisor.Supervisor {
	return supervisor.New(supervisor.Options{
		TownRoot: townRoot,
		Tmux:     nukeKiller{h},
		Restart: func(seat supervisor.Seat) error {
			return h.restartSessionExec(workDir, seat.Rig+"/"+seat.Name)
		},
		Escalate: func(seat supervisor.Seat, line string) {
			key := "restart-budget:" + supervisor.IntentSeat(seat).String()
			_ = h.notify(townRoot).Escalate(context.Background(), notify.Escalation{
				Severity:    "HIGH",
				Description: "restart budget exhausted: " + supervisor.IntentSeat(seat).String(),
				Reason:      line,
				Fingerprint: key,
			})
		},
	})
}

// nukeKiller is the supervisor's session killer for witness hosts: the nuke
// executor (graceful Ctrl-C, then kill), faked by tests.
type nukeKiller struct{ h *handlers }

func (k nukeKiller) KillSessionWithProcesses(name string) error {
	k.h.nukeKillSessionExec(name)
	return nil
}
