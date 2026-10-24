package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
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
	"github.com/steveyegge/gastown/internal/land"
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
gt:ready-to-land, needs-human, or deferred, are never
taken, and neither is a bead submitted for landing whose READY TO LAND block
has reached the read before the label (gt-kr5xv). Each candidate is linted (see
gt spec lint), and
polecat_pool.shape_gate says what the verdict does:

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

A candidate is held when its own text names a path a bead already hooked to a
working seat names: two beads on one defect filed inside a dispatch window
share no wording but do share the files that carry the fix, and the second seat
would duplicate the first (gt-z80p9). The held bead is the record — no state
file remembers a dispatch — and the hold lifts when that bead closes.

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
func specPlanHold(report *specDispatchReport, env specDispatchEnv, c specCandidate, full specdispatch.Spec, verdict specdispatch.Verdict) {
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
}

func newSpecTickReads() *specTickReads {
	return &specTickReads{shows: map[string]*beads.Issue{}}
}

// call counts one bd call the tick issued outside show and the client below.
func (r *specTickReads) call() {
	if r != nil {
		r.mu.Lock()
		r.calls++
		r.mu.Unlock()
	}
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
// what it returned.
func (r *specTickReads) show(id string, read func() (*beads.Issue, error)) (*beads.Issue, error) {
	if r == nil {
		return read()
	}
	if issue, ok := r.shows[id]; ok {
		return issue, nil
	}
	issue, err := read()
	r.calls++
	if err == nil && issue != nil {
		r.shows[id] = issue
	}
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
	for _, id := range ids {
		if issue, ok := c.reads.shows[id]; ok {
			out[id] = issue
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	found, err := c.Client.ShowMultiple(missing)
	c.reads.calls++
	for id, issue := range found {
		out[id] = issue
		if issue != nil {
			c.reads.shows[id] = issue
		}
	}
	return out, err
}

func (c specTickClient) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.reads.calls++
	return c.Client.List(opts)
}

func (c specTickClient) Children(parentID string) ([]*beads.Issue, error) {
	c.reads.calls++
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

// specHoldKnobs is the dispatcher's own settings the peer-hold question reads:
// the candidate filter's priority ceiling, and the shape gate with the
// template it lints against. Reading them from the town is the caller's
// (specHoldKnobsFrom), so the hold rule itself is a function of the bead and
// these values.
type specHoldKnobs struct {
	MaxPriority int
	ShapeGate   string
	Template    specdispatch.Template
	// Warn is where a question left unanswered because its inputs would not
	// read says so; nil says nothing, for a caller that builds knobs by hand.
	Warn io.Writer
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
	}
	if knobs.ShapeGate != specdispatch.ShapeGateOff {
		knobs.Template = specdispatch.LoadTemplate(specTemplatePath(sd))
	}
	return knobs, true
}

// specPeerHeldFrom reports why the spec dispatcher would not sling peer, and
// "" when the peer is a bead it would take. It asks the five questions the
// tick asks of a candidate before spending a seat: the candidate filter
// (specdispatch.Eligible), readiness — the ready query lists only beads whose
// blocking dependencies have all resolved, so a peer with one still open is
// never reached at all — the formula declaration, the shape gate's verdict,
// and the container rule. The overlap guard reads the answer on an unstarted
// peer, so a cluster whose head is held dispatches its next eligible bead
// instead of none (gt-00gee, gt-mknmb).
//
// The questions read the peer's own pool row rather than the bead: the fields
// they need are the ones a pool fetch already holds (sling.Readiness), so a
// peer costs the guard no bd read. The child set is the exception — bd's
// parent-child view unions the issue and wisp tables, which the pool's flat
// listing does not — so it is read here from the database the pool came from.
//
// A hold the tick decides from its own state rather than the bead — seat
// availability, the per-tick limit, the pre-dispatch overlap hold, a red-main
// revert in flight — is not answered here: those lift within a tick or two, so
// the peer they hold is one the cluster should still wait for. Nor is a hold
// that holds every bead of a rig together, such as a parked or docked rig: the
// comparison pool is the candidate's own rig, so such a hold can never tell a
// peer apart from the bead being slung. Nor is the tick's other formula skip, a
// declaration the rig has no formula for (gt-j5cr2): answering that one needs
// the formula-existence query the row cannot give, and asking it would spend a
// bd read per weighed peer on a hold that is rare and self-clearing, since
// whoever renamed the formula names the new one in the bead.
//
// Unknown is not held: a peer whose row a degraded pool fetch left unread, and
// a child set that will not read, answer "" and the guard keeps refusing. That
// reads wrong as a delayed dispatch, where the other answer reads wrong as two
// seats on one defect.
func specPeerHeldFrom(store beads.Client, knobs specHoldKnobs, peer sling.Duplicate) string {
	spec := specFromPeer(peer)
	if ok, why := specdispatch.Eligible(spec, knobs.MaxPriority); !ok {
		return why
	}
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
		if verdict := specdispatch.Lint(spec, knobs.Template); !verdict.Clean() {
			// The first refusal, as the lint's own one-line verdict names it:
			// the whole ShapeNote can run to every missing section, and this
			// reason is printed per overlapping bead in the guard's report.
			return verdict.Field + ": " + verdict.Reason
		}
	}
	children, err := store.Children(peer.ID)
	if err != nil {
		warnSpecHoldUnread(knobs.Warn, "children of "+peer.ID, err)
		return ""
	}
	spec.Children = childSpecs(children)
	return specdispatch.OpenChildHold(spec)
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

// maxNamedBlockers bounds the blocker ids specPeerBlocked names, so one widely
// blocked peer does not turn a hold line into a wall.
const maxNamedBlockers = 3

// specPeerBlocked names the blocking dependencies that keep the peer off the
// ready board, and "" when it has none open. The dispatcher's draw is that
// board, so a peer the query leaves out is never slung however eligible it is:
// ranking it ahead of a candidate refuses the rest of its cluster until the
// dependency closes (gt-mknmb).
func specPeerBlocked(open []string) string {
	if len(open) == 0 {
		return ""
	}
	named, more := open, 0
	if len(named) > maxNamedBlockers {
		more = len(named) - maxNamedBlockers
		named = named[:maxNamedBlockers]
	}
	reason := "blocked by " + strings.Join(named, ", ")
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
// shape the overlap guard asks it in: the knobs come from the town, and the
// peer's children from the database its comparison pool came from.
func specPeerHeldPinned(townRoot, beadsDir string, peer sling.Duplicate) string {
	return specPeerHeldPinnedTo(specHoldWarn, townRoot, beadsDir, peer)
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
func specPeerHeldPinnedTo(warn io.Writer, townRoot, beadsDir string, peer sling.Duplicate) string {
	knobs, ok := specHoldKnobsFrom(townRoot, warn)
	if !ok {
		return ""
	}
	return specPeerHeldFrom(slingStores{}.pinnedAt(beadsDir), knobs, peer)
}

func runSpecLint(cmd *cobra.Command, args []string) error {
	townRoot, _ := workspace.FindFromCwd()
	path := specLintTemplate
	if path == "" {
		path = specTemplatePath(loadSpecDispatchConfig(townRoot))
	}
	spec, err := showSpec(townRoot, args[0])
	return specLint(cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], spec, err, path, specLintJSON)
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
	// chose a seat for and whose sling was then deferred (content overlap, a
	// pool backpressure refusal). The entries stay in Skipped too, so the tick
	// line and its cap count them as before; the daemon logs each one on its
	// own 'deferred:' line, where the cap cannot truncate it (gt-4wcqd).
	Deferred []specDispatchEntry `json:"deferred,omitempty"`
	// Notices are lines the tick decided on but no single candidate owns,
	// logged once each (gt-wgyca).
	Notices      []string      `json:"notices,omitempty"`
	SlingsRun    int           `json:"slings_run,omitempty"`
	SlowestSling time.Duration `json:"slowest_sling_ns,omitempty"`
	// The phase times of the tick, read on the tick's clock; the daemon logs
	// them on its 'tick took' line. A held tick runs no phase, so none is set.
	RosterTook     time.Duration `json:"roster_ns,omitempty"`
	BoardTook      time.Duration `json:"board_ns,omitempty"`
	CandidatesTook time.Duration `json:"candidates_ns,omitempty"`
	SlingTook      time.Duration `json:"sling_ns,omitempty"`
	Errors         []string      `json:"errors,omitempty"`
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
}

// specDispatchEnv is every side effect a tick has, so the cycle runs on fakes
// in tests.
type specDispatchEnv struct {
	Hold func() string
	// StartupBackoff reports why an automatic sling of beadID must wait: its
	// last sling failed at session start and the resting window has not
	// elapsed (dispatch.StartupBackoff). Nil consults no record, which is what
	// a tick with no town to read wants.
	StartupBackoff func(beadID string) string
	Candidates     func() specBoardRead
	Show           func(beadID string) (specdispatch.Spec, error)
	// ShowFresh is Show that never answers from an earlier read of the tick: the
	// last-moment re-read before a sling is the check that a submission did not
	// land in the window, so it asks bd. Nil falls back to Show (gt-u8rbh).
	ShowFresh func(beadID string) (specdispatch.Spec, error)
	// Children reads a bead's direct children for the container rule
	// (gt-gektq). The set is read here and handed to specdispatch, which never
	// queries.
	Children func(beadID string) ([]specdispatch.Child, error)
	// Held reads the beads a working seat already holds in a rig, for the
	// pre-dispatch overlap hold (gt-z80p9). The record is the rig's own open
	// beads; nothing here keeps a state file of what it slung.
	Held    func(rig string) ([]specdispatch.HeldBead, error)
	RigHold func(rig string) string
	// RevertInFlight is the revert the rig's red-main owner has building or
	// queued, from the state the landing worker writes; nil is none. It holds
	// that rig's red-main beads, never any other bead (gt-zkdwt).
	RevertInFlight func(rig string) *specdispatch.Revert
	Roster         func() (specRoster, error)
	// Annotate adds text as a comment unless the bead already carries a
	// comment starting with key, so each kind of note lands once.
	Annotate func(beadID, key, text string) error
	AddLabel func(beadID, label string) error
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
	// MaxPriority is the ceiling on a candidate's priority number
	// (polecat_pool.max_priority): a bead numbered higher is backlog.
	MaxPriority int
	// ShapeGate is what the lint's verdict does to a candidate: off runs no
	// lint, warn holds a refused bead and comments the verdict, refuse also
	// labels it needs-shape (polecat_pool.shape_gate, gt-cq5gb, gt-f8ppx).
	ShapeGate string
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
	if hold := env.Hold(); hold != "" {
		report.Hold = hold
		return report
	}
	rosterAt := env.Now()
	roster, err := env.Roster()
	report.RosterTook = env.Now().Sub(rosterAt)
	if err != nil {
		// A roster that cannot be counted is not an empty one: dispatching on
		// it could exceed every cap.
		report.Errors = append(report.Errors, "roster unreadable, not dispatching: "+err.Error())
		return report
	}
	budget := env.Budget
	budget.Seats = append([]specdispatch.Seat(nil), env.Budget.Seats...)
	budget.SetLive(roster.Live)
	budget.SetDeadHooked(roster.DeadHooked)
	budget.NewestSpawn = roster.Newest
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

	boardAt := env.Now()
	read := env.Candidates()
	report.BoardTook = env.Now().Sub(boardAt)
	report.Errors = append(report.Errors, read.Errors...)
	report.Candidates = len(read.Candidates)
	report.LabeledFailed = read.LabeledFailed
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
	// One read per rig per tick of what the working seats already hold, which
	// every candidate of that rig is compared against (gt-z80p9).
	heldReads := map[string]heldRead{}

	candidatesAt := env.Now()
	for _, c := range read.Candidates {
		id := c.Spec.ID
		// The spec is the board row the candidate was drawn from, so the loop
		// spends no read per candidate (gt-bqj8q). The last-moment re-read
		// before the sling is what catches a bead that changed since the board.
		full := c.Spec
		if ok, why := specdispatch.Eligible(full, env.MaxPriority); !ok {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s (%s)", id, specdispatch.SkipNotEligible, why)})
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
		shapeNote := ""
		if env.ShapeGate != specdispatch.ShapeGateOff {
			verdict := specdispatch.Lint(full, env.Template)
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

		// The beads the working seats hold are the record of the work already
		// in flight: a candidate whose own text names a path one of them names
		// is that work's defect filed a second time, and a seat spent on it
		// duplicates a session already on the fix (gt-z80p9).
		if why, err := specHeldHold(env, c.Rig, full, heldReads); err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		} else if why != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s%s", id, specdispatch.SkipHeld, why)})
			continue
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
		// as it is on the peer path, and every candidate gets the read.
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
		if hold := env.RigHold(c.Rig); hold != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: %s %s", id, specdispatch.SkipRigHold, hold)})
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
		pending = append(pending, specPendingSling{c: c, id: id, seat: seat, resume: resume, gone: gone, shapeNote: shapeNote, entry: entry})
		budget.Bump(seat.Agent, budget.Now)
		// The slings run together, so a later candidate of this tick that
		// names the same files is held now, as it was when the earlier sling
		// finished first and had hooked its bead (the content-overlap hold,
		// gt-z80p9).
		specNotePendingHeld(heldReads, c.Rig, id, seat.Agent, full)
	}
	report.CandidatesTook = env.Now().Sub(candidatesAt)
	slingAt := env.Now()
	specRunPendingSlings(env, pending, &report, &budget)
	report.SlingTook = env.Now().Sub(slingAt)
	return report
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
	entry     specDispatchEntry
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
		// Only a refusal proves the sling spawned nothing. A failure or an
		// exhausted contention may have hooked the bead or started a session
		// before it returned, so their seat stays charged for this tick (gt-pto2f).
		if o.refused {
			budget.Release(o.entry.Agent)
		}
		if o.label != "" {
			if err := env.AddLabel(o.entry.Bead, o.label); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", o.entry.Bead, err))
			}
		}
		for _, n := range o.notes {
			if err := env.Annotate(o.entry.Bead, n[0], n[1]); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", o.entry.Bead, err))
			}
		}
	}
	report.SlingsRun = len(pending)
	report.SlowestSling = slowest
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
		if isSpecOverlapRefusal(err) {
			entry.Line = fmt.Sprintf("%s: sling deferred, content overlap holds it (work in flight, or unstarted work the dispatcher would sling first): %s", id, firstErrLine(err))
			return done(specSlingOutcome{kind: specSlingSkipped, entry: entry, refused: true})
		}
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
	o := specSlingOutcome{kind: specSlingDispatched, entry: entry}
	if p.gone != "" {
		note := fmt.Sprintf("%s%s: resume branch %s is gone from origin; dispatched fresh (gt-gzhin.3)", specDispatchNotePrefix, id, p.gone)
		o.notes = append(o.notes, [2]string{note, note})
	}
	return done(o)
}

// heldRead is one rig's held-bead read, memoized for the tick so a rig with
// several candidates reads its held set once.
type heldRead struct {
	beads []specdispatch.HeldBead
	err   error
}

// specHeldHold is the pre-dispatch overlap hold: the reason a candidate is the
// same defect a working seat already holds, empty when it is not, and an error
// when the held set could not be read — an unreadable set is not an empty one,
// so the candidate waits for the next tick rather than duplicating a seat
// (gt-z80p9). A candidate naming no path cannot be caught this way, so it never
// pays for the read.
// specNotePendingHeld adds a candidate the tick is about to sling to the
// rig's held set, with the files its text names, so the candidates after it
// in the same tick are held on the same overlap the seats' hooked beads
// cause. A rig whose held read failed stays failed.
func specNotePendingHeld(reads map[string]heldRead, rig, id, agent string, full specdispatch.Spec) {
	paths := specHoldFiles(extractOverlapRefs(full.Title, full.Description, full.Design, full.Notes).Files, full.Description)
	if len(paths) == 0 {
		return
	}
	read := reads[rig]
	if read.err != nil {
		return
	}
	read.beads = append(read.beads, specdispatch.HeldBead{ID: id, Assignee: "this tick's sling on " + agent, Paths: paths})
	reads[rig] = read
}

func specHeldHold(env specDispatchEnv, rig string, full specdispatch.Spec, reads map[string]heldRead) (string, error) {
	paths := specHoldFiles(extractOverlapRefs(full.Title, full.Description, full.Design, full.Notes).Files, full.Description)
	if len(paths) == 0 {
		return "", nil
	}
	read, ok := reads[rig]
	if !ok {
		beads, err := env.Held(rig)
		read = heldRead{beads: beads, err: err}
		reads[rig] = read
	}
	if read.err != nil {
		return "", fmt.Errorf("%s: cannot read the beads the seats hold: %v", full.ID, read.err)
	}
	return specdispatch.HeldOverlap(paths, read.beads), nil
}

// specHoldFiles is the file set the overlap hold reads for one bead: the paths
// its text names, minus the reference documents its Constraints section does
// not list as work (gt-6t44f). The sling-time guard keeps every named path.
func specHoldFiles(files []string, description string) []string {
	listed := extractContentRefs(specdispatch.Sections(description)["constraints"]).Files
	return specdispatch.HoldFiles(files, listed)
}

// specHeldStatuses are the statuses that mean a working seat holds the bead:
// the sling hooks it, and the polecat's claim makes it in_progress.
var specHeldStatuses = []string{beads.StatusHooked, string(beads.StatusInProgress)}

// heldBeadsFrom reduces the beads one store holds to the fields the overlap
// rule reads. It reads the same refs the sling-time duplicate guard compares
// (listDuplicateCandidates), so the dispatcher and the guard cannot disagree
// about what a bead's text names (gt-z80p9).
//
// An epic is not held: its prose enumerates its children's files, so a claimed
// epic or wayfinder map would otherwise hold every ready child that names one.
// The sling-time guard skips epics for the same reason (findDuplicateMatches).
//
// An unassigned bead is not held: hooked or in_progress means a seat only when
// an assignee names one, and one stray handoff bead would otherwise hold every
// ready bead naming the same file until an operator closed it by hand
// (gt-xrog0).
//
// The read is fresh, not the guard's cached pool: that pool is a snapshot of
// the database at fetch time, while the seat this hold protects is the bead's
// own live status.
func heldBeadsFrom(store beads.Client, warn func(beadsDir string, err error), beadsDir string) ([]specdispatch.HeldBead, error) {
	cands, err := listDuplicateCandidates(store, warn, beadsDir, specHeldStatuses, time.Time{})
	if err != nil {
		return nil, err
	}
	out := make([]specdispatch.HeldBead, 0, len(cands))
	for _, c := range cands {
		if c.IsEpic() || c.Ready.Assignee == "" {
			continue
		}
		out = append(out, specdispatch.HeldBead{ID: c.ID, Assignee: c.Ready.Assignee, Paths: c.HoldFiles})
	}
	return out, nil
}

// specHeldBeads reads the beads a working seat already holds in one rig, as the
// paths their own text names (gt-z80p9). The record is the rig's own open
// beads, never a state file of what the dispatcher slung, and the rig is the
// candidate's: a sling is a rig's own.
//
// A bead a crashed seat left hooked reads as held here too. The patrol readies
// that work, which clears the hold, so the cost of being wrong is a delayed
// dispatch rather than a duplicated session.
func specHeldBeads(reads *specTickReads, townRoot, rig string) ([]specdispatch.HeldBead, error) {
	if rig == "" {
		return nil, nil
	}
	beadsDir := beads.ResolveBeadsDir(filepath.Join(townRoot, rig))
	return heldBeadsFrom(reads.client(beads.NewPinned(beadsDir)), warnDuplicateEnrichmentFailed, beadsDir)
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

// isSpecOverlapRefusal reports whether a sling error is the content-overlap
// guard's refusal. The guard refuses work in flight, and an unstarted peer only
// while that peer comes first in dispatch order and the dispatcher would sling
// it, so the refusal clears on its own once the peer is slung or closes: the
// dispatcher defers the bead rather than labeling it out of the queue
// (gt-q6zoo, gt-nlkba, gt-00gee).
func isSpecOverlapRefusal(err error) bool {
	return errors.Is(err, errSlingDuplicateContent)
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
	start := time.Now()
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
	details := specDetailCacheFor(townRoot, os.Getenv(dispatch.SpecDetailOwnerEnv))
	specDetailCache.Store(details)
	// The held beads already annotated, kept across the daemon's ticks.
	annotations := newSpecAnnotatedTick(townRoot, os.Getenv(dispatch.SpecDetailOwnerEnv), func(id string) beads.Client {
		reads.call()
		return beads.New(resolveBeadDirFromTownRoot(townRoot, id))
	})
	env := specDispatchEnv{
		Hold:           func() string { return dispatch.OperatorHold(townRoot) },
		StartupBackoff: func(id string) string { return dispatch.StartupBackoff(townRoot, id) },
		Candidates: func() specBoardRead {
			read := specCandidates(townRoot, maxPriority, func(rigPath string) ([]*beads.Issue, error) {
				reads.call()
				return specReadyBoard(rigPath)
			})
			read = specBoardDesigns(reads, func(rig string) beads.Client {
				return beads.NewPinned(beads.ResolveBeadsDir(filepath.Join(townRoot, rig)))
			}, read)
			annotations.observe(read)
			return read
		},
		Show: func(id string) (specdispatch.Spec, error) {
			return showSpecWith(reads, id, func(id string) (*beads.Issue, error) { return showBead(townRoot, id) })
		},
		ShowFresh: func(id string) (specdispatch.Spec, error) { return showSpec(townRoot, id) },
		Children: func(id string) ([]specdispatch.Child, error) {
			reads.call()
			return specChildren(townRoot, id)
		},
		Held:    func(rig string) ([]specdispatch.HeldBead, error) { return specHeldBeads(reads, townRoot, rig) },
		RigHold: func(rig string) string { return dispatch.RigHold(townRoot, rig) },
		RevertInFlight: func(rig string) *specdispatch.Revert {
			return rigRevertInFlight(townRoot, rig, func(beadID string) bool {
				reads.call()
				return revertBeadOpen(townRoot, rig, beadID)
			})
		},
		Roster: func() (specRoster, error) {
			// The same seat picture `gt daemon dispatch-check` nudges from:
			// live sessions, in-flight claims, and seats mid-landing. Two
			// pictures of one pool is how the dispatcher ends up slinging into
			// a seat the nudge just called free (gt-thy6r).
			seats, err := specRosterSnapshot(reads, townRoot, ts.PolecatPool)
			if err != nil {
				return specRoster{}, fmt.Errorf("listing polecat seats: %w", err)
			}
			// Kept for the slings: each one's seat decision reads this
			// snapshot plus the live sessions and claims of its moment
			// (snapshotOccupied) instead of every polecat directory again.
			snapshot.Store(&seats)
			return specRosterFrom(seats.sessions, ts), nil
		},
		Annotate: annotations.annotate,
		AddLabel: func(id, label string) error {
			return poolBeadLabelAdd(townRoot, id, label)
		},
		BranchOnOrigin: func(rig, branch string) (bool, error) {
			return polecat.BranchOnOrigin(filepath.Join(townRoot, rig), branch)
		},
		FormulaExists: func(rig, formula string) error {
			return verifyFormulaExists(formula, filepath.Join(townRoot, rig), townRoot)
		},
		Sling: func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (string, error) {
			var occupied func() ([]poolSession, error)
			if seats := snapshot.Load(); seats != nil {
				occupied = snapshotOccupied(*seats, newPoolSessionLister, time.Now)
			}
			return slingSpec(townRoot, c, resumeBranch, seat, occupied)
		},
		Sleep:    time.Sleep,
		Now:      time.Now,
		Template: specdispatch.LoadTemplate(tmplPath),
		Budget:   budget,
		PerTick:  perTick,
		DryRun:   specDispatchDryRun,

		MaxPriority: maxPriority,
		ShapeGate:   ts.PolecatPool.GetShapeGate(),
	}
	report := runSpecDispatchCycle(env)
	annotations.save()
	report.DetailsFetched, report.DetailsCached, report.DetailsUnread = details.Counts()
	if report.Hold == "" {
		report.Notices = append(report.Notices, specTickCost(reads, time.Since(start), report.SlingsRun, report.SlowestSling))
	}
	return report, nil
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

// specRosterSnapshot is the seat picture poolSeatSessions draws, with its
// polecat-state reads batched per rig and read through the tick's ledger, and
// the seats that read released kept beside it for the tick's slings
// (snapshotOccupied).
func specRosterSnapshot(reads *specTickReads, townRoot string, pool *config.PolecatPool) (poolSeatSnapshot, error) {
	return specRosterSnapshotWith(reads, townRoot, pool, newPoolSessionLister(), func(dir string) beads.Client { return beads.New(dir) }, time.Now)
}

// specRosterSnapshotWith is specRosterSnapshot with the session lister, the
// store for a directory and the clock passed in, so a test can run this wiring
// over a fake. The time it began is taken before any session is listed, so a
// session created after it counts as new.
func specRosterSnapshotWith(reads *specTickReads, townRoot string, pool *config.PolecatPool, lister sessionLister, storeFor func(dir string) beads.Client, now func() time.Time) (poolSeatSnapshot, error) {
	store := func(dir string) beads.Client { return reads.client(storeFor(dir)) }
	batch := newPoolSeatBatch(townRoot,
		func(rigName string) beads.Client { return store(filepath.Join(townRoot, rigName)) },
		func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) })
	work := func(ids []string) (map[string]*beads.Issue, error) {
		return store(townRoot).ShowMultiple(ids)
	}
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
	var read specBoardRead
	names, err := knownRigNames(townRoot)
	if err != nil {
		read.Errors = []string{err.Error()}
		return read
	}
	var specs []specdispatch.Spec
	rigOf := map[string]string{}
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
		for _, issue := range issues {
			s := specFromIssue(issue)
			// A bead the failed label is holding is counted, not taken: the
			// count is what the health line surfaces, and Eligible would drop
			// it from candidacy anyway (gt-q6zoo).
			if s.HasLabel(specdispatch.DispatchFailedLabel) {
				labeled[s.ID] = true
				continue
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
			specs = append(specs, s)
		}
	}
	specdispatch.Order(specs)
	read.Candidates = make([]specCandidate, 0, len(specs))
	for _, s := range specs {
		read.Candidates = append(read.Candidates, specCandidate{Spec: s, Rig: rigOf[s.ID]})
	}
	read.LabeledFailed = len(labeled)
	return read
}

// specBoardDesigns fills the design a board row may leave out. A row without
// one is either a bead with no design or one the row dropped, and the rig's
// own store answers which. The
// candidates that need it are read with one ShowMultiple per rig, never one
// show per candidate (gt-bqj8q). A candidate whose design cannot be read is
// dropped with its error: the design is part of the spec the lint reads, so
// the bead waits for the next tick rather than dispatching without it.
func specBoardDesigns(reads *specTickReads, storeFor func(rig string) beads.Client, read specBoardRead) specBoardRead {
	var rigs []string
	byRig := map[string][]int{}
	for i, c := range read.Candidates {
		if c.Spec.Design != "" {
			continue
		}
		if _, seen := byRig[c.Rig]; !seen {
			rigs = append(rigs, c.Rig)
		}
		byRig[c.Rig] = append(byRig[c.Rig], i)
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
			if issue := issues[id]; issue != nil {
				read.Candidates[i].Spec.Design = issue.Design
				continue
			}
			dropped[i] = true
			reason := "not found"
			if err != nil {
				reason = firstErrLine(err)
			}
			read.Errors = append(read.Errors, fmt.Sprintf("%s: cannot read design: %s", id, reason))
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

// specReadyArgs is the ready query: unassigned work beads, with the
// dispatcher's exclusions and the town's non-dispatchable families filtered
// server-side, unlimited. The retired label spec and type feature are not
// queried on (gt-mmsr2); the non-work types (an epic, the runtime families)
// are excluded instead, so the board holds work beads only.
//
// A board-carried label is deliberately not among the server-side exclusions:
// the board must carry the beads it holds so the tick can count them and name
// them in its skip line, and the health line can show the count (gt-q6zoo,
// gt-k1g3e). Eligible still keeps them out of the candidate set.
//
// The count is read from this board, not a second query, because `bd ready
// --json` hydrates each issue's labels array. That is the CLI's own answer for
// the bd on PATH (RunBdJSONAllowStale runs it as a subprocess); the notes in
// internal/cmd/ready.go and internal/beads about `bd ready --json` carrying no
// labels describe the store-backed readers, which build their issue list from
// the store and never see this JSON. A live board captured 2026-10-02 with
// these exact args carried labels on 61 of 72 issues (every issue that has
// any), and TestCapturedSpecBoardCarriesLabels pins the shape against a
// captured board whose labeled bead is the one the dispatcher itself labeled.
func specReadyArgs() []string {
	exclude := append([]string(nil), constants.NonDispatchableBeadLabels...)
	exclude = append(exclude, specdispatch.ReadyQueryLabels()...)
	return []string{
		"ready", "--json",
		"--unassigned",
		"--exclude-label", strings.Join(exclude, ","),
		"--exclude-type", strings.Join(specdispatch.NonWorkBeadTypes(), ","),
		"--limit", "0",
	}
}

// specReadyBoard runs the ready query in one rig.
//
// keep-raw (gt-7iwy0.4.10): the ready query stays on the argv path. It is the
// one dispatcher query whose filters (--unassigned, --exclude-label,
// --exclude-type, --limit 0) must run server-side against the rig's own
// database, and Client.ReadyAll answers a different, bookkeeping-only
// exclusion set; expressing this one typed would push the dispatcher's
// exclusions into a Go filter that no longer matches what bd ready selects,
// changing which beads gt spec dispatch picks. RunBdJSONAllowStale pins the
// rig database and asks bd's stale-read bypass, which no Client method does.
func specReadyBoard(rigPath string) ([]*beads.Issue, error) {
	out, err := beads.RunBdJSONAllowStale(rigPath, specReadyArgs()...)
	if err != nil {
		return nil, err
	}
	return parseSpecReady(out)
}

// parseSpecReady accepts a bare JSON array or an envelope with an "issues"
// array; empty output is an empty board.
func parseSpecReady(out []byte) ([]*beads.Issue, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var issues []*beads.Issue
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &issues); err != nil {
			return nil, fmt.Errorf("parsing bd ready: %w", err)
		}
		return issues, nil
	}
	var env struct {
		Issues []*beads.Issue `json:"issues"`
		Data   []*beads.Issue `json:"data"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return nil, fmt.Errorf("parsing bd ready: %w", err)
	}
	if env.Issues != nil {
		return env.Issues, nil
	}
	return env.Data, nil
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
		TownRoot:         townRoot,
		BeadsDir:         beadsDir,
	}
}

// slingSpec dispatches one clean spec through the shared rig-dispatch path.
// The agent is explicit, so it wins over the bead's route:* labels in the
// pool; the pool still enforces its own caps and may refuse. occupied, when
// set, is the seat occupancy read the pool decision uses (the tick's
// snapshot); nil reads the town.
func slingSpec(townRoot string, c specCandidate, resumeBranch string, seat specdispatch.SeatChoice, occupied func() ([]poolSession, error)) (string, error) {
	targetBeadsDir, ok := beads.ResolveRepoAliasBeadsDir(townRoot, c.Rig)
	if !ok {
		return "", fmt.Errorf("cannot resolve rig %q beads database for %s", c.Rig, c.Spec.ID)
	}
	params := specSlingParams(townRoot, targetBeadsDir, resolveFormula("", false, townRoot, c.Rig), c, resumeBranch, seat)
	d := realSlingDeps()
	d.beadInfoInTown = refuseOpenBlockers(d.beadInfoInTown)
	if occupied != nil {
		spawn := d.spawnPolecat
		d.spawnPolecat = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
			opts.occupied = occupied
			return spawn(rigName, opts)
		}
	}
	result, err := d.executeSling(params)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", errors.New("sling returned no result")
	}
	return result.PolecatName, nil
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
		if why := specPeerBlocked(beads.UnresolvedBlockerIDs(info.Dependencies)); why != "" {
			return nil, fmt.Errorf("%s %s is %s", dispatch.SlingRefusalMarker, beadID, why)
		}
		return info, nil
	}
}
