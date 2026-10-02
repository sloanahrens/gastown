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
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Spec dispatcher (gt-4k3fj.5).
//
// `gt spec lint <id>` validates one work bead's shape against the D10 spec
// template and prints one line (--json prints every refusal). `gt spec
// dispatch` is one tick of the dispatcher: every ready, unassigned work bead
// across operational rigs, in priority/created/id order, is linted and — when
// clean and a seat is free — slung through executeSling in-process. The label
// spec and type feature are retired and accepted-but-ignored (gt-mmsr2). The
// daemon's
// spec_dispatch ticker runs `gt spec dispatch --json` on its cadence, the way
// mayor_dispatch runs `gt daemon dispatch-check` (internal/cmd imports
// internal/daemon, so the call cannot go the other way).
//
// Seat accounting counts every live polecat session plus the in-flight seat
// claims other slings hold, whoever slung them. seat-refill nudges and the
// mayor's slings both land in that count, so the ticker never pushes past a
// cap another path already filled — it skips the tick instead. Turn
// seat-refill off when the ticker is the town's only dispatcher, so the roster
// is the operator's to read.

const (
	specDispatchActor       = "daemon/spec-dispatch"
	specDispatchNotePrefix  = "spec-dispatch: "
	defaultSpecHookedAgent  = "claude-sonnet"
	defaultSpecMaxHooked    = 2
	defaultSpecMaxOverflow  = 2
	defaultSpecMaxPerTick   = 1
	specLintExitRefused     = 1
	specLintExitNeedsPlan   = 2
	specDispatchCallerLabel = "spec-dispatch"
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
	Long: `Check a work bead's shape against the spec template
(~/.claude/docs/agents/spec-template.md, or daemon.json
patrols.spec_dispatch.template) — the same lint the spec dispatcher runs before
it allocates a seat:

  - every "## " section of the template present and non-empty
    (Goal, Constraints, Out of scope, Gate, Size)
  - 1-6 acceptance items (3-6 preferred)
  - Size is one worker, one MR

Shape is a property of every work bead: the lint checks a task, bug or feature
the same way and never refuses one for its type. The label spec is retired: it
is accepted and ignored. Epics, agent beads (gt:agent) and wisps are refused
as "not a work bead" without reading a shape.

Prints one line naming the bead and the first missing field. --json prints
{id, ok, needs_planning, refusals[]} instead, listing every failure.

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

Candidates are ready, unassigned, open work beads in every operational rig,
ordered by priority, then created_at, then id. Epics, agent beads, wisps and
the other runtime families are never candidates; the retired label spec and
type feature are accepted and ignored (gt-mmsr2). Beads labeled
gt:ready-to-land, needs-human or needs-mayor-review, or deferred, are never
taken. Each candidate is linted (see gt spec lint):

  - refused: one line, one comment on the bead, never dispatched
  - needs planning: label needs-planning added, one comment, never dispatched
  - clean: slung onto the first free seat within the budget

Every dispatch's sling args tell the polecat to test install paths in a
temporary INSTALL_DIR.

Budget comes from polecat_pool in settings/config.json (overflow_agent,
max_overflow, min_spawn_gap) and patrols.spec_dispatch in mayor/daemon.json
(hooked_agent, max_hooked, prefer_hooked, max_per_tick).
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
	}
}

// showSpec reads one bead in full (bd show), routed by its prefix.
func showSpec(townRoot, beadID string) (specdispatch.Spec, error) {
	issue, err := showBead(townRoot, beadID)
	if err != nil {
		return specdispatch.Spec{}, fmt.Errorf("bead %s not found: %w", beadID, err)
	}
	return specFromIssue(issue), nil
}

func runSpecLint(cmd *cobra.Command, args []string) error {
	townRoot, _ := workspace.FindFromCwd()
	path := specLintTemplate
	if path == "" {
		path = specTemplatePath(loadSpecDispatchConfig(townRoot))
	}
	spec, err := showSpec(townRoot, args[0])
	return specLint(cmd.OutOrStdout(), args[0], spec, err, path, specLintJSON)
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
// the one line; the exit code is the same either way.
func specLint(out io.Writer, beadID string, spec specdispatch.Spec, showErr error, templatePath string, asJSON bool) error {
	report := specLintReport{ID: beadID, Refusals: []specdispatch.Refusal{}}
	if showErr != nil {
		report.Refusals = append(report.Refusals, specdispatch.Refusal{Field: "bead", Reason: showErr.Error()})
		return emitSpecLint(out, report, fmt.Sprintf("%s: spec lint refused: bead: %v", beadID, showErr), specLintExitRefused, asJSON)
	}
	verdict := specdispatch.Lint(spec, specdispatch.LoadTemplate(templatePath))
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
	return emitSpecLint(out, report, verdict.Line(beadID), code, asJSON)
}

func emitSpecLint(out io.Writer, report specLintReport, line string, code int, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
	} else {
		fmt.Fprintln(out, line)
	}
	if code == 0 {
		return nil
	}
	return NewSilentExit(code)
}

// specDispatchReport is one tick's outcome; the daemon logs it.
type specDispatchReport struct {
	Hold       string              `json:"hold,omitempty"`
	Template   string              `json:"template"`
	Roster     string              `json:"roster"`
	Candidates int                 `json:"candidates"`
	Dispatched []specDispatchEntry `json:"dispatched"`
	Refused    []specDispatchEntry `json:"refused"`
	Planning   []specDispatchEntry `json:"planning"`
	Skipped    []specDispatchEntry `json:"skipped"`
	Failed     []specDispatchEntry `json:"failed"`
	Errors     []string            `json:"errors,omitempty"`
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

// specRoster is the live polecat picture the budget is built from: live
// polecats (and in-flight seat claims) per agent, and the newest spawn.
type specRoster struct {
	Live   map[string]int
	Newest time.Time
}

// specDispatchEnv is every side effect a tick has, so the cycle runs on fakes
// in tests.
type specDispatchEnv struct {
	Hold       func() string
	Candidates func() ([]specCandidate, []string)
	Show       func(beadID string) (specdispatch.Spec, error)
	RigHold    func(rig string) string
	Roster     func() (specRoster, error)
	// Annotate adds text as a comment unless the bead already carries a
	// comment starting with key, so each kind of note lands once.
	Annotate func(beadID, key, text string) error
	AddLabel func(beadID, label string) error
	Sling    func(c specCandidate, seat specdispatch.SeatChoice) (polecat string, err error)
	Sleep    func(time.Duration)
	Now      func() time.Time
	Template specdispatch.Template
	Budget   specdispatch.Budget // agents, caps and preferences; live counts filled per tick
	PerTick  int
	DryRun   bool
}

// runSpecDispatchCycle is one tick. It never guesses: a bead the lint refuses
// is annotated and left alone; a bead that needs planning is labeled and
// left for the planner; a clean bead is slung only onto a free seat, and a skip leaves it ready for the next tick.
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

	candidates, errs := env.Candidates()
	report.Errors = append(report.Errors, errs...)
	report.Candidates = len(candidates)
	perTick := env.PerTick
	if perTick <= 0 {
		perTick = defaultSpecMaxPerTick
	}

	for _, c := range candidates {
		id := c.Spec.ID
		full, err := env.Show(id)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		// The ready board is a snapshot; re-check the fresh bead.
		if ok, why := specdispatch.Eligible(full); !ok {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: not eligible (%s)", id, why)})
			continue
		}
		verdict := specdispatch.Lint(full, env.Template)
		line := verdict.Line(id)
		switch verdict.Route {
		case specdispatch.RouteRefuse:
			report.Refused = append(report.Refused, specDispatchEntry{Bead: id, Rig: c.Rig, Line: line})
			if !env.DryRun {
				if err := env.Annotate(id, specDispatchNotePrefix+line, specDispatchNotePrefix+line); err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
				}
			}
			continue
		case specdispatch.RoutePlanning:
			report.Planning = append(report.Planning, specDispatchEntry{Bead: id, Rig: c.Rig, Line: line})
			if !env.DryRun {
				// The planner (gt-4k3fj.7) is not built: route by label and
				// say so once. Nothing is spawned.
				if !full.HasLabel(specdispatch.NeedsPlanningLabel) {
					if err := env.AddLabel(id, specdispatch.NeedsPlanningLabel); err != nil {
						report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", id, err))
					}
				}
				key := fmt.Sprintf("%s%s: routed to the planner", specDispatchNotePrefix, id)
				if err := env.Annotate(id, key, fmt.Sprintf("%s (gt-4k3fj.7), not to a polecat: %s", key, verdict.Reason)); err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
				}
			}
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
		seat := specdispatch.ChooseSeat(budget)
		if seat.Skip {
			report.Skipped = append(report.Skipped, specDispatchEntry{Bead: id, Rig: c.Rig, Line: fmt.Sprintf("%s: no seat: %s", id, seat.Reason)})
			continue
		}
		entry := specDispatchEntry{Bead: id, Rig: c.Rig, Agent: seat.Agent}
		if env.DryRun {
			entry.Line = fmt.Sprintf("%s: would sling to %s on %s (%s)", id, c.Rig, seat.Agent, seat.Reason)
			report.Dispatched = append(report.Dispatched, entry)
			budget.Bump(seat.Agent, budget.Now)
			continue
		}

		var polecat string
		attempts, err := specdispatch.RetryOnContention(specdispatch.RetryAttempts, env.Sleep, rand.New(rand.NewSource(env.Now().UnixNano())), func() error { //nolint:gosec // G404: backoff jitter
			var slingErr error
			polecat, slingErr = env.Sling(c, seat)
			return slingErr
		})
		if err != nil {
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
			failLine := fmt.Sprintf("%s: dispatch failed, left unassigned: %s", id, why)
			entry.Line = failLine
			report.Failed = append(report.Failed, entry)
			// The label takes the bead out of the candidate set, so a sling
			// that fails for a reason other than capacity does not spawn and
			// roll back a polecat every tick. Removing it retries.
			if err := env.AddLabel(id, specdispatch.DispatchFailedLabel); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: label: %v", id, err))
			}
			key := fmt.Sprintf("%s%s: dispatch failed", specDispatchNotePrefix, id)
			text := fmt.Sprintf("%s%s (remove label %s to retry)", specDispatchNotePrefix, failLine, specdispatch.DispatchFailedLabel)
			if err := env.Annotate(id, key, text); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: annotate: %v", id, err))
			}
			continue
		}
		entry.Polecat = polecat
		entry.Line = fmt.Sprintf("%s: slung to %s/%s on %s (%s)", id, c.Rig, polecat, seat.Agent, seat.Reason)
		if attempts > 1 {
			entry.Line += fmt.Sprintf(" after %d attempts", attempts)
		}
		report.Dispatched = append(report.Dispatched, entry)
		budget.Bump(seat.Agent, budget.Now)
	}
	return report
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
//   - the pool's overflow_agent, capped at max_overflow (default 2 when unset)
//   - hooked_agent (default claude-sonnet), capped at max_hooked (default 2)
//
// Order is the list above, reversed under prefer_hooked. An agent named twice
// keeps its first seat.
func specBudgetFromConfig(ts *config.TownSettings, sd *config.SpecDispatchConfig) specdispatch.Budget {
	hookedAgent, hookedCap := defaultSpecHookedAgent, defaultSpecMaxHooked
	prefer := false
	if sd != nil {
		if sd.HookedAgent != "" {
			hookedAgent = sd.HookedAgent
		}
		switch {
		case sd.MaxHooked < 0:
			hookedCap = 0
		case sd.MaxHooked > 0:
			hookedCap = sd.MaxHooked
		}
		prefer = sd.PreferHooked
	}

	var b specdispatch.Budget
	var overflow *specdispatch.Seat
	if ts != nil && ts.PolecatPool != nil && ts.PolecatPool.OverflowAgent != "" {
		pool := ts.PolecatPool
		capacity := pool.MaxOverflow
		if capacity <= 0 {
			capacity = defaultSpecMaxOverflow
		}
		overflow = &specdispatch.Seat{Agent: pool.OverflowAgent, Cap: capacity}
	}
	if ts != nil && ts.PolecatPool != nil {
		b.MinSpawnGap = ts.PolecatPool.MinSpawnGapD()
	}
	hooked := &specdispatch.Seat{Agent: hookedAgent, Cap: hookedCap}
	order := []*specdispatch.Seat{overflow, hooked}
	if prefer {
		order = []*specdispatch.Seat{hooked, overflow}
	}
	seen := map[string]bool{}
	for _, seat := range order {
		if seat == nil || seen[seat.Agent] {
			continue
		}
		seen[seat.Agent] = true
		b.Seats = append(b.Seats, *seat)
	}
	return b
}

// specRosterFrom counts live polecats (and in-flight seat claims) per agent.
// An empty GT_AGENT runs the polecat role default.
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

	env := specDispatchEnv{
		Hold:       func() string { return dispatch.OperatorHold(townRoot) },
		Candidates: func() ([]specCandidate, []string) { return specCandidates(townRoot) },
		Show:       func(id string) (specdispatch.Spec, error) { return showSpec(townRoot, id) },
		RigHold:    func(rig string) string { return dispatch.RigHold(townRoot, rig) },
		Roster: func() (specRoster, error) {
			sessions, err := listPolecatSessions(newPoolSessionLister(), townRoot)
			if err != nil {
				return specRoster{}, fmt.Errorf("listing polecat sessions: %w", err)
			}
			claims, err := poolSeatClaimSessions(townRoot, "")
			if err != nil {
				return specRoster{}, fmt.Errorf("reading seat claims: %w", err)
			}
			return specRosterFrom(append(sessions, claims...), ts), nil
		},
		Annotate: func(id, key, text string) error { return annotateSpecOnce(townRoot, id, key, text) },
		AddLabel: func(id, label string) error { return poolBeadLabelAdd(townRoot, id, label) },
		Sling: func(c specCandidate, seat specdispatch.SeatChoice) (string, error) {
			return slingSpec(townRoot, c, seat)
		},
		Sleep:    time.Sleep,
		Now:      time.Now,
		Template: specdispatch.LoadTemplate(tmplPath),
		Budget:   specBudgetFromConfig(ts, sd),
		PerTick:  perTick,
		DryRun:   specDispatchDryRun,
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
	fmt.Fprintf(w, "spec dispatch: %d candidate(s); roster %s; template %s\n", r.Candidates, r.Roster, r.Template)
	for _, group := range []struct {
		name    string
		entries []specDispatchEntry
	}{{"dispatched", r.Dispatched}, {"refused", r.Refused}, {"planning", r.Planning}, {"skipped", r.Skipped}, {"failed", r.Failed}} {
		for _, e := range group.entries {
			fmt.Fprintf(w, "  %-10s %s\n", group.name, e.Line)
		}
	}
	for _, e := range r.Errors {
		fmt.Fprintf(w, "  error      %s\n", e)
	}
}

// specCandidates reads every operational rig's ready work beads and keeps the
// eligible ones, ordered across rigs. A rig whose board cannot be read is
// reported and skipped; the rest still dispatch.
func specCandidates(townRoot string) ([]specCandidate, []string) {
	names, err := knownRigNames(townRoot)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var errs []string
	var specs []specdispatch.Spec
	rigOf := map[string]string{}
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
		issues, err := specReadyBoard(rigPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		for _, issue := range issues {
			s := specFromIssue(issue)
			// The ready board is a snapshot: the full bead is re-read before
			// any decision.
			if ok, _ := specdispatch.Eligible(s); !ok {
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
	out := make([]specCandidate, 0, len(specs))
	for _, s := range specs {
		out = append(out, specCandidate{Spec: s, Rig: rigOf[s.ID]})
	}
	return out, errs
}

// specReadyArgs is the ready query: unassigned work beads, with the
// dispatcher's exclusions and the town's non-dispatchable families filtered
// server-side, unlimited. The retired label spec and type feature are not
// queried on (gt-mmsr2); the non-work types (an epic, the runtime families)
// are excluded instead, so the board holds work beads only.
func specReadyArgs() []string {
	exclude := append(append([]string(nil), constants.NonDispatchableBeadLabels...), specdispatch.ExcludedLabels()...)
	return []string{
		"ready", "--json",
		"--unassigned",
		"--exclude-label", strings.Join(exclude, ","),
		"--exclude-type", strings.Join(specdispatch.NonWorkBeadTypes(), ","),
		"--limit", "0",
	}
}

// specReadyBoard runs the ready query in one rig. A var so tests can serve a
// board without a live bd.
var specReadyBoard = func(rigPath string) ([]*beads.Issue, error) {
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

// specSlingParams is the sling a spec dispatch makes. The agent is explicit;
// there is no auto-convoy, so a failed dispatch
// leaves the bead unassigned for the next tick rather than handing it to a
// convoy re-feed loop; and every dispatch carries the host-safety instruction.
func specSlingParams(townRoot, beadsDir, formula string, c specCandidate, seat specdispatch.SeatChoice) SlingParams {
	return SlingParams{
		BeadID:           c.Spec.ID,
		RigName:          c.Rig,
		FormulaName:      formula,
		Agent:            seat.Agent,
		Args:             specdispatch.HostSafetyPrompt,
		FormulaFailFatal: true,
		NoConvoy:         true,
		CallerContext:    specDispatchCallerLabel,
		NoBoot:           true,
		TownRoot:         townRoot,
		BeadsDir:         beadsDir,
	}
}

// slingSpec dispatches one clean spec through the shared rig-dispatch path.
// The agent is explicit, so it wins over the bead's route:* labels in the
// pool; the pool still enforces its own caps and may refuse.
func slingSpec(townRoot string, c specCandidate, seat specdispatch.SeatChoice) (string, error) {
	targetBeadsDir, ok := beads.ResolveRepoAliasBeadsDir(townRoot, c.Rig)
	if !ok {
		return "", fmt.Errorf("cannot resolve rig %q beads database for %s", c.Rig, c.Spec.ID)
	}
	params := specSlingParams(townRoot, targetBeadsDir, resolveFormula("", false, townRoot, c.Rig), c, seat)
	result, err := executeSling(params)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", errors.New("sling returned no result")
	}
	return result.PolecatName, nil
}
