package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/models"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Spec dispatcher (gt-4k3fj.5, gt-4k3fj.8.8).
//
// `gt spec lint <id>` validates one work bead's shape against the D10 spec
// template and prints one line (--json prints every refusal). `gt spec
// dispatch` is one tick of the dispatcher: every ready, unassigned work bead
// of a dispatchable type (task/bug/feature) across operational rigs, at or
// above the operator's priority ceiling, in priority/created/id order, is
// linted and — when it clears the shape gate and a seat is free — slung through
// executeSling in-process. The label spec and type feature are retired and
// accepted-but-ignored (gt-mmsr2): neither admits or refuses a bead now. The
// daemon's spec_dispatch ticker runs `gt spec dispatch --json` on its cadence
// (internal/cmd imports internal/daemon, so the call cannot go the other way).
//
// This is the dispatcher that replaced the seat-refill plugin (gt-4k3fj.8.8):
// the pool's seats (polecat_pool.seats) and the town's total
// (polecat_pool.max_total, gt-bcpj5), the operator's
// ceiling (polecat_pool.max_priority) and the shape gate
// (polecat_pool.shape_gate) all come from the same keys the plugin read, so the
// town dispatches the same beads it did before the move.
//
// A rig whose red-main owner has a revert in flight holds that rig's red-main
// beads out of the candidate set: a seat spent on the fix forward would race
// the revert over the same package (gt-zkdwt).
//
// A bead the patrol tick readied after its polecat died carries the branch its
// work survives on (patrol_scan's resume_branch note). The dispatch resumes
// that branch — the equivalent of `gt sling --branch` — so the reopened work
// continues where it stopped instead of starting a second polecat from main
// (gt-gzhin.3).
//
// Seat accounting counts every live polecat session plus the in-flight seat
// claims other slings hold, whoever slung them. A hand-run sling lands in that
// same count, so the ticker never pushes past a cap another path already filled
// — it skips the tick instead.

const (
	specDispatchActor       = "daemon/spec-dispatch"
	specDispatchNotePrefix  = "spec-dispatch: "
	defaultSpecMaxPerTick   = 0 // 0: as many as there are free seats
	specLintExitRefused     = 1
	specLintExitNeedsPlan   = 2
	specDispatchCallerLabel = "spec-dispatch"
	// specShapeLabel is what a refuse-mode shape gate labels a bead whose
	// shape the lint rejected; a bead routed to the planner wears
	// specdispatch.NeedsPlanningLabel (gt-cq5gb, carried over from seat-refill).
	specShapeLabel = "needs-shape"
	// specPlanNeedsPlan and specPlanProposed are the two states a spec the
	// lint routes to planning is reported in: no plan proposal yet, and a
	// proposal the operator files by hand (gt-4k3fj.14).
	specPlanNeedsPlan = "needs plan"
	specPlanProposed  = "plan proposed"
	// specStaleReason is the skip reason for a candidate the last-moment
	// re-read before its sling no longer admits: the board's read is a
	// snapshot, so a submission or another holder can take the bead between
	// that read and the seat being spent (gt-01gix).
	specStaleReason = "the ready board read is stale"
)

var (
	specDispatchJSON   bool
	specDispatchDryRun bool
	specLintJSON       bool
	specLintTemplate   string
)

var specCmd = &cobra.Command{
	Use:     "spec",
	GroupID: GroupWork,
	Short:   "Lint and dispatch spec beads (D10 template)",
	RunE:    requireSubcommand,
}

var specLintCmd = &cobra.Command{
	Use:   "lint <bead-id>",
	Short: "Check a work bead's shape against the spec template; exit 0 when clean",
	Long: `Check a work bead's shape against the spec template — the same lint the
spec dispatcher runs before it allocates a seat.

The required "## " sections are the template file's when that file exists:
daemon.patrols.spec_dispatch.template in settings/config.json, else
~/.claude/docs/agents/spec-template.md. No such file is needed — with no
template file, or one carrying no "## " heading, the built-in sections are
checked instead: Goal, Constraints, Out of scope, Gate and Size.

  - every required "## " section present and non-empty
  - 1-6 acceptance items (3-6 preferred)
  - Size is one worker, one MR

Shape is a property of every work bead: the lint checks a task, bug or feature
the same way and never refuses one for its type. The label spec is retired: it
is accepted and ignored. Epics, agent beads (gt:agent) and wisps are refused
as "not a work bead" without reading a shape.

Prints one line naming the bead and the first missing field. --json prints
{id, ok, needs_planning, refusals[]} instead, listing every failure. A human-form
run that does not exit 0 also writes the required skeleton to stderr — every
section with a one-phrase hint and the command that re-lints the bead.

Exit codes:
  0  clean: the dispatcher would slot it
  1  refused: a required field is missing
  2  needs planning: label needs-planning, a Size that says planning, or more
     than 6 acceptance items`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runSpecLint,
}

var specDispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Run one spec-dispatcher tick (the daemon's spec_dispatch patrol)",
	Long: `Run one tick of the spec dispatcher.

Candidates are ready, unassigned, open task/bug/feature beads in every
operational rig, numbered at or above polecat_pool.max_priority's ceiling
(default 2) and ordered by priority, then created_at, then id. Epics, agent
beads, wisps and the other runtime families are never candidates; the retired
label spec and type feature are accepted and ignored (gt-mmsr2). Beads labeled
gt:ready-to-land, overseer-only, operator-only, needs-overseer, gt:needs-overseer,
needs-operator, gt:needs-operator, or deferred are never taken, and neither is a
bead submitted for landing whose READY TO LAND block has reached the read before
the label (gt-kr5xv). overseer-only means the overseer acts or vets first and the
dispatcher skips the bead; operator-only means only the operator can settle it,
and the overseer sets it after trying. needs-overseer and needs-operator are the
escalation labels for those two; land writes them with the gt: prefix, and both
spellings hold. The old human-only, needs-human and gt:needs-human labels still
hold the same way until they are retired. Each candidate is linted (see gt spec
lint), and polecat_pool.shape_gate says what the verdict does:

  - off: no lint runs; the bead is dispatched like any other
  - warn (the default): a bead the lint refuses is held — skipped with the
    reason "unshaped: <fields>" and left ready for the next tick — and the
    verdict is left on it as one comment (SHAPE: ...). A bead that needs
    planning is held too, labeled needs-planning, and the tick names which
    planning state it is in — "needs plan" while it waits for a plan job's
    PLAN PROPOSAL, "plan proposed" once that block is in its notes and only
    the operator can file the children (gt-4k3fj.14, gt-tod3q). A bead labeled
    spec-shape-waived waives the lint and dispatches anyway
  - refuse: as warn, and a refused bead also wears the needs-shape label; the
    needs-planning bead is held exactly as under warn (gt-tod3q). Under either
    gate the tick spawns no planner (the planner was retired, gt-4rizh), so a
    held bead waits for the operator

Beads labeled red-main are held while the red-main owner is reverting the
breakage they were filed for: the landing worker records a revert in flight in
the rig's red-main state file (<town>/.runtime/red-main/<rig>.json), and that
rig's red-main beads stay ready instead of racing the revert with a fix
forward. The hold lifts when the revert lands or is rejected, so the next tick
takes them again (gt-zkdwt).

A candidate is not held for content overlap with work a seat already has in
hand, and the tick does not defer a sling for one either: beads that name the
same tests or files are slung like any others, and a rebase conflict at landing
costs less than a seat left waiting (gt-e1ufo). A manual gt sling still refuses
an overlap unless --force is given.

A clean candidate is re-read once more immediately before its sling and dropped
if the bead has changed since the board was read — a submission in that window
would otherwise be slung to a seat already landing it, and a re-read that fails
holds the candidate for the next tick rather than slinging blind (gt-01gix).
One that still holds is slung onto a free seat within the budget, drawn at
random among the agents with a free, open seat in polecat_pool.seats. A bead
carrying an agent:<name> label draws only among that agent's seats, and waits
when that seat is closed or full.

Every dispatch's sling args tell the polecat to test install paths in a
temporary INSTALL_DIR.

With --background-slings the tick runs no sling: it prints each one it planned
under "pending", and the daemon runs them as gt spec sling while the next tick
goes on. --in-flight names the slings still running, so the tick leaves their
beads alone and counts their seats as taken (gt-5h70s).

Budget comes from polecat_pool in settings/config.json (seats, max_total,
timezone, min_spawn_gap) and daemon.patrols.spec_dispatch
in settings/config.json (max_per_tick).
A town that has not run gt config migrate still keeps that patrol block in
mayor/daemon.json. Both blocks are read on every tick, so a seat added, moved or
closed applies at the next tick — no daemon restart. A block that does not parse
stops the tick: it dispatches nothing until a read works, rather than run with
the seat the bad block dropped. The operator hold file and ESTOP stop the tick.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runSpecDispatch,
}

func init() {
	specLintCmd.Flags().StringVar(&specLintTemplate, "template", "", "Spec template path (default daemon.patrols.spec_dispatch.template in settings/config.json, else ~/.claude/docs/agents/spec-template.md)")
	specLintCmd.Flags().BoolVar(&specLintJSON, "json", false, "Print {id, ok, needs_planning, refusals[]} instead of one line")
	specDispatchCmd.Flags().BoolVar(&specDispatchJSON, "json", false, "Output the tick report as JSON")
	specDispatchCmd.Flags().BoolVar(&specDispatchDryRun, "dry-run", false, "Decide and report without slinging, labeling or commenting")
	specCmd.AddCommand(specLintCmd, specDispatchCmd)
	rootCmd.AddCommand(specCmd)
}

// specPlanState is what a needs-planning spec is waiting on: the plan job's
// PLAN PROPOSAL block in its notes means the operator's turn (gt-4k3fj.13,
// gt-4k3fj.14).
func specPlanState(notes string) string {
	if specdispatch.HasPlanProposal(notes) {
		return specPlanProposed
	}
	return specPlanNeedsPlan
}

// specPlanLine is the tick's line for a needs-planning spec: the state, and
// what the state means for the next reader.
func specPlanLine(id, state, reason string) string {
	if state == specPlanProposed {
		return fmt.Sprintf("%s: %s: the %s block in its notes is the operator's to file", id, state, specdispatch.PlanProposalMarker)
	}
	return fmt.Sprintf("%s: %s: %s", id, state, reason)
}

// specPlanNote is the text behind specPlanLine, left on the bead once per
// state: the PLAN PROPOSAL block appearing is what earns the second note.
func specPlanNote(key, state, reason string) string {
	if state == specPlanProposed {
		return key + ": the plan job proposed its children; nothing is filed from a proposal"
	}
	return fmt.Sprintf("%s: %s; a plan job proposes the children into the notes and files nothing", key, reason)
}

// specPlanHold takes a spec that needs planning off the dispatch path under
// every gate that holds one — warn as well as refuse (gt-tod3q). The tick
// reports the bead by its planning state, labels it needs-planning, and
// comments once per state. Nothing is spawned: the planner was retired
// (gt-4rizh), and a planner files nothing from a proposal anyway (gt-4k3fj.13,
// gt-4k3fj.14). The caller always skips the sling.
func specPlanHold(report *specDispatchReport, env specDispatchEnv, c specCandidate, full specdispatch.Spec, verdict specdispatch.LintVerdict) {
	state := specPlanState(full.Notes)
	report.Planning = append(report.Planning, specDispatchEntry{Bead: full.ID, Rig: c.Rig, Line: specPlanLine(full.ID, state, verdict.Reason)})
	if env.DryRun {
		return
	}
	if !full.HasLabel(specdispatch.NeedsPlanningLabel) {
		if err := env.AddLabel(full.ID, specdispatch.NeedsPlanningLabel); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", full.ID, err))
		}
	}
	key := fmt.Sprintf("%s%s: %s", specDispatchNotePrefix, full.ID, state)
	if err := env.Annotate(full.ID, key, specPlanNote(key, state, verdict.Reason)); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", full.ID, err))
	}
}

// specFromIssue reduces a bead to the fields the lint reads.
func specFromIssue(issue *beads.Issue) specdispatch.Spec {
	if issue == nil {
		return specdispatch.Spec{}
	}
	return specdispatch.Spec{
		ID:           issue.ID,
		Title:        issue.Title,
		Type:         issue.Type,
		Status:       issue.Status,
		Assignee:     issue.Assignee,
		Priority:     issue.Priority,
		CreatedAt:    issue.CreatedAt,
		Labels:       issue.Labels,
		Description:  issue.Description,
		Design:       issue.Design,
		Notes:        issue.Notes,
		Acceptance:   issue.AcceptanceCriteria,
		CommentCount: issue.CommentCount,
		Ephemeral:    issue.Ephemeral,

		SubmittedForLanding: specLiveLandingRequest(issue.Notes),
	}
}

// specLiveLandingRequest reports whether notes carry a landing request that
// nothing later settled: a complete READY TO LAND block after the last LANDING
// RECORD and the last MERGE REJECTION. gt done writes that block with the
// gt:ready-to-land label in one update (internal/done markReadyToLand); the
// label is the half of an answer known to go missing (gt-q6zoo), which leaves
// the notes as the signal that survives. Holding on the block keeps such a
// bead off the candidate board and out of a sling (gt-kr5xv). A land or a rejection written after the block settles it,
// so a landed bead and a rework are left to their own paths.
func specLiveLandingRequest(notes string) bool {
	if _, ok := land.ParseReadyNote(notes); !ok {
		return false
	}
	submitted := strings.LastIndex(notes, land.ReadyNoteMarker+"\n")
	settled := strings.LastIndex(notes, land.LandingNoteMarker)
	if rejected := strings.LastIndex(notes, land.MergeRejectionNoteMarker); rejected > settled {
		settled = rejected
	}
	return submitted > settled
}

// showSpec reads one bead in full (bd show), routed by its prefix.
func showSpec(townRoot, beadID string) (specdispatch.Spec, error) {
	return showSpecWith(nil, beadID, func(id string) (*beads.Issue, error) { return showBead(townRoot, id) })
}

// showSpecWith is showSpec through the tick's ledger when reads is one, so a
// bead the tick already read is not read again (gt-u8rbh).
func showSpecWith(reads *specTickReads, beadID string, read func(id string) (*beads.Issue, error)) (specdispatch.Spec, error) {
	issue, err := reads.show(beadID, func() (*beads.Issue, error) { return read(beadID) })
	if err != nil {
		return specdispatch.Spec{}, fmt.Errorf("bead %s not found: %w", beadID, err)
	}
	return specFromIssue(issue), nil
}

// specTickReads is one tick's read ledger: a bead any read of the tick fetched
// is served from here, and every read that reaches bd is counted (gt-u8rbh).
// It lives for one tick; the pre-sling re-read (specDispatchEnv.ShowFresh)
// bypasses it. A nil ledger reads through and counts nothing.
type specTickReads struct {
	mu    sync.Mutex
	shows map[string]*beads.Issue
	calls int
	// phase is the phase the tick has entered (specPhase*), and phaseCalls the
	// calls made under each one, so the daemon's 'tick took' line can name what
	// a phase costs in bd calls as well as in time (gt-139dp).
	phase      string
	phaseCalls map[string]int
}

// The phases a tick counts its reads under, named as the 'tick took' line
// names its times.
const (
	specPhaseRoster     = "roster"
	specPhaseBoard      = "board"
	specPhaseCandidates = "candidates"
)

func newSpecTickReads() *specTickReads {
	return &specTickReads{shows: map[string]*beads.Issue{}, phaseCalls: map[string]int{}}
}

// call counts one bd call the tick issued outside show and the client below.
func (r *specTickReads) call() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.count()
	r.mu.Unlock()
}

// count adds one call to the ledger. The caller holds the lock: every write to
// the per-phase counters and the total happens under it, so a read made
// concurrently by show and ShowMultiple cannot tear them.
func (r *specTickReads) count() {
	r.calls++
	if r.phase != "" {
		r.phaseCalls[r.phase]++
	}
}

// setPhase makes name the phase the ledger counts calls under until the next
// setPhase or endPhase. An empty name leaves calls uncounted by phase.
func (r *specTickReads) setPhase(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.phase = name
	r.mu.Unlock()
}

// endPhase leaves no phase current and returns how many calls were made under
// the one that was.
func (r *specTickReads) endPhase() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.phaseCalls[r.phase]
	r.phase = ""
	return n
}

// Calls is how many bd calls the tick has issued through the ledger.
func (r *specTickReads) Calls() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// show returns bead id from the ledger, or runs read (one bd call) and keeps
// what it returned. The lock is not held across the read, which is the slow
// part; the map and the counters are touched either side of it, under the lock.
func (r *specTickReads) show(id string, read func() (*beads.Issue, error)) (*beads.Issue, error) {
	if r == nil {
		return read()
	}
	r.mu.Lock()
	issue, ok := r.shows[id]
	r.mu.Unlock()
	if ok {
		return issue, nil
	}
	issue, err := read()
	r.mu.Lock()
	r.count()
	if err == nil && issue != nil {
		r.shows[id] = issue
	}
	r.mu.Unlock()
	return issue, err
}

// client is store with the ledger in front of its reads: Show and ShowMultiple
// answer from beads the tick already read and ask bd only for the rest, and
// every call that does reach bd is counted.
func (r *specTickReads) client(store beads.Client) beads.Client {
	if r == nil {
		return store
	}
	return specTickClient{Client: store, reads: r}
}

// specTickClient is a beads.Client whose reads go through a specTickReads.
type specTickClient struct {
	beads.Client
	reads *specTickReads
}

func (c specTickClient) Show(id string) (*beads.Issue, error) {
	return c.reads.show(id, func() (*beads.Issue, error) { return c.Client.Show(id) })
}

// ShowMultiple reads only the ids the ledger does not hold. Like the store's
// own, it returns what it found beside the error of a group that failed.
func (c specTickClient) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	out := make(map[string]*beads.Issue, len(ids))
	var missing []string
	c.reads.mu.Lock()
	for _, id := range ids {
		if issue, ok := c.reads.shows[id]; ok {
			out[id] = issue
		} else {
			missing = append(missing, id)
		}
	}
	c.reads.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	found, err := c.Client.ShowMultiple(missing)
	c.reads.mu.Lock()
	defer c.reads.mu.Unlock()
	c.reads.count()
	for id, issue := range found {
		out[id] = issue
		if issue != nil {
			c.reads.shows[id] = issue
		}
	}
	return out, err
}

func (c specTickClient) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.reads.count()
	return c.Client.List(opts)
}

func (c specTickClient) Children(parentID string) ([]*beads.Issue, error) {
	c.reads.count()
	return c.Client.Children(parentID)
}

// specChildren reads a bead's direct children and reduces them to what the
// container rule reads (gt-gektq). The read is slingStores.children, whose
// comment says why it is bd's view and not the raw dependency scan.
func specChildren(townRoot, beadID string) ([]specdispatch.Child, error) {
	issues, err := slingStores{}.children(townRoot, beadID)
	if err != nil {
		return nil, err
	}
	return childSpecs(issues), nil
}

// childSpecs reduces a child set to what the container rule reads.
func childSpecs(issues []*beads.Issue) []specdispatch.Child {
	out := make([]specdispatch.Child, 0, len(issues))
	for _, issue := range issues {
		out = append(out, specdispatch.Child{ID: issue.ID, Status: issue.Status})
	}
	return out
}

// specChildrenRead is the children read of one rig's candidates: one ChildrenOf
// for the ids, then one ShowMultiple for the ids ChildrenOf left out.
// ChildrenOf drops a childless parent (beads.parseChildrenMapJSON), so an id it
// leaves out is either childless or unresolved; ShowMultiple tells them apart,
// since it returns exactly the ids that exist. A childless id that exists is
// keyed with an empty list, and an id neither read answers has no key.
func specChildrenRead(c beads.Client, ids []string) (map[string][]*beads.Issue, error) {
	kids, err := c.ChildrenOf(ids...)
	if err != nil {
		return nil, err
	}
	if kids == nil {
		kids = map[string][]*beads.Issue{}
	}
	var absent []string
	for _, id := range ids {
		if _, ok := kids[id]; !ok {
			absent = append(absent, id)
		}
	}
	if len(absent) == 0 {
		return kids, nil
	}
	exist, err := c.ShowMultiple(absent)
	if err != nil {
		return nil, err
	}
	for id := range exist {
		kids[id] = []*beads.Issue{}
	}
	return kids, nil
}

// specChildrenReader is the default children read of specCandidateChildren: the
// rig's database at dir, read through stores. The tick passes the zero value,
// which opens bd; a test passes a store that counts its reads.
func specChildrenReader(stores slingStores) func(dir string, ids []string) (map[string][]*beads.Issue, error) {
	return func(dir string, ids []string) (map[string][]*beads.Issue, error) {
		return specChildrenRead(stores.pinnedAt(dir), ids)
	}
}

// specCandidateChildren is the tick's children read for the candidates of a
// board read: one bd view per rig the candidates live in, answered per bead,
// with a per-bead fallback when the grouped read fails so one unreadable bead
// does not cost the whole rig its children (gt-km9jt). A candidate the verdict
// memory refuses is not read at all (see childrenWanted). Every other candidate
// is read fresh each tick: a child's change does not move its parent's row, and
// a child added to a childless parent does not move the parent's row either, so
// no remembered child set could be trusted to hold the container hold.
//
// The default read keeps the childless parents a resolved answer names (see
// specChildrenRead), so only an id with no key is read alone. Almost every
// candidate is childless, and a read that drops those would cost each of them a
// second bd call (gt-4u2gt).
func specCandidateChildren(townRoot string, reads *specTickReads, cands []specCandidate, verdicts *specCandidateVerdicts, read func(dir string, ids []string) (map[string][]*beads.Issue, error)) func(beadID string) ([]specdispatch.Child, error) {
	byDir := map[string][]string{}
	unread := map[string]bool{}
	for _, c := range cands {
		if !verdicts.childrenWanted(c) {
			unread[c.Spec.ID] = true
			continue
		}
		dir := resolveBeadDirFromTownRoot(townRoot, c.Spec.ID)
		byDir[dir] = append(byDir[dir], c.Spec.ID)
	}
	if read == nil {
		read = specChildrenReader(slingStores{})
	}
	var mu sync.Mutex
	kids := map[string][]specdispatch.Child{}
	errs := map[string]error{}
	done := map[string]bool{}
	return func(beadID string) ([]specdispatch.Child, error) {
		mu.Lock()
		defer mu.Unlock()
		if unread[beadID] {
			return nil, nil
		}
		dir := resolveBeadDirFromTownRoot(townRoot, beadID)
		if !done[dir] {
			done[dir] = true
			ids := byDir[dir]
			if len(ids) == 0 {
				ids = []string{beadID}
			}
			reads.call()
			group, err := read(dir, ids)
			if err != nil {
				// One parent the grouped read could not answer fails the whole
				// read, so each bead is read alone: the rest still get their
				// children and only the unreadable bead reports. With the default
				// read each alone read is two bd calls, so a failed rig costs
				// about two calls per candidate. A failure is rare, and the
				// per-bead read keeps one unreadable bead from failing the rest
				// (gt-4u2gt).
				for _, id := range ids {
					reads.call()
					single, readErr := read(dir, []string{id})
					if readErr != nil {
						errs[id] = readErr
						continue
					}
					kids[id] = childSpecs(single[id])
				}
			}
			for id, is := range group {
				kids[id] = childSpecs(is)
			}
			if err == nil {
				// The grouped read keys a childless parent that exists with an
				// empty list (specChildrenRead), so a parent missing from a
				// successful answer is one bd did not resolve. It is read
				// alone: a read of the one parent that succeeds and still has no
				// entry for it means bd has no children for it, and a failed
				// read is that bead's error. Reading the grouped absence as "no
				// children" would hide a held container's children from the
				// container hold (gt-k3me9).
				for _, id := range ids {
					if _, answered := group[id]; answered {
						continue
					}
					reads.call()
					single, readErr := read(dir, []string{id})
					if readErr != nil {
						errs[id] = readErr
						continue
					}
					kids[id] = childSpecs(single[id])
				}
			}
		}
		if err := errs[beadID]; err != nil {
			return nil, err
		}
		return kids[beadID], nil
	}
}

// specHoldKnobs is the dispatcher's own settings the peer-hold question reads:
// the candidate filter's priority ceiling, and the shape gate with the
// template it lints against. Reading them from the town is the caller's
// (specHoldKnobsFrom), so the hold rule itself is a function of the bead and
// these values.
type specHoldKnobs struct {
	MaxPriority int
	ShapeGate   string
	Template    specdispatch.Template
	// Backoff answers why the automatic dispatcher is resting beadID after a
	// failed session start, "" when it is not (dispatch.StartupBackoff). It is
	// the one hold the tick keeps outside the bead that can last an hour, so
	// the guard has to read it. Nil is a caller that never wired the reader,
	// not a bead with no record: the guard cannot tell a resting bead from a
	// dispatchable one, answers "not held", and refuses the resting bead's
	// whole cluster for as long as it rests — the standoff the answer exists
	// to end (gt-mqa3r). specPeerHeldFrom says so on Warn rather than skipping
	// the question in silence.
	Backoff func(beadID string) string
	// Target reads the files the branch beadID's rig lands on holds, for the
	// shape gate's path check (gt-7t1im). The guard asks what the tick asks, so
	// a peer whose Constraints name a file the branch never got is held here
	// too. Nil is a caller that never wired the reader: the lint then refuses a
	// peer naming a file, which holds its cluster rather than reading the peer
	// as next.
	Target func(beadID string) *specdispatch.TargetTree
	// Warn is where a question left unanswered because its inputs would not
	// read says so; nil falls back to specHoldWarn, so a knob set built by hand
	// cannot make an unanswered question silent.
	Warn io.Writer
}

// warnWriter is where this knob set's unanswered questions go: its own writer,
// or the package's stderr one when a caller built the knobs by hand. Nil must
// not mean silence: an unanswered question reads as "not held", and that
// silent answer is the fail-open the reporting exists to make visible.
func (k specHoldKnobs) warnWriter() io.Writer {
	if k.Warn == nil {
		return specHoldWarn
	}
	return k.Warn
}

// target asks the knob set for beadID's target inventory. A set built without
// the reader answers nil, which the lint refuses a file-naming bead for: the
// peer is held rather than read as next on a check that never ran.
func (k specHoldKnobs) target(beadID string) *specdispatch.TargetTree {
	if k.Target == nil {
		return nil
	}
	return k.Target(beadID)
}

// specHoldKnobsFrom reads the knobs from the town's settings — the same keys
// runSpecDispatch reads them from. The second result is false when those
// settings will not load, which leaves the hold question unanswered rather
// than answered wrongly — and says so on warn, since an unanswered question
// disables every peer hold. The knobs carry warn on for the reads that follow.
func specHoldKnobsFrom(townRoot string, warn io.Writer) (specHoldKnobs, bool) {
	path := config.TownSettingsPath(townRoot)
	ts, err := config.LoadOrCreateTownSettings(path)
	if err != nil {
		warnSpecHoldUnread(warn, "town settings "+path, err)
		return specHoldKnobs{}, false
	}
	sd := loadSpecDispatchConfig(townRoot)
	knobs := specHoldKnobs{
		Warn:        warn,
		MaxPriority: ts.PolecatPool.GetMaxPriority(),
		ShapeGate:   ts.PolecatPool.GetShapeGate(),
		Backoff:     func(beadID string) string { return dispatch.StartupBackoff(townRoot, beadID) },
		Target:      cachedSpecTarget(townRoot, openRigTree),
	}
	if knobs.ShapeGate != specdispatch.ShapeGateOff {
		knobs.Template = specdispatch.LoadTemplate(specTemplatePath(sd))
	}
	return knobs, true
}

// specPeerHeldFrom reports why the spec dispatcher would not sling peer, and
// "" when the peer is a bead it would take. It asks the questions the tick asks
// of a candidate before spending a seat: the candidate filter
// (specdispatch.Eligible), whether a working seat already holds the peer's
// defect (specPeerHeldBySeat), the startup backoff, readiness — the ready query
// lists only beads whose blocking dependencies have all resolved, so a peer
// with one still open is never reached at all — the formula declaration, the
// shape gate's verdict, and the container rule. The sling-time overlap guard
// asks this of an unstarted peer, so a cluster whose head is held dispatches
// its next eligible bead instead of none (gt-00gee, gt-mknmb).
//
// The questions read the peer's own pool row rather than the bead: the fields
// they need are the ones a pool fetch already holds (sling.Readiness), so a
// peer costs the guard no bd read. The two exceptions carry their inputs: the
// child set, because bd's parent-child view unions the issue and wisp tables
// which the pool's flat listing does not, so it is read here from the database
// the pool came from; and the held set specPeerHeldBySeat weighs, which is
// reduced from the pool the guard is already reading.
//
// Whether a working seat holds the peer, and the startup backoff, are answered
// because both outlast a tick: a peer resting after a failed session start is
// not the dispatcher's next move for up to an hour (gt-mqa3r), and a peer a
// working seat already has in hand is skipped until that seat closes. Either
// one read as next refuses its whole cluster for as long as it holds. Answering
// whether a working seat holds the peer is also what keeps the backoff answer
// from lapsing into that refusal: once a rest ends the peer is next again, and
// if live work held it all along, the hold — not the rest — is why it cannot be
// slung (gt-36qvt).
//
// A hold the tick decides from its own state that lifts within a tick or two —
// seat availability, the per-tick limit, a red-main revert in flight — is not
// answered: the peer it holds is one the cluster should still wait for.
//
// Nor is a hold that holds every bead of a rig together, such as a parked or
// docked rig: the comparison pool is the candidate's own rig, so such a hold
// can never tell a peer apart from the bead being slung. Nor is the tick's
// other formula skip, a declaration the rig has no formula for (gt-j5cr2):
// answering that one needs the formula-existence query the row cannot give,
// and asking it would spend a bd read per weighed peer on a hold that is rare
// and self-clearing, since whoever renamed the formula names the new one in
// the bead.
//
// Unknown is not held: a peer whose row a degraded pool fetch left unread, and
// a child set that will not read, answer "" and the guard keeps refusing. That
// reads wrong as a delayed dispatch, where the other answer reads wrong as two
// seats on one defect.
func specPeerHeldFrom(store beads.Client, knobs specHoldKnobs, peer sling.Duplicate, pool []sling.Duplicate) string {
	spec := specFromPeer(peer)
	if ok, why := specdispatch.Eligible(spec, knobs.MaxPriority); !ok {
		return why
	}
	// Whether a working seat holds the peer comes before the backoff skip, so
	// the reason reported is the first that holds it.
	if why := specPeerHeldBySeat(peer, pool); why != "" {
		return why
	}
	// The startup backoff is a throttle the tick keeps outside the bead: a head
	// resting for up to an hour is not the dispatcher's next move, so it must
	// not refuse its whole cluster for the length of its rest (gt-mqa3r). A
	// nil reader is a caller that never wired the question; skipping it reads
	// the resting head as next and refuses the cluster, so it is reported as
	// the unanswered read it is rather than passed over in silence.
	if knobs.Backoff == nil {
		warnSpecHoldUnread(knobs.warnWriter(), "the startup backoff", errors.New("no backoff reader configured"))
	} else if why := knobs.Backoff(peer.ID); why != "" {
		return why
	}
	// A peer's open blockers keep it off the ready board the dispatcher draws
	// from, so the query never slings it however eligible it is, and ranking it
	// ahead of a candidate refuses the rest of its cluster until the dependency
	// closes (gt-mknmb).
	if why := specPeerBlocked(peer.Ready.OpenBlockers); why != "" {
		return why
	}
	// A declaration the tick cannot read is a skip: the bead stays ready and is
	// never slung with a declaration the dispatcher could not parse (gt-j5cr2).
	// Left unasked here, a head in that state answered "the dispatcher would
	// sling it" and so refused every lower-ranked bead sharing its test for as
	// long as its declaration stayed broken, while it was itself never slung —
	// the cluster's whole tail waiting on a bead the dispatcher cannot take
	// (gt-7cmjp). The declaration is the row's own labels and vars section, so
	// the question costs the guard no read.
	if _, err := specdispatch.FormulaRunOf(spec); err != nil {
		return err.Error()
	}
	if knobs.ShapeGate != specdispatch.ShapeGateOff && !spec.HasLabel(specdispatch.ShapeWaivedLabel) {
		spec.Target = knobs.target(peer.ID)
		if verdict := specdispatch.Lint(spec, knobs.Template); !verdict.Clean() {
			// The first refusal, as the lint's own one-line verdict names it:
			// the whole ShapeNote can run to every missing section, and this
			// reason is printed per overlapping bead in the guard's report.
			return verdict.Field + ": " + verdict.Reason
		}
	}
	children, err := store.Children(peer.ID)
	if err != nil {
		warnSpecHoldUnread(knobs.warnWriter(), "children of "+peer.ID, err)
		return ""
	}
	spec.Children = childSpecs(children)
	return specdispatch.OpenChildHold(spec)
}

// specPeerHeldBySeat asks whether a working seat already has the defect a peer
// names: why it does, "" when none does. The held set is reduced from the
// guard's own pool by heldBeadFrom, the rule that says which beads hold, so the
// question costs no bd read, and the peer's own text is reduced by specHoldFiles
// and extractOverlapRefs, so the two sides cannot disagree about one defect.
//
// Without this question the peer-hold answer lapses the moment a rest ends: the
// peer stops resting, the guard reads it as next, and it refuses the whole
// cluster behind a peer that live work will hold for as long as that work runs
// (gt-36qvt).
func specPeerHeldBySeat(peer sling.Duplicate, pool []sling.Duplicate) string {
	if len(pool) == 0 {
		return ""
	}
	held := make([]specdispatch.HeldBead, 0, len(pool))
	for _, other := range pool {
		if other.ID == peer.ID || !specSeatHolds(other.Status) {
			continue
		}
		if h, ok := heldBeadFrom(other); ok {
			held = append(held, h)
		}
	}
	if len(held) == 0 {
		return ""
	}
	// The peer's own priority decides whether the hold takes it: an urgent peer
	// the tick would sling despite an overlap is next, so the guard refuses the
	// candidate behind it exactly as it does for an unheld peer (gt-iydx1).
	return specdispatch.HeldOverlap(specHoldFiles(peer.Refs.Files, peer.Ready.Description, peer.Refs.Text), peer.Refs.Tests, peer.Priority, held).Reason
}

// specSeatHolds reports whether a bead in status is one a working seat has in
// hand: the statuses the guard peer question reads (specHeldStatuses).
func specSeatHolds(status string) bool {
	for _, s := range specHeldStatuses {
		if status == s {
			return true
		}
	}
	return false
}

// warnSpecHoldUnread tells the operator a peer-hold question went unanswered
// because what it reads would not load, so the peer counts as not held. The
// production caller passes stderr, so it cannot corrupt a stdout report.
func warnSpecHoldUnread(w io.Writer, what string, err error) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "%s spec peer-hold check degraded: could not read %s, treating the peer as not held: %v\n",
		style.Dim.Render("Warning:"), what, err)
}

// maxNamedBlockers bounds the blockers a blocked reason names, so one widely
// blocked bead does not turn a hold line into a wall.
const maxNamedBlockers = 3

// specBlocked names why the tick may not sling a candidate: sling.Admit's
// reasons, which name the status each blocker was read with, under the prefix
// its skip line carries (gt-ndad8).
func specBlocked(blockers []string) string {
	return specNamedBlockers(specdispatch.SkipBlocked, blockers)
}

// specPeerBlocked names the blocking dependencies that keep a peer off the
// ready board, and "" when it has none open: the dispatcher's draw is that
// board, so a peer the query leaves out is never slung however eligible it is,
// and ranking it ahead of a candidate refuses the rest of its cluster until the
// dependency closes (gt-mknmb).
func specPeerBlocked(open []string) string {
	return specNamedBlockers(specdispatch.SkipPeerBlocked, open)
}

// specNamedBlockers joins at most maxNamedBlockers ids under prefix.
func specNamedBlockers(prefix string, named []string) string {
	if len(named) == 0 {
		return ""
	}
	more := 0
	if len(named) > maxNamedBlockers {
		more = len(named) - maxNamedBlockers
		named = named[:maxNamedBlockers]
	}
	reason := prefix + strings.Join(named, ", ")
	if more > 0 {
		reason += fmt.Sprintf(" (+%d more)", more)
	}
	return reason
}

// specFromPeer is the peer's pool row as the dispatcher's predicates read it:
// the row's own fields plus the readiness half the pool fetch recovered for it.
// It is specFromIssue's set but for the child set (specPeerHeldFrom reads that)
// and the ephemeral flag, which a pool listing does not carry.
func specFromPeer(peer sling.Duplicate) specdispatch.Spec {
	return specdispatch.Spec{
		ID:          peer.ID,
		Title:       peer.Title,
		Type:        peer.IssueType,
		Status:      peer.Status,
		Assignee:    peer.Ready.Assignee,
		Priority:    peer.Priority,
		CreatedAt:   peer.CreatedAt,
		Labels:      peer.Ready.Labels,
		Description: peer.Ready.Description,
		Design:      peer.Ready.Design,
		Notes:       peer.Ready.Notes,
		Acceptance:  peer.Ready.Acceptance,

		SubmittedForLanding: specLiveLandingRequest(peer.Ready.Notes),
	}
}

// specPeerHeldPinned is specPeerHeldFrom against the rig's own beads, the
// shape the overlap guard asks it in: the knobs come from the town, the peer's
// children from the database its comparison pool came from, and the working
// seats' beads from that same pool.
func specPeerHeldPinned(townRoot, beadsDir string, peer sling.Duplicate, pool []sling.Duplicate) string {
	return specPeerHeldPinnedTo(specHoldWarn, townRoot, beadsDir, peer, pool)
}

// specHoldWarn is stderr, each distinct warning once: the guard asks the hold
// question for every peer, and a corrupt settings file fails it identically
// for all of them.
var specHoldWarn io.Writer = &onceWriter{w: os.Stderr}

// onceWriter passes each distinct write through to w once and drops repeats.
type onceWriter struct {
	w    io.Writer
	mu   sync.Mutex
	seen map[string]bool
}

func (o *onceWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[string(p)] {
		return len(p), nil
	}
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[string(p)] = true
	return o.w.Write(p)
}

// specPeerHeldPinnedTo is specPeerHeldPinned with the writer its warnings go to.
func specPeerHeldPinnedTo(warn io.Writer, townRoot, beadsDir string, peer sling.Duplicate, pool []sling.Duplicate) string {
	knobs, ok := specHoldKnobsFrom(townRoot, warn)
	if !ok {
		return ""
	}
	return specPeerHeldFrom(slingStores{}.pinnedAt(beadsDir), knobs, peer, pool)
}

func runSpecLint(cmd *cobra.Command, args []string) error {
	townRoot, _ := workspace.FindFromCwd()
	path := specLintTemplate
	if path == "" {
		path = specTemplatePath(loadSpecDispatchConfig(townRoot))
	}
	spec, err := specForLint(townRoot, args[0], func(id string) (specdispatch.Spec, error) { return showSpec(townRoot, id) }, openRigTree)
	return specLint(cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], spec, err, path, specLintJSON)
}

// specForLint reads beadID for gt spec lint with the target inventory the
// lint's path check reads (gt-7t1im). A bead that did not read is linted on its
// read failure alone, so no inventory is asked for it.
func specForLint(townRoot, beadID string, show func(string) (specdispatch.Spec, error), open treeOpener) (specdispatch.Spec, error) {
	spec, err := show(beadID)
	if err != nil {
		return spec, err
	}
	spec.Target = specLintTarget(townRoot, beadID, open)
	return spec, nil
}

// specLintTarget is the target-branch inventory gt spec lint checks beadID's
// Constraints against (gt-7t1im). A bead of no rig, a town bead included, gets
// a tree carrying that error rather than none: the lint then holds a bead
// naming a path with the error named.
func specLintTarget(townRoot, beadID string, open treeOpener) *specdispatch.TargetTree {
	rig := beads.GetRigNameForPrefix(townRoot, beads.ExtractPrefix(beadID))
	if rig == "" {
		return &specdispatch.TargetTree{Err: "no rig holds " + beadID}
	}
	return specTargetTree(filepath.Join(townRoot, rig), open)
}

// cachedSpecTarget is specLintTarget keyed by rig and read once per caller: the
// overlap guard asks it of every peer in a cluster, and a rig's branch does not
// change inside one question. A bead of no rig is answered per bead, since the
// error names it.
func cachedSpecTarget(townRoot string, open treeOpener) func(beadID string) *specdispatch.TargetTree {
	trees := map[string]*specdispatch.TargetTree{}
	return func(beadID string) *specdispatch.TargetTree {
		rig := beads.GetRigNameForPrefix(townRoot, beads.ExtractPrefix(beadID))
		if rig == "" {
			return specLintTarget(townRoot, beadID, open)
		}
		tree, ok := trees[rig]
		if !ok {
			tree = specLintTarget(townRoot, beadID, open)
			trees[rig] = tree
		}
		return tree
	}
}

// treeReader reads a revision's file inventory: a *git.Git in a tick, a fake
// in a test.
type treeReader interface {
	TreeFileBlobs(rev string) (map[string]string, error)
}

// treeOpener opens the treeReader of a repo directory.
type treeOpener func(repoDir string) treeReader

// openRigTree reads a rig repo's inventories with git.
func openRigTree(repoDir string) treeReader { return git.NewGitWithDir(repoDir, "") }

// specTargetTree is the inventory the lint checks a rig's Constraints paths
// against: the branch the rig's landings target, read from the repo under
// rigPath (gt-7t1im). Every failure is carried in the tree and never dropped: a
// tree that could not be read must not read as one holding no files.
//
// origin's branch is read first, the local one only when origin has none: a
// local main can lag the branch a landing worker targets, and reading it would
// refuse a bead whose prerequisite has already landed.
func specTargetTree(rigPath string, open treeOpener) *specdispatch.TargetTree {
	branch, err := land.RigDefaultBranch(rigPath)
	if err != nil {
		return &specdispatch.TargetTree{Err: "cannot read the rig's default branch: " + firstErrLine(err)}
	}
	// The config names the branch, so a name that is not one literal revision is
	// held before it reaches git: a leading "-" is read as an option and ".."
	// as a range, and neither is a branch a rig reads files from.
	if !validTargetBranch(branch) {
		return &specdispatch.TargetTree{Branch: branch, Err: fmt.Sprintf("the rig's default branch %q is not a branch name", branch)}
	}
	repo := rigPruneRepoDir(rigPath)
	if repo == "" {
		return &specdispatch.TargetTree{Branch: branch, Err: "no git repo under " + rigPath}
	}
	reader := open(repo)
	var lastErr error
	for _, ref := range []string{"origin/" + branch, branch} {
		blobs, err := reader.TreeFileBlobs(ref)
		if err != nil {
			lastErr = err
			continue
		}
		files := make(map[string]struct{}, len(blobs))
		for path := range blobs {
			files[path] = struct{}{}
		}
		return &specdispatch.TargetTree{Branch: branch, Files: files}
	}
	return &specdispatch.TargetTree{Branch: branch, Err: fmt.Sprintf("cannot read %s in %s: %s", branch, repo, firstErrLine(lastErr))}
}

// validTargetBranch reports whether name can be handed to git as one branch
// revision. The name comes from a rig's config.json, so it is checked before
// the read: a leading "-" would be taken as an ls-tree option, and ".." or
// "^"/"~" (refused below as not a branch-name character) would be taken as a
// revision range or another ref, not the branch a landing targets (gt-7t1im).
func validTargetBranch(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '/':
		default:
			return false
		}
	}
	return true
}

// specLintReport is the --json shape for one bead, so a shell caller can read
// the verdict without parsing prose: ok is the exit-0 route, needs_planning
// the exit-2 route, and refusals lists every shape failure in check order.
type specLintReport struct {
	ID            string                 `json:"id"`
	OK            bool                   `json:"ok"`
	NeedsPlanning bool                   `json:"needs_planning"`
	Refusals      []specdispatch.Refusal `json:"refusals"`
}

// specLint prints the lint verdict for beadID's spec (or for showErr, the
// failure to read it) against the template at templatePath, and returns the
// exit code as a SilentExit: 0 dispatchable, specLintExitRefused,
// specLintExitNeedsPlan. With asJSON, it prints a specLintReport instead of
// the one line; the exit code is the same either way. A human-form run that
// does not exit 0 also writes the required skeleton to errOut, after the
// one-line verdict on out, so a refusal shows what a right bead looks like
// (gt-ngtev).
func specLint(out, errOut io.Writer, beadID string, spec specdispatch.Spec, showErr error, templatePath string, asJSON bool) error {
	tmpl := specdispatch.LoadTemplate(templatePath)
	report := specLintReport{ID: beadID, Refusals: []specdispatch.Refusal{}}
	if showErr != nil {
		report.Refusals = append(report.Refusals, specdispatch.Refusal{Field: "bead", Reason: showErr.Error()})
		return emitSpecLint(out, errOut, tmpl, report, fmt.Sprintf("%s: spec lint refused: bead: %v", beadID, showErr), specLintExitRefused, asJSON)
	}
	verdict := specdispatch.Lint(spec, tmpl)
	report.OK = verdict.Clean()
	report.NeedsPlanning = verdict.Route == specdispatch.RoutePlanning
	report.Refusals = append(report.Refusals, verdict.Refusals...)
	code := specLintExitRefused
	switch verdict.Route {
	case specdispatch.RouteDispatch:
		code = 0
	case specdispatch.RoutePlanning:
		code = specLintExitNeedsPlan
	}
	return emitSpecLint(out, errOut, tmpl, report, verdict.Line(beadID), code, asJSON)
}

func emitSpecLint(out, errOut io.Writer, tmpl specdispatch.Template, report specLintReport, line string, code int, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
	} else {
		fmt.Fprintln(out, line)
		if code != 0 {
			// Best effort: the exit code carries the verdict, not this write.
			fmt.Fprintf(errOut, "%s", tmpl.Skeleton(report.ID))
		}
	}
	if code == 0 {
		return nil
	}
	return NewSilentExit(code)
}

// specDispatchReport is one tick's outcome; the daemon logs it.
type specDispatchReport struct {
	Hold       string `json:"hold,omitempty"`
	Template   string `json:"template"`
	Roster     string `json:"roster"`
	Candidates int    `json:"candidates"`
	// LabeledFailed counts the ready beads the tick found carrying
	// specdispatch.DispatchFailedLabel: they are out of the queue until the
	// label is cleared, and the health line surfaces the count (gt-q6zoo).
	LabeledFailed int `json:"labeled_failed,omitempty"`
	// DetailsFetched, DetailsCached and DetailsUnread count the pool beads
	// whose design and notes the tick showed, answered from the daemon's
	// cache, and left for a later tick by the per-tick limit (gt-l0vlf).
	DetailsFetched int                 `json:"details_fetched,omitempty"`
	DetailsCached  int                 `json:"details_cached,omitempty"`
	DetailsUnread  int                 `json:"details_unread,omitempty"`
	Dispatched     []specDispatchEntry `json:"dispatched"`
	Refused        []specDispatchEntry `json:"refused"`
	Planning       []specDispatchEntry `json:"planning"`
	Skipped        []specDispatchEntry `json:"skipped"`
	Failed         []specDispatchEntry `json:"failed"`
	// Deferred is the sling-stage part of Skipped: the candidates the tick
	// chose a seat for and whose sling was then deferred (a pool backpressure
	// refusal, a serialization failure that outlasted its retries, a sandbox
	// image or cache volume the seat lacks). The entries stay in Skipped too,
	// so the tick line and its cap count them as before; the daemon logs each
	// one on its own 'deferred:' line, where the cap cannot truncate it
	// (gt-4wcqd).
	Deferred []specDispatchEntry `json:"deferred,omitempty"`
	// Notices are lines the tick decided on but no single candidate owns,
	// logged once each (gt-wgyca).
	Notices []string `json:"notices,omitempty"`
	// Pending is the slings a --background-slings tick planned and did not run:
	// the daemon runs each as `gt spec sling` (spec_background.go, gt-5h70s).
	Pending      []specSlingJob `json:"pending,omitempty"`
	SlingsRun    int            `json:"slings_run,omitempty"`
	SlowestSling time.Duration  `json:"slowest_sling_ns,omitempty"`
	// The phase times of the tick, read on the tick's clock; the daemon logs
	// them on its 'tick took' line. A held tick runs no phase, so none is set.
	RosterTook     time.Duration `json:"roster_ns,omitempty"`
	BoardTook      time.Duration `json:"board_ns,omitempty"`
	CandidatesTook time.Duration `json:"candidates_ns,omitempty"`
	SlingTook      time.Duration `json:"sling_ns,omitempty"`
	// The bd reads each phase made, counted by the tick's ledger; the daemon
	// logs them on its 'tick took' line beside the phase times, so a slow
	// phase is also known by how many calls it cost (gt-139dp).
	RosterCalls     int      `json:"roster_calls,omitempty"`
	BoardCalls      int      `json:"board_calls,omitempty"`
	CandidatesCalls int      `json:"candidates_calls,omitempty"`
	Errors          []string `json:"errors,omitempty"`
}

type specDispatchEntry struct {
	Bead    string `json:"bead"`
	Rig     string `json:"rig,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Polecat string `json:"polecat,omitempty"`
	Line    string `json:"line"`
}

// specCandidate is a ready bead with the rig it lives in.
type specCandidate struct {
	Spec specdispatch.Spec
	Rig  string
	// UpdatedAt and DepCount are the board row's updated_at and dependency
	// count: the key a stored shape-and-label verdict is served against, so an
	// unchanged candidate is not read again (gt-km9jt).
	UpdatedAt string
	DepCount  int
	// Blockers are the bead's blocking edges, from the board's detail read, for
	// the admission check (sling.Admit). The ready board carries no dependency
	// rows (gt-ndad8), so an empty slice is a bead the read answered as having
	// none; a candidate whose edges could not be read is dropped before the
	// tick sees it, because a bead with no list is not a bead with no blockers
	// (specBoardDetails, gt-o70a9's rule for Admit's caller).
	Blockers []beads.IssueDep
	// Run is the formula, variables and actor the bead declares for its own
	// sling; the zero value is a bead that declares none (gt-j5cr2). The tick
	// fills it from the fresh read, before the seat is chosen.
	Run specdispatch.FormulaRun
	// order is the candidate's position among the slings of its tick, set
	// when the tick collects it; the slings run concurrently, so it is what
	// keeps a record of them in candidate order.
	order int
}

// specRoster is the live polecat picture the budget is built from: the seats
// the pool counts as taken per agent — live polecats, in-flight seat claims,
// the seats mid-landing, and the seats whose session is gone while their bead
// is still hooked (poolSeatSessions) — and the newest spawn.
type specRoster struct {
	Live map[string]int
	// DeadHooked counts, per agent, the seats inside Live that no tmux session
	// holds: a crashed polecat waiting for its supervised restart (gt-tldj4).
	DeadHooked map[string]int
	// ParkedStale counts, per agent, the parked seats with no session that have
	// been parked past agentpause.ParkedSeatRelease. They are not in Live: the seat is
	// released, and the count says so (gt-t7k92).
	ParkedStale map[string]int
	// Unrecorded names the polecats counted under config.UnknownSeatAgent
	// because they record no agent and the seat map has several seats.
	Unrecorded []string
	Newest     time.Time
	// Draining counts, per agent, the polecats the sling's own pool check holds
	// beyond the roster: a draining session the roster read as released, which
	// the sling still counts (no known creation time to release it by), or a
	// session new since the roster read (specDrainingSessions). The tick adds
	// them to its total, so the pre-check reads the count the sling refuses on
	// (gt-97ppg).
	Draining map[string]int
}

// specDispatchEnv is every side effect a tick has, so the cycle runs on fakes
// in tests.
type specDispatchEnv struct {
	Hold func() string
	// PendingInstall is the pending-install marker in force now, nil when no
	// install is pending: a gt install is in flight and no new sling starts
	// (installpending, gt-2rp64). It is read at the tick's clock, so a test's
	// Now judges the marker. Nil reads no marker.
	PendingInstall func(now time.Time) (*installpending.Marker, error)
	// StartupBackoff reports why an automatic sling of beadID must wait: its
	// last sling failed at session start and the resting window has not
	// elapsed (dispatch.StartupBackoff). Nil consults no record, which is what
	// a tick with no town to read wants.
	StartupBackoff func(beadID string) string
	Candidates     func() specBoardRead
	// Verdicts is the shape-and-label memory the scan serves an unchanged
	// candidate from (specCandidateVerdicts). Nil decides every candidate from
	// the bead it read, which is what a tick that keeps nothing does.
	Verdicts *specCandidateVerdicts
	Show     func(beadID string) (specdispatch.Spec, error)
	// ShowFresh is Show that never answers from an earlier read of the tick: the
	// last-moment re-read before a sling is the check that a submission did not
	// land in the window, so it asks bd. Nil falls back to Show (gt-u8rbh).
	ShowFresh func(beadID string) (specdispatch.Spec, error)
	// Children reads a bead's direct children for the container rule
	// (gt-gektq). The set is read here and handed to specdispatch, which never
	// queries.
	Children func(beadID string) ([]specdispatch.Child, error)
	RigHold  func(rig string) string
	// RevertInFlight is the revert the rig's red-main owner has building or
	// queued, from the state the landing worker writes; nil is none. It holds
	// that rig's red-main beads, never any other bead (gt-zkdwt).
	RevertInFlight func(rig string) *specdispatch.Revert
	Roster         func() (specRoster, error)
	// Annotate adds text as a comment unless the bead already carries a
	// comment starting with key, so each kind of note lands once.
	Annotate func(beadID, key, text string) error
	AddLabel func(beadID, label string) error
	// DropTimeoutRetry removes the timeout label and the kill count a sling
	// killed at its bound left on a bead. A failure of any other kind calls it,
	// so the dispatcher does not sling that bead again on a kill's retries
	// (gt-vaivp). Nil drops nothing.
	DropTimeoutRetry func(beadID string) error
	// BranchOnOrigin reports whether a bead's recorded resume branch still
	// exists on the rig's origin. An error means the answer is unknown, and
	// the candidate is left for the next tick (gt-gzhin.3).
	BranchOnOrigin func(rig, branch string) (bool, error)
	// FormulaExists errors when a formula a bead declares is not one the rig
	// can sling. It is asked only for a bead that declares one (gt-j5cr2).
	FormulaExists func(rig, formula string) error
	Sling         func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (polecat string, err error)
	Sleep         func(time.Duration)
	Now           func() time.Time
	Template      specdispatch.Template
	Budget        specdispatch.Budget // agents, caps and preferences; live counts filled per tick
	PerTick       int
	DryRun        bool
	// TownRoot is the town the tick runs on: its verdict ledger lives under
	// that town's runtime directory (specdispatch.LedgerPath, gt-8thox). Empty
	// records no ledger, which is what a test that checks only what the tick
	// decided gets.
	TownRoot string
	// LedgerWarn is where a ledger the tick could not read or write says so;
	// nil is stderr. The tick itself never fails over it.
	LedgerWarn io.Writer
	// Ledger is the file operations the ledger is read and written through;
	// nil is the town's own file (specdispatch.LoadLedger and Ledger.Save). A
	// test that needs one of them to fail injects it here, instead of making
	// the host's permissions decide whether the failure happens (gt-tcsnr).
	Ledger *specLedgerFile
	// Background hands the tick's slings back in the report instead of running
	// them (specSlingJob): the daemon runs each one in the background, so the
	// tick returns once it has planned (gt-5h70s).
	Background bool
	// InFlight is the slings the daemon still has running from earlier ticks.
	// Each one counts as a taken seat, its bead is not a candidate, and its
	// files hold overlapping candidates as a hooked bead's would (gt-5h70s).
	InFlight []specInFlight
	// MaxPriority is the ceiling on a candidate's priority number
	// (polecat_pool.max_priority): a bead numbered higher is backlog.
	MaxPriority int
	// ShapeGate is what the lint's verdict does to a candidate: off runs no
	// lint, warn holds a refused bead and comments the verdict, refuse also
	// labels it needs-shape (polecat_pool.shape_gate, gt-cq5gb, gt-f8ppx).
	ShapeGate string
	// OMThreshold reads the review threshold from the rig's own .om.json: the
	// number the overrun-stop box bands (gt-b9j3o.4). 0 is a rig that sets
	// none, and an error is a read that failed, which the tick reports rather
	// than reading as 0.
	OMThreshold func(rig string) (float64, error)
	// Target reads the files the branch a rig's work lands on holds, for the
	// lint's path check (gt-7t1im). Nil asks nothing, which is what a test of
	// some other hold wants; the tick itself always wires it.
	Target func(rig string) *specdispatch.TargetTree
	// Reads is the ledger the tick's own reads go through, so the tick can
	// name the phase each one was made under (gt-139dp). Nil counts no phase,
	// which is what a test of what the tick decided wants.
	Reads *specTickReads
}

// specLedgerFile is the verdict ledger's two file operations, so a test can
// drive the tick through a read or a write that fails without depending on the
// permissions of the host it runs on. A nil field is the real operation.
type specLedgerFile struct {
	Load func(path string) (*specdispatch.Ledger, error)
	Save func(path string, ledger *specdispatch.Ledger) error
}

// ledgerFile is the env's ledger access, or the town's own file when it sets
// none.
func (e specDispatchEnv) ledgerFile() specLedgerFile {
	f := specLedgerFile{
		Load: specdispatch.LoadLedger,
		Save: func(path string, l *specdispatch.Ledger) error { return l.Save(path) },
	}
	if e.Ledger != nil {
		if e.Ledger.Load != nil {
			f.Load = e.Ledger.Load
		}
		if e.Ledger.Save != nil {
			f.Save = e.Ledger.Save
		}
	}
	return f
}

// target asks the env for rig's target inventory. A tick built without the
// reader lets the lint check no paths.
func (e specDispatchEnv) target(rig string) *specdispatch.TargetTree {
	if e.Target == nil {
		return nil
	}
	return e.Target(rig)
}

// overrunThreshold asks the env for rig's review threshold. A tick built with no
// reader is a construction error, not a rig that sets no rubric: it reports and
// flags nothing, so the gap is visible rather than silently unflagging every
// bead.
func (e specDispatchEnv) overrunThreshold(rig string) (float64, error) {
	if e.OMThreshold == nil {
		return 0, errors.New("this tick reads no review threshold")
	}
	return e.OMThreshold(rig)
}

// showFresh is the read of a bead made at the last moment before its sling.
func (e specDispatchEnv) showFresh(beadID string) (specdispatch.Spec, error) {
	if e.ShowFresh != nil {
		return e.ShowFresh(beadID)
	}
	return e.Show(beadID)
}

// runSpecDispatchCycle is one tick. It never guesses: under the default warn
// gate a bead the lint refuses is held — skipped unshaped and commented once —
// and one that needs planning is held too, labeled needs-planning and reported
// by its planning state; under refuse the refused bead is also labeled
// needs-shape, and the planning bead is held the same way. A planning bead is
// never slung under either gate (gt-tod3q): it is the planner's, and the tick
// only labels, comments and reports its state ("needs plan" or "plan proposed",
// gt-4k3fj.14). Off runs no lint. A bead labeled spec-shape-waived waives the
// lint and dispatches under any gate (gt-f8ppx). A container — a bead with at
// least one open child — is held until its children close, under any gate
// (gt-gektq), and so is a candidate whose own text names a path a bead already
// hooked to a working seat names: the same defect filed twice inside one
// dispatch window (gt-z80p9). A candidate is slung only onto a free seat that
// takes it, and a skip leaves it ready for the next tick.
func runSpecDispatchCycle(env specDispatchEnv) specDispatchReport {
	report := specDispatchReport{Template: env.Template.Source}
	defer func() { specRecordTickVerdicts(env, report) }()
	// The ledger states what this tick saw, so every way out of the tick
	// records — a held tick and a tick that read no candidates record an empty
	// set, which clears the verdicts of beads the dispatcher has since taken
	// (gt-8thox). A dry run decides without acting, so it records nothing and
	// leaves the last real tick's reasons in place.
	if hold := env.Hold(); hold != "" {
		report.Hold = hold
		return report
	}
	// A gt install in flight stops every new sling, this tick's slings
	// included, before the tick spends a read on them (gt-2rp64).
	if hold, errLine := env.installPendingStop(); hold != "" || errLine != "" {
		report.Hold = hold
		if errLine != "" {
			report.Errors = append(report.Errors, errLine)
		}
		return report
	}
	env.Reads.setPhase(specPhaseRoster)
	rosterAt := env.Now()
	roster, err := env.Roster()
	report.RosterTook = env.Now().Sub(rosterAt)
	report.RosterCalls = env.Reads.endPhase()
	if err != nil {
		// A roster that cannot be counted is not an empty one: dispatching on
		// it could exceed every cap.
		report.Errors = append(report.Errors, "roster unreadable, not dispatching: "+err.Error())
		return report
	}
	budget := env.Budget
	budget.Seats = append([]specdispatch.Seat(nil), env.Budget.Seats...)
	budget.SetLive(specLiveWithInFlight(roster.Live, env.InFlight))
	budget.SetDeadHooked(roster.DeadHooked)
	budget.SetDraining(roster.Draining)
	budget.NewestSpawn = specNewestSpawn(roster.Newest, env.InFlight)
	// The picture reads each seat's window at the tick's clock; the budget was
	// built a moment before, and its construction time is no longer the one the
	// report speaks for (gt-t8new).
	budget.Now = env.Now()
	report.Roster = budget.Picture()
	if len(roster.Unrecorded) > 0 {
		sort.Strings(roster.Unrecorded)
		report.Notices = append(report.Notices, fmt.Sprintf(
			"roster: %d polecat(s) record no agent and count under %q, on no seat: %s",
			len(roster.Unrecorded), config.UnknownSeatAgent, strings.Join(roster.Unrecorded, ", ")))
	}

	env.Reads.setPhase(specPhaseBoard)
	boardAt := env.Now()
	read := env.Candidates()
	report.BoardTook = env.Now().Sub(boardAt)
	report.BoardCalls = env.Reads.endPhase()
	report.Errors = append(report.Errors, read.Errors...)
	report.Candidates = len(read.Candidates)
	report.LabeledFailed = read.LabeledFailed
	report.Notices = append(report.Notices, read.Retried...)
	// The read's own skips are the board-carried refusals: the tick and the
	// dry run print them beside the ones the tick finds itself (gt-k1g3e).
	report.Skipped = append(report.Skipped, read.Skipped...)
	// The tick slings at most perTick candidates. The config ceiling wins when
	// set; otherwise the bound is the free seats the roster shows, so a tick
	// fills the pool instead of three seats of nine (2026-10-21: max_per_tick
	// 3 bound 10 of 32 daytime ticks while seats turned over every 7 minutes).
	perTick := env.PerTick
	if perTick <= 0 {
		perTick = budget.FreeSeats()
	}
	var pending []specPendingSling
	// One notice per rig per tick: the revert record is the rig's, not a
	// candidate's, so a rig with several red-main beads still logs it once
	// (gt-wgyca).
	staleNoted := map[string]bool{}
	// The overrun box reads a rig's review threshold, once per rig, and its
	// failure is reported once rather than once per candidate (gt-b9j3o.4).
	thresholds := specOMThresholds{env: env, reads: map[string]specOMThresholdRead{}}
	thresholdNoted := map[string]bool{}
	inFlight := map[string]bool{}
	for _, f := range env.InFlight {
		inFlight[f.Bead] = true
	}

	env.Reads.setPhase(specPhaseCandidates)
	candidatesAt := env.Now()
	for _, c := range read.Candidates {
		id := c.Spec.ID
		// A sling of an earlier tick is still running for this bead: it may not
		// have hooked it yet, so the board still shows it ready (gt-5h70s).
		if inFlight[id] {
			continue
		}
		// The spec is the board row the candidate was drawn from, so the loop
		// spends no read per candidate (gt-bqj8q). The last-moment re-read
		// before the sling is what catches a bead that changed since the board.
		full := c.Spec
		// The shape and label verdicts of a candidate whose board row has not
		// moved are the ones the last tick reached, so it is decided from the
		// record and its design is not read again (specBoardDetails). A refused
		// candidate is not read at all, and its skip is all the tick knows
		// (gt-km9jt).
		remembered, served := env.Verdicts.lookUp(c)
		eligible, why := specdispatch.Eligible(full, env.MaxPriority)
		if served {
			eligible, why = remembered.Eligible, remembered.Skip
		}
		if !eligible {
			if !served {
				env.Verdicts.record(c, specCandidateVerdict{Skip: why})
			}
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s (%s)", id, specdispatch.SkipNotEligible, why)})
			continue
		}
		// A held rig is skipped before any read or write of its beads, and
		// before the admission check and the per-tick limit below: the hold,
		// not a blocker or the tick's seat count, is what stops these beads, so
		// their skip line names the hold rather than a reason that would hide it
		// (gt-vty1v).
		if hold := env.RigHold(c.Rig); hold != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s %s", id, specdispatch.SkipRigHold, hold)})
			continue
		}
		// Blocked-work admission (gt-e3hl2, gt-ndad8): a bead one of whose
		// blocking edges is still unresolved is work something else has to
		// finish first, so a seat spent on it is a seat spent twice. The ready
		// board is not that check — bd's ready query reads a deferred blocker as
		// resolved, and this dispatcher does not, so a bead the board lists can
		// still be blocked (gt-ndad8) — and the board is a snapshot of a tick
		// that can run for minutes (gt-ewrtm). The edges are the board's detail
		// read (specBoardDetails), so the loop spends no read here, and a
		// candidate whose edges could not be read never reached it. A refusal is
		// a skip, not a failure: the bead stays ready and the next tick asks
		// again once the blocker closes.
		//
		// The edges are this tick's own read (specBoardDetails), never remembered.
		blockers := c.Blockers
		if why := specBlocked(sling.Admit(blockers)); why != "" {
			// Not remembered: the bead stays ready and the next tick reads its
			// edges again.
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s", id, why)})
			continue
		}
		// A bead may declare the formula that runs it. A declaration the tick
		// cannot read, or a formula the rig does not have, is a skip with its
		// reason: the bead stays ready and is never slung with the wrong
		// formula (gt-j5cr2).
		run, err := specdispatch.FormulaRunOf(full)
		if err != nil {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s", id, err)})
			continue
		}
		if run.Declared() {
			if err := env.FormulaExists(c.Rig, run.Formula); err != nil {
				report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s: %s", id, specdispatch.SkipUnknownFormula, run.Formula)})
				continue
			}
		}
		c.Run = run
		// A red-main bead is the fix forward for a breakage the owner is
		// already undoing; a seat spent on it races the revert over the same
		// package (gt-zkdwt). The hold lifts as soon as the revert lands, is
		// rejected, or turns out never to have been filed.
		rv, staleWhy := specdispatch.ResolveRevert(env.RevertInFlight(c.Rig), env.Now())
		if staleWhy != "" && !staleNoted[c.Rig] {
			// The record is ignored, so say so once: a silent ignore hides a
			// crashed revert behind a held bead (gt-wgyca).
			staleNoted[c.Rig] = true
			report.Notices = append(report.Notices, fmt.Sprintf("%s: ignoring stale revert: %s", c.Rig, staleWhy))
		}
		if why := specdispatch.RedMainHold(full, rv); why != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s%s", id, specdispatch.SkipHeld, why)})
			continue
		}
		// The verdict is read under every gate: the gate decides what a refusal
		// does to the candidate, and the overrun box asks the lint the same
		// question a dispatcher does — is this bead shaped — with no gate at all
		// (gt-b9j3o.4). The target branch's files are the candidate's rig's: a
		// bead whose Constraints name a file the branch never got is work on a
		// base that is not there (gt-7t1im).
		var verdict specdispatch.LintVerdict
		if served {
			verdict = remembered.Shape
		} else {
			full.Target = env.target(c.Rig)
			verdict = specdispatch.Lint(full, env.Template)
			env.Verdicts.record(c, specCandidateVerdict{Eligible: true, Shaped: true, Shape: verdict})
		}
		shapeNote := ""
		if env.ShapeGate != specdispatch.ShapeGateOff {
			switch {
			case verdict.Clean():
			case full.HasLabel(specdispatch.ShapeWaivedLabel):
				// The operator waived this bead's shape lint (gt-f8ppx): it
				// dispatches under any gate, with the verdict left on it as the
				// one comment, in seat-refill's own note text (gt-cq5gb).
				shapeNote = verdict.ShapeNote()
			case env.ShapeGate == specdispatch.ShapeGateWarn:
				// The default gate holds a bead the lint refuses, and one that
				// needs planning, rather than slinging it (gt-f8ppx, gt-tod3q):
				// a flash polecat handed work the bead does not pin down wanders
				// off it, and a spec one worker cannot take in one MR is the
				// planner's, not a seat's. A refused bead gets the one SHAPE
				// note — written here, not on the dispatch path below, because a
				// held bead never reaches that path — and stays unlabeled in
				// warn, for the operator to shape or waive. A planning bead is
				// labeled needs-planning and commented by specPlanHold, the same
				// as under refuse.
				if verdict.Route == specdispatch.RoutePlanning {
					specPlanHold(&report, env, c, full, verdict)
					continue
				}
				if reason := verdict.UnshapedReason(); reason != "" {
					report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s", id, reason)})
					if !env.DryRun {
						if note := verdict.ShapeNote(); note != "" {
							if err := env.Annotate(id, note, note); err != nil {
								report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
							}
						}
					}
					continue
				}
				shapeNote = verdict.ShapeNote()
			default:
				line := verdict.Line(id)
				switch verdict.Route {
				case specdispatch.RouteRefuse:
					report.Refused = append(report.Refused, specDispatchEntry{Bead: id, Rig: c.Rig, Line: line})
					if !env.DryRun {
						// The label is the overseer's queue of beads to reshape,
						// and a labeled bead stays a candidate: reshaping it is
						// what puts it back within the seat's reach.
						if !full.HasLabel(specShapeLabel) {
							if err := env.AddLabel(id, specShapeLabel); err != nil {
								report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", id, err))
							}
						}
						if note := verdict.ShapeNote(); note != "" {
							if err := env.Annotate(id, note, note); err != nil {
								report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
							}
						}
					}
					continue
				case specdispatch.RoutePlanning:
					specPlanHold(&report, env, c, full, verdict)
					continue
				}
			}
		}

		// A bead whose children are open is a container, not a unit of work:
		// its scope is already sliced into children a seat will take, so
		// spending one on the parent duplicates them (gt-gektq). An unreadable
		// child set is not an empty one — the bead might be a container — so it
		// is left for the next tick rather than slung.
		// The ready board's dependent_count cannot answer this. bd counts only
		// incoming blocks edges there, while the parent-child edge is a
		// separate type, so the count is zero for a parent whose only children
		// are parent-child ones (gt-603wu). The children view is the predicate,
		// as it is on the peer path, and it is read every tick: the memory
		// keeps no children (specCandidateChildren). A refused candidate never
		// reaches this line, since the eligibility check above skips it, so its
		// children are left unread on purpose.
		children, err := env.Children(id)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: cannot read children: %v", id, err))
			continue
		}
		full.Children = children
		if why := specdispatch.OpenChildHold(full); why != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s%s", id, specdispatch.SkipHeld, why)})
			continue
		}

		if len(pending)+len(report.Dispatched) >= perTick {
			limit := fmt.Sprintf("%s %d reached", specdispatch.SkipPerTickLimit, perTick)
			if perTick == 0 {
				limit = specdispatch.SkipNoSeat + " no free seat this tick"
			}
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s", id, limit)})
			continue
		}
		// A bead whose last sling failed at session start rests out its backoff
		// before an automatic dispatcher tries it again (gt-811f6). The failed
		// sling leaves the bead open and unassigned, so without this the next
		// tick re-slings it into the same failure and spends one of its respawn
		// attempts each time. An operator's own `gt sling` is the retry the
		// backoff leaves room for and does not come through here.
		if env.StartupBackoff != nil {
			if why := env.StartupBackoff(id); why != "" {
				report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s %s", id, specdispatch.SkipStartupBackoff, why)})
				continue
			}
		}
		// Re-read the clock at the choice, per candidate: the board read and the
		// holds above can carry the tick past a seat's window boundary, and the
		// snapshot taken when the budget was built is not the clock the choice
		// runs on (gt-t8new).
		budget.Now = env.Now()
		seat := specdispatch.ChooseSeat(budget, full)
		if seat.Skip {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s %s", id, specdispatch.SkipNoSeat, seat.Reason)})
			continue
		}
		// A bead readied again after its holder died carries the branch its
		// work survives on (patrol_scan's resume_branch note). Continuing that
		// branch keeps the work; a fresh sling would start a second polecat
		// from main over it (gt-gzhin.2).
		recorded := patrolscan.ResumeBranchFromNotes(full.Notes)
		resume, gone := "", ""
		if recorded != "" {
			exists, err := env.BranchOnOrigin(c.Rig, recorded)
			if err != nil {
				// Unknown is not gone: a failed probe must not hand preserved
				// work to a polecat starting from main.
				report.Errors = append(report.Errors, fmt.Sprintf("%s: cannot check resume branch %s: %v", id, recorded, err))
				continue
			}
			if exists {
				resume = recorded
			} else {
				gone = recorded
			}
		}
		// The overrun-stop flag, decided on the seat the tick just chose
		// (gt-b9j3o.4): a bead in the box is slung as it would be, and the note
		// travels with the sling rather than being written here — a note that
		// says the bead was dispatched must not be written before the sling
		// that dispatches it. A threshold that cannot be read is reported once
		// for its rig and flags nothing: an unknown threshold is not a match.
		overrunNote := ""
		verdictOverrun, err := specdispatch.InOverrunBox(specdispatch.OverrunFacts{
			Type: full.Type, Priority: full.Priority, Shaped: verdict.Clean(), Labels: len(full.Labels),
			Model:     models.ModelFamily(seat.Agent),
			Threshold: func() (float64, error) { return thresholds.get(c.Rig) },
		})
		switch {
		case err != nil:
			if !thresholdNoted[c.Rig] {
				thresholdNoted[c.Rig] = true
				report.Errors = append(report.Errors, fmt.Sprintf("%s: cannot read the overrun box's review threshold: %v", c.Rig, err))
			}
		case verdictOverrun.Match:
			overrunNote = specdispatch.OverrunNote(models.ModelFamily(seat.Agent), verdictOverrun.Threshold)
		}
		// The warn-mode verdict goes on the bead beside its sling, once per
		// distinct verdict (seat-refill's shape_note).
		if shapeNote != "" && !env.DryRun {
			if err := env.Annotate(id, shapeNote, shapeNote); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
			}
		}
		entry := specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent}
		if env.DryRun {
			entry.Line = fmt.Sprintf("%s: would sling to %s on %s (%s)", id, c.Rig, seat.Agent, seat.Reason)
			entry.Line += specFormulaNote(c.Run) + specResumeNote(resume, gone)
			if shapeNote != "" {
				entry.Line += "; " + shapeNote
			}
			if overrunNote != "" {
				entry.Line += "; " + overrunNote
			}
			report.Dispatched = append(report.Dispatched, entry)
			budget.Bump(seat.Agent, budget.Now)
			continue
		}

		// Everything the tick knows about this bead is a read from before the
		// seat was chosen, and a submission can land in that window (gt-ue13q,
		// gt-01gix). Re-read at the last moment and put the fresh bead through
		// the same predicate the board drew with — the label, the READY TO LAND
		// block through SubmittedForLanding, and an assignee all hold there. A
		// read that fails holds too: unknown is not eligible.
		latest, err := env.showFresh(id)
		if err != nil {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent, Line: fmt.Sprintf("%s: %s: re-read failed: %s", id, specStaleReason, firstErrLine(err))})
			continue
		}
		if ok, why := specdispatch.Eligible(latest, env.MaxPriority); !ok {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent, Line: fmt.Sprintf("%s: %s: %s", id, specStaleReason, why)})
			continue
		}
		// Collected now, slung below: the seat is taken on the budget at once
		// so the next candidate's choice sees it, and the slings of one tick
		// run together instead of one after another (the sling, not the
		// reads, was the tick: 45-70 s each in series, 2026-10-21).
		c.order = len(pending)
		pending = append(pending, specPendingSling{c: c, id: id, seat: seat, resume: resume, gone: gone, shapeNote: shapeNote, overrunNote: overrunNote, entry: entry})
		budget.Bump(seat.Agent, budget.Now)
	}
	report.CandidatesTook = env.Now().Sub(candidatesAt)
	report.CandidatesCalls = env.Reads.endPhase()
	slingAt := env.Now()
	if env.Background {
		specHandOffPendingSlings(pending, &report)
	} else {
		specRunPendingSlings(env, pending, &report, &budget)
	}
	report.SlingTook = env.Now().Sub(slingAt)
	return report
}

// The classes the verdict ledger records a bead under (gt-8thox): the tick
// list the bead landed in. They are declared here, not in specdispatch, so the
// package that decides which list a bead goes in is the one that names it.
const (
	specVerdictSkipped  = "skipped"
	specVerdictRefused  = "refused"
	specVerdictPlanning = "planning"
	specVerdictFailed   = "failed"
	specVerdictDeferred = "deferred"
)

// specTickVerdicts folds a finished tick's decisions into ledger entries: one
// per bead the tick did not sling, carrying its class and the line it was
// reported under as the reason. The precedence is the order of the lists
// below: a bead in two of them takes the class and reason of the later one, so
// a bead the tick both skipped and deferred is recorded as deferred (gt-8thox).
// The fold is deterministic — each list keeps the report's order, and a bead
// that appears twice keeps the slot of its first appearance — but the ledger
// keys by bead, so nothing downstream depends on that order.
func specTickVerdicts(report specDispatchReport) []specdispatch.Entry {
	lists := []struct {
		class   string
		entries []specDispatchEntry
	}{
		{specVerdictSkipped, report.Skipped},
		{specVerdictRefused, report.Refused},
		{specVerdictPlanning, report.Planning},
		{specVerdictFailed, report.Failed},
		{specVerdictDeferred, report.Deferred},
	}
	var out []specdispatch.Entry
	at := map[string]int{}
	for _, l := range lists {
		for _, e := range l.entries {
			entry := specdispatch.Entry{Bead: e.Bead, Rig: e.Rig, Class: l.class, Reason: e.Line}
			if i, ok := at[e.Bead]; ok {
				out[i] = entry
				continue
			}
			at[e.Bead] = len(out)
			out = append(out, entry)
		}
	}
	return out
}

// specRecordTickVerdicts records the tick's verdicts in its town's ledger
// (gt-8thox). A tick with no town to record to, and a dry run, record nothing:
// the dry run decides without acting, so its reasons would overwrite the real
// tick's.
func specRecordTickVerdicts(env specDispatchEnv, report specDispatchReport) {
	if env.TownRoot == "" || env.DryRun {
		return
	}
	warn := env.LedgerWarn
	if warn == nil {
		warn = os.Stderr
	}
	specRecordVerdictsTo(warn, env.TownRoot, env.Now(), specTickVerdicts(report), env.ledgerFile())
}

// specVerdictLedgerLockPath is the flock that serializes the ticks writing one
// town's ledger. It sits beside the ledger rather than on it, because the
// ledger's own file is replaced by a rename, which would carry a lock taken on
// it away with the old file.
func specVerdictLedgerLockPath(path string) string { return path + ".flock" }

// specLockVerdictLedger tries the lock that makes one town's load-fold-save a
// single step, and does not wait for it: a holder that is stuck would hold up
// every later tick. Two ticks — the daemon's and an operator's `gt spec
// dispatch` — that each loaded before either saved would both fold onto the
// same file and the second save would drop the first's sighting, losing a tick
// of every verdict it held (gt-tcsnr). The second result is false when another
// tick holds the lock.
func specLockVerdictLedger(path string) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, fmt.Errorf("creating the ledger directory: %w", err)
	}
	return lock.FlockTryAcquire(specVerdictLedgerLockPath(path))
}

// specRecordVerdictsTo folds one tick's entries into the verdict ledger at
// townRoot and saves it, all under the ledger's lock. A ledger it cannot read
// or write is one warning line on warn and nothing more: the ledger reports on
// the tick, and a tick that decided and slung correctly is not failed by the
// file it could not write. A ledger it could not read is left as it is rather
// than recorded onto empty, because that would reset the first-seen time of
// every verdict it holds (gt-8thox). A ledger that does not parse is the
// exception: it can never be read again, so the tick replaces it and says so,
// where leaving it would warn on every tick and heal nothing (gt-tcsnr). The
// tick moves those bytes aside first, to a sibling named for the tick's time,
// and says where they are: a replaced ledger never loses what it could not
// read. If the bytes cannot be moved aside, the ledger is not replaced.
//
// A tick that finds another tick holding the ledger records nothing and says
// so: it neither waits for that tick nor saves onto the file it is replacing.
func specRecordVerdictsTo(warn io.Writer, townRoot string, at time.Time, entries []specdispatch.Entry, file specLedgerFile) {
	path := specdispatch.LedgerPath(townRoot)
	unlock, held, err := specLockVerdictLedger(path)
	if err != nil {
		fmt.Fprintf(warn, "spec dispatch: verdict ledger not recorded: %v\n", err)
		return
	}
	if !held {
		fmt.Fprintf(warn, "spec dispatch: verdict ledger not recorded: another tick is saving it\n")
		return
	}
	defer unlock()
	ledger, err := file.Load(path)
	var corrupt *specdispatch.CorruptLedgerError
	if err != nil && !errors.As(err, &corrupt) {
		fmt.Fprintf(warn, "spec dispatch: verdict ledger not recorded: %v\n", err)
		return
	}
	var kept string
	if corrupt != nil {
		kept = path + ".corrupt-" + at.UTC().Format("20060102T150405Z")
		if err := os.Rename(path, kept); err != nil {
			fmt.Fprintf(warn, "spec dispatch: verdict ledger not recorded: keeping the unparsable ledger: %v\n", err)
			return
		}
	}
	ledger.Record(at, entries)
	if err := file.Save(path, ledger); err != nil {
		fmt.Fprintf(warn, "spec dispatch: verdict ledger not recorded: %v\n", err)
		return
	}
	if corrupt != nil {
		fmt.Fprintf(warn, "spec dispatch: verdict ledger replaced: %v; the unparsable bytes are kept at %s\n", corrupt, kept)
	}
}

// specOMThresholdRead is one rig's review-threshold read: the value, or the
// error it failed with. A failed read keeps its error rather than reading as 0,
// so the box treats it as unknown and the tick logs it (gt-b9j3o.4).
type specOMThresholdRead struct {
	value float64
	err   error
}

// specOMThresholds is a tick's memo of each rig's review threshold. The overrun
// box asks it lazily and per candidate; the file it reads does not change
// inside a tick, so the read happens once per rig.
type specOMThresholds struct {
	env   specDispatchEnv
	reads map[string]specOMThresholdRead
}

// get answers rig's review threshold, reading it on the first ask.
func (t *specOMThresholds) get(rig string) (float64, error) {
	r, ok := t.reads[rig]
	if !ok {
		r.value, r.err = t.env.overrunThreshold(rig)
		t.reads[rig] = r
	}
	return r.value, r.err
}

// specPendingSling is a candidate the tick has decided to sling: its seat is
// chosen and counted on the budget; the last-moment re-read and the sling
// itself are still to run.
type specPendingSling struct {
	c         specCandidate
	id        string
	seat      specdispatch.SeatChoice
	resume    string
	gone      string
	shapeNote string
	// overrunNote is the overrun-stop flag the sling writes on its bead once it
	// has dispatched it; empty for a bead outside the box (gt-b9j3o.4).
	overrunNote string
	entry       specDispatchEntry
}

// specSlingOutcome is what one sling produced, applied to the report in
// candidate order after every sling of the tick has returned. The writes a
// sling owes the bead (the failed label, the notes) are collected here and
// made on the tick's goroutine, so the report and the bead writes keep the
// candidate order and the env's writers are never called concurrently.
type specSlingOutcome struct {
	kind  specSlingKind
	entry specDispatchEntry
	label string      // a label to add to the bead, when set
	notes [][2]string // Annotate key, text pairs
	// refused is set when the sling was refused before it spawned, or after it
	// rolled its spawn back: its charged seat is handed back. Any other sling
	// that did not succeed keeps the charge for the tick (gt-pto2f).
	refused bool
	took    time.Duration
}

type specSlingKind int

const (
	specSlingDispatched specSlingKind = iota
	specSlingSkipped
	specSlingFailed
)

// specRunPendingSlings runs the tick's slings concurrently, one goroutine per
// pending candidate, and applies their outcomes in candidate order. Two
// slings cannot take one seat: each writes its in-flight claim under the pool
// decision lock before its session exists (gt-t8q5), and the occupancy each
// one reads is the tick's snapshot plus the live sessions and claims of the
// moment (snapshotOccupied).
//
// A refused sling gives its charged seat back (gt-9x16z): the tick charged the
// seat when it collected the candidate, and the charge stays in the budget only
// while a sling of this tick may still fill it. A sling that failed in any
// other way keeps its charge until the next tick's snapshot recounts the seats.
func specRunPendingSlings(env specDispatchEnv, pending []specPendingSling, report *specDispatchReport, budget *specdispatch.Budget) {
	if len(pending) == 0 {
		return
	}
	outcomes := make([]specSlingOutcome, len(pending))
	var wg sync.WaitGroup
	for i := range pending {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = specSlingOne(env, pending[i])
		}(i)
	}
	wg.Wait()
	var slowest time.Duration
	for _, o := range outcomes {
		if o.took > slowest {
			slowest = o.took
		}
		specApplyOutcome(env, report, o)
		// Only a refusal proves the sling spawned nothing. A failure or an
		// exhausted contention may have hooked the bead or started a session
		// before it returned, so their seat stays charged for this tick (gt-pto2f).
		if o.refused {
			budget.Release(o.entry.Agent)
		}
	}
	report.SlingsRun = len(pending)
	report.SlowestSling = slowest
}

// specApplyOutcome adds one sling's outcome to the report and makes the writes
// the sling owes its bead: the failed label and the notes (specSlingOutcome).
// A failed background sling goes through it too, so it labels its bead as a
// foreground failure does (gt-5h70s).
func specApplyOutcome(env specDispatchEnv, report *specDispatchReport, o specSlingOutcome) {
	switch o.kind {
	case specSlingDispatched:
		report.Dispatched = append(report.Dispatched, o.entry)
	case specSlingSkipped:
		report.Skipped = append(report.Skipped, o.entry)
		// The tick line caps its skipped list, so a deferral can land in the
		// unnamed tail; the daemon logs this list one line per entry, so
		// every deferral's bead and reason stay readable (gt-4wcqd).
		report.Deferred = append(report.Deferred, o.entry)
	case specSlingFailed:
		report.Failed = append(report.Failed, o.entry)
	}
	if o.label != "" {
		if err := env.AddLabel(o.entry.Bead, o.label); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", o.entry.Bead, err))
		}
		// A failure that is not a kill at the bound spends the retries an
		// earlier kill left on the bead: its label stays for a person, as any
		// other failure's does (gt-vaivp).
		if o.label == specdispatch.DispatchFailedLabel && env.DropTimeoutRetry != nil {
			if err := env.DropTimeoutRetry(o.entry.Bead); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: dropping the timeout retry: %v", o.entry.Bead, err))
			}
		}
	}
	for _, n := range o.notes {
		if err := env.Annotate(o.entry.Bead, n[0], n[1]); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", o.entry.Bead, err))
		}
	}
}

// specSlingOne is one sling with its contention retries; the last-moment
// re-read already passed on the tick goroutine. It calls nothing that writes
// the bead; the outcome says what the tick should write.
func specSlingOne(env specDispatchEnv, p specPendingSling) specSlingOutcome {
	start := env.Now()
	id, c, seat, entry := p.id, p.c, p.seat, p.entry
	done := func(o specSlingOutcome) specSlingOutcome {
		o.took = env.Now().Sub(start)
		return o
	}
	var polecat string
	attempts, err := specdispatch.RetryOnContention(specdispatch.RetryAttempts, env.Sleep, rand.New(rand.NewSource(env.Now().UnixNano())), func() error { //nolint:gosec // G404: backoff jitter
		var slingErr error
		polecat, slingErr = env.Sling(c, p.resume, seat)
		return slingErr
	})
	if err != nil {
		// A sandbox image that lags the installed gt is the seat's precondition,
		// cleared by rebuilding the image; the label this path would add would
		// hold the bead out of the queue until an operator cleared it. The
		// attempt may have hooked the bead before the refusal, so the seat stays
		// charged for this tick (gt-pto2f), and the next tick slings it again.
		if errors.Is(err, session.ErrImageDrift) {
			entry.Line = fmt.Sprintf("%s: sling deferred, the sandbox image lags the installed gt (it is rebuilt after an install): %s", id, firstErrLine(err))
			return done(specSlingOutcome{kind: specSlingSkipped, entry: entry})
		}
		// A rig's cache volume that cannot be created is the seat's precondition
		// too, not the bead's. It is deferred for the same reason as the drift
		// above (gt-td6pl).
		if errors.Is(err, session.ErrCacheVolume) {
			entry.Line = fmt.Sprintf("%s: sling deferred, the sandbox cache volume cannot be created: %s", id, firstErrLine(err))
			return done(specSlingOutcome{kind: specSlingSkipped, entry: entry})
		}
		if isSpecSlingRefusal(err) {
			entry.Line = fmt.Sprintf("%s: sling deferred: %s", id, firstErrLine(err))
			return done(specSlingOutcome{kind: specSlingSkipped, entry: entry, refused: true})
		}
		// Contention that outlasted every retry is a deferral, not a failure:
		// the label this path would add excludes the bead until an operator
		// clears it, and contention clears by itself. The next tick's fresh
		// re-read refuses a bead with an assignee, so a sling that hooked the
		// bead before its error is not slung twice (gt-9x16z). The attempt may
		// have hooked it, so the seat stays charged for this tick (gt-pto2f).
		if specdispatch.IsSerializationFailure(err) {
			entry.Line = fmt.Sprintf("%s: sling deferred, %v after %d attempts: %s", id, specdispatch.ErrRetriesExhausted, attempts, firstErrLine(err))
			return done(specSlingOutcome{kind: specSlingSkipped, entry: entry})
		}
		// A failed sling is not known to have spawned nothing: a hook or a
		// session start can fail after it took effect, and the rollback is
		// best-effort. Its seat stays charged for this tick (gt-pto2f).
		excluded := fmt.Sprintf("excluded from the queue until label %s is cleared", specdispatch.DispatchFailedLabel)
		failLine := fmt.Sprintf("%s: dispatch failed, left unassigned: %s; %s", id, firstErrLine(err), excluded)
		entry.Line = failLine
		note := specDispatchNotePrefix + failLine
		return done(specSlingOutcome{kind: specSlingFailed, entry: entry, label: specdispatch.DispatchFailedLabel, notes: [][2]string{{note, note}}})
	}
	entry.Polecat = polecat
	entry.Line = fmt.Sprintf("%s: slung to %s/%s on %s (%s)", id, c.Rig, polecat, seat.Agent, seat.Reason)
	entry.Line += specFormulaNote(c.Run) + specResumeNote(p.resume, p.gone)
	if attempts > 1 {
		entry.Line += fmt.Sprintf(" after %d attempts", attempts)
	}
	if p.shapeNote != "" {
		entry.Line += "; " + p.shapeNote
	}
	o := specSlingOutcome{kind: specSlingDispatched}
	if p.overrunNote != "" {
		// The flag rides the outcome, not the tick: the note says the bead was
		// dispatched, and a sling that deferred or failed dispatched nothing
		// (gt-b9j3o.4).
		entry.Line += "; " + p.overrunNote
		o.notes = append(o.notes, [2]string{specdispatch.OverrunNoteKey, p.overrunNote})
	}
	o.entry = entry
	if p.gone != "" {
		note := fmt.Sprintf("%s%s: resume branch %s is gone from origin; dispatched fresh (gt-gzhin.3)", specDispatchNotePrefix, id, p.gone)
		o.notes = append(o.notes, [2]string{note, note})
	}
	return done(o)
}

// specHoldFiles is the file set the peer-hold question reads for one bead: the
// paths its text names, minus the reference documents its Constraints section
// does not list as work (gt-6t44f). A bead whose Constraints section is missing,
// empty or unparsed keeps every path: it has not said which cited documents it
// changes, and an unknown set is not an empty one (gt-zs8fq). The sling-time
// guard keeps every named path either way. A Constraints section that names no
// path (a follow-up's fixed text) counts the documents its Goal names as the
// work instead (gt-1e5s8).
// A missing or empty Goal leaves that work unknown too, so it keeps every path
// as well (gt-zs8fq). text is the bead's own words the files were extracted
// from (contentRefs.Text): a path every mention of which carries the
// (read-only) marker is not work and drops out (gt-5v13p).
func specHoldFiles(files []string, description, text string) []string {
	sections := specdispatch.Sections(description)
	constraints, ok := sections["constraints"]
	if !ok || constraints == "" {
		return files
	}
	listed := extractContentRefs(constraints).Files
	if len(listed) == 0 {
		if sections["goal"] == "" {
			return files
		}
		listed = extractContentRefs(sections["goal"]).Files
	}
	return specdispatch.HoldFiles(files, listed, text)
}

// specHeldStatuses are the statuses that mean a working seat holds the bead:
// the sling hooks it, and the polecat's claim makes it in_progress.
var specHeldStatuses = []string{beads.StatusHooked, string(beads.StatusInProgress)}

// heldBeadFrom reduces one compared bead to the fields the peer-hold question
// reads, or reports false when no working seat holds it. An epic is not held: its
// prose enumerates its children's files, so a claimed epic or wayfinder map
// would otherwise hold every ready child that names one (the sling-time guard
// skips epics for the same reason, findDuplicateMatches). Nor is a bead no
// polecat seat holds (specdispatch.WorksSeat, gt-xrog0, gt-asvow).
//
// The guard's peer-hold question is the one caller that reduces through here,
// so a seat that holds a peer is read the same way whenever it is asked.
func heldBeadFrom(c duplicateCandidate) (specdispatch.HeldBead, bool) {
	if c.IsEpic() || !specdispatch.WorksSeat(c.Ready.Assignee, c.Ready.Labels) {
		return specdispatch.HeldBead{}, false
	}
	return specdispatch.HeldBead{
		ID:       c.ID,
		Assignee: c.Ready.Assignee,
		Paths:    specHoldFiles(c.Refs.Files, c.Ready.Description, c.Refs.Text),
		Tests:    c.Refs.Tests,
	}, true
}

// specFormulaNote names the formula a bead declared, for the tick's dispatched
// and dry-run lines; a bead that declares none adds nothing (gt-j5cr2).
func specFormulaNote(run specdispatch.FormulaRun) string {
	if !run.Declared() {
		return ""
	}
	return ", formula " + run.Formula
}

// specResumeNote names the branch a dispatch continues, or the recorded
// branch that was gone, for the tick's dispatched line.
func specResumeNote(resume, gone string) string {
	switch {
	case resume != "":
		return "; resumed " + resume
	case gone != "":
		return "; resume branch " + gone + " gone"
	}
	return ""
}

func isSpecSlingRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errPoolBackpressure) {
		return true
	}
	_, ok := dispatch.SlingRefusalReason(err.Error())
	return ok
}

func firstErrLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// specBudgetFor is the tick's budget: one seat per entry of polecat_pool.seats,
// in the loader's order (sorted by agent name). A bead with no agent:<name>
// label draws uniformly among the agents with a free, open seat; a labeled
// bead goes only to that agent's seat and waits when it is closed or full
// (gt-mukgk).
//
// polecat_pool.max_total, when set, caps the seats together on top of each
// seat's own cap: a town whose agents are each inside their cap still has the
// total to answer to (gt-bcpj5).
func specBudgetFor(townRoot string, ts *config.TownSettings, now time.Time) (specdispatch.Budget, error) {
	if ts == nil {
		return specdispatch.Budget{}, nil
	}
	return specBudgetFromSeats(ts, now, config.AgentResolver(townRoot, ts))
}

// specBudgetFromSeats builds the seat list from polecat_pool.seats: one seat per
// agent, in the loader's order (sorted by agent name). A seat's window is
// carried on the seat and read at choice time against Budget.Now, so a seat
// shut then takes no spawn while still counting its live polecats and saying
// when it opens (gt-t8new). known is the loader's agent resolver
// (config.AgentResolver). A map the loader rejects is an error: the daemon
// checked it at start, so a tick that meets one dispatches nothing rather than
// run on part of the list.
func specBudgetFromSeats(ts *config.TownSettings, now time.Time, known func(agent string) bool) (specdispatch.Budget, error) {
	var b specdispatch.Budget
	pool := ts.PolecatPool
	b.MinSpawnGap = pool.MinSpawnGapD()
	b.MaxTotal = pool.GetMaxTotal()
	seats, err := config.Seats(ts, known)
	if err != nil {
		return b, fmt.Errorf("reading the seat map: %w", err)
	}
	// Now is the construction time: a picture drawn before the tick refreshes it
	// reads each window as of the seats' read (gt-t8new).
	b.Now = now
	for _, s := range seats {
		seat := specdispatch.Seat{Agent: s.Agent, Cap: s.Cap, PinnedOnly: s.PinnedOnly}
		// Copy a window only when there is one: a typed nil *Window stored in
		// the SeatWindow interface would read as a window and be called on.
		if s.Window != nil {
			seat.Window = s.Window
		}
		b.Seats = append(b.Seats, seat)
	}
	return b, nil
}

// specRosterFrom counts the seats the pool counts as taken (live polecats,
// in-flight seat claims, seats mid-landing, and dead-hooked seats) per agent.
// A dead-hooked seat also counts in DeadHooked, so the picture can say why a
// full seat shows fewer live sessions than its number (gt-tldj4); it has no
// spawn time, so it never arms the stagger. A parkedStale seat is not counted
// in Live; it is counted in ParkedStale (gt-t7k92).
//
// A polecat that records no agent is counted on the seat map's only seat, and
// under config.UnknownSeatAgent when the map has several: the tick lists it
// (Unrecorded) and never counts it on a seat it may not hold (gt-mukgk).
func specRosterFrom(sessions []poolSession, ts *config.TownSettings) specRoster {
	r := specRoster{Live: map[string]int{}, DeadHooked: map[string]int{}, ParkedStale: map[string]int{}}
	var unrecorded string
	if ts != nil {
		unrecorded = ts.PolecatPool.UnrecordedAgent()
	}
	for _, s := range sessions {
		agent := s.agent
		if agent == "" {
			agent = unrecorded
		}
		if agent == config.UnknownSeatAgent {
			r.Unrecorded = append(r.Unrecorded, s.name)
		}
		if s.parkedStale {
			r.ParkedStale[agent]++
			continue
		}
		r.Live[agent]++
		if s.deadHooked {
			r.DeadHooked[agent]++
		}
		if s.created.After(r.Newest) {
			r.Newest = s.created
		}
	}
	return r
}

// specRosterOf is the tick's roster: the seats the roster read (specRosterFrom),
// plus the draining polecats the sling's own pool check holds beyond them. The
// sling's check reads occupied (snapshotOccupied over the same snapshot), and
// poolHeldCount counts it, so the tick's total and the sling's total come from
// one occupancy read. A failed read is an error, not an empty count: a tick
// that cannot see what a sling will count does not dispatch (gt-97ppg).
func specRosterOf(seats poolSeatSnapshot, ts *config.TownSettings, occupied func() ([]poolSession, error)) (specRoster, error) {
	roster := specRosterFrom(seats.sessions, ts)
	held, err := occupied()
	if err != nil {
		return specRoster{}, fmt.Errorf("counting the seats a sling will see: %w", err)
	}
	roster.Draining = specRosterFrom(specDrainingSessions(seats.sessions, held), ts).Live
	return roster, nil
}

// specRosterStep is a tick's Roster step: the seat snapshot is read once and
// kept for the tick's slings through keep, and the roster is counted over one
// sling-side occupancy read that occupied builds from that snapshot. The read
// happens once per tick, not once per candidate: specRosterOf calls it exactly
// once, and the cycle calls this once (gt-97ppg). A failed read is an error, so
// the tick reports it and dispatches nothing on it.
func specRosterStep(read func() (poolSeatSnapshot, error), ts *config.TownSettings, occupied func(poolSeatSnapshot) func() ([]poolSession, error), keep func(poolSeatSnapshot)) (specRoster, error) {
	seats, err := read()
	if err != nil {
		return specRoster{}, fmt.Errorf("listing polecat seats: %w", err)
	}
	keep(seats)
	return specRosterOf(seats, ts, occupied(seats))
}

// specDispatchRoster is the tick's Roster step over one town. The seat snapshot
// and the sling-side occupancy read the same lister, the occupancy once per tick
// (specRosterStep), so a test can drive the wiring with fakes (gt-97ppg).
func specDispatchRoster(reads *specTickReads, townRoot string, ts *config.TownSettings, lister func() sessionLister, storeFor func(dir string) beads.Client, snapshot *atomic.Pointer[poolSeatSnapshot]) func() (specRoster, error) {
	return func() (specRoster, error) {
		return specRosterStep(
			func() (poolSeatSnapshot, error) {
				return specRosterSnapshotWith(reads, townRoot, ts.PolecatPool, lister(), storeFor, time.Now)
			},
			ts,
			func(seats poolSeatSnapshot) func() ([]poolSession, error) {
				return snapshotOccupied(seats, lister, sandboxSeatDeathsFor(townRoot), time.Now)
			},
			// Kept for the slings: each one's seat decision reads this
			// snapshot plus the live sessions and claims of its moment
			// (snapshotOccupied) instead of every polecat directory again.
			func(seats poolSeatSnapshot) { snapshot.Store(&seats) },
		)
	}
}

// specDrainingSessions is the sling-side occupancy beyond the roster: each held
// session the roster did not already count. Both reads name an occupancy the
// same way (specOccupancyKey), so one occupancy is counted once however many
// entries describe it: a seat the roster counted dead-hooked and the listing
// now shows live, a seat claim the roster holds, and a polecat session whose
// role names no seat. What remains is the draining session the roster released
// and the sling still counts, and any session new since the roster read
// (gt-97ppg).
//
// held never carries a seat claim: snapshotOccupied lists tmux sessions and the
// roster's dead and landing seats, and the sling reads claims under its own
// lock. An in-flight sling enters the total once, through specLiveWithInFlight,
// and its entry has no key that could match a session. The one overlap left is a
// sling that spawned between the roster read and the listing: it counts in both
// terms for one tick, which errs toward fewer slings (gt-97ppg).
func specDrainingSessions(rosterSeats, held []poolSession) []poolSession {
	counted := map[string]bool{}
	for _, s := range rosterSeats {
		counted[specOccupancyKey(s)] = true
	}
	var out []poolSession
	for _, s := range held {
		key := specOccupancyKey(s)
		if counted[key] {
			continue
		}
		counted[key] = true
		out = append(out, s)
	}
	return out
}

// specOccupancyKey names what a session holds, the same way on the roster and on
// the sling's read: its pool seat, or, for a session that names no seat (a seat
// claim, or a polecat whose GT_ROLE does not parse), its own name. A key of ""
// would let every such session collide with every other one.
func specOccupancyKey(s poolSession) string {
	if key := s.seatKey(); key != "" {
		return "seat " + key
	}
	return "session " + s.name
}

// loadSpecDispatchConfig returns daemon.json's patrols.spec_dispatch, or nil.
// It reads nil for a block that does not parse as well as for an absent one, so
// it is for a caller that has a fallback: gt spec lint falls back to the
// operator's default template. The tick reads through readSpecDispatchConfig.
func loadSpecDispatchConfig(townRoot string) *config.SpecDispatchConfig {
	if townRoot == "" {
		return nil
	}
	if pc := daemon.LoadPatrolConfig(townRoot); pc != nil && pc.Patrols != nil {
		return pc.Patrols.SpecDispatch
	}
	return nil
}

// readSpecDispatchConfig is the tick's read of patrols.spec_dispatch, telling
// an absent block (nil, nil) from one that does not parse (nil, err): the tick
// stops on a block it cannot read rather than run on a picture that silently
// lost the second seat (gt-jdnab).
func readSpecDispatchConfig(townRoot string) (*config.SpecDispatchConfig, error) {
	if townRoot == "" {
		return nil, nil
	}
	pc, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return nil, err
	}
	if pc == nil || pc.Patrols == nil {
		return nil, nil
	}
	return pc.Patrols.SpecDispatch, nil
}

// specTickTown reads the two files a tick runs on: the town settings that hold
// the pool seat and the patrol block that holds the second one. Both are read
// per tick, so a seat the operator changed is the seat the next tick runs with
// (gt-jdnab); a patrol block that does not parse is an error rather than an
// absent one, which is what stops the tick.
func specTickTown(townRoot string) (*config.TownSettings, *config.SpecDispatchConfig, error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return nil, nil, fmt.Errorf("loading town settings: %w", err)
	}
	sd, err := readSpecDispatchConfig(townRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the seat settings: %w", err)
	}
	return ts, sd, nil
}

// specTemplatePath is the one resolver for the spec template path: the
// spec_dispatch template field, else the operator's default file. Lint and
// dispatch both use it so they check one shape.
func specTemplatePath(sd *config.SpecDispatchConfig) string {
	configured := ""
	if sd != nil {
		configured = sd.Template
	}
	return specdispatch.ResolveTemplatePath(configured)
}

func runSpecDispatch(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	report, err := runSpecDispatchAt(townRoot)
	if err != nil {
		return err
	}
	if specDispatchJSON {
		out, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return nil
	}
	printSpecDispatchReport(cmd, report)
	return nil
}

// runSpecDispatchAt runs one tick of the spec dispatcher over the town rooted
// at townRoot, reporting what it decided (specDispatchEnv). It is the tick
// itself, with the town resolution and the output left to runSpecDispatch, so
// the tick a seat file stops is the tick a test can run.
func runSpecDispatchAt(townRoot string) (specDispatchReport, error) {
	return runSpecDispatchAtWith(townRoot, os.Getenv(dispatch.SpecDetailOwnerEnv), specDispatchDryRun)
}

// runSpecDispatchAtWith is runSpecDispatchAt with the daemon that started the
// tick and the dry-run flag given, so a test drives the census wiring without
// the process environment (gt-139dp).
func runSpecDispatchAtWith(townRoot, owner string, dryRun bool) (specDispatchReport, error) {
	start := time.Now()
	inFlight, err := parseSpecInFlight(specDispatchInFlight)
	if err != nil {
		return specDispatchReport{}, err
	}
	ts, sd, err := specTickTown(townRoot)
	if err != nil {
		return specDispatchReport{}, err
	}
	tmplPath := specTemplatePath(sd)
	perTick := defaultSpecMaxPerTick
	if sd != nil && sd.MaxPerTick > 0 {
		perTick = sd.MaxPerTick
	}
	// The roster's seat picture, kept for the slings of this tick.
	var snapshot atomic.Pointer[poolSeatSnapshot]
	// The sandbox phase line of each sling that starts a sandbox session (gt-0uh64).
	slingTraces := &specSlingTraces{}
	if slingActor == "" {
		slingActor = specDispatchActor
	}
	maxPriority := ts.PolecatPool.GetMaxPriority()

	budget, err := specBudgetFor(townRoot, ts, time.Now())
	if err != nil {
		return specDispatchReport{}, err
	}
	// The tick's bd reads, kept for the tick alone (gt-u8rbh).
	reads := newSpecTickReads()
	// The pool's design and notes, kept across the daemon's ticks (gt-l0vlf).
	details := specDetailCacheFor(townRoot, owner)
	specDetailCache.Store(details)
	// The held beads already annotated, kept across the daemon's ticks.
	annotations := newSpecAnnotatedTick(townRoot, owner, func(id string) beads.Client {
		reads.call()
		return beads.New(resolveBeadDirFromTownRoot(townRoot, id))
	})
	// The install job writes its marker under the same daemon dir this reader
	// resolves, so the tick and the job agree on the file (gt-vc34c).
	pendingInstall := installpending.StoreFor(townRoot)
	// Each rig's target inventory, read once per tick: the lint asks it per
	// candidate, and the files a branch holds do not change inside a tick
	// (gt-7t1im).
	targets := map[string]*specdispatch.TargetTree{}
	targetOf := func(rig string) *specdispatch.TargetTree {
		if tree, ok := targets[rig]; ok {
			return tree
		}
		tree := specTargetTree(filepath.Join(townRoot, rig), openRigTree)
		targets[rig] = tree
		return tree
	}
	template := specdispatch.LoadTemplate(tmplPath)
	// The shape-and-label verdicts of the candidates the scan already read,
	// kept across the daemon's ticks beside the annotations (gt-km9jt).
	verdicts := newSpecCandidateVerdicts(annotations.memory,
		specCandidateInputsFor(template, ts.PolecatPool.GetShapeGate(), maxPriority, targetOf))
	// The tick's children read, grouped per rig once the board is known.
	var children func(beadID string) ([]specdispatch.Child, error)
	env := specDispatchEnv{
		Hold:           func() string { return dispatch.OperatorHold(townRoot) },
		PendingInstall: func(now time.Time) (*installpending.Marker, error) { return pendingInstall.Pending(now) },
		StartupBackoff: func(id string) string { return dispatch.StartupBackoff(townRoot, id) },
		Candidates: func() specBoardRead {
			// A dry run writes nothing, so it reports the labeled beads as
			// held: the clear is a real retry the run would make, and a preview
			// must not leave the beads changed (gt-vaivp).
			var retry *specTimeoutRetry
			if !specDispatchDryRun {
				retry = &specTimeoutRetry{
					due: func(rigPath string) (map[string]bool, error) {
						reads.call()
						return specTimedOutRetriesIn(rigPath)
					},
					clear: specRetryTimedOutSling,
				}
			}
			read := specCandidatesRetrying(townRoot, maxPriority, func(rigPath string) ([]*beads.Issue, error) {
				reads.call()
				return specReadyBoard(rigPath)
			}, IsRigParkedOrDocked, retry)
			read = specBoardDetails(reads, func(rig string) beads.Client {
				return beads.NewPinned(beads.ResolveBeadsDir(filepath.Join(townRoot, rig)))
			}, read, verdicts)
			annotations.observe(read)
			children = specCandidateChildren(townRoot, reads, read.Candidates, verdicts, nil)
			return read
		},
		Verdicts: verdicts,
		Show: func(id string) (specdispatch.Spec, error) {
			return showSpecWith(reads, id, func(id string) (*beads.Issue, error) { return showBead(townRoot, id) })
		},
		ShowFresh: func(id string) (specdispatch.Spec, error) { return showSpec(townRoot, id) },
		// The children of every candidate in one bd view per rig, read fresh
		// each tick: a child closed or added without moving its parent's row is
		// still seen by the next tick's container hold (gt-km9jt, gt-qcl61).
		Children: func(id string) ([]specdispatch.Child, error) {
			if children == nil {
				reads.call()
				return specChildren(townRoot, id)
			}
			return children(id)
		},
		RigHold: func(rig string) string { return dispatch.RigHold(townRoot, rig) },
		RevertInFlight: func(rig string) *specdispatch.Revert {
			return rigRevertInFlight(townRoot, rig, func(beadID string) bool {
				reads.call()
				return revertBeadOpen(townRoot, rig, beadID)
			})
		},
		// The same seat picture `gt daemon dispatch-check` nudges from:
		// live sessions, in-flight claims, and seats mid-landing. Two
		// pictures of one pool is how the dispatcher ends up slinging into
		// a seat the nudge just called free (gt-thy6r).
		Roster:   specDispatchRoster(reads, townRoot, ts, newPoolSessionLister, func(dir string) beads.Client { return beads.New(dir) }, &snapshot),
		Annotate: annotations.annotate,
		AddLabel: func(id, label string) error {
			return poolBeadLabelAdd(townRoot, id, label)
		},
		DropTimeoutRetry: func(id string) error {
			return beads.NewPinned(resolveBeadDirFromTownRoot(townRoot, id)).Update(id, beads.UpdateOptions{
				RemoveLabels:  []string{specdispatch.TimeoutLabel},
				UnsetMetadata: []string{specdispatch.TimeoutAttemptsKey},
			})
		},
		BranchOnOrigin: func(rig, branch string) (bool, error) {
			return polecat.BranchOnOrigin(filepath.Join(townRoot, rig), branch)
		},
		FormulaExists: func(rig, formula string) error {
			return verifyFormulaExists(formula, filepath.Join(townRoot, rig), townRoot)
		},
		Sling: func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (string, error) {
			// The trace starts on the job's first line, so everything the sling
			// does — the seat picture it reads, the engine's stages, the spawn's
			// seat claim and the sandbox's phases — is timed on one timeline
			// (gt-7nctk). A nil trace here would leave the pre-sling work out.
			trace := session.NewSandboxTrace(time.Now)
			var occupied func() ([]poolSession, error)
			if seats := snapshot.Load(); seats != nil {
				occupied = snapshotOccupied(*seats, newPoolSessionLister, sandboxSeatDeathsFor(townRoot), time.Now)
			}
			name, err := slingSpec(townRoot, c, resumeBranch, seat, occupied, trace)
			slingTraces.add(specSlingTraceLine(c.Spec.ID, seat.Agent, trace, err))
			return name, err
		},
		Sleep:    time.Sleep,
		Now:      time.Now,
		Template: template,
		Budget:   budget,
		PerTick:  perTick,
		DryRun:   dryRun,
		// The verdict ledger the dashboard reads, under this town's runtime
		// directory (gt-8thox).
		TownRoot: townRoot,

		Reads: reads,

		Background: specDispatchBackground,
		InFlight:   inFlight,

		MaxPriority: maxPriority,
		ShapeGate:   ts.PolecatPool.GetShapeGate(),
		// The overrun box bands the reviewer's own threshold, read from the
		// rig's checkout — the tree its landings are reviewed from
		// (gt-b9j3o.4).
		OMThreshold: func(rig string) (float64, error) {
			return land.OMThreshold(filepath.Join(townRoot, rig))
		},
		Target: targetOf,
	}
	report := runSpecDispatchCycle(env)
	report.Notices = append(report.Notices, slingTraces.notices()...)
	annotations.save()
	report.DetailsFetched, report.DetailsCached, report.DetailsUnread = details.Counts()
	if report.Hold == "" {
		// The slings of a background tick run after it, so they are no part of
		// its cost.
		report.Notices = append(report.Notices, specTickCost(reads, time.Since(start), report.SlingsRun, report.SlowestSling))
	}
	// The census is written for the daemon that started the tick, so it can log
	// the subprocess's calls under its own census line; a failure to write it is
	// reported rather than read as no calls (gt-139dp).
	if err := specWriteCensus(townRoot, owner, dryRun); err != nil {
		report.Errors = append(report.Errors, "bd census rows not written: "+err.Error())
	}
	return report, nil
}

// specWriteCensus writes the tick's bd census rows to the town's census file
// (dispatch.SpecCensusPath) for the daemon that started it to log. A tick run by
// hand has no owner to write for, and a dry run is not the tick whose calls the
// daemon reports, so neither writes any (gt-139dp).
func specWriteCensus(townRoot, owner string, dryRun bool) error {
	if owner == "" || dryRun {
		return nil
	}
	return beads.WriteCensusSink(dispatch.SpecCensusPath(townRoot), beads.CensusSnapshotAndReset())
}

// specTickCost is the tick's one cost line. It counts the reads the tick makes
// itself. The sling path, the pre-sling re-read and the writes are not counted.
func specTickCost(reads *specTickReads, took time.Duration, slings int, slowest time.Duration) string {
	line := fmt.Sprintf("tick cost: %d bd read call(s) outside the sling path in %s", reads.Calls(), took.Round(time.Millisecond))
	if slings > 0 {
		line += fmt.Sprintf("; %d sling(s) in parallel, slowest %s", slings, slowest.Round(time.Millisecond))
	}
	return line
}

// specSlingTraceLine is the line a sling that started a sandbox session adds to
// the tick's log: its phases with their durations, then how the sling ended. It
// is "" for a sling that touched no sandbox, so a tmux seat adds nothing. A
// multi-line error is cut to its first line so the notice stays one log line
// (gt-csltr).
func specSlingTraceLine(beadID, agent string, trace *session.SandboxTrace, err error) string {
	summary := trace.Summary()
	if summary == "" {
		return ""
	}
	outcome := "started"
	if err != nil {
		outcome = "error: " + firstErrLine(err)
	}
	return fmt.Sprintf("sling %s seat %s: %s; outcome: %s", beadID, agent, summary, outcome)
}

// specSlingTraces collects the sling lines of one tick. The slings run in
// parallel, so it is locked, and its lines come back sorted so the log reads the
// same from run to run.
type specSlingTraces struct {
	mu    sync.Mutex
	lines []string
}

func (s *specSlingTraces) add(line string) {
	if line == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *specSlingTraces) notices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.lines...)
	sort.Strings(out)
	return out
}

// specRosterSnapshotWith is the seat picture poolSeatSessions draws, with its
// polecat-state reads batched per rig and read through the tick's ledger, and
// the seats that read released kept beside it for the tick's slings
// (snapshotOccupied). The session lister, the store for a directory and the
// clock are passed in, so a test can run this wiring over a fake. The time it
// began is taken before any session is listed, so a session created after it
// counts as new.
func specRosterSnapshotWith(reads *specTickReads, townRoot string, pool *config.PolecatPool, lister sessionLister, storeFor func(dir string) beads.Client, now func() time.Time) (poolSeatSnapshot, error) {
	store := func(dir string) beads.Client { return reads.client(storeFor(dir)) }
	batch := newPoolSeatBatch(townRoot,
		func(rigName string) beads.Client { return store(filepath.Join(townRoot, rigName)) },
		func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) })
	// The landing seats' beads are read from the batch where it already holds
	// them, so a tick with a seat mid-landing spends no town-wide read on it
	// (gt-4efmk).
	work := batch.landingWork(store(townRoot))
	return poolSeatSnapshotWith(lister, townRoot, batch.disposition, work, pool, now)
}

// poolSeatSnapshotWith reads poolSeatSessionsWith through a recorder of the
// seats the disposition releases, so a tick's slings can count them free too.
func poolSeatSnapshotWith(lister sessionLister, townRoot string, disposition polecatDispositionFunc, work poolSeatWorkFunc, pool *config.PolecatPool, now func() time.Time) (poolSeatSnapshot, error) {
	at := now()
	recorded, released := releasedSeatRecorder(disposition)
	sessions, err := poolSeatSessionsWith(lister, townRoot, recorded, work, pool)
	if err != nil {
		return poolSeatSnapshot{}, err
	}
	return poolSeatSnapshot{sessions: sessions, released: released(), at: at}, nil
}

func printSpecDispatchReport(cmd *cobra.Command, r specDispatchReport) {
	w := cmd.OutOrStdout()
	if r.Hold != "" {
		fmt.Fprintf(w, "spec dispatch held: %s\n", r.Hold)
		return
	}
	summary := fmt.Sprintf("spec dispatch: %d candidate(s)", r.Candidates)
	if r.LabeledFailed > 0 {
		summary += fmt.Sprintf(", %d held by %s", r.LabeledFailed, specdispatch.DispatchFailedLabel)
	}
	fmt.Fprintf(w, "%s; roster %s; template %s\n", summary, r.Roster, r.Template)
	for _, group := range []struct {
		name    string
		entries []specDispatchEntry
	}{{"dispatched", r.Dispatched}, {"refused", r.Refused}, {"planning", r.Planning}, {"skipped", r.Skipped}, {"failed", r.Failed}} {
		for _, e := range group.entries {
			fmt.Fprintf(w, "  %-10s %s\n", group.name, e.Line)
		}
	}
	for _, n := range r.Notices {
		fmt.Fprintf(w, "  %-10s %s\n", "note", n)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(w, "  error      %s\n", e)
	}
}

// rigRevertInFlight reads the revert a rig's red-main owner has building or
// queued from the state file the landing worker keeps for that rig
// (daemon.RedMainStatePath). A missing or unreadable file is no revert: the
// owner writes the state only while a revert is in flight, so a read error
// must not hold a rig's red-main beads out of the candidate set forever
// (gt-zkdwt). A malformed state is the same silence, by specdispatch.ParseRevert.
//
// A record whose bead is filed only holds while that bead is open: a closed
// or missing bead means the revert already finished — or never started — and
// holding on it would strand the rig (gt-wgyca). beadOpen is that question,
// passed in so a test can answer it without a rig's beads database.
func rigRevertInFlight(townRoot, rig string, beadOpen func(beadID string) bool) *specdispatch.Revert {
	if rig == "" {
		return nil
	}
	raw, err := os.ReadFile(daemon.RedMainStatePath(townRoot, rig))
	if err != nil {
		return nil
	}
	rv := specdispatch.ParseRevert(raw)
	if rv == nil || rv.Bead == "" {
		return rv
	}
	if !beadOpen(rv.Bead) {
		return nil
	}
	return rv
}

// revertBeadOpen reports whether the revert bead the state records is still
// open in the rig's beads. Its complement — closed, missing, or unreadable,
// the last fail-open like the state read above — is not an open revert.
func revertBeadOpen(townRoot, rig, beadID string) bool {
	b := beads.NewPinned(beads.ResolveBeadsDir(filepath.Join(townRoot, rig)))
	is, err := b.Show(beadID)
	if err != nil || is == nil {
		return false
	}
	return revertStatusOpen(is.Status)
}

// revertStatusOpen reports whether a revert bead's status still counts as an
// open revert. Only a closed or tombstoned bead does not.
func revertStatusOpen(status string) bool {
	return !beads.IssueStatus(strings.TrimSpace(status)).IsTerminal()
}

// specBoard reads one rig's ready board. It is a parameter of specCandidates
// rather than a package var so a test can serve a fake board without swapping
// process state (internal/testpolicy's no-global-swap).
type specBoard func(rigPath string) ([]*beads.Issue, error)

// specBoardRead is one tick's read of the ready boards: the eligible
// candidates, the ready beads the spec-dispatch-failed label is holding out of
// the queue, and the rigs whose boards could not be read.
type specBoardRead struct {
	Candidates []specCandidate
	// LabeledFailed counts the ready beads the tick found carrying
	// specdispatch.DispatchFailedLabel. They are out of the queue until the
	// label is cleared, and the count is the operator's signal that a bead is
	// stuck there (gt-q6zoo).
	LabeledFailed int
	// Skipped are the refusals of the board-carried labels
	// (specdispatch.BoardCarriedLabels): a bead the dispatcher leaves alone for
	// a reason the operator has to read, reported instead of dropped unseen
	// (gt-k1g3e). The tick appends them to its own skip lines.
	Skipped []specDispatchEntry
	// Retried names the beads whose failure label the read cleared so they could
	// be slung again (specTimedOutRetries). The tick logs them as notices.
	Retried []string
	Errors  []string
}

// specCandidates reads every operational rig's ready work beads and keeps the
// eligible ones, ordered across rigs. A rig whose board cannot be read is
// reported and skipped; the rest still dispatch. maxPriority is the operator's
// ceiling on a candidate's priority number — the same value the per-candidate
// re-check uses.
func specCandidates(townRoot string, maxPriority int, board specBoard) specBoardRead {
	return specCandidatesWith(townRoot, maxPriority, board, IsRigParkedOrDocked)
}

// specCandidatesWith is specCandidates with the rig's park-and-dock check
// passed in, so a test can hand the read a verdict bd cannot be made to give
// (gt-6d06g).
func specCandidatesWith(townRoot string, maxPriority int, board specBoard, rigStatus func(townRoot, rigName string) (bool, string)) specBoardRead {
	return specCandidatesRetrying(townRoot, maxPriority, board, rigStatus, nil)
}

// specCandidatesRetrying is specCandidatesWith plus retry, the read of a rig's
// beads a killed-at-its-bound sling left labeled and whose retries remain: the
// tick slings such a bead again instead of waiting for a person (gt-vaivp).
func specCandidatesRetrying(townRoot string, maxPriority int, board specBoard, rigStatus func(townRoot, rigName string) (bool, string), retry *specTimeoutRetry) specBoardRead {
	var read specBoardRead
	names, err := knownRigNames(townRoot)
	if err != nil {
		read.Errors = []string{err.Error()}
		return read
	}
	var specs []specdispatch.Spec
	rigOf := map[string]string{}
	updatedAt := map[string]string{}
	depCount := map[string]int{}
	labeled := map[string]bool{}
	for _, name := range names {
		rigPath := filepath.Join(townRoot, name)
		if _, err := os.Stat(rigPath); err != nil {
			continue
		}
		// The rig is held either way, but an unverified rig is reported: its
		// park state could not be read, so the tick log has to say the rig was
		// held for lack of a verdict, not that it is parked (gt-6d06g).
		if blocked, why := rigStatus(townRoot, name); blocked {
			if why == sling.RigStatusUnverified {
				read.Errors = append(read.Errors, fmt.Sprintf("%s: rig status unverified, not dispatching", name))
			}
			continue
		}
		if !hasBeadsDatabase(beads.ResolveBeadsDir(rigPath)) {
			continue
		}
		issues, err := board(rigPath)
		if err != nil {
			read.Errors = append(read.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		// The rig's retry set is read on the first labeled bead the board
		// holds, once: a rig whose board holds none pays nothing for it.
		var retries map[string]bool
		var retryErr error
		probed := false
		for _, issue := range issues {
			s := specFromIssue(issue)
			// A bead the failed label is holding is counted, not taken: the
			// count is what the health line surfaces, and Eligible would drop
			// it from candidacy anyway (gt-q6zoo). A bead slung again after the
			// read cleared its label is a candidate of this tick.
			if s.HasLabel(specdispatch.DispatchFailedLabel) {
				if retry != nil && !probed {
					probed = true
					retries, retryErr = retry.due(rigPath)
					if retryErr != nil {
						read.Errors = append(read.Errors, fmt.Sprintf("%s: cannot tell which beads a kill at the bound may retry: %v", name, retryErr))
					}
				}
				// A retry set that could not be read is no answer: only a bead
				// the set names is slung again, so a failed read holds every
				// labeled bead out rather than retrying on a guess.
				if retryErr != nil || retry == nil || !retries[s.ID] {
					labeled[s.ID] = true
					continue
				}
				if err := retry.clear(rigPath, s.ID); err != nil {
					read.Errors = append(read.Errors, fmt.Sprintf("%s: %v", s.ID, err))
					labeled[s.ID] = true
					continue
				}
				// The row was read with the label on it, and Eligible refuses
				// the label; the bead's store no longer carries it.
				s.Labels = slices.DeleteFunc(s.Labels, func(l string) bool { return l == specdispatch.DispatchFailedLabel })
				read.Retried = append(read.Retried, fmt.Sprintf("%s: cleared %s for a retry of a sling killed at its bound", s.ID, specdispatch.DispatchFailedLabel))
			}
			// The ready board is a snapshot: the full bead is re-read before
			// any decision. A refusal is dropped here, and a refusal of a
			// board-carried label is reported instead: the operator's label is
			// what holds the bead, so the tick log and the dry run say so
			// rather than leaving the bead an unexplained absence (gt-k1g3e).
			if ok, why := specdispatch.Eligible(s, maxPriority); !ok {
				if specdispatch.BoardCarried(s) {
					read.Skipped = append(read.Skipped, specDispatchEntry{Bead: s.ID, Rig: name, Line: fmt.Sprintf("%s: %s (%s)", s.ID, specdispatch.SkipNotEligible, why)})
				}
				continue
			}
			if _, dup := rigOf[s.ID]; dup {
				continue
			}
			rigOf[s.ID] = name
			updatedAt[s.ID] = issue.UpdatedAt
			depCount[s.ID] = issue.DependencyCount
			specs = append(specs, s)
		}
	}
	specdispatch.Order(specs)
	read.Candidates = make([]specCandidate, 0, len(specs))
	for _, s := range specs {
		read.Candidates = append(read.Candidates, specCandidate{Spec: s, Rig: rigOf[s.ID], UpdatedAt: updatedAt[s.ID], DepCount: depCount[s.ID]})
	}
	read.LabeledFailed = len(labeled)
	return read
}

// specTimeoutRetry is the read's answer for beads a sling killed at its bound
// left labeled: due names, in one list call per rig, the beads whose retries
// remain, and clear removes one such bead's failure label so it can be slung
// again. Both are passed in, so a test drives the decision and the write
// without a live bd. nil retries nothing and leaves every labeled bead held.
type specTimeoutRetry struct {
	due   func(rigPath string) (map[string]bool, error)
	clear func(rigPath, beadID string) error
}

// specTimedOutRetriesIn is the retry probe over the rig's own store.
func specTimedOutRetriesIn(rigPath string) (map[string]bool, error) {
	return specTimedOutRetriesFrom(beads.NewPinned(beads.ResolveBeadsDir(rigPath)))
}

// specTimedOutRetriesFrom is specTimedOutRetriesIn over a store the caller
// supplies, so a test runs the probe against a fake instead of a live bd.
func specTimedOutRetriesFrom(b beads.Client) (map[string]bool, error) {
	issues, err := b.List(beads.ListOptions{
		Labels: []string{specdispatch.DispatchFailedLabel, specdispatch.TimeoutLabel},
		Limit:  0,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the beads a kill at the bound left labeled: %w", err)
	}
	retries := make(map[string]bool, len(issues))
	for _, issue := range issues {
		if _, retry := specdispatch.RetryAfterTimeout(issue.Labels, issue.Metadata); retry {
			retries[issue.ID] = true
		}
	}
	return retries, nil
}

// specRetryTimedOutSling clears the spec-dispatch-failed label of a bead whose
// sling was killed at its bound while a retry is left: the kill is the host's
// load, which clears by itself, so the label is what would keep the bead
// waiting for a person (gt-vaivp). The timeout label and the kill count stay,
// so the next kill counts on from them.
func specRetryTimedOutSling(rigPath, beadID string) error {
	return specRetryTimedOutSlingFrom(beads.NewPinned(beads.ResolveBeadsDir(rigPath)), beadID)
}

// specRetryTimedOutSlingFrom is specRetryTimedOutSling over a store the caller
// supplies, so a test runs the clear against a fake instead of a live bd.
func specRetryTimedOutSlingFrom(b beads.Client, beadID string) error {
	if err := b.Update(beadID, beads.UpdateOptions{RemoveLabels: []string{specdispatch.DispatchFailedLabel}}); err != nil {
		return fmt.Errorf("clearing the failed label: %w", err)
	}
	return nil
}

// specBoardDetails fills the two things a board row cannot: the design the lint
// reads, which a row may leave out, and the blocking edges the admission check
// runs sling.Admit on, which a row never carries (gt-ndad8). The rig's own
// store answers both, with one ShowMultiple per rig over the candidates that
// need them — never one show per candidate (gt-bqj8q). A design the row already
// carries is kept as it is; the edges are the read's, because a row that says
// nothing about dependencies is not a row that says there are none.
//
// A candidate the memory remembers needs no design: its shape and label verdict
// are served. One with no blocking edge (dependency_count 0, most of the board)
// reads nothing. One with an edge is read in full like a fresh candidate: a
// blocker that closes or reopens, or an edge swapped for another of the same
// count, moves neither the dependent's row nor its dependency_count, so the
// edges are never remembered (gt-km9jt, gt-qcl61).
//
// A candidate the read does not answer is dropped with its error: the edges are
// unread, and a bead with no list is not a bead with no blockers (gt-o70a9). It
// waits for the next tick rather than dispatching blind.
//
// A candidate the board itself refuses — an excluded label, a status, an
// assignee — costs no read once its unchanged row's refusal is known: it is
// never slung, so it needs neither the design nor the edges.
func specBoardDetails(reads *specTickReads, storeFor func(rig string) beads.Client, read specBoardRead, verdicts *specCandidateVerdicts) specBoardRead {
	var fresh []int
	for i, c := range read.Candidates {
		if verdicts.refused(c) {
			continue
		}
		if _, ok := verdicts.lookUp(c); ok && c.DepCount == 0 {
			continue
		}
		fresh = append(fresh, i)
	}
	var rigs []string
	byRig := map[string][]int{}
	for _, i := range fresh {
		rig := read.Candidates[i].Rig
		if _, seen := byRig[rig]; !seen {
			rigs = append(rigs, rig)
		}
		byRig[rig] = append(byRig[rig], i)
	}
	dropped := map[int]bool{}
	for _, rig := range rigs {
		idx := byRig[rig]
		ids := make([]string, 0, len(idx))
		for _, i := range idx {
			ids = append(ids, read.Candidates[i].Spec.ID)
		}
		issues, err := reads.client(storeFor(rig)).ShowMultiple(ids)
		for _, i := range idx {
			id := read.Candidates[i].Spec.ID
			issue := issues[id]
			if issue == nil {
				dropped[i] = true
				reason := "not found"
				if err != nil {
					reason = firstErrLine(err)
				}
				read.Errors = append(read.Errors, fmt.Sprintf("%s: cannot read design and blockers: %s", id, reason))
				continue
			}
			if read.Candidates[i].Spec.Design == "" {
				read.Candidates[i].Spec.Design = issue.Design
			}
			read.Candidates[i].Blockers = issue.Dependencies
		}
	}
	if len(dropped) == 0 {
		return read
	}
	kept := make([]specCandidate, 0, len(read.Candidates)-len(dropped))
	for i, c := range read.Candidates {
		if !dropped[i] {
			kept = append(kept, c)
		}
	}
	read.Candidates = kept
	return read
}

// specReadyOptions is the dispatcher's board query: unassigned work beads,
// with the dispatcher's exclusions and the town's non-dispatchable families
// filtered server-side, unlimited. The retired label spec and type feature are
// not queried on (gt-mmsr2); the non-work types (an epic, the runtime
// families) are excluded instead, so the board holds work beads only.
//
// A board-carried label is deliberately not among the server-side exclusions:
// the board must carry the beads it holds so the tick can count them and name
// them in its skip line, and the health line can show the count (gt-q6zoo,
// gt-k1g3e). Eligible still keeps them out of the candidate set.
//
// The count is read from this board, not a second query, because bd ready
// --json hydrates each issue's labels array. The notes in internal/cmd/ready.go
// and internal/beads about `bd ready --json` carrying no labels describe the
// store-backed readers, which build their issue list from the store and never
// see this JSON. A live board captured 2026-10-02 with these exact filters
// carried labels on 61 of 72 issues (every issue that has any), and
// TestCapturedSpecBoardCarriesLabels pins the shape against a captured board
// whose labeled bead is the one the dispatcher itself labeled.
func specReadyOptions() beads.ReadyOptions {
	exclude := append([]string(nil), constants.NonDispatchableBeadLabels...)
	exclude = append(exclude, specdispatch.ReadyQueryLabels()...)
	return beads.ReadyOptions{
		Unassigned:    true,
		ExcludeLabels: exclude,
		ExcludeTypes:  specdispatch.NonWorkBeadTypes(),
		Limit:         0,
	}
}

// specReadyBoard runs the ready query in one rig, against the rig's own
// database. beads.NewPinned pins BEADS_DIR to the database resolved from that
// rig whatever database the tick's environment names, forces the read
// read-only, asks bd's stale-read bypass when the installed bd takes it, and
// carries bd's stderr into a failure.
// TestReadyFilteredCarriesThePinnedReadOnlyPolicy pins that policy on the
// ReadyFiltered query the dispatcher sends.
func specReadyBoard(rigPath string) ([]*beads.Issue, error) {
	return specReadyBoardFrom(beads.NewPinned(beads.ResolveBeadsDir(rigPath)))
}

// specReadyBoardFrom is specReadyBoard's answer for a given store. A board bd
// reports as cut is read as a partial board: the tick acts on what it got, and
// the sentinel says only that there may be more rows.
func specReadyBoardFrom(store beads.Client) ([]*beads.Issue, error) {
	issues, err := store.ReadyFiltered(specReadyOptions())
	var truncated *beads.ErrReadyTruncated
	if errors.As(err, &truncated) {
		return issues, nil
	}
	return issues, err
}

// annotateSpecOnce adds text as a comment unless the bead already carries a
// comment starting with key, so a bead the dispatcher refuses every tick is
// annotated once. A comment list that cannot be read is an error, not "no
// comments": writing on a guess would repeat the note every tick.
func annotateSpecOnce(b beads.Client, beadID, key, text string) error {
	_, err := annotateSpecWrote(b, beadID, key, text)
	return err
}

// annotateSpecWrote is annotateSpecOnce saying whether it added the comment.
func annotateSpecWrote(b beads.Client, beadID, key, text string) (bool, error) {
	comments, err := b.Comments(beadID)
	if err != nil {
		return false, fmt.Errorf("reading comments: %w", err)
	}
	if hasCommentWithPrefix(comments, key) {
		return false, nil
	}
	if err := b.AddComment(beadID, text); err != nil {
		return false, err
	}
	return true, nil
}

func hasCommentWithPrefix(comments []beads.Comment, key string) bool {
	key = strings.TrimSpace(key)
	for _, c := range comments {
		if strings.HasPrefix(strings.TrimSpace(c.Text), key) {
			return true
		}
	}
	return false
}

// specSlingParams is the sling a spec dispatch makes. The agent is explicit; a
// failed dispatch leaves the bead unassigned for the next tick; and every
// dispatch carries the host-safety instruction. A non-empty resumeBranch is
// the gt sling --branch a recovered bead's surviving work resumes on.
//
// A bead that declares its own formula (gt-j5cr2) is slung with that formula,
// its variables, and — for a scheduled run — the scheduler's actor; the rig's
// default formula, passed as formula, applies to every other bead.
func specSlingParams(townRoot, beadsDir, formula string, c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) SlingParams {
	if c.Run.Declared() {
		formula = c.Run.Formula
	}
	return SlingParams{
		BeadID:           c.Spec.ID,
		RigName:          c.Rig,
		FormulaName:      formula,
		Vars:             c.Run.Vars,
		Actor:            c.Run.Actor,
		Agent:            seat.Agent,
		ResumeBranch:     resumeBranch,
		Args:             specdispatch.HostSafetyPrompt,
		FormulaFailFatal: true,
		CallerContext:    specDispatchCallerLabel,
		NoBoot:           true,
		// The tick slings every ready bead it reaches, overlapping or not, so the
		// sling-time guard admits an overlap rather than refusing it (gt-e1ufo).
		AdmitOverlap: true,
		TownRoot:     townRoot,
		BeadsDir:     beadsDir,
	}
}

// slingSpec dispatches one clean spec through the shared rig-dispatch path.
// The agent is explicit, so it wins over the bead's route:* labels in the
// pool; the pool still enforces its own caps and may refuse. occupied, when
// set, is the seat occupancy read the pool decision uses (the tick's
// snapshot); nil reads the town. trace times every stage of the sling, from the
// seat picture it reads to the end of the session start; nil times nothing.
func slingSpec(townRoot string, c specCandidate, resumeBranch string, seat specdispatch.SeatChoice, occupied func() ([]poolSession, error), trace *session.SandboxTrace) (string, error) {
	targetBeadsDir, ok := beads.ResolveRepoAliasBeadsDir(townRoot, c.Rig)
	if !ok {
		return "", fmt.Errorf("cannot resolve rig %q beads database for %s", c.Rig, c.Spec.ID)
	}
	params := specSlingParams(townRoot, targetBeadsDir, resolveFormula("", false, townRoot, c.Rig), c, resumeBranch, seat)
	// The engine and the spawner name each stage they run on this trace, so the
	// note's total is the sling's own time rather than the sandbox's share of
	// it (gt-7nctk).
	params.Steps = trace.Steps()
	d := realSlingDeps()
	d.beadInfoInTown = refuseOpenBlockers(d.beadInfoInTown)
	d.spawnPolecat = specSlingSpawn(d.spawnPolecat, occupied, trace)
	result, err := d.executeSling(params)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", errors.New("sling returned no result")
	}
	return result.PolecatName, nil
}

// specSlingSpawn is the spawn a spec sling asks for: the tick's trace rides it
// so the session start times the sandbox phases for the tick's log (gt-0uh64),
// and the tick's seat snapshot, when it has one, is the occupancy read the pool
// decision uses instead of listing every polecat again (gt-thy6r).
func specSlingSpawn(spawn func(string, SlingSpawnOptions) (*SpawnedPolecatInfo, error), occupied func() ([]poolSession, error), trace *session.SandboxTrace) func(string, SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	return func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		opts.trace = trace
		if occupied != nil {
			opts.occupied = occupied
		}
		return spawn(rigName, opts)
	}
}

// refuseOpenBlockers wraps the bead read a dispatch starts from so the spec
// dispatcher's sling refuses a bead that has a blocking dependency open. The
// ready board it drew the bead from is a snapshot, and a tick that runs for
// minutes can sling a bead whose blocker was added after the board was read
// (gt-ewrtm). The read is the one the sling guards already make, so the check
// costs no bd call. The refusal leads with the sling-refusal marker, so the
// tick defers the bead and the next board leaves it out until the blocker
// closes. An operator's gt sling does not run through here, and may still sling
// a blocked bead on purpose.
//
// Only the first read is checked. executeSling reads the bead again after the
// spawn to see whether a hold was written meanwhile (gt-0k7kb); that read has
// its own question and its own message, and a blocker refusal there would be
// reported as a field that "cannot be re-read". Later reads pass through.
func refuseOpenBlockers(read func(townRoot, beadID string) (*beadInfo, error)) func(townRoot, beadID string) (*beadInfo, error) {
	var started atomic.Bool
	return func(townRoot, beadID string) (*beadInfo, error) {
		info, err := read(townRoot, beadID)
		if started.Swap(true) || err != nil || info == nil {
			return info, err
		}
		if why := specBlocked(beads.UnresolvedBlockerIDs(info.Dependencies)); why != "" {
			return nil, fmt.Errorf("%s %s is %s", dispatch.SlingRefusalMarker, beadID, why)
		}
		return info, nil
	}
}
