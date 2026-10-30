package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	"github.com/steveyegge/gastown/internal/tiersweep"
)

// The tier_sweep job runs origin/main's shell tests through the Go runner
// (internal/shelltier) and its integration and race tiers through the Go
// command (cmd/tier-sweep), on an interval: the tiers `make gate` does not run,
// which landings gate only for the submissions that can move them
// (gt-vsct7.5). It replaces the overseer's
// hourly :37 cron, so it is silent while green and costs nothing while main is
// unchanged.
//
// A cycle checks origin/main out detached in one fixed private worktree outside
// the town root (landingWorkRoot), runs the shell tier every time and the
// integration and race tiers after it. It runs no `make gate`:
// every landing already gates the merged tree (gt-vsct7.8).
//
// The integration stage takes the container-gate slot and starts as soon as the
// shell stage ends: the sweep runs on its own Docker engine, so no CI gate
// shares one with it (gt-qpeei.8).
//
// Three things it deliberately does not do. It does not revert a red main: a
// sweep RED files a bead, and the landing worker's red-main owner decides the
// revert. It does not write `red-main` beads: that label's close-on-green is
// the post-land runner's, so a sweep bead under it would be closed by an
// unrelated green landing. It does not hold a gate-class slot: the sweep's own
// role is <rig>/tier-sweep, and the command's inner `gt slot run` calls inherit
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

	// tierSweepLogTailLines is how much of a stage's output the daemon log
	// keeps. The whole log is written to the state file's log path.
	tierSweepLogTailLines = 20

	// tierSweepTierCommand runs the integration and race tiers, taking the tier
	// names as arguments. Its summary, progress and unknown-tier lines are the
	// text the parsers below read. The command is resolved in the checked-out
	// tree, so a sweep runs the Go source main has.
	tierSweepTierCommand = "go run ./cmd/tier-sweep"

	// tierSweepShellRunner is the shell stage's command in place of the tier
	// script: the Go runner that runs every shell test in the manifest
	// (gt-u560q.4). It takes no per-script filter, so the sweep bead names it
	// whatever unit is red.
	tierSweepShellRunner = "go run ./internal/shelltier/cmd/shelltier --all"

	// tierSweepShellScriptBudget is the --budget passed to the shell runner: the
	// per-script limit, 10m. The whole stage is bounded by tierSweepShellBudget.
	tierSweepShellScriptBudget = "10m"

	// tierSweepShellCommand is the Go shell stage's full command. A red shell
	// unit's sweep bead names the command that ran for its rig (Command on the
	// tier result), which is this one in a tree that ships the Go runner.
	tierSweepShellCommand = tierSweepShellRunner + " --budget " + tierSweepShellScriptBudget

	// The Go commands run only in a tree that ships their directories. A rig
	// that ships its own script (devops) has neither, and the stage falls back
	// to the script (gt-j0wki).
	tierSweepShellDir = "internal/shelltier/cmd/shelltier"
	tierSweepTierDir  = "cmd/tier-sweep"

	// tierSweepScriptPath is the rig's own tier script. A checked-out tree that
	// ships no Go command runs it with the tiers as arguments (resolveIn).
	tierSweepScriptPath = "scripts/tier-sweep.sh"

	// tierSweepNoCommandReason is the RED reason of a stage whose tree ships
	// neither the Go command nor the script. Nothing ran, so nothing is green.
	tierSweepNoCommandReason = "no tier-sweep command in this tree"

	// tierSweepRedLabel marks a bead the sweep filed for one failing unit. It
	// is deliberately not landworker.LabelRedMain: the post-land runner's
	// close-on-green would close a sweep bead on an unrelated landing.
	tierSweepRedLabel = "tier-sweep-red"

	// tierSweepRetryMinGap is the least time between a red cycle and the early
	// re-run its closed fix beads allow, so a sweep that stays red on every new
	// landing cannot loop faster than this.
	tierSweepRetryMinGap = 10 * time.Minute

	// defaultTierSweepStaleAfter is how old the last sweep may be before a
	// pending upgrade restart stops deferring the next one (gt-zvqn8). Every
	// landing that changes the binary leaves a restart pending, so during a run
	// of landings the flag is set almost continuously and an unbounded deferral
	// starves the only gate over the integration and race tiers. Ninety minutes
	// is an hour and a half: a sweep that missed its own hour has missed half of
	// the next one too.
	defaultTierSweepStaleAfter = 90 * time.Minute

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
//
// Two writers own it, tierSweepStampOwed and tierSweepRigCycle, and both run on
// the one tier_sweep goroutine triggerTierSweep single-flights with
// tierSweepRunning: the record's read-modify-writes never interleave, and a
// third writer needs that guard or a lock of its own (gt-o3hb4).
type tierSweepState struct {
	LastRun time.Time `json:"last_run"`
	// LastSHA is the tip the last cycle saw: every tier that cycle ran was
	// judged there.
	LastSHA string `json:"last_sha"`
	// Partial is true while the last cycle stopped before running every stage
	// it planned. Tiers then holds the tiers that cycle judged at LastSHA plus
	// the verdicts carried forward for the tiers it never reached, and the tip
	// owes the rest of itself to the next one (gt-icmdq).
	Partial bool `json:"partial,omitempty"`
	// EarlyRun is true while an early re-run wrote the record rather than the
	// hourly town-wide cycle. An early re-run runs inside the interval, so its
	// record is no evidence about a town-wide cycle that deferred, and the
	// cadence run reading it re-derives the tip instead of taking it for a
	// deferred cycle's (gt-1911b).
	EarlyRun bool `json:"early_run,omitempty"`
	// LastGreenSHA is the tip of the last cycle that covered every tier the
	// rig's script offers of the ones the sweep is scheduled to cover, and was
	// green throughout. It is the skip key: while origin/main equals it there
	// is nothing new to sweep.
	LastGreenSHA string `json:"last_green_sha"`
	// Tiers is the last verdict per tier, each carrying the tip it was taken
	// at.
	Tiers map[string]tierSweepTierResult `json:"tiers"`
	// OwedSince is when this rig was first seen owing a sweep. The zero time
	// means it is caught up, which is why the field carries no omitempty: a
	// zero time.Time is written out whatever its tag says (gt-icmdq).
	OwedSince time.Time `json:"owed_since"`
}

// tierSweepTierResult is one tier's last verdict.
type tierSweepTierResult struct {
	Verdict     string   `json:"verdict"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	FailedNames []string `json:"failed_names"`
	// Log is the file the stage's whole output was written to.
	Log string `json:"log,omitempty"`
	// LogDir is the directory the rig's script wrote each unit's own log into,
	// from the summary line's "(logs <dir>)" group. The stage log holds only
	// what the script echoed, so a slowcheck finding is reachable only here
	// (gt-j6vlz). Omitted when the sweep that wrote the record reported none,
	// which is also how a record written before the field existed reads.
	LogDir string `json:"log_dir,omitempty"`
	// Command is the command the stage ran for this tier in the rig's checked-out
	// tree: the Go command, or the rig's own script. The sweep bead's check
	// command names it. Empty when no command ran (gt-j0wki).
	Command string `json:"command,omitempty"`
	// SlotWaitSeconds is how long the tier's stage waited for the
	// container-gate slot before holding it, in whole seconds. The stage's run
	// time is separate from it, so a slow tier that was mostly a queue reads
	// as such. Set for the tiers of a stage that held the slot, and omitted
	// when there was no wait (gt-sy14g).
	SlotWaitSeconds int `json:"slot_wait_seconds,omitempty"`
	// SHA is the tip this verdict was taken at: the cycle's own tip for a tier
	// that cycle ran, an earlier one for a tier a later Partial cycle carried
	// forward (gt-euxf1). It, not the record's LastSHA, is what a verdict's
	// failing names belong to.
	SHA string `json:"sha,omitempty"`
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

// tierSweepStaleAfter is how old the last sweep may be before a pending upgrade
// restart stops deferring the next one, defaulting to 90m and reading a value
// that does not parse, or is not positive, as unset.
func tierSweepStaleAfter(config *DaemonPatrolConfig) time.Duration {
	if c := tierSweepConfig(config); c != nil && c.RestartStaleAfterStr != "" {
		if d, err := time.ParseDuration(c.RestartStaleAfterStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultTierSweepStaleAfter
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
	promoted := p.Promote(st.State, sha)
	// The push above runs for seconds against the network, and the landing
	// worker writes its verdict into this same record meanwhile: re-read and
	// carry over only the promotion State, so the verdict that landed during
	// the push is kept rather than overwritten with the copy from before it
	// (gt-8iq4h).
	if st, err = state.Load(); err != nil {
		d.logger.Printf("tier_sweep: %s: re-reading the main state: %v", rigName, err)
		return
	}
	st.State = promoted
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
	// runner, when set, is the command the stage runs in place of the tier
	// script: the shell stage's Go runner.
	runner string
	// goDir is the directory the stage's Go command needs in the checked-out
	// tree. The Go command runs only where it exists (resolveIn).
	goDir string
	// script is true when resolveIn found the Go command missing and the tree
	// ships scripts/tier-sweep.sh: the stage runs that script with its tiers.
	script bool
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
	if s.runner != "" {
		return s.runner
	}
	if s.script {
		return "bash " + tierSweepScriptPath + " " + strings.Join(s.tiers, " ")
	}
	return tierSweepTierCommand + " " + strings.Join(s.tiers, " ")
}

// resolveIn picks the command the stage runs in a checked-out tree. has reports
// whether the tree ships a path, and an error when it cannot tell. The Go
// command runs where its directory is; a tree without it runs the rig's script
// with the same tiers; a tree with neither has no command (runnable is false),
// and the stage is RED, never skipped or GREEN. A check that errors returns
// that error, so a tree that cannot be read is not taken for one without the
// command. The returned stage is what the cycle runs and reads: a script stage
// carries no runner, so its output is read as the script's.
func (s tierSweepStage) resolveIn(has func(rel string) (bool, error)) (st tierSweepStage, runnable bool, err error) {
	goOK, err := has(s.goDir)
	if err != nil {
		return s, false, err
	}
	if goOK {
		return s, true, nil
	}
	scriptOK, err := has(tierSweepScriptPath)
	if err != nil {
		return s, false, err
	}
	if scriptOK {
		s.runner, s.script = "", true
		return s, true, nil
	}
	return s, false, nil
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
	// reason, when set, is the RED reason a tier with no verdict line takes in
	// place of the exit-code reason: a tree with no command to run.
	reason string
	// slotWait is how long the stage's step waited for the container-gate slot
	// before holding it, from the step's SlotWait. Elapsed begins after the
	// hold, so a stage that sat out a queue reads as its own work alone
	// (gt-sy14g).
	slotWait time.Duration
}

// tierSweepSeams replace the sweep's outside effects in tests; the zero value
// is production. Each is resolved at use, so a test replaces only the one it
// needs.
type tierSweepSeams struct {
	// mainSHA fetches and reads origin/main in the rig repository.
	mainSHA func(ctx context.Context, repo string) (string, error)
	// localMainSHA reads origin/main from the rig repository as of its last
	// fetch, without fetching: the restart deferral asks it on every heartbeat.
	localMainSHA func(repo string) (string, error)
	// worktree checks sha out detached at dir, returning a cleanup.
	worktree func(ctx context.Context, repo, dir, sha string) (func(), error)
	// run executes one stage in dir.
	run func(ctx context.Context, dir string, st tierSweepStage) tierSweepStageResult
	// treeHas reports whether the checked-out tree at dir ships rel, a file or a
	// directory, and an error when the tree cannot be read. Each stage's command
	// is picked from it before the stage runs.
	treeHas func(dir, rel string) (bool, error)
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
	Update(id string, opts beads.UpdateOptions) error
	CloseWithReason(reason string, ids ...string) error
}

// triggerTierSweep runs one tier_sweep cycle on its own goroutine when the job
// is due: a cycle runs the shell tier and then the integration suite, so
// inline it would stall the heartbeat.
func (d *Daemon) triggerTierSweep() {
	if !d.isPatrolActive("tier_sweep") {
		return
	}
	if !d.upgradeRestartPending.Load() {
		// No restart pending: the next one logs its skip again.
		d.tierSweepSkipLogged.Store(false)
		d.tierSweepStaleLogged.Store(false)
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "tier_sweep", d.tierSweepRun.last(), now, tierSweepInterval(d.patrolConfig))
	// Inside the interval, a rig whose last cycle was red may still be due early
	// (gt-giai1). The state files pick the candidates; the bead store and
	// origin/main are read on the cycle's goroutine.
	var early []string
	if !dec.due {
		early = d.tierSweepEarlyCandidates(now)
		if len(early) == 0 {
			return
		}
	}
	if !d.tierSweepRunning.CompareAndSwap(false, true) {
		return
	}
	// The hold the restart reads starts here, with the cycle, not with the
	// marker: a sweep that begins while the restart has already waited out
	// tierSweepRestartCap gets that much of its own (gt-zvqn8).
	d.tierSweepStartedAt.Store(now.UnixNano())
	if dec.warn != "" {
		d.logger.Printf("tier_sweep: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.tierSweepRunning.Store(false)
		if !dec.due {
			// An early re-run leaves the patrol's hourly clock alone: the rig's
			// own record carries the gap, and a heartbeat that finds main
			// unmoved must not consume the hour.
			d.runTierSweepEarly(early, now)
			return
		}
		if !d.runTierSweep() {
			// Nothing accomplished: leave the gate open so the next heartbeat
			// retries.
			return
		}
		d.recordPatrolRun(&d.tierSweepRun, "tier_sweep", d.clk().Now())
	}()
}

// tierSweepEarlyDue reports whether st is a red record old enough to re-run
// before the interval: the last cycle was not green, and at least
// tierSweepRetryMinGap has passed since it ran. The fix beads and a moved
// origin/main are the other two conditions, checked by runTierSweepEarly.
func tierSweepEarlyDue(st tierSweepState, now time.Time) bool {
	return st.LastSHA != "" && st.LastSHA != st.LastGreenSHA && now.Sub(st.LastRun) >= tierSweepRetryMinGap
}

// tierSweepEarlyCandidates lists the configured rigs whose record passes
// tierSweepEarlyDue.
func (d *Daemon) tierSweepEarlyCandidates(now time.Time) []string {
	var out []string
	for _, rig := range tierSweepRigs(d.patrolConfig, d.getKnownRigs()) {
		st, err := readTierSweepState(d.config.TownRoot, rig)
		if err != nil {
			continue
		}
		if tierSweepEarlyDue(st, now) {
			out = append(out, rig)
		}
	}
	return out
}

// tierSweepRedBeadsOpen reports whether rig has a sweep bead that is open or
// in progress: its fix has not landed.
func tierSweepRedBeadsOpen(bd tierSweepBeadStore, rig string) (bool, error) {
	prefix := tierSweepRigBeadPrefix(rig)
	for _, status := range []string{"open", string(beads.StatusInProgress), beads.StatusHooked} {
		issues, err := bd.List(beads.ListOptions{Status: status, Label: tierSweepRedLabel, Priority: -1, Limit: 0})
		if err != nil {
			return false, err
		}
		for _, is := range issues {
			if is != nil && strings.HasPrefix(is.Title, prefix) {
				return true, nil
			}
		}
	}
	return false, nil
}

// runTierSweepEarly re-runs the candidate rigs whose red beads have all
// closed and whose origin/main has moved since their last cycle. A rig that
// fails either test is left to the normal interval, without a word in the log.
func (d *Daemon) runTierSweepEarly(rigs []string, now time.Time) {
	if d.tierSweepDeferForRestart(now) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tierSweepRunBudget)
	defer cancel()

	var cycle *dogCycle
	defer func() {
		if cycle != nil {
			cycle.close()
		}
	}()
	for _, rig := range rigs {
		if ctx.Err() != nil {
			return
		}
		state, err := readTierSweepState(d.config.TownRoot, rig)
		if err != nil || !tierSweepEarlyDue(state, now) {
			continue
		}
		bd := d.tierSweepBeads(rig)
		if bd == nil {
			continue
		}
		open, err := tierSweepRedBeadsOpen(bd, rig)
		if err != nil {
			d.logger.Printf("tier_sweep: %s: listing %s beads: %v", rig, tierSweepRedLabel, err)
			continue
		}
		if open {
			continue
		}
		repo := filepath.Join(d.config.TownRoot, rig, ".repo.git")
		if _, err := os.Stat(repo); err != nil {
			continue
		}
		mainSHA := d.tierSweepSeams.mainSHA
		if mainSHA == nil {
			mainSHA = tierSweepMainSHA
		}
		sha, err := mainSHA(ctx, repo)
		if err != nil {
			d.logger.Printf("tier_sweep: %s: cannot read origin/main (%v); deferring the early re-run", rig, err)
			continue
		}
		// A tip the last cycle covered stands; one it only partly reached does
		// not, which is what Partial records (gt-icmdq).
		if sha == "" || sha == state.LastGreenSHA || tierSweepTipJudged(state, sha) {
			continue
		}
		d.logger.Printf("tier_sweep: %s: red bead(s) closed and main moved; re-running now", rig)
		if cycle == nil {
			cycle = d.startDogCycle("tier_sweep")
		}
		d.tierSweepRigCycle(ctx, cycle, rig, repo, sha, state, now, tierSweepEarlyRun)
	}
}

// tierSweepDeferForRestart reports whether a pending upgrade restart defers
// the cycle at now, logging the skip once per pending restart, not once per
// heartbeat.
//
// The deferral is bounded by the age of the oldest sweep still owed (gt-zvqn8,
// gt-v8j5x). Every landing that
// changes the binary installs it and leaves a restart pending, so during a run
// of landings the flag is set almost continuously; an unbounded deferral left
// the integration and race tiers — which only a sweep gates — unswept for
// hours. A sweep at or past tierSweepStaleAfter therefore runs anyway, and the
// restart waits for it (upgradeHold, tierSweepRestartCap). A rig with no sweep
// record yet has no age to measure, so that case defers as before.
//
// The age both lines carry is the owed interval (tierSweepAge), and the run
// line has a latch of its own: shared with the skip line's, a restart that had
// already deferred a younger sweep would swallow it (gt-icmdq).
func (d *Daemon) tierSweepDeferForRestart(now time.Time) bool {
	if !d.upgradeRestartPending.Load() {
		return false
	}
	age, swept, owed := d.tierSweepAge(now)
	// The read above resolved these starts, so the dating write happens here:
	// tierSweepAge only reads (gt-icmdq).
	for _, o := range owed {
		d.tierSweepStampOwed(o.rig, o.since)
	}
	bound := tierSweepStaleAfter(d.patrolConfig)
	switch {
	case swept && age >= bound:
		if d.tierSweepStaleLogged.CompareAndSwap(false, true) {
			d.logger.Printf("tier_sweep: the owed sweep is %s old, past the %s bound; sweeping now rather than waiting out the pending restart",
				age.Round(time.Minute), bound)
		}
		return false
	case swept:
		if d.tierSweepSkipLogged.CompareAndSwap(false, true) {
			d.logger.Printf("tier_sweep: an upgrade restart is pending; skipping this cycle (owed sweep %s old, bound %s)",
				age.Round(time.Minute), bound)
		}
		return true
	default:
		if d.tierSweepSkipLogged.CompareAndSwap(false, true) {
			d.logger.Printf("tier_sweep: an upgrade restart is pending; skipping this cycle (no sweep recorded yet)")
		}
		return true
	}
}

// tierSweepOwed is a rig whose owed interval this read resolved a start for,
// with that start: the caller writes it (tierSweepStampOwed). The read itself
// writes nothing (gt-o3hb4).
type tierSweepOwed struct {
	rig   string
	since time.Time
}

// tierSweepAge is how long the oldest rig that still owes a sweep has owed it,
// and the rigs whose owed interval this read had to date itself.
//
// The interval is dated from OwedSince, which the caller writes. A record with
// no stamp predates the field, and what this read can date it from depends on
// what it can see: the tip that just moved past the record's green sweep, or —
// with no green sweep to compare origin/main against — its own last run. A tip
// this read cannot read dates nothing, and leaves the last run measuring, so
// the next read that can read the tip dates the interval from the move it sees
// then rather than freezing a guess for good (gt-o3hb4).
//
// A rig caught up with origin/main is left out: its LastRun goes stale while the
// patrol runs hourly, and counting it would retire the restart's deferral for
// the busy rigs (gt-v8j5x). A rig whose tip cannot be read counts as not caught
// up, since a sweep that may be starved must not be hidden.
//
// The second result is false when no remaining rig has a readable record yet —
// nothing is known to have been swept, so nothing is starved. A rig with no
// record is skipped rather than counted as infinitely stale: a rig that is
// configured but has no repository is skipped by every cycle and would
// otherwise make every sweep measure as stale.
func (d *Daemon) tierSweepAge(now time.Time) (time.Duration, bool, []tierSweepOwed) {
	var age time.Duration
	var owed []tierSweepOwed
	found := false
	for _, rig := range tierSweepRigs(d.patrolConfig, d.getKnownRigs()) {
		st, err := readTierSweepState(d.config.TownRoot, rig)
		if err != nil || st.LastRun.IsZero() {
			continue
		}
		since := st.OwedSince
		dated := false
		switch {
		case st.LastGreenSHA == "":
			// No green sweep to compare origin/main against, so no read is
			// asked for: the last run is the only start this record has.
			if since.IsZero() {
				since, dated = st.LastRun, true
			}
		default:
			main, readErr := d.tierSweepLocalMain(rig)
			switch {
			case readErr != nil:
				// The tip cannot be read, so this read cannot tell whether the
				// rig owes a sweep at all, and the record keeps no start: the
				// last run measures the age until a read can read the tip.
				if since.IsZero() {
					since = st.LastRun
				}
			case main == st.LastGreenSHA:
				// Caught up with the green sweep that covered it: nothing is
				// owed.
				continue
			case since.IsZero():
				// The tip moved past the green sweep, so the interval begins
				// at this read.
				since, dated = now, true
			}
		}
		if dated {
			owed = append(owed, tierSweepOwed{rig: rig, since: since})
		}
		rigAge := now.Sub(since)
		if !found || rigAge > age {
			age = rigAge
		}
		found = true
	}
	return age, found, owed
}

// tierSweepLocalMain reads rig's origin/main as the repository last fetched it,
// without fetching.
func (d *Daemon) tierSweepLocalMain(rig string) (string, error) {
	read := d.tierSweepSeams.localMainSHA
	if read == nil {
		read = tierSweepLocalMainSHA
	}
	return read(filepath.Join(d.config.TownRoot, rig, ".repo.git"))
}

// tierSweepStampOwed records when rig's owed interval began, leaving a record
// that already carries a stamp alone. The record is re-read for the write, so a
// stamp never carries a stale copy of the cycle's fields; the read-modify-write
// is safe under the record's one-writer rule (tierSweepState, gt-o3hb4).
func (d *Daemon) tierSweepStampOwed(rig string, since time.Time) {
	st, err := readTierSweepState(d.config.TownRoot, rig)
	if err != nil || !st.OwedSince.IsZero() {
		return
	}
	st.OwedSince = since
	if err := writeTierSweepState(d.config.TownRoot, rig, st); err != nil {
		d.logger.Printf("tier_sweep: %s: recording when the sweep became owed: %v", rig, err)
	}
}

// runTierSweep is one cycle over every configured rig. It reports whether the
// cycle reached a verdict, as opposed to deferring with nothing accomplished.
// A pending upgrade restart defers the cycle, unless the last sweep is already
// past tierSweepStaleAfter: the job stays due and the next heartbeat retries,
// so the restart does not cost the hour, and a run of landings cannot cost the
// tiers their gate.
func (d *Daemon) runTierSweep() bool {
	if d.tierSweepDeferForRestart(d.clk().Now()) {
		return false
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
	if tierSweepTipJudged(state, sha) && d.tierSweepJudgedByADeferredCycle(state) {
		// This run is the retry of a cycle that deferred, so the tip's verdict
		// is already in the record and re-deriving it costs the rig its whole
		// stage budget. A cadence run still re-sweeps an unchanged tip: that is
		// how a red recovers without waiting for main to move (gt-zq1pb).
		cycle.skipStep(rig, "origin/main "+shortSHA(sha)+" already judged by the last cycle")
		d.logger.Printf("tier_sweep: %s: origin/main %s already judged by the last cycle; skipping the re-run", rig, shortSHA(sha))
		return true
	}
	return d.tierSweepRigCycle(ctx, cycle, rig, repo, sha, state, now, tierSweepTownWideRun)
}

// tierSweepTipJudged reports whether state already carries a verdict for sha:
// the last cycle saw that tip and ran every stage it planned at it, so the
// record holds a verdict for every tier the sweep covers. A tip that cycle only
// partly reached is not judged, because the record leaves tiers of it unjudged
// (gt-icmdq).
func tierSweepTipJudged(state tierSweepState, sha string) bool {
	return sha != "" && sha == state.LastSHA && !state.Partial
}

// tierSweepJudgedByADeferredCycle reports whether the record that judged a
// rig's tip was written by a cycle that never judged every rig. The sweep's own
// last-run clock advances only on a cycle that did, so a rig record newer than
// that clock is an unfinished cycle's work and the run reading it is that
// cycle's retry (gt-zq1pb). A record an early re-run wrote, and one past the
// sweep's staleness bound, are neither: their tips are the cadence run's to
// judge, so a red that recovered is re-read rather than held until main moves
// (gt-1911b).
func (d *Daemon) tierSweepJudgedByADeferredCycle(state tierSweepState) bool {
	if state.EarlyRun {
		return false
	}
	if d.clk().Now().Sub(state.LastRun) >= tierSweepStaleAfter(d.patrolConfig) {
		return false
	}
	// The clock is read the way the trigger reads it, with the completion this
	// process recorded: after a failed clock save the disk alone is older than
	// the cycle that judged the tip, and reading it would call that cycle's
	// verdict a deferral's (gt-zq1pb).
	lastRun, found, _, err := latestPatrolLastRun(d.config.TownRoot, "tier_sweep", d.tierSweepRun.last())
	if err != nil {
		// An unreadable clock says nothing about a deferral, so the tip keeps
		// the re-run every other cadence run gets.
		return false
	}
	if !found {
		// No cycle has ever completed, so the record came from one that did not.
		return true
	}
	return state.LastRun.After(lastRun)
}

// tierSweepStages is the stages one cycle runs for rig: the shell tier, then
// the integration suite (integration and race). Every cycle runs both, whatever
// the hour, so a green cycle always covers every tier.
func tierSweepStages(rig string) []tierSweepStage {
	return []tierSweepStage{{
		tiers:  []string{"shell"},
		budget: tierSweepShellBudget,
		runner: tierSweepShellCommand,
		goDir:  tierSweepShellDir,
		step:   "shell",
	}, {
		tiers:  []string{"integration", "race"},
		budget: tierSweepIntegrationBudget,
		goDir:  tierSweepTierDir,
		slot:   true,
		role:   rig + "/tier-sweep",
		step:   "test",
	}}
}

// tierSweepTreeHas is the production tree check: rel exists under dir. Only a
// path that is not there means the tree does not ship it. Any other stat error
// (a permission failure, or a parent that is a file) is returned, so the stage
// is RED with that error rather than read as a tree without the command.
func tierSweepTreeHas(dir, rel string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, rel))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// tierSweepTierList names the tiers a cycle's stages will run, in the order it
// runs them, for the "sweep started" line: "shell, integration, race".
func tierSweepTierList(stages []tierSweepStage) string {
	var tiers []string
	for _, st := range stages {
		tiers = append(tiers, st.tiers...)
	}
	return strings.Join(tiers, ", ")
}

// tierSweepRunKind names which run writes a record: the hourly town-wide cycle,
// whose records a deferral's retry may read as its own work, or an early re-run,
// whose records it may not (gt-1911b).
type tierSweepRunKind bool

const (
	tierSweepTownWideRun tierSweepRunKind = false
	tierSweepEarlyRun    tierSweepRunKind = true
)

// tierSweepRigCycle runs the stages, writes the record, and files or closes the
// beads the verdicts imply. The record says which run wrote it: kind is
// tierSweepEarlyRun for an early re-run.
func (d *Daemon) tierSweepRigCycle(ctx context.Context, cycle *dogCycle, rig, repo, sha string, state tierSweepState, now time.Time, kind tierSweepRunKind) bool {
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

	stages := tierSweepStages(rig)
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
	// notOffered names the tiers the rig's script declined to run, which have
	// no verdict at all (gt-qgrdy).
	notOffered := map[string]bool{}
	deferred := false
	treeHas := seams.treeHas
	if treeHas == nil {
		treeHas = tierSweepTreeHas
	}
	for _, planned := range stages {
		// The stage's own start, so `gt tail` shows a slow or hung stage on the
		// line that names it (gt-iqzr0).
		stageStart := d.clk().Now()
		// The command is picked from the tree this cycle checked out, before the
		// stage runs (gt-j0wki). A tree with neither the Go command nor the
		// script runs nothing, and a tree that cannot be read runs nothing
		// either: the stage is RED with the reason, never skipped.
		st, runnable, treeErr := planned.resolveIn(func(rel string) (bool, error) { return treeHas(dir, rel) })
		res := tierSweepStageResult{exitCode: -1, ran: true, output: tierSweepNoCommandReason + "\n", reason: tierSweepNoCommandReason}
		switch {
		case treeErr != nil:
			res.output, res.reason = treeErr.Error()+"\n", treeErr.Error()
		case runnable:
			res = run(ctx, dir, st)
		}
		stageElapsed := d.clk().Now().Sub(stageStart)
		if !res.ran {
			// The stage never started (a slot the sweep could not take, say):
			// nothing was learned about the tree, so the cycle is incomplete
			// and the next heartbeat retries it.
			d.logger.Printf("tier_sweep: %s: %s stage did not run (%v); deferring", rig, st.tiers[0], res.err)
			deferred = true
			continue
		}
		reported := tierSweepStageReported(st, res)
		if !res.timedOut && ctx.Err() != nil && !tierSweepStageJudged(st, reported) {
			// The cycle's own deadline ended a stage that never reached a
			// verdict. Land's gate marks a step TimedOut only when the step's
			// own timeout killed it, so a stage cut off here arrives as exit -1
			// with no summary line, and reading that as RED files a bead
			// against a stage that never finished. Nothing was learned about
			// the tree, so the cycle defers (gt-zq1pb).
			d.logger.Printf("tier_sweep: %s: %s stage: the cycle's deadline ended it (exit %d, %v); deferring",
				rig, st.tiers[0], res.exitCode, ctx.Err())
			deferred = true
			continue
		}
		declined := tierSweepDeclined(st, res)
		unreached := tierSweepUnreachedAfterDeclined(st.tiers, declined)
		logPath := d.tierSweepWriteLog(rig, st.tiers[0], res.output)
		for _, tier := range st.tiers {
			ts, ok := reported[tier]
			if !ok {
				if declined[tier] || unreached[tier] {
					// The rig's own script says it has no such tier and exits
					// 2 without a summary line. There is nothing to judge and
					// nothing to fix, so the tier leaves the cycle's results
					// (gt-qgrdy). Only the script's own line says so: a crash
					// or a kill exits without it and stays RED below.
					if !notOffered[tier] {
						d.logger.Printf("tier_sweep: %s: %s not offered by the rig's script", rig, tier)
					}
					notOffered[tier] = true
					continue
				}
				// No summary line: the script was killed, regressed its output,
				// or never reached the tier. That is RED, never GREEN, and the
				// reason stands in for the failing unit the script did not name.
				reason := fmt.Sprintf("no verdict line (exit %d)", res.exitCode)
				if res.reason != "" {
					reason = res.reason
				} else if res.timedOut {
					reason = "timeout"
				} else if res.exitCode == 0 {
					reason = "no verdict line"
				}
				ts = tierSweepTierResult{Verdict: tierSweepRed, Failed: 1, FailedNames: []string{reason}}
				d.logger.Printf("tier_sweep: %s: %s %s: %s", rig, tier, tierSweepRed, reason)
			}
			ts.Log = logPath
			// The command that ran for this tier in this rig's tree, which the
			// sweep bead's check command names (gt-j0wki). Empty when none ran.
			if runnable {
				ts.Command = st.command()
			}
			// The stage held the slot, so its wait is the tiers' own: a tier
			// whose wall was mostly the queue says so in the record, apart from
			// the run time (gt-sy14g).
			if st.slot {
				ts.SlotWaitSeconds = int(res.slotWait.Round(time.Second).Seconds())
			}
			results[tier] = ts
		}
		waitNote := ""
		// Under a second reads as no wait, and the line is about the sweep, not
		// a clock tick.
		if res.slotWait >= time.Second {
			waitNote = fmt.Sprintf(" (slot wait %s)", res.slotWait.Round(time.Second))
		}
		d.logger.Printf("tier_sweep: %s: %s finished (exit %d) in %s%s; last lines:\n%s",
			rig, st.tiers[0], res.exitCode, stageElapsed.Round(time.Second), waitNote, lastLines(res.output, tierSweepLogTailLines))
	}

	if len(results) == 0 {
		// Nothing ran at all: the record would be a lie. When the script
		// declined every tier there is no verdict to record either, so the
		// cycle is not green and the last record stands (gt-qgrdy).
		reason := "no stage ran"
		if len(notOffered) > 0 {
			reason = "no tier offered by the rig's script"
		}
		d.logger.Print(tierSweepAbortedLine(rig, sha, reason))
		return false
	}

	state.LastRun = now
	state.EarlyRun = bool(kind)
	// The record this cycle replaces, read before the fields below rebind it:
	// its LastSHA dates the verdicts the cycle carries forward.
	prior := state
	state.Tiers = tierSweepRecordedTiers(results, prior, stages, sha, deferred, notOffered)
	state.LastSHA = sha
	state.Partial = deferred
	// LastGreenSHA advances only on a cycle whose every stage ran green over
	// the tiers the script offers: a deferred cycle ran fewer stages than it
	// planned and says nothing about the rest, and a tier the script does not
	// offer has no verdict to hold the tip back (gt-qgrdy).
	green := !deferred && tierSweepAllGreen(results, stages, notOffered)
	if green {
		state.LastGreenSHA = sha
		// Caught up with the tip it covered: nothing is owed.
		state.OwedSince = time.Time{}
	} else if state.OwedSince.IsZero() {
		// This starts the owed clock for a record with no green tip to compare
		// against; the heartbeat dates the rest (tierSweepAge). A cycle must
		// leave an interval that is already running alone, or a rig that fails
		// every hour would hold a fresh age and never reach the bound that
		// retires the restart's deferral (gt-icmdq).
		state.OwedSince = now
	}
	if err := writeTierSweepState(d.config.TownRoot, rig, state); err != nil {
		d.logger.Printf("tier_sweep: %s: writing the sweep record: %v", rig, err)
		d.logger.Print(tierSweepAbortedLine(rig, sha, "sweep record not written"))
		return false
	}
	// A cycle that covered every tier green is the verdict that advances the
	// rig's GitHub main (gt-fn9e6.38): it is the only check that saw every
	// tier at this commit. A red or partial cycle promotes nothing.
	if green {
		d.tierSweepPromote(rig, repo, sha)
	}
	cycle.closeStep(rig)
	d.logger.Print(tierSweepSweptLine(rig, sha, results, stages, d.clk().Now().Sub(now)))

	d.tierSweepRecordBeads(rig, sha, results)
	return !deferred
}

// tierSweepRecordedTiers is the verdict map one cycle leaves in the record:
// every tier the cycle ran, under this cycle's tip, plus — for a cycle that
// stopped before running every stage it planned — the verdict the previous
// cycle took for each tier the schedule names and this one never reached. A
// tier nothing re-ran has no new verdict, and dropping the old one reads as a
// clear the sweep never took: the red tier's beads stay open while its
// attention item retires (gt-euxf1). A tier the schedule no longer names is
// dropped rather than carried, because no cycle would ever re-run it, and so is
// a tier the rig's script declined this cycle: its earlier verdict was taken
// when the script still offered it, and carrying it would show that verdict as
// current (gt-qgrdy).
func tierSweepRecordedTiers(results map[string]tierSweepTierResult, prior tierSweepState, stages []tierSweepStage, sha string, deferred bool, notOffered map[string]bool) map[string]tierSweepTierResult {
	recorded := make(map[string]tierSweepTierResult, len(results))
	for tier, res := range results {
		res.SHA = sha
		recorded[tier] = res
	}
	if !deferred {
		// Every stage ran, so this cycle has a verdict of its own for every
		// tier the schedule names; there is nothing to carry.
		return recorded
	}
	for _, st := range stages {
		for _, tier := range st.tiers {
			if _, ran := results[tier]; ran || notOffered[tier] {
				continue
			}
			carried, ok := prior.Tiers[tier]
			if !ok {
				continue
			}
			if carried.SHA == "" {
				// A record written before a verdict carried its own tip dates
				// every tier in it at its LastSHA. Passing that tip on keeps
				// the readers from falling back to this cycle's and dating
				// the verdict to a commit nothing judged it at (gt-euxf1).
				carried.SHA = prior.LastSHA
			}
			recorded[tier] = carried
		}
	}
	return recorded
}

// tierSweepStageJudged reports whether a stage reported a summary line for
// every tier it covers: only then did the stage reach a verdict of its own,
// rather than being cut off before it could print one.
func tierSweepStageJudged(st tierSweepStage, reported map[string]tierSweepTierResult) bool {
	for _, tier := range st.tiers {
		if _, ok := reported[tier]; !ok {
			return false
		}
	}
	return true
}

// tierSweepAllGreen reports whether every tier of every stage the rig's script
// offers came back GREEN, with no stage left unrun. A cycle in which the script
// offered no tier at all judged nothing, so it is not green.
func tierSweepAllGreen(results map[string]tierSweepTierResult, stages []tierSweepStage, notOffered map[string]bool) bool {
	judged := false
	for _, st := range stages {
		for _, tier := range st.tiers {
			if notOffered[tier] {
				// Not part of this rig's gate, so it has no verdict that could
				// hold the green tip back (gt-qgrdy).
				continue
			}
			if results[tier].Verdict != tierSweepGreen {
				return false
			}
			judged = true
		}
	}
	return judged
}

// tierSweepSweptLine is the log line that closes a cycle. The dashboard reads
// it back (parseTierSweepVerdicts), so it has one writer.
func tierSweepSweptLine(rig, sha string, results map[string]tierSweepTierResult, stages []tierSweepStage, elapsed time.Duration) string {
	return fmt.Sprintf("tier_sweep: %s: swept %s (shell %s%s) in %s", rig, shortSHA(sha),
		results["shell"].Verdict, tierSweepVerdictSummary(results, stages),
		elapsed.Round(time.Second))
}

// tierSweepAbortedLine is the log line that closes a cycle that logged "sweep
// started" and then ended without a swept line. The dashboard reads it back
// (tierSweepAbortedRe), so it has one writer; the reason holds no parenthesis.
func tierSweepAbortedLine(rig, sha, reason string) string {
	return fmt.Sprintf("tier_sweep: %s: sweep aborted %s (%s)", rig, shortSHA(sha), reason)
}

// TierSweepAbortedLine renders the aborted line for the cycle on sha, so a
// reader of the daemon log is tested against the line the daemon writes.
func TierSweepAbortedLine(rig, sha, reason string) string {
	return tierSweepAbortedLine(rig, sha, reason)
}

// TierSweepSweptLine renders the swept line for a cycle that planned the given
// stages, each a list of tiers, given the verdict of each tier that ran. A tier
// absent from verdicts did not run. It lets a reader of the daemon log be tested
// against the line the daemon writes rather than a hand-written copy.
func TierSweepSweptLine(rig, sha string, stages [][]string, verdicts map[string]string, elapsed time.Duration) string {
	results := make(map[string]tierSweepTierResult, len(verdicts))
	for tier, v := range verdicts {
		results[tier] = tierSweepTierResult{Verdict: v}
	}
	planned := make([]tierSweepStage, len(stages))
	for i, tiers := range stages {
		planned[i] = tierSweepStage{tiers: tiers}
	}
	return tierSweepSweptLine(rig, sha, results, planned, elapsed)
}

// tierSweepVerdictSummary renders the non-shell verdicts for the cycle log.
func tierSweepVerdictSummary(results map[string]tierSweepTierResult, stages []tierSweepStage) string {
	var parts []string
	for _, st := range stages {
		for _, tier := range st.tiers {
			if tier == "shell" {
				continue
			}
			parts = append(parts, tier+" "+results[tier].Verdict)
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
	clean  func(dir string) error
	remove func(repo, dir string) error
	prune  func(repo string) error
	add    func(repo, dir, sha string) error
}

// tierSweepGit is the production surface: the calls tierSweepCheckout makes
// against the rig repository.
func tierSweepGit() tierSweepGitOps {
	return tierSweepGitOps{
		moveTo: func(dir, sha string) error { return git.NewGit(dir).CheckoutDetachForce(sha) },
		clean:  func(dir string) error { return git.NewGit(dir).CleanForceIgnored() },
		remove: func(repo, dir string) error { return git.NewGit(repo).WorktreeRemove(dir, true) },
		prune:  func(repo string) error { return git.NewGit(repo).WorktreePrune() },
		add:    func(repo, dir, sha string) error { return git.NewGit(repo).WorktreeAddDetached(dir, sha) },
	}
}

func tierSweepCheckoutWith(g tierSweepGitOps, repo, dir, sha string) (func(), error) {
	if _, err := os.Stat(dir); err == nil {
		if err := g.moveTo(dir, sha); err == nil {
			// The worktree is reused, so whatever the last cycle built is still
			// in it: checkout --force restores tracked files only, and an
			// ignored leftover (a stale binary, a build cache) can decide the
			// verdict the sweep is about to take. Clean it before the sweep
			// reads anything, and fail the cycle rather than sweep a tree that
			// could not be cleaned (gt-oyrav).
			if err := g.clean(dir); err != nil {
				return nil, err
			}
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

// tierSweepLocalMainSHA returns the tip of origin/main as the repository last
// fetched it.
func tierSweepLocalMainSHA(repo string) (string, error) {
	sha, err := git.NewGit(repo).Rev("origin/main")
	if err != nil {
		return "", fmt.Errorf("resolving origin/main: %w", err)
	}
	return strings.TrimSpace(sha), nil
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
		slotWait: step.SlotWait,
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

// tierSweepSummaryRE matches the sweep's one summary line per tier
// (cmd/tier-sweep), whose elapsed time trails the log marker:
//
//	tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.aB12) in 2m14s
//
// The elapsed group is optional so a line from an older sweep, with no
// duration, still parses.
var tierSweepSummaryRE = regexp.MustCompile(`^tier-sweep: (\S+) (GREEN|RED) passed=(\d+) failed=(\d+) skipped=(\d+)(?: failed:(.*?))?(?: \(logs ([^)]*)\))?(?: in (\S+))?\s*$`)

// tierSweepUnknownTierRE matches the line cmd/tier-sweep prints for a
// tier it does not offer, before exiting 2 without a summary for it (gt-qgrdy).
var tierSweepUnknownTierRE = regexp.MustCompile(`^tier-sweep: unknown tier (\S+)`)

// tierSweepDeclineExit is the exit status cmd/tier-sweep returns for a tier it
// does not offer, after printing the unknown-tier line.
const tierSweepDeclineExit = 2

// parseTierSweepUnknownTiers reads the tiers a stage's output names as tiers
// the rig's script does not offer.
func parseTierSweepUnknownTiers(output string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		if m := tierSweepUnknownTierRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// tierSweepDeclined names the tiers a stage declined to run. Only the rig's
// script declines, and only by exiting tierSweepDeclineExit at a tier it does
// not offer. The Go command offers every tier the stage asks for, so a Go
// stage's output never excuses a tier, whatever it prints.
func tierSweepDeclined(st tierSweepStage, res tierSweepStageResult) map[string]bool {
	if !st.script || res.exitCode != tierSweepDeclineExit {
		return map[string]bool{}
	}
	return parseTierSweepUnknownTiers(res.output)
}

// tierSweepUnreachedAfterDeclined names the stage's tiers a declined tier left
// unrun. The script runs its tiers one at a time and exits 2 at a tier it does
// not offer, so a decline ends the stage: any other tier with no summary line
// was never judged, whichever order the rig's script runs its list in, and it
// leaves the cycle's results as the declined tier does (gt-bdva9). The caller
// reads this only for a tier with no line, so a tier that did report a verdict
// keeps it. A stage with no declined tier of its own reaches nothing (gt-zlcl6).
func tierSweepUnreachedAfterDeclined(tiers []string, declined map[string]bool) map[string]bool {
	out := map[string]bool{}
	ended := false
	for _, tier := range tiers {
		if declined[tier] {
			ended = true
			break
		}
	}
	if !ended {
		return out
	}
	for _, tier := range tiers {
		if !declined[tier] {
			out[tier] = true
		}
	}
	return out
}

// parseTierSweepSummaries reads the verdicts a stage's output reported, keyed
// by tier. The summary's "(logs <dir>)" group is the script's log directory,
// kept in LogDir as well as Log: the caller replaces Log with the path of the
// stage log it writes, and the unit logs are only findable through the
// directory (gt-j6vlz).
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
			LogDir:      m[7],
		}
	}
	return out
}

// tierSweepStageReported reads the verdicts one stage's output reported. The
// shell stage's output is the runner's, so it is read through
// tiersweep.ShellVerdict, the same rule the sweep's own shell tier applies. A
// non-zero exit overrules a GREEN summary line (the runner's exit 2 is a usage
// or log-dir failure). That GREEN line is withheld, so the tier is unreported,
// and the caller records a tier with no verdict as RED.
//
// The integration and race stage is read the same way, with two exceptions.
// The RED exit (tierSweepRedExit) beside a RED line stands: the command exits 1
// when any tier it ran was RED, so that RED is what the exit reports and the
// GREEN line beside it is the other tier's verdict (gt-xyigz). And the rig's
// script exits 2 at a tier it does not offer, after the tiers before it have
// printed their verdicts, and those verdicts stand (gt-qgrdy). Any other exit
// withholds GREEN, and so does an unknown-tier line under exit 0
// (tierSweepExitStands).
func tierSweepStageReported(st tierSweepStage, res tierSweepStageResult) map[string]tierSweepTierResult {
	if st.runner == "" {
		reported := parseTierSweepSummaries(res.output)
		if !tierSweepExitStands(st, res, reported) {
			withholdTierSweepGreen(reported)
		}
		return reported
	}
	summary, _, red := tiersweep.ShellVerdict(res.output, res.exitCode)
	reported := parseTierSweepSummaries(summary)
	if red {
		withholdTierSweepGreen(reported)
	}
	return reported
}

// tierSweepRedExit is the one exit status cmd/tier-sweep reports for a tier that
// came back RED: it returns 1 after the last tier's line when any tier was RED,
// and takes 2 only for a command line, log directory or tier it cannot handle
// (cmd/tier-sweep/main.go:8-10, internal/tiersweep/sweep.go:87-94,
// internal/tiersweep/options.go:50). The rig's own script follows the same two
// codes. Any other status, a signal's -1 included, is not a tier's verdict.
const tierSweepRedExit = 1

// tierSweepExitStands reports whether a non-shell stage's exit leaves its GREEN
// verdicts standing. A declined stage stands (tierSweepDeclined), and so does
// the RED exit when the output accounts for it with a RED line: the stage is one
// command for two tiers, so tierSweepRedExit over a RED integration says nothing
// about the race tier that reported GREEN beside it (gt-xyigz). Only that status
// counts as the RED exit; 2 and a signal's -1 withhold GREEN whatever the output
// printed. With no RED line to account for it, only exit 0 stands, and only when
// the output names no unknown tier: the script declines by exiting 2, so a
// script that prints the decline line and exits 0 is not the decline shape, and
// its GREEN verdicts are withheld.
func tierSweepExitStands(st tierSweepStage, res tierSweepStageResult, reported map[string]tierSweepTierResult) bool {
	if len(tierSweepDeclined(st, res)) > 0 {
		return true
	}
	if res.exitCode == tierSweepRedExit && tierSweepAnyRed(reported) {
		return true
	}
	return res.exitCode == 0 && len(parseTierSweepUnknownTiers(res.output)) == 0
}

// tierSweepAnyRed reports whether a tier's own line came back RED.
func tierSweepAnyRed(reported map[string]tierSweepTierResult) bool {
	for _, ts := range reported {
		if ts.Verdict == tierSweepRed {
			return true
		}
	}
	return false
}

// withholdTierSweepGreen drops every GREEN verdict from reported, so the tiers
// they named are unreported and the caller records them RED.
func withholdTierSweepGreen(reported map[string]tierSweepTierResult) {
	for tier, ts := range reported {
		if ts.Verdict == tierSweepGreen {
			delete(reported, tier)
		}
	}
}

// tierSweepBeadPrefix keys an open sweep bead for one rig and tier.
func tierSweepBeadPrefix(rig, tier string) string {
	return tierSweepRigBeadPrefix(rig) + tier + ": "
}

// tierSweepRigBeadPrefix is the title prefix every sweep bead for rig shares.
func tierSweepRigBeadPrefix(rig string) string {
	return fmt.Sprintf("tier sweep (%s): ", rig)
}

// tierSweepBeads opens the rig's writable bead store, or nil when none opens.
func (d *Daemon) tierSweepBeads(rig string) tierSweepBeadStore {
	if open := d.tierSweepSeams.beads; open != nil {
		return open(rig)
	}
	rigPath := filepath.Join(d.config.TownRoot, rig)
	return beads.NewPinned(beads.ResolveBeadsDir(rigPath), beads.WithBin(d.bdPathOrDefault()))
}

// tierSweepRecordBeads files one bead per failing unit of a RED tier, and
// closes a GREEN tier's open beads, never one a polecat has claimed. A
// bead-store error is logged and the sweep continues: the verdict is already
// recorded. A tier whose open beads cannot be listed is left alone, since
// filing without knowing which units already have a bead would open a
// duplicate P1 for each of them.
func (d *Daemon) tierSweepRecordBeads(rig, sha string, results map[string]tierSweepTierResult) {
	bd := d.tierSweepBeads(rig)
	if bd == nil {
		return
	}
	for tier, ts := range results {
		existing, err := tierSweepOpenBeads(bd, rig, tier)
		if err != nil {
			d.logger.Printf("tier_sweep: %s: listing %s beads for the %s tier, leaving it unrecorded: %v", rig, tierSweepRedLabel, tier, err)
			continue
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

// tierSweepUnitBead is the sweep bead standing for one failing unit.
type tierSweepUnitBead struct {
	id string
	// claimed is true when a polecat holds the bead (in progress or hooked).
	claimed bool
	// issue is the bead as the list read it, so a red run can tell whether
	// its text is in shape.
	issue *beads.Issue
}

// tierSweepOpenBeads maps the failing unit each unlanded sweep bead for rig and
// tier was filed for to its bead. A bead a polecat has claimed is in progress
// or hooked, not open, and still stands for the same failure
// (tierSweepRedBeadsOpen counts the same three statuses). When one unit has
// beads under several statuses the open one wins. A status list that fails
// fails the whole lookup: with one status unread the sweep cannot tell which
// units already have a bead.
func tierSweepOpenBeads(bd tierSweepBeadStore, rig, tier string) (map[string]tierSweepUnitBead, error) {
	prefix := tierSweepBeadPrefix(rig, tier)
	out := map[string]tierSweepUnitBead{}
	for _, status := range []string{"open", string(beads.StatusInProgress), beads.StatusHooked} {
		issues, err := bd.List(beads.ListOptions{Status: status, Label: tierSweepRedLabel, Priority: -1, Limit: 0})
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is == nil {
				continue
			}
			if name, ok := strings.CutPrefix(is.Title, prefix); ok && name != "" {
				if _, seen := out[name]; !seen {
					out[name] = tierSweepUnitBead{id: is.ID, claimed: status != "open", issue: is}
				}
			}
		}
	}
	return out, nil
}

// tierSweepFileRed comments on the open bead for name, else files one. The
// pattern is the red-main owner's fileOrComment (internal/landworker/redmain.go).
func tierSweepFileRed(d *Daemon, bd tierSweepBeadStore, rig, tier, name, sha string, ts tierSweepTierResult, open tierSweepUnitBead) {
	detail := tierSweepRedDetail(tier, sha, ts)
	if open.id != "" {
		if err := bd.AddComment(open.id, "still red: "+detail); err != nil {
			d.logger.Printf("tier_sweep: %s: commenting on %s: %v", rig, open.id, err)
		}
		if !open.claimed {
			d.tierSweepReshapeOpen(bd, rig, tier, name, sha, ts, open)
		}
		return
	}
	description, acceptance := tierSweepBeadShape(tier, name, sha, ts, d.tierSweepTemplate())
	if _, err := bd.Create(beads.CreateOptions{
		Title:       tierSweepBeadPrefix(rig, tier) + name,
		Labels:      []string{tierSweepRedLabel},
		Priority:    1,
		Description: description,
		Acceptance:  acceptance,
	}); err != nil {
		d.logger.Printf("tier_sweep: %s: filing a bead for %s %s: %v", rig, tier, name, err)
	}
}

// tierSweepCloseGreen closes the open beads a GREEN tier no longer needs. A
// bead a polecat has claimed is left to its owner: closing it would pull the
// bead out from under the polecat working it.
func tierSweepCloseGreen(d *Daemon, bd tierSweepBeadStore, rig, tier, sha string, byUnit map[string]tierSweepUnitBead) {
	for name, b := range byUnit {
		if b.claimed {
			continue
		}
		reason := fmt.Sprintf("%s tier green on main at %s (tier sweep)", tier, shortSHA(sha))
		if err := bd.CloseWithReason(reason, b.id); err != nil {
			d.logger.Printf("tier_sweep: %s: closing %s (%s %s): %v", rig, b.id, tier, name, err)
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
