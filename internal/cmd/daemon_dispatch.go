package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Idle-seat dispatch check (gt-59o9).
//
// The mayor is event-driven: it wakes on a slot opening, an escalation, or
// mail. Once it has declined to dispatch and no polecat is running, no further
// slot ever opens, so nothing wakes it again — on 2026-09-21 the town sat idle
// for 5.5 hours with 372 ready beads in gastown alone. The daemon's
// mayor_dispatch patrol runs this check on a cadence and nudges the mayor when
// seats are free and there is work to take them.
//
// The numbers live here rather than in the daemon because the seat model does:
// polecat_pool's own accounting (sling_pool.go) and the landing-queue depth
// rule (sling_backpressure.go) are both in this package, and internal/cmd imports
// internal/daemon, so the dependency cannot run the other way. The daemon
// shells out to `gt daemon dispatch-check --json`.

const (
	// defaultDispatchLandingCeiling is the landing-queue depth (open
	// gt:ready-to-land beads) above which the check stops asking the mayor to
	// sling into a rig. It is the operator's stated rule ("skip rigs with >12
	// beads waiting to land"), used when the rig does not set
	// merge_queue.max_ready_for_dispatch of its own — that knob, where set, is
	// the rig's own answer and wins. The key keeps its merge_queue name so
	// existing settings files still load.
	defaultDispatchLandingCeiling = 12
)

// dispatchPriorityCeiling is the operator's ceiling on a dispatchable bead's
// priority number: polecat_pool.max_priority, default 2 (P0-P2). Everything
// numbered higher is backlog, and a nudge that leads with backlog is a nudge
// the mayor learns to ignore.
//
// It is policy, not a constant (gt-h2kyc): the same key bounds the spec
// dispatcher's candidate intake (internal/cmd/spec.go), so raising it moves
// both the work the dispatcher takes and the work this check names.
func dispatchPriorityCeiling(townRoot string) (int, error) {
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return 0, fmt.Errorf("loading town settings for the dispatch ceiling: %w", err)
	}
	return settings.PolecatPool.GetMaxPriority(), nil
}

// dispatchSeats is the town's polecat-seat picture: how many polecats could be
// spawned right now.
type dispatchSeats struct {
	// Source names which seat model produced these numbers, or "none" when the
	// town configures no seat model at all.
	Source string `json:"source"`

	// Capacity is the number of seats the model admits.
	Capacity int `json:"capacity"`

	// Occupied is how many of them live polecat sessions hold.
	Occupied int `json:"occupied"`

	// Free is Capacity - Occupied, floored at zero.
	Free int `json:"free"`

	// Uncapped marks a pool whose seat has no cap: such a seat is always
	// available, so Free is reported as at least one.
	Uncapped bool `json:"uncapped,omitempty"`
}

// dispatchRig is one rig's side of the picture: how much dispatchable work it
// holds, and whether its landing queue can absorb more.
type dispatchRig struct {
	Rig string `json:"rig"`

	// Ready counts actionable ready beads at or above the priority ceiling.
	Ready int `json:"ready"`

	// Urgent is the P0/P1 subset of Ready. The split matters: a rig with 200
	// P2 beads is not the same signal as a rig with one P1.
	Urgent int `json:"urgent"`

	// ReadyToLand is the rig's landing-queue depth, measured the way the
	// sling backpressure guard measures it. Zero when the queue could not be
	// read — an unreadable queue is not evidence of a full one. The JSON name
	// predates the landing worker and is kept for the daemon's reader.
	ReadyToLand int `json:"ready_mrs"`

	// LandingCeiling is the depth at which this rig stops being a dispatch
	// target.
	LandingCeiling int `json:"mr_ceiling"`

	// Backpressure marks a rig over its ceiling: named in the nudge, never
	// counted as a reason to nudge.
	Backpressure bool `json:"backpressure"`

	// Parked marks a rig that is parked or docked. Not a dispatch target at
	// all, so it is left out of the picture entirely. ParkReason says which of
	// the two, since they are released by different commands.
	Parked     bool   `json:"parked,omitempty"`
	ParkReason string `json:"park_reason,omitempty"`
}

// dispatchCheck is what `gt daemon dispatch-check --json` prints.
type dispatchCheck struct {
	Seats dispatchSeats `json:"seats"`
	Rigs  []dispatchRig `json:"rigs"`

	// MaxPriority is the ceiling the ready counts were taken at
	// (polecat_pool.max_priority), so a reader can tell P0-P2 from P0-P4.
	MaxPriority int `json:"max_priority"`

	// Actionable is the total ready work in rigs that can absorb it.
	Actionable int `json:"actionable"`

	// Nudge is the decision. Message is the nudge text, empty when Nudge is
	// false.
	Nudge   bool   `json:"nudge"`
	Message string `json:"message,omitempty"`
}

var (
	daemonDispatchJSON bool
)

var daemonDispatchCheckCmd = &cobra.Command{
	Use:   "dispatch-check",
	Short: "Report free polecat seats and actionable ready work",
	Long: `Report whether the town has free polecat seats and work to fill them.

The daemon's mayor_dispatch patrol runs this on a cadence and nudges the mayor
when the answer is yes. The mayor is event-driven — a "no dispatch" decision
opens no slot and wakes it again — so without a timer on the seat picture the
town can idle indefinitely with ready work on the board.

Seats come from polecat_pool (the admission point every spawn path reads), or
from scheduler.max_polecats when no pool is configured. Work counts actionable
ready beads per rig up to polecat_pool.max_priority (default P2), excluding bead
families that are bookkeeping rather than dispatchable work (agent beads,
escalations, merge slots, messages, epics) and rigs whose landing queue is
already deeper than the rule allows.

Examples:
  gt daemon dispatch-check            # human-readable
  gt daemon dispatch-check --json     # the shape the daemon patrol consumes`,
	RunE: runDaemonDispatchCheck,
}

func init() {
	daemonCmd.AddCommand(daemonDispatchCheckCmd)
	daemonDispatchCheckCmd.Flags().BoolVar(&daemonDispatchJSON, "json", false, "Output as JSON")
}

func runDaemonDispatchCheck(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	check, err := buildDispatchCheck(townRoot)
	if err != nil {
		return err
	}

	if daemonDispatchJSON {
		out, err := json.MarshalIndent(check, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	printDispatchCheck(check)
	return nil
}

// buildDispatchCheck gathers the seat picture, each rig's ready work, and the
// decision that follows from them.
func buildDispatchCheck(townRoot string) (*dispatchCheck, error) {
	seats, err := dispatchSeatPicture(townRoot)
	if err != nil {
		return nil, err
	}
	ceiling, err := dispatchPriorityCeiling(townRoot)
	if err != nil {
		return nil, err
	}

	rigs, err := dispatchRigPictures(townRoot, ceiling)
	if err != nil {
		return nil, err
	}

	check := &dispatchCheck{Seats: seats, Rigs: rigs, MaxPriority: ceiling}
	for _, rig := range rigs {
		if rig.Parked || rig.Backpressure {
			continue
		}
		check.Actionable += rig.Ready
	}
	check.Nudge, check.Message = dispatchDecision(seats, rigs, ceiling)
	return check, nil
}

// dispatchDecision is the patrol's whole policy, in one pure function: nudge,
// or stay silent. It returns the nudge text when it fires and "" when it does
// not. maxPriority is the ceiling the rigs' ready counts were taken at, named
// in the nudge so the mayor reads the same number the check counted.
//
// It stays silent for exactly three reasons, and each one is a nudge that
// would have been wrong: no seat model is configured (the town has no answer
// to "is a seat free?"), no seat is free (nothing could take the work), or
// every rig with ready work is over its landing-queue ceiling (the work could
// not land any sooner). Silence is the default, because a cadence that fires
// without news is a cadence the mayor learns to ignore.
func dispatchDecision(seats dispatchSeats, rigs []dispatchRig, maxPriority int) (bool, string) {
	if seats.Free < 1 {
		return false, ""
	}

	// Rigs worth naming: parked ones are not dispatch targets, and a
	// backpressured one is named but marked — "the board is empty" and "the
	// board is full but the landing queue is deeper than the rule allows" are different
	// answers, and the mayor needs to be able to tell them apart.
	var named []string
	var held []string
	actionable := 0
	for _, rig := range rigs {
		if rig.Parked || rig.Ready == 0 {
			continue
		}
		if rig.Backpressure {
			held = append(held, fmt.Sprintf("%s=%d waiting to land (ceiling %d)", rig.Rig, rig.ReadyToLand, rig.LandingCeiling))
			continue
		}
		actionable += rig.Ready
		named = append(named, rig.describe())
	}
	if actionable == 0 {
		return false, ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Idle-seat check: %d of %d polecat seats are free (%d working).",
		seats.Free, seats.Capacity, seats.Occupied)
	fmt.Fprintf(&b, " Actionable P0-P%d ready beads: %s.", maxPriority, strings.Join(named, ", "))
	if len(held) > 0 {
		fmt.Fprintf(&b, " Held by landing-queue depth: %s.", strings.Join(held, ", "))
	}
	b.WriteString(" Sling now, or mail the overseer why not — idle seats are a fault.")
	return true, b.String()
}

// describe renders one rig for the nudge: its ready count, and the P0/P1
// subset when it has one. The subset is shown only when non-zero — "(P0/P1 0)"
// on every rig in a five-rig town is noise, and the zero case is exactly the
// one the mayor is scanning past.
func (r dispatchRig) describe() string {
	if r.Urgent == 0 {
		return fmt.Sprintf("%s=%d", r.Rig, r.Ready)
	}
	return fmt.Sprintf("%s=%d (P0/P1 %d)", r.Rig, r.Ready, r.Urgent)
}

// dispatchSeatPicture reports the seats the town would admit a spawn into.
// Two models can gate a spawn — polecat_pool (the admission point every spawn
// path reads, gt-4lbz) and scheduler.max_polecats (polecat_capacity.go) — and
// a town that configures both is bound by the tighter one, so that is the one
// reported.
func dispatchSeatPicture(townRoot string) (dispatchSeats, error) {
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return dispatchSeats{}, fmt.Errorf("loading town settings for dispatch seats: %w", err)
	}

	var sessions []poolSession
	if settings.PolecatPool != nil && settings.PolecatPool.OverflowAgent != "" {
		sessions, err = listPolecatSessions(newPoolSessionLister(), townRoot)
		if err != nil {
			return dispatchSeats{}, fmt.Errorf("listing polecat sessions for dispatch seats: %w", err)
		}
	}

	pool := poolSeatPicture(settings.PolecatPool, sessions)
	scheduled, err := schedulerSeatPicture(townRoot)
	if err != nil {
		return dispatchSeats{}, err
	}

	switch {
	case pool.Source == "":
		return scheduled, nil
	case scheduled.Source == "":
		return pool, nil
	case scheduled.Free < pool.Free:
		scheduled.Source = pool.Source + "+" + scheduled.Source
		return scheduled, nil
	default:
		pool.Source = pool.Source + "+" + scheduled.Source
		return pool, nil
	}
}

// poolSeatPicture reports the pool's seats, counting the live sessions the
// caller listed. Source is empty when no pool is configured, which is the
// signal that this model has nothing to say.
func poolSeatPicture(pool *config.PolecatPool, sessions []poolSession) dispatchSeats {
	if pool == nil || pool.OverflowAgent == "" {
		return dispatchSeats{}
	}

	// An uncapped seat is unbounded room, so reporting it as one free seat is
	// a floor rather than a count — the honest answer to "could a sling spawn
	// right now?" without pretending the pool has a size it does not.
	capped := pool.OverflowCapped()
	capacity := 0
	if capped {
		capacity = pool.MaxOverflow
	}
	seats := dispatchSeats{
		Source:   "polecat_pool",
		Capacity: capacity,
		Occupied: poolSeatCount(pool, sessions),
		Uncapped: !capped,
	}
	seats.Free = capacity - seats.Occupied
	if seats.Free < 0 {
		seats.Free = 0
	}
	if seats.Uncapped && seats.Free < 1 {
		seats.Free = 1
	}
	return seats
}

// schedulerSeatPicture reports the scheduler capacity model's seats. Source is
// empty when scheduler.max_polecats is unset, which is the common case: the
// default is -1, meaning no cap.
func schedulerSeatPicture(townRoot string) (dispatchSeats, error) {
	max, err := configuredSchedulerMaxPolecats(townRoot)
	if err != nil {
		return dispatchSeats{}, err
	}
	if max <= 0 {
		return dispatchSeats{}, nil
	}

	snapshot, err := polecatCapacitySnapshotForTown(townRoot)
	if err != nil {
		return dispatchSeats{}, fmt.Errorf("reading polecat capacity: %w", err)
	}
	return dispatchSeats{
		Source:   "scheduler",
		Capacity: snapshot.Max,
		Occupied: snapshot.occupied(),
		Free:     snapshot.Free,
	}, nil
}

// dispatchRigPictures reports every known rig's dispatchable work, counted at
// the operator's priority ceiling.
func dispatchRigPictures(townRoot string, maxPriority int) ([]dispatchRig, error) {
	names, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}

	rigs := make([]dispatchRig, 0, len(names))
	for _, name := range names {
		rigPath := filepath.Join(townRoot, name)
		if _, err := os.Stat(rigPath); err != nil {
			// A rig in the registry with no directory on disk is not a
			// dispatch target and not a reason to fail the check.
			continue
		}

		rig := dispatchRig{Rig: name}
		if parked, reason := IsRigParkedOrDocked(townRoot, name); parked {
			// Parked and docked rigs are not dispatch targets, so their beads
			// are neither counted nor queried: a parked rig's backlog is
			// backed up by intent, and reporting it would ask the mayor to
			// sling into a rig the sling path itself refuses (gt-59o9).
			rig.Parked = true
			rig.ParkReason = reason
			rigs = append(rigs, rig)
			continue
		}

		// Pass the rig's revert in flight, so a red-main bead the owner is
		// already undoing is not counted as work to sling (gt-1fiv4). A stale
		// record is resolved away first, or a crashed revert would hold the
		// rig's beads out of the count forever (gt-wgyca).
		live, _ := specdispatch.ResolveRevert(rigRevertInFlightPinned(townRoot, name), time.Now())
		ready, urgent, err := countActionableReady(rigPath, maxPriority, live)
		if err != nil {
			return nil, fmt.Errorf("counting ready work for %s: %w", name, err)
		}
		rig.Ready, rig.Urgent = ready, urgent
		rig.ReadyToLand, rig.LandingCeiling = rigLandingQueueDepth(rigPath, name, newDispatchMRLister(rigPath))
		rig.Backpressure = rig.ReadyToLand > rig.LandingCeiling
		rigs = append(rigs, rig)
	}

	// Most work first: the nudge leads with the rig the mayor should look at.
	sort.SliceStable(rigs, func(i, j int) bool {
		if rigs[i].Parked != rigs[j].Parked {
			return !rigs[i].Parked
		}
		return rigs[i].Ready > rigs[j].Ready
	})
	return rigs, nil
}

// knownRigNames reads the rig registry.
func knownRigNames(townRoot string) ([]string, error) {
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading rigs config for dispatch check: %w", err)
	}
	names := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// countActionableReady counts the rig's actionable ready beads up to the
// operator's priority ceiling, and the P0/P1 subset of them. rv is the revert
// the rig's red-main owner has in flight, which holds that rig's red-main beads
// out of the count (isActionableReadyBead).
//
// A read failure is returned, not swallowed. Under-reporting ready work is the
// one error this check cannot absorb: it produces exactly the silence the
// patrol exists to break, and produces it invisibly. A rig with no beads
// database at all is not a failure — it has no ready work to report.
func countActionableReady(rigPath string, maxPriority int, rv *specdispatch.Revert) (ready, urgent int, err error) {
	issues, err := readyIssuesUnlimited(rigPath, readyBoardFor)
	if err != nil {
		return 0, 0, err
	}
	for _, issue := range filterIdentityBeads(issues) {
		if !isActionableReadyBead(issue, maxPriority, rv) {
			continue
		}
		ready++
		if issue.Priority < 2 {
			urgent++
		}
	}
	return ready, urgent, nil
}

// readyBoard reads a rig's whole ready board.
type readyBoard interface {
	ReadyAll() ([]*beads.Issue, error)
}

// readyBoardFor returns the bd client readyIssuesUnlimited reads a rig's
// board through.
func readyBoardFor(rigPath string) readyBoard {
	return beads.New(rigPath)
}

// readyIssuesUnlimited returns every ready issue in the rig.
//
// It reads through ReadyAll: one machine-mode bd ready --limit 0 whose
// envelope reports whether bd cut the page, rather than Ready(), which
// inherits bd's default limit of 100. The board this check counts was 373
// beads the day it was found reading as 100, and the count is the whole point
// of the check (gt-59o9). A truncated answer is an error, never a count.
//
// A rig with no beads database returns no issues: it is a rig that has never
// been initialized, not a read failure. boardFor opens the rig's board
// (readyBoardFor).
func readyIssuesUnlimited(rigPath string, boardFor func(rigPath string) readyBoard) ([]*beads.Issue, error) {
	if !hasBeadsDatabase(beads.ResolveBeadsDir(rigPath)) {
		return nil, nil
	}
	issues, err := boardFor(rigPath).ReadyAll()
	if err != nil {
		return nil, fmt.Errorf("reading the ready board for %s: %w", rigPath, err)
	}
	return issues, nil
}

// hasBeadsDatabase reports whether a resolved beads directory holds a database
// this check could read.
//
// It is a filesystem test on purpose. The one error this check cannot absorb is
// under-reporting ready work, so an unreachable Dolt server must still surface
// as a read failure; asking whether a database is *configured* keeps the two
// apart (gt-ka00).
func hasBeadsDatabase(beadsDir string) bool {
	// Embedded mode: the data directory sits beside the config, and a .beads
	// directory carrying one need not carry a metadata.json to name it. bd
	// writes embeddeddolt/<database>/.dolt; dolt/ is the older layout.
	for _, embedded := range []string{"dolt", "embeddeddolt"} {
		if info, err := os.Stat(filepath.Join(beadsDir, embedded)); err == nil && info.IsDir() {
			return true
		}
	}

	// Server mode: metadata.json names the database and the server keeps it in
	// the town's .dolt-data/. A metadata.json tracked from another workspace
	// names a database this server may not have, which is the same empty rig
	// as one that was never initialized.
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json")) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return false
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return true // Unparseable — assume initialized, matching bdDatabaseExists
	}
	db := townconfig.DatabaseForBeadsDir(beadsDir)
	if meta.DoltMode != "server" || db == "" {
		return true // Not a server-mode database reference — assume initialized
	}
	townRoot := beads.FindTownRoot(filepath.Dir(beadsDir))
	if townRoot == "" {
		return true // No town to look in — assume initialized
	}
	_, err = os.Stat(filepath.Join(townRoot, ".dolt-data", db))
	return !os.IsNotExist(err)
}

// patrolSuppressedTitlePrefixes are notification envelopes this patrol does not
// count as dispatchable work, over and above beads.IsNonDispatchableBead.
//
// These are notices *about* work, not a kind of work, and they stay out of the
// shared predicate because the two callers can afford different mistakes here.
// A missed nudge costs silence until the mayor next reads the board; a hidden
// row costs the work itself, and the board still shows a "main_branch_test:
// <diagnosis>" bug — a real one is filed, gt-59yz — and a "STATE_COLLAPSE
// <rig>" notice asking the mayor to reopen and re-dispatch.
var patrolSuppressedTitlePrefixes = []string{
	"STATE_COLLAPSE",
	"[HIGH]",
	"[CRITICAL]",
	"[MEDIUM]",
	"main_branch_test:",
}

// isActionableReadyBead reports whether a ready bead is work the mayor could
// sling: at or above maxPriority's floor, and not one of the town's
// bookkeeping families. rv is the rig's revert in flight, which holds that
// rig's red-main beads.
//
// Epics are excluded as containers. `gt sling` accepts one, but a container's
// children are what a polecat takes, and they appear in ready on their own — a
// nudge naming the epic would point the mayor at the wrong row.
func isActionableReadyBead(issue *beads.Issue, maxPriority int, rv *specdispatch.Revert) bool {
	if issue == nil {
		return false
	}
	if issue.Priority < 0 || issue.Priority > maxPriority {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(issue.Type), "epic") {
		return false
	}
	if beads.IsNonDispatchableBead(issue) {
		return false
	}
	// Work the operator reserved is not the mayor's to sling: naming one here
	// is what sends the mayor into a sling that refuses (gt-21pl0). The rule is
	// the convoy feeders' own, so this check and the spec dispatcher's draw the
	// same line the sling guard does.
	if dispatch.OperatorReservation(issue.Labels, issue.Assignee) != "" {
		return false
	}
	// A red-main bead is the fix forward for a breakage the rig's owner is
	// already undoing; counting it here nudges the mayor to sling the very fix
	// the revert supersedes (gt-1fiv4). The rule is the spec dispatcher's own
	// (specdispatch.RedMainHold), so the patrol and the dispatcher hold the same
	// beads (gt-zkdwt).
	if specdispatch.RedMainHold(specdispatch.Spec{Labels: issue.Labels}, rv) != "" {
		return false
	}

	title := strings.TrimSpace(issue.Title)
	for _, prefix := range patrolSuppressedTitlePrefixes {
		if strings.HasPrefix(title, prefix) {
			return false
		}
	}
	return true
}

// rigLandingQueueDepth reports the rig's count of beads waiting to land and
// the ceiling it is measured against. The ceiling is the rig's own
// merge_queue.max_ready_for_dispatch when set, else the operator's default.
//
// An unreadable queue reports zero depth, mirroring the sling backpressure
// guard's fail-open: a queue we cannot read is not evidence of a queue that is
// full, and suppressing the nudge on a Dolt hiccup would silence the patrol
// for the one reason it exists.
func rigLandingQueueDepth(rigPath, rigName string, lister dispatchMRLister) (ready, ceiling int) {
	ceiling = defaultDispatchLandingCeiling
	if configured := rig.ResolveMergeQueueConfig(filepath.Dir(rigPath), rigName).GetMaxReadyForDispatch(); configured > 0 {
		ceiling = configured
	}
	ready, err := countReadyToLand(lister, rigName)
	if err != nil {
		return 0, ceiling
	}
	return ready, ceiling
}

// printDispatchCheck renders the check for a human: the seat line, one line per
// rig, and the nudge text when there is one.
func printDispatchCheck(check *dispatchCheck) {
	if check.Seats.Source == "" || check.Seats.Source == "none" {
		fmt.Println("Seats: unknown — no polecat pool and no scheduler.max_polecats configured.")
	} else {
		fmt.Printf("Seats: %d of %d free (%d occupied) via %s\n",
			check.Seats.Free, check.Seats.Capacity, check.Seats.Occupied, check.Seats.Source)
	}
	fmt.Printf("Actionable P0-P%d ready: %d\n", check.MaxPriority, check.Actionable)

	for _, rig := range check.Rigs {
		switch {
		case rig.Parked:
			fmt.Printf("  %-10s %s — not a dispatch target\n", rig.Rig, rig.ParkReason)
		case rig.Backpressure:
			fmt.Printf("  %-10s %3d ready (P0/P1 %d), %d waiting to land > ceiling %d — held\n",
				rig.Rig, rig.Ready, rig.Urgent, rig.ReadyToLand, rig.LandingCeiling)
		default:
			fmt.Printf("  %-10s %3d ready (P0/P1 %d), %d waiting to land\n",
				rig.Rig, rig.Ready, rig.Urgent, rig.ReadyToLand)
		}
	}

	if check.Nudge {
		fmt.Printf("\nNUDGE:\n%s\n", check.Message)
		return
	}
	fmt.Println("\nNo nudge: seats are full, work is exhausted, or every rig with work is holding.")
}
