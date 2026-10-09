package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/promote"
	"github.com/steveyegge/gastown/internal/util"
)

// The tier_sweep job runs scripts/tier-sweep.sh against origin/main on an
// interval: the tiers `make gate` does not run, which landings gate only for
// the submissions that can move them (gt-vsct7.5). It replaces the overseer's
// hourly :37 cron, so it is silent while green and costs nothing while main is
// unchanged.
//
// A cycle checks origin/main out detached in one fixed private worktree outside
// the town root (landingWorkRoot), runs the shell tier every time and the
// integration and race tiers on even local hours. It runs no `make gate`:
// every landing already gates the merged tree (gt-vsct7.8).
//
// A cycle whose config sets runner_idle_command waits, bounded by
// runner_idle_wait, for the CI runner to report idle before the integration
// stage takes the container-gate slot, and starts it anyway when the cap runs
// out, logging that it did so under load (gt-5ejux).
//
// Three things it deliberately does not do. It does not revert a red main: a
// sweep RED files a bead, and the landing worker's red-main owner decides the
// revert. It does not write `red-main` beads: that label's close-on-green is
// the post-land runner's, so a sweep bead under it would be closed by an
// unrelated green landing. It does not hold a gate-class slot: the sweep's own
// role is <rig>/tier-sweep, and the script's inner `gt slot run` calls inherit
// that role and ride the one hold reentrantly instead of taking a slot each.

const (
	// defaultTierSweepInterval is how long one cycle holds this job off.
	defaultTierSweepInterval = time.Hour

	// tierSweepShellBudget bounds the shell stage, and
	// tierSweepIntegrationBudget the integration stage (integration and race in
	// one run). They are compiled in named constants: a stage that outlives its
	// budget is RED with reason timeout, never GREEN.
	tierSweepShellBudget       = 20 * time.Minute
	tierSweepIntegrationBudget = 90 * time.Minute

	// tierSweepRunBudget bounds one whole cycle: both stages, the container-gate
	// slot wait included.
	tierSweepRunBudget = 2 * time.Hour

	// tierSweepRunnerIdlePoll is how often the pre-slot runner-idle wait asks
	// the configured runner_idle_command again, and tierSweepRunnerIdleGrace
	// is how long one poll's shell may outlive its output pipe before it is
	// cut off.
	tierSweepRunnerIdlePoll  = 15 * time.Second
	tierSweepRunnerIdleGrace = 10 * time.Second

	// defaultTierSweepRunnerIdleWait caps that wait when runner_idle_wait is
	// unset.
	defaultTierSweepRunnerIdleWait = 10 * time.Minute

	// tierSweepLogTailLines is how much of a stage's output the daemon log
	// keeps. The whole log is written to the state file's log path.
	tierSweepLogTailLines = 20

	// tierSweepScript is the tier entry point inside the checked-out tree. The
	// tree ships its own copy, so a sweep never runs a script main does not
	// have.
	tierSweepScript = "scripts/tier-sweep.sh"

	// tierSweepRedLabel marks a bead the sweep filed for one failing unit. It
	// is deliberately not landworker.LabelRedMain: the post-land runner's
	// close-on-green would close a sweep bead on an unrelated landing.
	tierSweepRedLabel = "tier-sweep-red"

	// tierSweepNoName stands in for the failing unit when a RED summary named
	// none.
	tierSweepNoName = "(no failing unit named)"
)

// The tier verdicts, the words the script's summary line uses.
const (
	tierSweepGreen = "GREEN"
	tierSweepRed   = "RED"
)

// tierSweepState is one rig's sweep record, .runtime/tier-sweep/<rig>.json.
// gt report --hour reads the same file (internal/cmd/report.go).
type tierSweepState struct {
	LastRun time.Time `json:"last_run"`
	// LastSHA is the origin/main tip the last cycle saw, green or not.
	LastSHA string `json:"last_sha"`
	// LastGreenSHA is the tip of the last cycle that covered every tier the
	// sweep is scheduled to cover and was green throughout. It is the skip
	// key: while origin/main equals it there is nothing new to sweep.
	LastGreenSHA string `json:"last_green_sha"`
	// Tiers is the last verdict per tier, carried forward for a tier a cycle
	// did not run (the integration tiers on an odd hour).
	Tiers map[string]tierSweepTierResult `json:"tiers"`
}

// tierSweepTierResult is one tier's last verdict.
type tierSweepTierResult struct {
	Verdict     string   `json:"verdict"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	FailedNames []string `json:"failed_names"`
	// Log is the file the stage's whole output was written to.
	Log string `json:"log,omitempty"`
}

func tierSweepStatePath(townRoot, rig string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "tier-sweep", rig+".json")
}

// readTierSweepState reads one rig's record. A rig with no record is an empty
// state, not an error: nothing has swept it yet.
func readTierSweepState(townRoot, rig string) (tierSweepState, error) {
	var st tierSweepState
	data, err := os.ReadFile(tierSweepStatePath(townRoot, rig))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return tierSweepState{}, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return tierSweepState{}, fmt.Errorf("parse %s: %w", tierSweepStatePath(townRoot, rig), err)
	}
	return st, nil
}

func writeTierSweepState(townRoot, rig string, st tierSweepState) error {
	return atomicfile.EnsureDirAndWriteJSON(tierSweepStatePath(townRoot, rig), st)
}

func tierSweepConfig(config *DaemonPatrolConfig) *TierSweepConfig {
	if config == nil || config.Patrols == nil {
		return nil
	}
	return config.Patrols.TierSweep
}

func tierSweepInterval(config *DaemonPatrolConfig) time.Duration {
	if c := tierSweepConfig(config); c != nil && c.IntervalStr != "" {
		if d, err := time.ParseDuration(c.IntervalStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultTierSweepInterval
}

// tierSweepRigs is the rigs a cycle sweeps: the configured ones, else
// ["gastown"] (the one rig the overseer's cron swept), within the known rigs.
func tierSweepRigs(config *DaemonPatrolConfig, known []string) []string {
	want := []string{"gastown"}
	if c := tierSweepConfig(config); c != nil && len(c.Rigs) > 0 {
		want = c.Rigs
	}
	var out []string
	for _, r := range known {
		for _, w := range want {
			if r == w {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// tierSweepRunnerIdleCommand is the configured CI-runner idle probe, or "" when
// the sweep waits for nothing.
func tierSweepRunnerIdleCommand(config *DaemonPatrolConfig) string {
	if c := tierSweepConfig(config); c != nil {
		return strings.TrimSpace(c.RunnerIdleCommand)
	}
	return ""
}

// tierSweepRunnerIdleWait is how long the pre-slot wait may poll, defaulting to
// 10m and reading a value that does not parse, or is not positive, as unset.
func tierSweepRunnerIdleWait(config *DaemonPatrolConfig) time.Duration {
	if c := tierSweepConfig(config); c != nil && c.RunnerIdleWaitStr != "" {
		if d, err := time.ParseDuration(c.RunnerIdleWaitStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultTierSweepRunnerIdleWait
}

// tierSweepCoversRig reports whether the tier sweep this daemon runs sweeps
// rig, which is what makes the sweep, not the post-land verdict, the owner of
// the rig's GitHub promotion (gt-fn9e6.38). The patrol being off counts as no
// sweep: a disabled sweep that still held the promotion would leave the rig
// promoting nowhere.
func (d *Daemon) tierSweepCoversRig(rig string) bool {
	return tierSweepCovers(d.patrolConfig, d.disabledPatrols, d.getKnownRigs(), rig)
}

// tierSweepCovers is the one predicate for "the sweep owns this rig": the
// patrol is on under the town's disabled list, and rig is one the sweep runs.
// A caller outside the daemon (TierSweepCoverageFor, and through it gt promote)
// shares it rather than restating the rule, so the coverage a promotion is
// guarded by cannot drift from the sweep that performs it (gt-qk0pi).
func tierSweepCovers(cfg *DaemonPatrolConfig, disabled map[string]bool, known []string, rig string) bool {
	if disabled["tier_sweep"] || !IsPatrolEnabled(cfg, "tier_sweep") {
		return false
	}
	for _, r := range tierSweepRigs(cfg, known) {
		if r == rig {
			return true
		}
	}
	return false
}

// TierSweepCoverage is what the town's tier sweep owns for one rig, as a
// caller outside the daemon reads it before publishing that rig's commit.
type TierSweepCoverage struct {
	// Covered is true when the town's tier_sweep patrol runs for the rig.
	Covered bool
	// LastGreenSHA is the rig's last fully green sweep, empty until one has
	// run: while the sweep owns the rig, no other commit is promotable
	// (gt-qk0pi).
	LastGreenSHA string
}

// TierSweepCoverageFor reads what the tier sweep configured for townRoot owns
// for rig: coverage from the patrol config and the town's disabled patrols,
// and the last green commit from the rig's record under .runtime/tier-sweep.
// An unparseable patrol config is an error rather than "not covered": the
// caller refuses to publish on this read, so it must not read a broken config
// as a town that promotes freely.
func TierSweepCoverageFor(townRoot, rig string) (TierSweepCoverage, error) {
	cfg, err := ReadPatrolConfig(townRoot)
	if err != nil {
		return TierSweepCoverage{}, fmt.Errorf("reading the daemon patrol config: %w", err)
	}
	if !tierSweepCovers(cfg, loadDisabledPatrolsFromTownSettings(townRoot), knownRigNames(townRoot), rig) {
		return TierSweepCoverage{}, nil
	}
	st, err := readTierSweepState(townRoot, rig)
	if err != nil {
		return TierSweepCoverage{}, err
	}
	return TierSweepCoverage{Covered: true, LastGreenSHA: st.LastGreenSHA}, nil
}

// knownRigNames is the town's rig list, the set tierSweepRigs intersects its
// configured rigs with. A list that cannot be read is empty, which is what the
// daemon's own read gives it: the sweep then covers no rig.
func knownRigNames(townRoot string) []string {
	parsed, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(parsed.Rigs))
	for name := range parsed.Rigs {
		names = append(names, name)
	}
	return names
}

// tierSweepPromoterFor resolves rig's promotion owner: a test's seam when it
// set one, else the daemon's own builder over the rig's repository and forgejo
// block. Nil means the rig has no promote_target and does not promote.
func (d *Daemon) tierSweepPromoterFor(rigName, repo string) *promote.Promoter {
	build := d.tierSweepSeams.promote
	if build == nil {
		build = d.tierSweepPromoter
	}
	return build(rigName, repo)
}

// tierSweepPromote advances rig's GitHub main to sha, the fully green sweep's
// commit, through the same promotion owner the post-land verdict uses: one
// push path, one lock, one record (gt-fn9e6.38). A rig with no promote_target
// has no promoter and nothing happens; a failure is recorded in the rig's
// main state, exactly as it is for a green post-land verdict.
func (d *Daemon) tierSweepPromote(rigName, repo, sha string) {
	if p := d.tierSweepPromoterFor(rigName, repo); p != nil {
		d.tierSweepPromoteWith(rigName, p, sha)
	}
}

// tierSweepRetryOwedPromotion retries the promotion a green sweep owed, from
// the cycle that skipped on unchanged main. Such a cycle runs no tier at all,
// so a promotion that failed when the sweep first went green - an unreachable
// target, a rejected push, a lock another promoter held - or that the rig only
// gained afterwards would wait for main to move, indefinitely on a quiet rig.
// The rig's own record names the commit the target holds, so this promotes
// exactly when it trails the sweep's LastGreenSHA; a rig that does not promote
// and a record already at the green commit both do nothing (gt-fn9e6.52).
func (d *Daemon) tierSweepRetryOwedPromotion(rigName, repo, sha string) {
	if sha == "" {
		// No green commit is recorded, so no promotion can be owed. The skip
		// guard already refuses an empty LastGreenSHA; this keeps the retry
		// safe on its own terms.
		return
	}
	state := fileMainState{path: RedMainStatePath(d.config.TownRoot, rigName)}
	st, err := state.Load()
	if err != nil {
		d.logger.Printf("tier_sweep: %s: reading the main state: %v", rigName, err)
		return
	}
	if st.State.LastPromoted == sha {
		return
	}
	p := d.tierSweepPromoterFor(rigName, repo)
	if p == nil {
		return
	}
	d.logger.Printf("tier_sweep: %s: promotion of %s is still owed; retrying it from the unchanged-main skip", rigName, shortSHA(sha))
	d.tierSweepPromoteWith(rigName, p, sha)
}

// tierSweepPromoteWith runs one promotion over an already-resolved promoter
// and records its outcome in the rig's main state. A failure stays recorded,
// exactly as a green post-land verdict's does.
func (d *Daemon) tierSweepPromoteWith(rigName string, p *promote.Promoter, sha string) {
	state := fileMainState{path: RedMainStatePath(d.config.TownRoot, rigName)}
	st, err := state.Load()
	if err != nil {
		d.logger.Printf("tier_sweep: %s: reading the main state: %v", rigName, err)
		return
	}
	st.State = p.Promote(st.State, sha)
	if err := state.Save(st); err != nil {
		d.logger.Printf("tier_sweep: %s: saving the promotion state: %v", rigName, err)
	}
}

// tierSweepStage is one script invocation of a cycle: the tiers the script is
// asked for, its budget, and whether it needs the container-gate slot.
type tierSweepStage struct {
	// tiers are the script's argv tiers, e.g. ["shell"] or
	// ["integration", "race"]. The run's verdicts are keyed by these.
	tiers  []string
	budget time.Duration
	// slot is true when the stage starts containers and so must run under the
	// town's container-gate slot.
	slot bool
	// role is the slot role the stage holds, once, for every tier it runs.
	role string
	// step names the gate step: "test" for a slot stage, because
	// land.WithSlot holds the slot around that name and no other.
	step string
}

// command is the shell command the stage runs in the checked-out tree.
func (s tierSweepStage) command() string {
	return "bash " + tierSweepScript + " " + strings.Join(s.tiers, " ")
}

// tierSweepStageResult is what one stage run produced.
type tierSweepStageResult struct {
	// output is the stage's combined stdout and stderr.
	output string
	// exitCode is the stage command's exit code; -1 when it never ran.
	exitCode int
	// timedOut is true when the stage's own budget killed it.
	timedOut bool
	// ran is false when the stage never started (the slot wait failed, or the
	// gate could not run it): nothing was learned about the tree.
	ran bool
	// err is why a stage that did not run never started.
	err error
}

// tierSweepSeams replace the sweep's outside effects in tests; the zero value
// is production. Each is resolved at use, so a test replaces only the one it
// needs.
type tierSweepSeams struct {
	// mainSHA fetches and reads origin/main in the rig repository.
	mainSHA func(ctx context.Context, repo string) (string, error)
	// worktree checks sha out detached at dir, returning a cleanup.
	worktree func(ctx context.Context, repo, dir, sha string) (func(), error)
	// run executes one stage in dir.
	run func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult
	// runnerIdle runs one runner_idle_command poll in dir: nil means the CI
	// runner is idle, any other error means it is busy.
	runnerIdle func(ctx context.Context, dir, command string) error
	// idleSleep is the pause between two runner-idle polls, and the seam a
	// test replaces so its clock, not the wall, decides the wait.
	idleSleep func(ctx context.Context, d time.Duration) error
	// beads opens the rig's writable bead store.
	beads func(rig string) tierSweepBeadStore
	// promote builds the rig's GitHub promotion owner, or nil when the rig
	// does not promote. Nil resolves to the daemon's own builder over the
	// rig's repository and forgejo block.
	promote func(rig, repo string) *promote.Promoter
}

// tierSweepBeadStore is the bead store the sweep files and closes beads
// through.
type tierSweepBeadStore interface {
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	AddComment(id, text string) error
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	CloseWithReason(reason string, ids ...string) error
}

// triggerTierSweep runs one tier_sweep cycle on its own goroutine when the job
// is due: a cycle runs the shell tier and, on even hours, the integration
// suite, so inline it would stall the heartbeat.
func (d *Daemon) triggerTierSweep() {
	if !d.isPatrolActive("tier_sweep") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "tier_sweep", time.Time{}, now, tierSweepInterval(d.patrolConfig))
	if !dec.due {
		return
	}
	if !d.tierSweepRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("tier_sweep: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.tierSweepRunning.Store(false)
		if !d.runTierSweep() {
			// Nothing accomplished: leave the gate open so the next heartbeat
			// retries.
			return
		}
		if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", d.clk().Now()); err != nil {
			d.logger.Printf("tier_sweep: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// runTierSweep is one cycle over every configured rig. It reports whether the
// cycle reached a verdict, as opposed to deferring with nothing accomplished.
func (d *Daemon) runTierSweep() bool {
	if d.upgradeRestartPending.Load() {
		d.logger.Printf("tier_sweep: an upgrade restart is pending; skipping this cycle")
		return true
	}
	rigs := tierSweepRigs(d.patrolConfig, d.getKnownRigs())
	if len(rigs) == 0 {
		d.logger.Printf("tier_sweep: no rig configured to sweep; nothing to do")
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), tierSweepRunBudget)
	defer cancel()

	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()

	verdict := true
	for _, rig := range rigs {
		if ctx.Err() != nil {
			return false
		}
		if !d.runTierSweepRig(ctx, cycle, rig, d.clk().Now()) {
			verdict = false
		}
	}
	return verdict
}

// runTierSweepRig sweeps one rig. It reports whether the rig reached a verdict;
// a rig whose origin/main or worktree could not be read deferred and is retried
// on the next heartbeat.
func (d *Daemon) runTierSweepRig(ctx context.Context, cycle *dogCycle, rig string, now time.Time) bool {
	seams := d.tierSweepSeams
	repo := filepath.Join(d.config.TownRoot, rig, ".repo.git")
	if _, err := os.Stat(repo); err != nil {
		cycle.skipStep(rig, "no repository at "+repo)
		d.logger.Printf("tier_sweep: %s: no repository at %s; nothing to sweep", rig, repo)
		return true
	}

	mainSHA := seams.mainSHA
	if mainSHA == nil {
		mainSHA = tierSweepMainSHA
	}
	sha, err := mainSHA(ctx, repo)
	if err != nil {
		cycle.skipStep(rig, "cannot read origin/main: "+err.Error())
		d.logger.Printf("tier_sweep: %s: cannot read origin/main (%v); deferring", rig, err)
		return false
	}

	state, err := readTierSweepState(d.config.TownRoot, rig)
	if err != nil {
		// A corrupt record is replaced rather than trusted: the write below
		// repairs it, and refusing to sweep would be silent starvation.
		d.logger.Printf("tier_sweep: %s: reading the sweep record: %v; replacing it", rig, err)
		state = tierSweepState{}
	}
	if sha != "" && sha == state.LastGreenSHA {
		// Nothing new to sweep, but the promotion the last green sweep named
		// may still be outstanding: retrying it here is the only place a quiet
		// rig would ever retry it (gt-fn9e6.52).
		d.tierSweepRetryOwedPromotion(rig, repo, state.LastGreenSHA)
		cycle.skipStep(rig, "origin/main "+shortSHA(sha)+" unchanged since the last green sweep")
		d.logger.Printf("tier_sweep: %s: origin/main %s unchanged since the last green sweep; skipping", rig, shortSHA(sha))
		return true
	}
	return d.tierSweepRigCycle(ctx, cycle, rig, repo, sha, state, now)
}

// tierSweepStages is the stages one cycle runs for rig: the shell tier every
// time, and the integration suite (integration and race) on even local hours.
// full reports whether the cycle covers every tier the sweep is scheduled to
// cover, which only an even-hour cycle does.
func tierSweepStages(rig string, now time.Time) (stages []tierSweepStage, full bool) {
	stages = []tierSweepStage{{
		tiers:  []string{"shell"},
		budget: tierSweepShellBudget,
		step:   "shell",
	}}
	if now.Hour()%2 == 0 {
		stages = append(stages, tierSweepStage{
			tiers:  []string{"integration", "race"},
			budget: tierSweepIntegrationBudget,
			slot:   true,
			role:   rig + "/tier-sweep",
			step:   "test",
		})
	}
	return stages, len(stages) > 1
}

// tierSweepTierList names the tiers a cycle's stages will run, in the order it
// runs them, for the "sweep started" line: "shell", or
// "shell, integration, race" on an even hour.
func tierSweepTierList(stages []tierSweepStage) string {
	var tiers []string
	for _, st := range stages {
		tiers = append(tiers, st.tiers...)
	}
	return strings.Join(tiers, ", ")
}

// tierSweepIdleWait is what the pre-slot runner-idle wait did, for the two
// places that report it: the stage's log line and the cycle's verdict summary.
type tierSweepIdleWait struct {
	// configured is true when runner_idle_command is set. A cycle with no such
	// key waited for nothing and logs nothing about a wait.
	configured bool
	// waited is how long the wait polled before the runner reported idle or
	// the cap ran out.
	waited time.Duration
	// underLoad is true when the cap ran out with the runner still busy: the
	// stage starts anyway, beside that load.
	underLoad bool
	// lastErr is the last busy poll's error, which the under-load line names so
	// a probe that cannot run at all is readable as such.
	lastErr error
}

// tierSweepAwaitRunnerIdle is the bounded wait that runs before a stage which
// takes the container-gate slot: it polls the configured runner_idle_command
// every tierSweepRunnerIdlePoll until the command exits 0, the runner_idle_wait
// cap runs out, or ctx is canceled (gt-5ejux). It runs before the stage, so the
// slot is never held while the sweep waits, and a busy CI runner delays the
// stage instead of stalling it beside the load that is already there. A
// canceled ctx returns its error rather than the cap's verdict, so a daemon
// drain is never held by the wait.
func (d *Daemon) tierSweepAwaitRunnerIdle(ctx context.Context, dir string) (wait tierSweepIdleWait, err error) {
	command := tierSweepRunnerIdleCommand(d.patrolConfig)
	if command == "" {
		// No probe configured: no wait at all, the behavior without the key.
		return tierSweepIdleWait{}, nil
	}
	poll := d.tierSweepSeams.runnerIdle
	if poll == nil {
		poll = tierSweepRunnerIdle
	}
	pause := d.tierSweepSeams.idleSleep
	if pause == nil {
		pause = sleepCtx
	}
	wait.configured = true
	start := d.clk().Now()
	defer func() { wait.waited = d.clk().Now().Sub(start) }()
	deadline := start.Add(tierSweepRunnerIdleWait(d.patrolConfig))
	for {
		if err := ctx.Err(); err != nil {
			return wait, err
		}
		busy := poll(ctx, dir, command)
		if busy == nil {
			return wait, nil
		}
		wait.lastErr = busy
		// Checked again here so a poll the cancel ended is read as the cancel,
		// not as the runner being busy at the cap.
		if err := ctx.Err(); err != nil {
			return wait, err
		}
		remaining := deadline.Sub(d.clk().Now())
		if remaining <= 0 {
			// The cap ran out with the runner still busy: the stage starts
			// anyway, and the cycle records that it ran under load.
			wait.underLoad = true
			return wait, nil
		}
		if remaining > tierSweepRunnerIdlePoll {
			remaining = tierSweepRunnerIdlePoll
		}
		if err := pause(ctx, remaining); err != nil {
			return wait, err
		}
	}
}

// tierSweepIdleLine renders one wait for the daemon log: the idle report, or
// that the cap ran out and the stage starts under load.
func tierSweepIdleLine(w tierSweepIdleWait) string {
	if !w.underLoad {
		return fmt.Sprintf("the CI runner reported idle after %s", w.waited.Round(time.Second))
	}
	line := fmt.Sprintf("the CI runner was still busy after %s; starting it under load", w.waited.Round(time.Second))
	if w.lastErr != nil {
		line += fmt.Sprintf(" (last check: %v)", w.lastErr)
	}
	return line
}

// tierSweepRunnerIdle runs one runner_idle_command poll in dir: exit 0 means
// the CI runner is idle, anything else — including a command that cannot start,
// which reads as busy rather than as a verdict about the tree — means it is
// busy.
func tierSweepRunnerIdle(ctx context.Context, dir, command string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // G204: the operator's own configured command
	cmd.Dir = dir
	// Its own process group, so a canceled poll kills what the command
	// started, not only the shell.
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = tierSweepRunnerIdleGrace
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

// sleepCtx waits d, or returns ctx's error when the wait is canceled first, so
// the runner-idle wait's own pause honors a cancel.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// tierSweepRigCycle runs the stages, writes the record, and files or closes the
// beads the verdicts imply.
func (d *Daemon) tierSweepRigCycle(ctx context.Context, cycle *dogCycle, rig, repo, sha string, state tierSweepState, now time.Time) bool {
	seams := d.tierSweepSeams

	dir, err := d.tierSweepWorkDir(rig)
	if err != nil {
		cycle.skipStep(rig, "work root: "+err.Error())
		d.logger.Printf("tier_sweep: %s: %v; deferring", rig, err)
		return false
	}
	worktree := seams.worktree
	if worktree == nil {
		worktree = tierSweepCheckout
	}
	cleanup, err := worktree(ctx, repo, dir, sha)
	if err != nil {
		cycle.skipStep(rig, "cannot check out "+shortSHA(sha)+": "+err.Error())
		d.logger.Printf("tier_sweep: %s: cannot check out %s: %v; deferring", rig, shortSHA(sha), err)
		return false
	}
	defer cleanup()

	stages, full := tierSweepStages(rig, now)
	// The cycle's start, logged before the first stage runs so the dashboard's
	// Tier sweeps pane reads a sweep as running from its true beginning, over
	// the stages it will run, instead of inferring it from the shell stage
	// finishing (gt-rntre). Nothing reaches here for a skipped cycle.
	d.logger.Printf("tier_sweep: %s: sweep started %s (%s)", rig, shortSHA(sha), tierSweepTierList(stages))
	run := seams.run
	if run == nil {
		run = d.tierSweepRunStage
	}
	results := map[string]tierSweepTierResult{}
	// loaded names the tiers whose stage waited its runner-idle cap out and ran
	// anyway, for the cycle's verdict summary.
	loaded := map[string]bool{}
	deferred := false
	for _, st := range stages {
		if st.slot {
			// The runner-idle wait runs here, before the stage: land.WithSlot
			// takes the container-gate slot inside the stage run below, so the
			// wait is never spent holding it (gt-5ejux).
			wait, err := d.tierSweepAwaitRunnerIdle(ctx, dir)
			if err != nil {
				// The wait honors the cycle's context, so this is the cycle
				// being canceled under it, not a verdict about the tree: the
				// stage does not start.
				d.logger.Printf("tier_sweep: %s: %s stage: the runner-idle wait stopped (%v); deferring", rig, st.tiers[0], err)
				deferred = true
				continue
			}
			if wait.configured {
				d.logger.Printf("tier_sweep: %s: %s stage: %s", rig, st.tiers[0], tierSweepIdleLine(wait))
			}
			if wait.underLoad {
				for _, tier := range st.tiers {
					loaded[tier] = true
				}
			}
		}
		// The stage's own start, so `gt tail` shows a slow or hung stage on the
		// line that names it (gt-iqzr0).
		stageStart := d.clk().Now()
		res := run(ctx, dir, st)
		stageElapsed := d.clk().Now().Sub(stageStart)
		if !res.ran {
			// The stage never started (a slot the sweep could not take, say):
			// nothing was learned about the tree, so the cycle is incomplete
			// and the next heartbeat retries it.
			d.logger.Printf("tier_sweep: %s: %s stage did not run (%v); deferring", rig, st.tiers[0], res.err)
			deferred = true
			continue
		}
		logPath := d.tierSweepWriteLog(rig, st.tiers[0], res.output)
		reported := parseTierSweepSummaries(res.output)
		for _, tier := range st.tiers {
			ts, ok := reported[tier]
			if !ok {
				// No summary line: the script was killed, regressed its output,
				// or never reached the tier. That is RED, never GREEN, and the
				// reason stands in for the failing unit the script did not name.
				reason := fmt.Sprintf("no verdict line (exit %d)", res.exitCode)
				if res.timedOut {
					reason = "timeout"
				} else if res.exitCode == 0 {
					reason = "no verdict line"
				}
				ts = tierSweepTierResult{Verdict: tierSweepRed, Failed: 1, FailedNames: []string{reason}}
				d.logger.Printf("tier_sweep: %s: %s %s: %s", rig, tier, tierSweepRed, reason)
			}
			ts.Log = logPath
			results[tier] = ts
		}
		d.logger.Printf("tier_sweep: %s: %s finished (exit %d) in %s; last lines:\n%s",
			rig, st.tiers[0], res.exitCode, stageElapsed.Round(time.Second), lastLines(res.output, tierSweepLogTailLines))
	}

	if len(results) == 0 {
		// Nothing ran at all: the record would be a lie.
		return false
	}

	if state.Tiers == nil {
		state.Tiers = map[string]tierSweepTierResult{}
	}
	for tier, ts := range results {
		state.Tiers[tier] = ts
	}
	state.LastRun, state.LastSHA = now, sha
	// LastGreenSHA advances only on a cycle that covered every tier the sweep
	// is scheduled to cover: a shell-only green says nothing about the
	// integration tiers, and advancing on it would skip them forever on a
	// quiet repo.
	green := !deferred && full && tierSweepAllGreen(results, stages)
	if green {
		state.LastGreenSHA = sha
	}
	if err := writeTierSweepState(d.config.TownRoot, rig, state); err != nil {
		d.logger.Printf("tier_sweep: %s: writing the sweep record: %v", rig, err)
		return false
	}
	// A cycle that covered every tier green is the verdict that advances the
	// rig's GitHub main (gt-fn9e6.38): it is the only check that saw every
	// tier at this commit. A red or partial cycle promotes nothing.
	if green {
		d.tierSweepPromote(rig, repo, sha)
	}
	cycle.closeStep(rig)
	d.logger.Printf("tier_sweep: %s: swept %s (shell %s%s) in %s", rig, shortSHA(sha),
		results["shell"].Verdict, tierSweepVerdictSummary(results, stages, loaded),
		d.clk().Now().Sub(now).Round(time.Second))

	d.tierSweepRecordBeads(rig, sha, results)
	return !deferred
}

// tierSweepAllGreen reports whether every tier of every stage came back GREEN,
// with no stage left unrun.
func tierSweepAllGreen(results map[string]tierSweepTierResult, stages []tierSweepStage) bool {
	for _, st := range stages {
		for _, tier := range st.tiers {
			if results[tier].Verdict != tierSweepGreen {
				return false
			}
		}
	}
	return true
}

// tierSweepVerdictSummary renders the non-shell verdicts for the cycle log. A
// tier in loaded ran beside a busy CI runner, and the summary says so: a RED
// under load has to be readable as a verdict taken under load, not as a clean
// one (gt-5ejux).
func tierSweepVerdictSummary(results map[string]tierSweepTierResult, stages []tierSweepStage, loaded map[string]bool) string {
	var parts []string
	for _, st := range stages {
		for _, tier := range st.tiers {
			if tier == "shell" {
				continue
			}
			verdict := tier + " " + results[tier].Verdict
			if loaded[tier] {
				verdict += " under load"
			}
			parts = append(parts, verdict)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// tierSweepWorkDir is the sweep's one fixed worktree for rig, beside the
// landing worker's throwaway worktrees so it is outside the town root (git
// refuses a worktree there).
func (d *Daemon) tierSweepWorkDir(rig string) (string, error) {
	configured := ""
	if c := landingWorkerConfig(d.patrolConfig); c != nil {
		configured = c.WorkRoot
	}
	root, err := landingWorkRoot(configured, d.config.TownRoot, rig)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "tier-sweep", "wt"), nil
}

// tierSweepCheckout checks sha out detached at the sweep's one fixed path. The
// worktree from an earlier cycle is moved to the new sha rather than recreated,
// so the checkout is reused; anything else at the path is dropped first, because
// a stale tree there would sweep the wrong commit.
func tierSweepCheckout(_ context.Context, repo, dir, sha string) (func(), error) {
	return tierSweepCheckoutWith(tierSweepGit(), repo, dir, sha)
}

// tierSweepGitOps is the git surface tierSweepCheckout drives. It is a value
// rather than direct calls so the unit tier, which runs no git, can drive the
// recovery below (gt-9yfgm).
type tierSweepGitOps struct {
	moveTo func(dir, sha string) error
	remove func(repo, dir string) error
	prune  func(repo string) error
	add    func(repo, dir, sha string) error
}

// tierSweepGit is the production surface: the calls tierSweepCheckout makes
// against the rig repository.
func tierSweepGit() tierSweepGitOps {
	return tierSweepGitOps{
		moveTo: func(dir, sha string) error { return git.NewGit(dir).CheckoutDetachForce(sha) },
		remove: func(repo, dir string) error { return git.NewGit(repo).WorktreeRemove(dir, true) },
		prune:  func(repo string) error { return git.NewGit(repo).WorktreePrune() },
		add:    func(repo, dir, sha string) error { return git.NewGit(repo).WorktreeAddDetached(dir, sha) },
	}
}

func tierSweepCheckoutWith(g tierSweepGitOps, repo, dir, sha string) (func(), error) {
	if _, err := os.Stat(dir); err == nil {
		if err := g.moveTo(dir, sha); err == nil {
			return func() {}, nil
		}
		// WorktreeRemove drops only a registered worktree, so a directory left
		// at the path by an unclean shutdown survives it and then wedges every
		// add with "already exists". Remove exactly this path - never its
		// parent or a sibling - and re-add the worktree (gt-9yfgm).
		if err := g.remove(repo, dir); err != nil {
			if err := os.RemoveAll(dir); err != nil {
				return nil, err
			}
		}
		_ = g.prune(repo)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if err := g.add(repo, dir, sha); err != nil {
		return nil, err
	}
	return func() {}, nil
}

// tierSweepMainSHA fetches origin/main and returns its tip.
func tierSweepMainSHA(ctx context.Context, repo string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g := git.NewGit(repo)
	if err := g.FetchBranch("origin", "main"); err != nil {
		return "", fmt.Errorf("fetching origin/main: %w", err)
	}
	sha, err := g.Rev("origin/main")
	if err != nil {
		return "", fmt.Errorf("resolving origin/main: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// tierSweepRunStage runs one stage in dir. The shell stage runs as its own
// step. The integration stage is named "test" so land.WithSlot holds the town's
// container-gate slot around it under the sweep's non-gate role: the script's
// inner `gt slot run` calls inherit that role and ride the one hold
// reentrantly.
func (d *Daemon) tierSweepRunStage(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult {
	var buf bytes.Buffer
	cg := land.CommandGate{
		Steps: []land.Step{{Name: st.step, Command: st.command(), Timeout: st.budget}},
		Out:   &buf,
	}
	if st.slot {
		cg = land.WithSlot(cg, d.config.TownRoot, st.role)
	}
	res := cg.Run(ctx, dir)
	if len(res.Steps) == 0 {
		return tierSweepStageResult{exitCode: -1, err: res.Err}
	}
	step := res.Steps[len(res.Steps)-1]
	return tierSweepStageResult{
		output:   buf.String(),
		exitCode: step.ExitCode,
		timedOut: step.TimedOut,
		ran:      true,
	}
}

// tierSweepWriteLog writes one stage's whole output to a fixed path per rig and
// tier, returning it for the record. A write that fails is logged and the
// sweep continues: the verdict is the exit code, not the log.
func (d *Daemon) tierSweepWriteLog(rig, tier, output string) string {
	path := filepath.Join(constants.TownRuntimePath(d.config.TownRoot), "tier-sweep", "logs", rig, tier+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		d.logger.Printf("tier_sweep: %s: creating the log directory: %v", rig, err)
		return ""
	}
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		d.logger.Printf("tier_sweep: %s: writing %s: %v", rig, path, err)
		return ""
	}
	return path
}

// tierSweepSummaryRE matches the script's one summary line per tier
// (scripts/tier-sweep.sh), whose elapsed time trails the log marker:
//
//	tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.aB12) in 2m14s
//
// The elapsed group is optional so a line from an older sweep, with no
// duration, still parses.
var tierSweepSummaryRE = regexp.MustCompile(`^tier-sweep: (\S+) (GREEN|RED) passed=(\d+) failed=(\d+) skipped=(\d+)(?: failed:(.*?))?(?: \(logs ([^)]*)\))?(?: in (\S+))?\s*$`)

// parseTierSweepSummaries reads the verdicts a stage's output reported, keyed
// by tier.
func parseTierSweepSummaries(output string) map[string]tierSweepTierResult {
	out := map[string]tierSweepTierResult{}
	for _, line := range strings.Split(output, "\n") {
		m := tierSweepSummaryRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		passed, _ := strconv.Atoi(m[3])
		failed, _ := strconv.Atoi(m[4])
		out[m[1]] = tierSweepTierResult{
			Verdict:     m[2],
			Passed:      passed,
			Failed:      failed,
			FailedNames: strings.Fields(m[6]),
			Log:         m[7],
		}
	}
	return out
}

// tierSweepBeadPrefix keys an open sweep bead for one rig and tier.
func tierSweepBeadPrefix(rig, tier string) string {
	return fmt.Sprintf("tier sweep (%s): %s: ", rig, tier)
}

// tierSweepBeads files one bead per failing unit of a RED tier, and closes a
// GREEN tier's open beads. A bead-store error is logged and the sweep
// continues: the verdict is already recorded.
func (d *Daemon) tierSweepRecordBeads(rig, sha string, results map[string]tierSweepTierResult) {
	open := d.tierSweepSeams.beads
	if open == nil {
		rigPath := filepath.Join(d.config.TownRoot, rig)
		bdPath := d.bdPathOrDefault()
		open = func(string) tierSweepBeadStore {
			return beads.NewPinned(beads.ResolveBeadsDir(rigPath), beads.WithBin(bdPath))
		}
	}
	bd := open(rig)
	if bd == nil {
		return
	}
	for tier, ts := range results {
		existing, err := tierSweepOpenBeads(bd, rig, tier)
		if err != nil {
			d.logger.Printf("tier_sweep: %s: listing %s beads: %v", rig, tierSweepRedLabel, err)
			existing = nil
		}
		if ts.Verdict != tierSweepRed {
			tierSweepCloseGreen(d, bd, rig, tier, sha, existing)
			continue
		}
		names := ts.FailedNames
		if len(names) == 0 {
			names = []string{tierSweepNoName}
		}
		for _, name := range names {
			tierSweepFileRed(d, bd, rig, tier, name, sha, ts, existing[name])
		}
	}
}

// tierSweepOpenBeads maps the failing unit each open sweep bead for rig and
// tier was filed for to its bead id.
func tierSweepOpenBeads(bd tierSweepBeadStore, rig, tier string) (map[string]string, error) {
	issues, err := bd.List(beads.ListOptions{Status: "open", Label: tierSweepRedLabel, Priority: -1, Limit: 0})
	if err != nil {
		return nil, err
	}
	prefix := tierSweepBeadPrefix(rig, tier)
	out := map[string]string{}
	for _, is := range issues {
		if is == nil {
			continue
		}
		if name, ok := strings.CutPrefix(is.Title, prefix); ok && name != "" {
			out[name] = is.ID
		}
	}
	return out, nil
}

// tierSweepFileRed comments on the open bead for name, else files one. The
// pattern is the red-main owner's fileOrComment (internal/landworker/redmain.go).
func tierSweepFileRed(d *Daemon, bd tierSweepBeadStore, rig, tier, name, sha string, ts tierSweepTierResult, openID string) {
	detail := fmt.Sprintf("%s tier RED on main at %s (%d passed, %d failed). Log: %s",
		tier, shortSHA(sha), ts.Passed, ts.Failed, ts.Log)
	if openID != "" {
		if err := bd.AddComment(openID, "still red: "+detail); err != nil {
			d.logger.Printf("tier_sweep: %s: commenting on %s: %v", rig, openID, err)
		}
		return
	}
	if _, err := bd.Create(beads.CreateOptions{
		Title:       tierSweepBeadPrefix(rig, tier) + name,
		Labels:      []string{tierSweepRedLabel},
		Priority:    1,
		Description: "The daemon's tier sweep found this red on main (gt-vsct7.5). " + detail,
	}); err != nil {
		d.logger.Printf("tier_sweep: %s: filing a bead for %s %s: %v", rig, tier, name, err)
	}
}

// tierSweepCloseGreen closes the open beads a GREEN tier no longer needs.
func tierSweepCloseGreen(d *Daemon, bd tierSweepBeadStore, rig, tier, sha string, open map[string]string) {
	for name, id := range open {
		reason := fmt.Sprintf("%s tier green on main at %s (tier sweep)", tier, shortSHA(sha))
		if err := bd.CloseWithReason(reason, id); err != nil {
			d.logger.Printf("tier_sweep: %s: closing %s (%s %s): %v", rig, id, tier, name, err)
		}
	}
}

// lastLines is out's final n lines; a shorter output is returned whole.
func lastLines(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
