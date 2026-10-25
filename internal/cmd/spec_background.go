package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Background slings (gt-5h70s).
//
// A sling spawns a worktree and a session and takes from one to five minutes on
// a loaded host, and the tick used to wait for its slowest sling before the
// next tick could start. With --background-slings the tick reads the board,
// plans, and returns; each sling it planned comes back in the report as a
// specSlingJob, and the daemon runs it as `gt spec sling --job`, tracking it in
// its in-flight set. The daemon hands that set back to every later tick
// (--in-flight) so the tick neither slings a running bead again nor hands out a
// seat a running sling is filling.

// specSlingJob is one sling the tick planned: the seat it chose and the
// decisions the sling needs from the tick, which the sling does not read again.
type specSlingJob struct {
	Bead      string                  `json:"bead"`
	Rig       string                  `json:"rig"`
	Agent     string                  `json:"agent"`
	Reason    string                  `json:"reason,omitempty"`
	Resume    string                  `json:"resume,omitempty"`
	Gone      string                  `json:"gone,omitempty"`
	ShapeNote string                  `json:"shape_note,omitempty"`
	Run       specdispatch.FormulaRun `json:"run"`
	// Paths and Tests are the files and test names the bead's text cites, for
	// the overlap hold on the candidates of later ticks while the sling runs.
	Paths []string `json:"paths,omitempty"`
	Tests []string `json:"tests,omitempty"`
}

// specInFlight is a running sling as the daemon hands it to the tick: the
// bead and the seat it occupies, the files it holds, and when it started. A
// sling has no session until it is well under way, so its start is the only
// spawn the roster cannot show (gt-vames). A zero Started is a sling with no
// start time: it still takes its seat.
type specInFlight struct {
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig"`
	Agent   string    `json:"agent"`
	Paths   []string  `json:"paths,omitempty"`
	Tests   []string  `json:"tests,omitempty"`
	Started time.Time `json:"started,omitzero"`
}

var (
	specDispatchBackground bool
	specDispatchInFlight   string
	specSlingJobJSON       string
)

var specSlingCmd = &cobra.Command{
	Use:    "sling",
	Short:  "Run one sling a spec-dispatcher tick planned (run by the daemon)",
	Hidden: true,
	Long: `Run one sling the tick handed back under --background-slings.

The job is the JSON object the tick printed for it. The sling is made as the
tick would have made it, and the report on stdout carries what the tick's
report carries for a sling: the dispatched, skipped or failed entry, and the
label and notes a failed sling owes its bead.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runSpecSling,
}

func init() {
	specDispatchCmd.Flags().BoolVar(&specDispatchBackground, "background-slings", false, "Plan the slings and hand them back in the report instead of running them")
	specDispatchCmd.Flags().StringVar(&specDispatchInFlight, "in-flight", "", "JSON list of the slings still running from earlier ticks ({bead, rig, agent, paths})")
	specSlingCmd.Flags().StringVar(&specSlingJobJSON, "job", "", "The sling job, as the tick's report printed it")
	specCmd.AddCommand(specSlingCmd)
}

// parseSpecInFlight reads the --in-flight value: empty is none.
func parseSpecInFlight(raw string) ([]specInFlight, error) {
	if raw == "" {
		return nil, nil
	}
	var out []specInFlight
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("reading --in-flight: %w", err)
	}
	return out, nil
}

// specLiveWithInFlight is the roster's live count with each running sling added
// to its agent: the sling is filling a seat the roster may not show yet, and a
// seat it already shows is counted twice until the sling returns. That errs
// toward fewer slings, never more, and the sling is the slow part either way.
func specLiveWithInFlight(live map[string]int, inFlight []specInFlight) map[string]int {
	if len(inFlight) == 0 {
		return live
	}
	out := maps.Clone(live)
	if out == nil {
		out = map[string]int{}
	}
	for _, f := range inFlight {
		out[f.Agent]++
	}
	return out
}

// specNewestSpawn is the spawn the min_spawn_gap stagger measures from: the
// newer of the newest polecat the roster shows and the newest running sling's
// start. The roster lacks a sling that has not spawned yet, so without the start
// a tick would plan another sling a moment after the last one (gt-vames).
func specNewestSpawn(rosterNewest time.Time, inFlight []specInFlight) time.Time {
	newest := rosterNewest
	for _, f := range inFlight {
		if f.Started.After(newest) {
			newest = f.Started
		}
	}
	return newest
}

// specHandOffPendingSlings puts the tick's planned slings in the report as
// jobs, and as dispatched entries so the tick's own accounting (the dispatch
// health, the idle run) sees a tick that dispatched. The sling's own report
// follows when the daemon has run it.
func specHandOffPendingSlings(pending []specPendingSling, report *specDispatchReport) {
	for _, p := range pending {
		entry := p.entry
		entry.Line = fmt.Sprintf("%s: sling to %s on %s (%s) started in the background", p.id, p.c.Rig, p.seat.Agent, p.seat.Reason)
		report.Dispatched = append(report.Dispatched, entry)
		report.Pending = append(report.Pending, specSlingJob{
			Bead: p.id, Rig: p.c.Rig, Agent: p.seat.Agent, Reason: p.seat.Reason,
			Resume: p.resume, Gone: p.gone, ShapeNote: p.shapeNote, Run: p.c.Run, Paths: p.paths, Tests: p.tests,
		})
	}
}

// pending is the job as the sling stage runs it.
func (j specSlingJob) pending() specPendingSling {
	spec := specdispatch.Spec{ID: j.Bead}
	return specPendingSling{
		c:         specCandidate{Spec: spec, Rig: j.Rig, Run: j.Run},
		id:        j.Bead,
		seat:      specdispatch.SeatChoice{Agent: j.Agent, Reason: j.Reason},
		resume:    j.Resume,
		gone:      j.Gone,
		shapeNote: j.ShapeNote,
		entry:     specDispatchEntry{Bead: j.Bead, Rig: j.Rig, Agent: j.Agent},
		paths:     j.Paths,
		tests:     j.Tests,
	}
}

// runSpecSlingJob makes one planned sling and reports it as the tick would
// have: through specSlingOne and specApplyOutcome, so a deferral, a failure and
// the label and notes a failure owes are the foreground ones.
func runSpecSlingJob(env specDispatchEnv, job specSlingJob) specDispatchReport {
	report := specDispatchReport{}
	o := specSlingOne(env, job.pending())
	specApplyOutcome(env, &report, o)
	report.SlingsRun = 1
	report.SlowestSling = o.took
	return report
}

// parseSpecSlingJob reads the --job value: the bead, rig and seat are the
// fields a sling cannot be made without.
func parseSpecSlingJob(raw string) (specSlingJob, error) {
	var job specSlingJob
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return job, fmt.Errorf("--job is not a sling job: %w", err)
	}
	var missing []string
	for _, f := range []struct{ name, value string }{{"bead", job.Bead}, {"rig", job.Rig}, {"agent", job.Agent}} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return job, fmt.Errorf("--job is not a sling job: no %s", strings.Join(missing, ", "))
	}
	return job, nil
}

// specSlingHold is the reason a planned sling is not made: a hold set after the
// tick planned it — the town-wide one, the rig's ESTOP, or the rig's dispatch
// hold (dispatch.RigHold) — stops it as it stops the tick.
func specSlingHold(townRoot, rig string) *specDispatchReport {
	if hold := dispatch.RigHold(townRoot, rig); hold != "" {
		return &specDispatchReport{Hold: hold}
	}
	return nil
}

func runSpecSling(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	job, err := parseSpecSlingJob(specSlingJobJSON)
	if err != nil {
		return err
	}
	if slingActor == "" {
		slingActor = specDispatchActor
	}
	traces := &specSlingTraces{}
	env := specDispatchEnv{
		Annotate: func(id, key, text string) error {
			return annotateSpecOnce(beads.New(resolveBeadDirFromTownRoot(townRoot, id)), id, key, text)
		},
		AddLabel: func(id, label string) error { return poolBeadLabelAdd(townRoot, id, label) },
		Sling: func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
			trace := session.NewSandboxTrace(time.Now)
			name, err := slingSpec(townRoot, c, resume, seat, nil, trace)
			traces.add(specSlingTraceLine(c.Spec.ID, seat.Agent, trace, err))
			return name, err
		},
		Sleep: time.Sleep,
		Now:   time.Now,
	}
	if held := specSlingHold(townRoot, job.Rig); held != nil {
		return printSpecSlingReport(cmd, *held)
	}
	report := runSpecSlingJob(env, job)
	report.Notices = append(report.Notices, traces.notices()...)
	return printSpecSlingReport(cmd, report)
}

func printSpecSlingReport(cmd *cobra.Command, report specDispatchReport) error {
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}
