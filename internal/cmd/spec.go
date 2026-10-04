package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/steward"
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
// daemon's spec_dispatch ticker runs `gt spec dispatch --json` on its cadence,
// the way mayor_dispatch runs `gt daemon dispatch-check` (internal/cmd imports
// internal/daemon, so the call cannot go the other way).
//
// This is the dispatcher that replaced the seat-refill plugin (gt-4k3fj.8.8):
// the pool's seats (overflow_agent/max_overflow, pro_*), the operator's
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
// claims other slings hold, whoever slung them. The mayor's slings land in that
// same count, so the ticker never pushes past a cap another path already filled
// — it skips the tick instead.

const (
	specDispatchActor      = "daemon/spec-dispatch"
	specDispatchNotePrefix = "spec-dispatch: "
	// defaultSpecHookedAgent is the agent the optional hooked seat runs when
	// spec_dispatch.max_hooked names a cap but no agent.
	defaultSpecHookedAgent = "claude-sonnet"
	// defaultSpecOverflowCap caps the pool's overflow seat when the pool leaves
	// max_overflow unset. The pool itself reads an unset max_overflow as
	// uncapped, but an uncapped dispatcher seat is an unbounded run of paid
	// sessions, so the dispatcher gives itself a cap it can defend rather than
	// treating the pool's silence as infinite room.
	defaultSpecOverflowCap  = 2
	defaultSpecMaxPerTick   = 1
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
daemon.json patrols.spec_dispatch.template, else
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
gt:ready-to-land, needs-human or needs-mayor-review, or deferred, are never
taken, and neither is a bead submitted for landing whose READY TO LAND block
has reached the read before the label (gt-kr5xv). Each candidate is linted (see
gt spec lint), and
polecat_pool.shape_gate says what the verdict does:

  - off: no lint runs; the bead is dispatched like any other
  - warn (the default): a bead the lint refuses is held — skipped with the
    reason "unshaped: <fields>" and left ready for the next tick — and the
    verdict is left on it as one comment (SHAPE: ...). A bead labeled
    spec-shape-waived waives the lint and dispatches anyway
  - refuse: as warn, and a held bead also wears the needs-shape label; a bead
    that needs planning wears the needs-planning label, and the tick names
    which planning state it is in — "needs plan" while it waits for a plan
    job's PLAN PROPOSAL, "plan proposed" once that block is in its notes and
    only the operator can file the children (gt-4k3fj.14). The tick spawns no
    planner: patrols.steward_plan in mayor/daemon.json runs the plan jobs,
    and it is off until the operator turns it on

Beads labeled red-main are held while the red-main owner is reverting the
breakage they were filed for: the landing worker records a revert in flight in
the rig's red-main state file (<town>/.runtime/red-main/<rig>.json), and that
rig's red-main beads stay ready instead of racing the revert with a fix
forward. The hold lifts when the revert lands or is rejected, so the next tick
takes them again (gt-zkdwt).

A clean candidate is re-read once more immediately before its sling and dropped
if the bead has changed since the board was read — a submission in that window
would otherwise be slung to a seat already landing it, and a re-read that fails
holds the candidate for the next tick rather than slinging blind (gt-01gix).
One that still holds is slung onto the first free seat within the budget: the
pool's overflow_agent (capped by max_overflow), then the pro seat (pro_agent,
capped by pro_max, taking only pro_label beads). The claude-sonnet hooked seat
is off unless patrols.spec_dispatch.max_hooked is set above zero.

Every dispatch's sling args tell the polecat to test install paths in a
temporary INSTALL_DIR.

Budget comes from polecat_pool in settings/config.json (overflow_agent,
max_overflow, min_spawn_gap, pro_agent, pro_max, pro_label) and
patrols.spec_dispatch in mayor/daemon.json (hooked_agent, max_hooked,
prefer_hooked, max_per_tick).
The operator hold file and ESTOP stop the tick.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runSpecDispatch,
}

func init() {
	specLintCmd.Flags().StringVar(&specLintTemplate, "template", "", "Spec template path (default daemon.json patrols.spec_dispatch.template, else ~/.claude/docs/agents/spec-template.md)")
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
	if steward.HasPlanProposal(notes) {
		return specPlanProposed
	}
	return specPlanNeedsPlan
}

// specPlanLine is the tick's line for a needs-planning spec: the state, and
// what the state means for the next reader.
func specPlanLine(id, state, reason string) string {
	if state == specPlanProposed {
		return fmt.Sprintf("%s: %s: the %s block in its notes is the operator's to file", id, state, steward.PlanProposalMarker)
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

// specFromIssue reduces a bead to the fields the lint reads.
func specFromIssue(issue *beads.Issue) specdispatch.Spec {
	if issue == nil {
		return specdispatch.Spec{}
	}
	return specdispatch.Spec{
		ID:          issue.ID,
		Title:       issue.Title,
		Type:        issue.Type,
		Status:      issue.Status,
		Assignee:    issue.Assignee,
		Priority:    issue.Priority,
		CreatedAt:   issue.CreatedAt,
		Labels:      issue.Labels,
		Description: issue.Description,
		Design:      issue.Design,
		Notes:       issue.Notes,
		Acceptance:  issue.AcceptanceCriteria,
		Ephemeral:   issue.Ephemeral,

		SubmittedForLanding: specLiveLandingRequest(issue.Notes),
	}
}

// specLiveLandingRequest reports whether notes carry a landing request that
// nothing later settled: a complete READY TO LAND block after the last LANDING
// RECORD and the last MERGE REJECTION. gt done writes that block before the
// gt:ready-to-land label (internal/done markReadyToLand), so a bead mid-write
// has the block and no label; the label is also the half of an answer known to
// go missing (gt-q6zoo), which leaves the notes as the signal that survives.
// Holding on the block keeps such a bead off the candidate board and out of a
// sling (gt-kr5xv). A land or a rejection written after the block settles it,
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
	issue, err := showBead(townRoot, beadID)
	if err != nil {
		return specdispatch.Spec{}, fmt.Errorf("bead %s not found: %w", beadID, err)
	}
	return specFromIssue(issue), nil
}

// specChildren reads a bead's direct children and reduces them to what the
// container rule reads (gt-gektq). The read is slingStores.children, whose
// comment says why it is bd's view and not the raw dependency scan.
func specChildren(townRoot, beadID string) ([]specdispatch.Child, error) {
	issues, err := slingStores{}.children(townRoot, beadID)
	if err != nil {
		return nil, err
	}
	out := make([]specdispatch.Child, 0, len(issues))
	for _, issue := range issues {
		out = append(out, specdispatch.Child{ID: issue.ID, Status: issue.Status})
	}
	return out, nil
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
	LabeledFailed int                 `json:"labeled_failed,omitempty"`
	Dispatched    []specDispatchEntry `json:"dispatched"`
	Refused       []specDispatchEntry `json:"refused"`
	Planning      []specDispatchEntry `json:"planning"`
	Skipped       []specDispatchEntry `json:"skipped"`
	Failed        []specDispatchEntry `json:"failed"`
	// Notices are lines the tick decided on but no single candidate owns,
	// logged once each (gt-wgyca).
	Notices []string `json:"notices,omitempty"`
	Errors  []string `json:"errors,omitempty"`
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
}

// specRoster is the live polecat picture the budget is built from: the seats
// the pool counts as taken per agent — live polecats, in-flight seat claims,
// and the seats mid-landing (poolSeatSessions) — and the newest spawn.
type specRoster struct {
	Live   map[string]int
	Newest time.Time
}

// specDispatchEnv is every side effect a tick has, so the cycle runs on fakes
// in tests.
type specDispatchEnv struct {
	Hold       func() string
	Candidates func() specBoardRead
	Show       func(beadID string) (specdispatch.Spec, error)
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
	// BranchOnOrigin reports whether a bead's recorded resume branch still
	// exists on the rig's origin. An error means the answer is unknown, and
	// the candidate is left for the next tick (gt-gzhin.3).
	BranchOnOrigin func(rig, branch string) (bool, error)
	Sling          func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (polecat string, err error)
	Sleep          func(time.Duration)
	Now            func() time.Time
	Template       specdispatch.Template
	Budget         specdispatch.Budget // agents, caps and preferences; live counts filled per tick
	PerTick        int
	DryRun         bool
	// MaxPriority is the ceiling on a candidate's priority number
	// (polecat_pool.max_priority): a bead numbered higher is backlog.
	MaxPriority int
	// ShapeGate is what the lint's verdict does to a candidate: off runs no
	// lint, warn holds a refused bead and comments the verdict, refuse also
	// labels it needs-shape (polecat_pool.shape_gate, gt-cq5gb, gt-f8ppx).
	ShapeGate string
}

// runSpecDispatchCycle is one tick. It never guesses: under the default warn
// gate a bead the lint refuses is held — skipped unshaped and commented once —
// while one that needs planning still dispatches; under refuse the held bead is
// also labeled needs-shape, and one that needs planning is labeled and reported
// by its planning state ("needs plan" or "plan proposed", gt-4k3fj.14) rather
// than slung; off runs no lint. A bead labeled spec-shape-waived waives the
// lint and dispatches under any gate (gt-f8ppx). A container — a bead with at
// least one open child — is held until its children close, under any gate
// (gt-gektq). A candidate is slung only onto a free seat that takes it, and a
// skip leaves it ready for the next tick.
func runSpecDispatchCycle(env specDispatchEnv) specDispatchReport {
	report := specDispatchReport{Template: env.Template.Source}
	if hold := env.Hold(); hold != "" {
		report.Hold = hold
		return report
	}
	roster, err := env.Roster()
	if err != nil {
		// A roster that cannot be counted is not an empty one: dispatching on
		// it could exceed every cap.
		report.Errors = append(report.Errors, "roster unreadable, not dispatching: "+err.Error())
		return report
	}
	budget := env.Budget
	budget.Seats = append([]specdispatch.Seat(nil), env.Budget.Seats...)
	budget.SetLive(roster.Live)
	budget.NewestSpawn = roster.Newest
	report.Roster = budget.Picture()

	read := env.Candidates()
	report.Errors = append(report.Errors, read.Errors...)
	report.Candidates = len(read.Candidates)
	report.LabeledFailed = read.LabeledFailed
	perTick := env.PerTick
	if perTick <= 0 {
		perTick = defaultSpecMaxPerTick
	}
	// One notice per rig per tick: the revert record is the rig's, not a
	// candidate's, so a rig with several red-main beads still logs it once
	// (gt-wgyca).
	staleNoted := map[string]bool{}

	for _, c := range read.Candidates {
		id := c.Spec.ID
		full, err := env.Show(id)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		// The ready board is a snapshot; re-check the fresh bead.
		if ok, why := specdispatch.Eligible(full, env.MaxPriority, budget.ReservedLabels()); !ok {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: not eligible (%s)", id, why)})
			continue
		}
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
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: held: %s", id, why)})
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
				// The default gate holds a bead the lint refuses rather than
				// slinging it (gt-f8ppx): a flash polecat handed work the bead
				// does not pin down wanders off it, so the bead waits for the
				// operator to shape it or waive the lint. The one SHAPE note
				// still lands so the bead's history says why; it is written
				// here, not on the dispatch path below, because a held bead
				// never reaches that path. A planning verdict is not a refusal
				// and still dispatches.
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
					// A needs-planning spec is in one of two states, and the
					// tick names which: waiting for a plan job's proposal, or
					// carrying one the operator files by hand. Nothing is
					// spawned either way — the plan patrol is the spawner
					// (gt-4k3fj.13, gt-4k3fj.14).
					state := specPlanState(full.Notes)
					report.Planning = append(report.Planning, specDispatchEntry{Bead: id, Rig: c.Rig, Line: specPlanLine(id, state, verdict.Reason)})
					if !env.DryRun {
						if !full.HasLabel(specdispatch.NeedsPlanningLabel) {
							if err := env.AddLabel(id, specdispatch.NeedsPlanningLabel); err != nil {
								report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", id, err))
							}
						}
						key := fmt.Sprintf("%s%s: %s", specDispatchNotePrefix, id, state)
						if err := env.Annotate(id, key, specPlanNote(key, state, verdict.Reason)); err != nil {
							report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
						}
					}
					continue
				}
			}
		}

		// A bead whose children are open is a container, not a unit of work:
		// its scope is already sliced into children a seat will take, so
		// spending one on the parent duplicates them (gt-gektq). An unreadable
		// child set is not an empty one — the bead might be a container — so it
		// is left for the next tick rather than slung.
		children, err := env.Children(id)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: cannot read children: %v", id, err))
			continue
		}
		full.Children = children
		if why := specdispatch.OpenChildHold(full); why != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: held: %s", id, why)})
			continue
		}

		if len(report.Dispatched) >= perTick {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: clean, per-tick limit %d reached", id, perTick)})
			continue
		}
		if hold := env.RigHold(c.Rig); hold != "" {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: rig hold: %s", id, hold)})
			continue
		}
		budget.Now = env.Now()
		seat := specdispatch.ChooseSeat(budget, full)
		if seat.Skip {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: no seat: %s", id, seat.Reason)})
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
			entry.Line += specResumeNote(resume, gone)
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
		latest, err := env.Show(id)
		if err != nil {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent, Line: fmt.Sprintf("%s: %s: re-read failed: %s", id, specStaleReason, firstErrLine(err))})
			continue
		}
		if ok, why := specdispatch.Eligible(latest, env.MaxPriority, budget.ReservedLabels()); !ok {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent, Line: fmt.Sprintf("%s: %s: %s", id, specStaleReason, why)})
			continue
		}

		var polecat string
		attempts, err := specdispatch.RetryOnContention(specdispatch.RetryAttempts, env.Sleep, rand.New(rand.NewSource(env.Now().UnixNano())), func() error { //nolint:gosec // G404: backoff jitter
			var slingErr error
			polecat, slingErr = env.Sling(c, resume, seat)
			return slingErr
		})
		if err != nil {
			if isSpecOverlapRefusal(err) {
				// The content-overlap guard refuses only live work, so the
				// refusal lifts on its own when the overlapping bead closes.
				// The bead stays ready and the next tick retries it, rather
				// than leaving a label for an operator to clear by hand
				// (gt-q6zoo).
				report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent, Line: fmt.Sprintf("%s: sling deferred, content overlap clears when the overlapping bead closes: %s", id, firstErrLine(err))})
				continue
			}
			if isSpecSlingRefusal(err) {
				// Capacity, not failure: the pool or the merge queue said
				// not now. The bead stays ready.
				report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent, Line: fmt.Sprintf("%s: sling deferred: %s", id, firstErrLine(err))})
				continue
			}
			why := firstErrLine(err)
			if specdispatch.IsSerializationFailure(err) {
				why = fmt.Sprintf("%v after %d attempts: %s", specdispatch.ErrRetriesExhausted, attempts, why)
			}
			excluded := fmt.Sprintf("excluded from the queue until label %s is cleared", specdispatch.DispatchFailedLabel)
			failLine := fmt.Sprintf("%s: dispatch failed, left unassigned: %s; %s", id, why, excluded)
			entry.Line = failLine
			report.Failed = append(report.Failed, entry)
			// The label takes the bead out of the candidate set, so a sling
			// that fails for a reason other than capacity does not spawn and
			// roll back a polecat every tick. Removing it retries. Only a
			// failure that cannot clear itself is labeled; an overlap is
			// deferred above instead (gt-q6zoo).
			if err := env.AddLabel(id, specdispatch.DispatchFailedLabel); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", id, err))
			}
			key := fmt.Sprintf("%s%s: dispatch failed", specDispatchNotePrefix, id)
			if err := env.Annotate(id, key, specDispatchNotePrefix+failLine); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
			}
			continue
		}
		entry.Polecat = polecat
		entry.Line = fmt.Sprintf("%s: slung to %s/%s on %s (%s)", id, c.Rig, polecat, seat.Agent, seat.Reason)
		entry.Line += specResumeNote(resume, gone)
		if attempts > 1 {
			entry.Line += fmt.Sprintf(" after %d attempts", attempts)
		}
		if shapeNote != "" {
			entry.Line += "; " + shapeNote
		}
		report.Dispatched = append(report.Dispatched, entry)
		budget.Bump(seat.Agent, budget.Now)
		if gone != "" {
			note := fmt.Sprintf("%s%s: resume branch %s is gone from origin; dispatched fresh (gt-gzhin.3)", specDispatchNotePrefix, id, gone)
			if err := env.Annotate(id, note, note); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
			}
		}
	}
	return report
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
// guard's refusal. The guard refuses only live work, so the refusal clears on
// its own when the overlapping bead closes: the dispatcher defers the bead
// rather than labeling it out of the queue (gt-q6zoo).
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

// specBudgetFromConfig builds the seat list from polecat_pool and
// patrols.spec_dispatch. Each seat is one agent with its own cap:
//
//   - the pool's overflow_agent, capped at max_overflow (the dispatcher's own
//     defaultSpecOverflowCap when the pool leaves it unset)
//   - the pro seat: pro_agent, capped at pro_max, taking only pro_label beads
//
// The hooked seat (spec_dispatch.hooked_agent, claude-sonnet by default) is
// off unless max_hooked names a cap above zero: the pool's agents are the
// town's seats, and a seat nobody asked for is a seat the dispatcher must not
// spend. Order is the list above; prefer_hooked moves the hooked seat first.
// An agent named twice keeps its first seat.
func specBudgetFromConfig(ts *config.TownSettings, sd *config.SpecDispatchConfig) specdispatch.Budget {
	var b specdispatch.Budget
	var pool *config.PolecatPool
	if ts != nil {
		pool = ts.PolecatPool
	}
	if pool != nil {
		b.MinSpawnGap = pool.MinSpawnGapD()
		if pool.OverflowAgent != "" {
			capacity := pool.MaxOverflow
			if capacity <= 0 {
				capacity = defaultSpecOverflowCap
			}
			b.Seats = append(b.Seats, specdispatch.Seat{Agent: pool.OverflowAgent, Cap: capacity})
		}
		if pool.GetProMax() > 0 {
			b.Seats = append(b.Seats, specdispatch.Seat{
				Agent: pool.GetProAgent(),
				Cap:   pool.GetProMax(),
				Label: pool.GetProLabel(),
			})
		}
	}

	if sd != nil && sd.MaxHooked > 0 {
		agent := sd.HookedAgent
		if agent == "" {
			agent = defaultSpecHookedAgent
		}
		hooked := specdispatch.Seat{Agent: agent, Cap: sd.MaxHooked}
		if sd.PreferHooked {
			b.Seats = append([]specdispatch.Seat{hooked}, b.Seats...)
		} else {
			b.Seats = append(b.Seats, hooked)
		}
	}

	seen := map[string]bool{}
	seats := b.Seats[:0:0]
	for _, seat := range b.Seats {
		if seat.Agent == "" || seen[seat.Agent] {
			continue
		}
		seen[seat.Agent] = true
		seats = append(seats, seat)
	}
	b.Seats = seats
	return b
}

// specRosterFrom counts the seats the pool counts as taken (live polecats,
// in-flight seat claims, and seats mid-landing) per agent. An empty GT_AGENT
// runs the polecat role default.
func specRosterFrom(sessions []poolSession, ts *config.TownSettings) specRoster {
	r := specRoster{Live: map[string]int{}}
	roleDefault := ""
	if ts != nil {
		roleDefault = ts.RoleAgents["polecat"]
	}
	for _, s := range sessions {
		agent := s.agent
		if agent == "" {
			agent = roleDefault
		}
		r.Live[agent]++
		if s.created.After(r.Newest) {
			r.Newest = s.created
		}
	}
	return r
}

// loadSpecDispatchConfig returns daemon.json's patrols.spec_dispatch, or nil.
func loadSpecDispatchConfig(townRoot string) *config.SpecDispatchConfig {
	if townRoot == "" {
		return nil
	}
	if pc := daemon.LoadPatrolConfig(townRoot); pc != nil && pc.Patrols != nil {
		return pc.Patrols.SpecDispatch
	}
	return nil
}

// specTemplatePath is the one resolver for the spec template path: the
// spec_dispatch template field, else the operator's default file. Lint and
// dispatch both use it so they check one shape.
func specTemplatePath(sd *config.SpecDispatchConfig) string {
	if sd != nil && sd.Template != "" {
		return sd.Template
	}
	return specdispatch.DefaultTemplatePath()
}

func runSpecDispatch(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}
	sd := loadSpecDispatchConfig(townRoot)
	tmplPath := specTemplatePath(sd)
	perTick := defaultSpecMaxPerTick
	if sd != nil && sd.MaxPerTick > 0 {
		perTick = sd.MaxPerTick
	}
	if slingActor == "" {
		slingActor = specDispatchActor
	}
	maxPriority := ts.PolecatPool.GetMaxPriority()

	budget := specBudgetFromConfig(ts, sd)
	env := specDispatchEnv{
		Hold: func() string { return dispatch.OperatorHold(townRoot) },
		Candidates: func() specBoardRead {
			return specCandidates(townRoot, maxPriority, specReadyBoard, budget.ReservedLabels())
		},
		Show:     func(id string) (specdispatch.Spec, error) { return showSpec(townRoot, id) },
		Children: func(id string) ([]specdispatch.Child, error) { return specChildren(townRoot, id) },
		RigHold:  func(rig string) string { return dispatch.RigHold(townRoot, rig) },
		RevertInFlight: func(rig string) *specdispatch.Revert {
			return rigRevertInFlightPinned(townRoot, rig)
		},
		Roster: func() (specRoster, error) {
			// The same seat picture `gt daemon dispatch-check` nudges from:
			// live sessions, in-flight claims, and seats mid-landing. Two
			// pictures of one pool is how the dispatcher ends up slinging into
			// a seat the nudge just called free (gt-thy6r).
			sessions, err := poolSeatSessions(newPoolSessionLister(), townRoot, ts.PolecatPool)
			if err != nil {
				return specRoster{}, fmt.Errorf("listing polecat seats: %w", err)
			}
			return specRosterFrom(sessions, ts), nil
		},
		Annotate: func(id, key, text string) error { return annotateSpecOnce(townRoot, id, key, text) },
		AddLabel: func(id, label string) error { return poolBeadLabelAdd(townRoot, id, label) },
		BranchOnOrigin: func(rig, branch string) (bool, error) {
			return polecat.BranchOnOrigin(filepath.Join(townRoot, rig), branch)
		},
		Sling: func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (string, error) {
			return slingSpec(townRoot, c, resumeBranch, seat)
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

// rigRevertInFlightPinned is rigRevertInFlight against the rig's own beads,
// the shape both dispatchers call it in.
func rigRevertInFlightPinned(townRoot, rig string) *specdispatch.Revert {
	return rigRevertInFlight(townRoot, rig, func(beadID string) bool {
		return revertBeadOpen(townRoot, rig, beadID)
	})
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
	Errors        []string
}

// specCandidates reads every operational rig's ready work beads and keeps the
// eligible ones, ordered across rigs. A rig whose board cannot be read is
// reported and skipped; the rest still dispatch. maxPriority is the operator's
// ceiling on a candidate's priority number, and reserved the labels the tick's
// seats reserve — the same pair the per-candidate re-check uses, so a bead the
// seats route is not dropped here before it can reach them.
func specCandidates(townRoot string, maxPriority int, board specBoard, reserved []string) specBoardRead {
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
		if parked, _ := IsRigParkedOrDocked(townRoot, name); parked {
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
			// any decision.
			if ok, _ := specdispatch.Eligible(s, maxPriority, reserved); !ok {
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

// specReadyArgs is the ready query: unassigned work beads, with the
// dispatcher's exclusions and the town's non-dispatchable families filtered
// server-side, unlimited. The retired label spec and type feature are not
// queried on (gt-mmsr2); the non-work types (an epic, the runtime families)
// are excluded instead, so the board holds work beads only.
//
// spec-dispatch-failed is deliberately not among the excluded labels: the
// board must carry the beads it holds so the tick can count them and the
// health line can show the count (gt-q6zoo). Eligible still keeps them out of
// the candidate set.
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
	for _, l := range specdispatch.ExcludedLabels() {
		if strings.EqualFold(l, specdispatch.DispatchFailedLabel) {
			continue
		}
		exclude = append(exclude, l)
	}
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
func annotateSpecOnce(townRoot, beadID, key, text string) error {
	b := beads.New(resolveBeadDirFromTownRoot(townRoot, beadID))
	comments, err := b.Comments(beadID)
	if err != nil {
		return fmt.Errorf("reading comments: %w", err)
	}
	if hasCommentWithPrefix(comments, key) {
		return nil
	}
	return b.AddComment(beadID, text)
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
func specSlingParams(townRoot, beadsDir, formula string, c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) SlingParams {
	return SlingParams{
		BeadID:           c.Spec.ID,
		RigName:          c.Rig,
		FormulaName:      formula,
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
// pool; the pool still enforces its own caps and may refuse.
func slingSpec(townRoot string, c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (string, error) {
	targetBeadsDir, ok := beads.ResolveRepoAliasBeadsDir(townRoot, c.Rig)
	if !ok {
		return "", fmt.Errorf("cannot resolve rig %q beads database for %s", c.Rig, c.Spec.ID)
	}
	params := specSlingParams(townRoot, targetBeadsDir, resolveFormula("", false, townRoot, c.Rig), c, resumeBranch, seat)
	result, err := executeSling(params)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", errors.New("sling returned no result")
	}
	return result.PolecatName, nil
}
