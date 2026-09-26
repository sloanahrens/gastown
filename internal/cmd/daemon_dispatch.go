package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
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
// polecat_pool's own accounting (sling_pool.go) and the merge-queue depth rule
// (sling_backpressure.go) are both in this package, and internal/cmd imports
// internal/daemon, so the dependency cannot run the other way. The daemon
// shells out to `gt daemon dispatch-check --json`, the way quota_resume shells
// out to `gt quota resume` (gt-749e).

const (
	// defaultDispatchReadyMRCeiling is the merge-queue depth above which the
	// check stops asking the mayor to sling into a rig. It is the operator's
	// stated rule ("skip rigs whose merge queue has >12 ready MRs"), used when
	// the rig does not set merge_queue.max_ready_for_dispatch of its own —
	// that knob, where set, is the rig's own answer and wins.
	defaultDispatchReadyMRCeiling = 12

	// maxDispatchPriority is the lowest priority treated as actionable: P0-P2.
	// Everything below is backlog, and a nudge that leads with backlog is a
	// nudge the mayor learns to ignore.
	maxDispatchPriority = 2

	// dispatchStoreTimeout bounds one rig's in-process ready query.
	dispatchStoreTimeout = 30 * time.Second
)

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

	// Uncapped marks a pool whose overflow seat has no cap: such a seat is
	// always available, so Free is reported as at least one.
	Uncapped bool `json:"uncapped,omitempty"`
}

// dispatchRig is one rig's side of the picture: how much dispatchable work it
// holds, and whether its merge queue can absorb more.
type dispatchRig struct {
	Rig string `json:"rig"`

	// Ready counts actionable P0-P2 ready beads.
	Ready int `json:"ready"`

	// Urgent is the P0/P1 subset of Ready. The split matters: a rig with 200
	// P2 beads is not the same signal as a rig with one P1.
	Urgent int `json:"urgent"`

	// ReadyMRs is the rig's merge-queue depth, measured the way the sling
	// backpressure guard measures it. Zero when the queue could not be read —
	// an unreadable queue is not evidence of a full one.
	ReadyMRs int `json:"ready_mrs"`

	// MRCeiling is the depth at which this rig stops being a dispatch target.
	MRCeiling int `json:"mr_ceiling"`

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
P0-P2 ready beads per rig, excluding bead families that are bookkeeping rather
than dispatchable work (agent beads, escalations, merge slots, messages, epics)
and rigs whose merge queue is already deeper than the rule allows.

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

	rigs, err := dispatchRigPictures(townRoot)
	if err != nil {
		return nil, err
	}

	check := &dispatchCheck{Seats: seats, Rigs: rigs}
	for _, rig := range rigs {
		if rig.Parked || rig.Backpressure {
			continue
		}
		check.Actionable += rig.Ready
	}
	check.Nudge, check.Message = dispatchDecision(seats, rigs)
	return check, nil
}

// dispatchDecision is the patrol's whole policy, in one pure function: nudge,
// or stay silent. It returns the nudge text when it fires and "" when it does
// not.
//
// It stays silent for exactly three reasons, and each one is a nudge that
// would have been wrong: no seat model is configured (the town has no answer
// to "is a seat free?"), no seat is free (nothing could take the work), or
// every rig with ready work is over its merge-queue ceiling (the work could
// not land any sooner). Silence is the default, because a cadence that fires
// without news is a cadence the mayor learns to ignore.
func dispatchDecision(seats dispatchSeats, rigs []dispatchRig) (bool, string) {
	if seats.Free < 1 {
		return false, ""
	}

	// Rigs worth naming: parked ones are not dispatch targets, and a
	// backpressured one is named but marked — "the board is empty" and "the
	// board is full but the queue is deeper than the rule allows" are different
	// answers, and the mayor needs to be able to tell them apart.
	var named []string
	var held []string
	actionable := 0
	for _, rig := range rigs {
		if rig.Parked || rig.Ready == 0 {
			continue
		}
		if rig.Backpressure {
			held = append(held, fmt.Sprintf("%s=%d ready MRs (ceiling %d)", rig.Rig, rig.ReadyMRs, rig.MRCeiling))
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
	fmt.Fprintf(&b, " Actionable P0-P2 ready beads: %s.", strings.Join(named, ", "))
	if len(held) > 0 {
		fmt.Fprintf(&b, " Held by merge-queue depth: %s.", strings.Join(held, ", "))
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
	if settings.PolecatPool != nil && settings.PolecatPool.LocalAgent != "" {
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
	if pool == nil || pool.LocalAgent == "" {
		return dispatchSeats{}
	}

	local, overflow, _ := poolSeatCounts(pool, sessions)

	// The overflow seat counts toward capacity only when the pool bounds it.
	// An uncapped overflow seat is unbounded room, so reporting it as one free
	// seat is a floor rather than a count — the honest answer to "could a sling
	// spawn right now?" without pretending the pool has a size it does not.
	capped := pool.OverflowCapped()
	capacity := pool.MaxLocal
	if capped {
		capacity += pool.MaxOverflow
	}
	seats := dispatchSeats{
		Source:   "polecat_pool",
		Capacity: capacity,
		Occupied: local + overflow,
	}
	if !capped && pool.OverflowAgent != "" {
		seats.Uncapped = true
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

// dispatchRigPictures reports every known rig's dispatchable work.
func dispatchRigPictures(townRoot string) ([]dispatchRig, error) {
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

		ready, urgent, err := countActionableReady(rigPath)
		if err != nil {
			return nil, fmt.Errorf("counting ready work for %s: %w", name, err)
		}
		rig.Ready, rig.Urgent = ready, urgent
		rig.ReadyMRs, rig.MRCeiling = rigMergeQueueDepth(rigPath, name)
		rig.Backpressure = rig.ReadyMRs > rig.MRCeiling
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

// countActionableReady counts the rig's actionable P0-P2 ready beads, and the
// P0/P1 subset of them.
//
// A read failure is returned, not swallowed. Under-reporting ready work is the
// one error this check cannot absorb: it produces exactly the silence the
// patrol exists to break, and produces it invisibly. A rig with no beads
// database at all is not a failure — it has no ready work to report.
func countActionableReady(rigPath string) (ready, urgent int, err error) {
	issues, err := readyIssuesUnlimited(rigPath)
	if err != nil {
		return 0, 0, err
	}
	for _, issue := range filterIdentityBeads(issues) {
		if !isActionableReadyBead(issue) {
			continue
		}
		ready++
		if issue.Priority < 2 {
			urgent++
		}
	}
	return ready, urgent, nil
}

// openDispatchReadyStore opens the in-process store readyIssuesUnlimited reads
// the board through. It is a var so tests can drive the patrol's board read
// against a store holding more than a page of ready work without a live Dolt.
var openDispatchReadyStore = func(b *beads.Beads, ctx context.Context) (beadsdk.Storage, func(), error) {
	return b.OpenStore(ctx)
}

// readyIssuesUnlimited returns every ready issue in the rig.
//
// It goes through the in-process store rather than beads.Beads.Ready()'s
// subprocess branch, which inherits bd's default limit of 100: the board this
// check counts was 373 beads the day it was found reading as 100, and the count
// is the whole point of the check (gt-59o9).
//
// That store branch is unbounded, so there is no cap for this caller to unwrap;
// see storeReadyWithFilter for why. TestReadyWorkFilterCarriesNoLimit pins the
// invariant — a Limit reintroduced there restores the silent under-report.
//
// A rig with no beads database returns no issues: it is a rig that has never
// been initialized, not a read failure.
func readyIssuesUnlimited(rigPath string) ([]*beads.Issue, error) {
	if !hasBeadsDatabase(beads.ResolveBeadsDir(rigPath)) {
		return nil, nil
	}

	b := beads.New(rigPath)
	ctx, cancel := context.WithTimeout(context.Background(), dispatchStoreTimeout)
	defer cancel()

	store, cleanup, err := openDispatchReadyStore(b, ctx)
	if err != nil {
		return nil, fmt.Errorf("opening beads store for %s: %w", rigPath, err)
	}
	defer cleanup()

	b.SetStore(store)
	return b.Ready()
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
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return true // Unparseable — assume initialized, matching bdDatabaseExists
	}
	if meta.DoltMode != "server" || meta.DoltDatabase == "" {
		return true // Not a server-mode database reference — assume initialized
	}
	townRoot := beads.FindTownRoot(filepath.Dir(beadsDir))
	if townRoot == "" {
		return true // No town to look in — assume initialized
	}
	_, err = os.Stat(filepath.Join(townRoot, ".dolt-data", meta.DoltDatabase))
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
// sling: P0-P2, and not one of the town's bookkeeping families.
//
// Epics are excluded as containers. `gt sling` accepts one, but a container's
// children are what a polecat takes, and they appear in ready on their own — a
// nudge naming the epic would point the mayor at the wrong row.
func isActionableReadyBead(issue *beads.Issue) bool {
	if issue == nil {
		return false
	}
	if issue.Priority < 0 || issue.Priority > maxDispatchPriority {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(issue.Type), "epic") {
		return false
	}
	if beads.IsNonDispatchableBead(issue) {
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

// rigMergeQueueDepth reports the rig's ready-MR count and the ceiling it is
// measured against. The ceiling is the rig's own
// merge_queue.max_ready_for_dispatch when set, else the operator's default.
//
// An unreadable queue reports zero depth, mirroring the sling backpressure
// guard's fail-open: a queue we cannot read is not evidence of a queue that is
// full, and suppressing the nudge on a Dolt hiccup would silence the patrol
// for the one reason it exists.
func rigMergeQueueDepth(rigPath, rigName string) (ready, ceiling int) {
	ceiling = defaultDispatchReadyMRCeiling
	if configured := rig.ResolveMergeQueueConfig(filepath.Dir(rigPath), rigName).GetMaxReadyForDispatch(); configured > 0 {
		ceiling = configured
	}
	ready, err := countReadyMergeRequests(newDispatchMRLister(rigPath), rigName)
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
	fmt.Printf("Actionable P0-P2 ready: %d\n", check.Actionable)

	for _, rig := range check.Rigs {
		switch {
		case rig.Parked:
			fmt.Printf("  %-10s %s — not a dispatch target\n", rig.Rig, rig.ParkReason)
		case rig.Backpressure:
			fmt.Printf("  %-10s %3d ready (P0/P1 %d), %d ready MRs > ceiling %d — held\n",
				rig.Rig, rig.Ready, rig.Urgent, rig.ReadyMRs, rig.MRCeiling)
		default:
			fmt.Printf("  %-10s %3d ready (P0/P1 %d), %d ready MRs\n",
				rig.Rig, rig.Ready, rig.Urgent, rig.ReadyMRs)
		}
	}

	if check.Nudge {
		fmt.Printf("\nNUDGE:\n%s\n", check.Message)
		return
	}
	fmt.Println("\nNo nudge: seats are full, work is exhausted, or every rig with work is holding.")
}
