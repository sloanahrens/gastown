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
	"github.com/steveyegge/gastown/internal/imagedrift"
	"github.com/steveyegge/gastown/internal/installpending"
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
	Bead      string `json:"bead"`
	Rig       string `json:"rig"`
	Agent     string `json:"agent"`
	Reason    string `json:"reason,omitempty"`
	Resume    string `json:"resume,omitempty"`
	Gone      string `json:"gone,omitempty"`
	ShapeNote string `json:"shape_note,omitempty"`
	// OverrunNote is the overrun-stop flag the sling writes on its bead once it
	// has dispatched it (gt-b9j3o.4): the tick that planned the sling decided
	// the box, and the job carries the answer to the process that runs it.
	OverrunNote string                  `json:"overrun_note,omitempty"`
	Run         specdispatch.FormulaRun `json:"run"`
}

// specInFlight is a running sling as the daemon hands it to the tick: the
// bead and the seat it occupies, and when it started. A sling has no session
// until it is well under way, so its start is the only spawn the roster cannot
// show (gt-vames). A zero Started is a sling with no start time: it still takes
// its seat.
type specInFlight struct {
	Bead    string    `json:"bead"`
	Rig     string    `json:"rig"`
	Agent   string    `json:"agent"`
	Started time.Time `json:"started,omitzero"`
}

var (
	specDispatchBackground bool
	specDispatchInFlight   string
	specSlingJobJSON       string
	// specSlingCensusPath is the --census-path the daemon passes: where this
	// sling writes the bd calls it made, for the daemon to fold into its census
	// line. Empty is a sling run by hand, which reports to no one (gt-3y319).
	specSlingCensusPath string
	// specSlingDetailCacheOwner is the daemon the sling's duplicate check reads
	// the design/notes cache for: the daemon passes the owner it named its
	// ticks with, since the tick that planned the sling is the one that filled
	// the cache (gt-nebs9).
	specSlingDetailCacheOwner string
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
	specDispatchCmd.Flags().StringVar(&specDispatchInFlight, "in-flight", "", "JSON list of the slings still running from earlier ticks ({bead, rig, agent})")
	specSlingCmd.Flags().StringVar(&specSlingJobJSON, "job", "", "The sling job, as the tick's report printed it")
	specSlingCmd.Flags().StringVar(&specSlingCensusPath, "census-path", "", "Write the sling's bd census rows to this file at exit")
	specSlingCmd.Flags().StringVar(&specSlingDetailCacheOwner, "detail-cache-owner", "",
		"The daemon whose design/notes cache the duplicate check reads (the daemon sets it)")
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
func specHandOffPendingSlings(env specDispatchEnv, pending []specPendingSling, report *specDispatchReport) {
	for _, p := range pending {
		entry := p.entry
		// A sling refused for the lagging image a moment ago holds the ones
		// behind it: no job process is started for them (gt-uisom).
		if held, line := env.imageDriftSkip(p.id, p.seat.Agent); held {
			entry.Line = line
			specApplyOutcome(env, report, specSlingOutcome{kind: specSlingSkipped, entry: entry})
			continue
		}
		entry.Line = fmt.Sprintf("%s: sling to %s on %s (%s) started in the background", p.id, p.c.Rig, p.seat.Agent, p.seat.Reason)
		report.Dispatched = append(report.Dispatched, entry)
		report.Pending = append(report.Pending, specSlingJob{
			Bead: p.id, Rig: p.c.Rig, Agent: p.seat.Agent, Reason: p.seat.Reason,
			Resume: p.resume, Gone: p.gone, ShapeNote: p.shapeNote, OverrunNote: p.overrunNote, Run: p.c.Run,
		})
	}
}

// pending is the job as the sling stage runs it.
func (j specSlingJob) pending() specPendingSling {
	spec := specdispatch.Spec{ID: j.Bead}
	return specPendingSling{
		c:           specCandidate{Spec: spec, Rig: j.Rig, Run: j.Run},
		id:          j.Bead,
		seat:        specdispatch.SeatChoice{Agent: j.Agent, Reason: j.Reason},
		resume:      j.Resume,
		gone:        j.Gone,
		shapeNote:   j.ShapeNote,
		overrunNote: j.OverrunNote,
		entry:       specDispatchEntry{Bead: j.Bead, Rig: j.Rig, Agent: j.Agent},
	}
}

// runSpecSlingJob makes one planned sling and reports it as the tick would
// have: through specSlingOne and specApplyOutcome, so a deferral, a failure and
// the label and notes a failure owes are the foreground ones.
func runSpecSlingJob(env specDispatchEnv, job specSlingJob) specDispatchReport {
	report := specDispatchReport{}
	// An install that started after the tick planned this job stops it, as the
	// holds specSlingHold reads do (gt-2rp64).
	if hold, errLine := env.installPendingStop(); hold != "" || errLine != "" {
		report.Hold = hold
		if errLine != "" {
			report.Errors = append(report.Errors, errLine)
		}
		return report
	}
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
	return runSpecSlingInTown(cmd, townRoot, specSlingJobJSON, specSlingCensusPath, beads.CensusSnapshotAndReset)
}

// runSpecSlingInTown runs the sling job describes and writes the bd calls it
// made to censusPath, the per-run file the daemon folds into its census line.
// The census source is a parameter so a test hands rows without a bd subprocess
// (gt-3y319).
func runSpecSlingInTown(cmd *cobra.Command, townRoot, jobJSON, censusPath string, census func() []beads.CensusRow) error {
	job, err := parseSpecSlingJob(jobJSON)
	if err != nil {
		return err
	}
	report := specSlingReport(townRoot, job)
	// The census is written whatever the sling decided, held or made, since the
	// bd calls it made are real either way. A write that fails costs the census
	// and never the sling: the report carries it once (gt-3y319).
	if censusErr := specWriteSlingCensus(censusPath, census); censusErr != nil {
		report.Errors = append(report.Errors, "bd census rows not written: "+censusErr.Error())
	}
	return printSpecSlingReport(cmd, report)
}

// specSlingReport makes the sling a job describes and returns its report. A
// hold that covers the rig stops it before the sling, as it stops the tick.
func specSlingReport(townRoot string, job specSlingJob) specDispatchReport {
	if held := specSlingHold(townRoot, job.Rig); held != nil {
		return *held
	}
	if slingActor == "" {
		slingActor = specDispatchActor
	}
	traces := &specSlingTraces{}
	env := specSlingEnv(townRoot, specSlingDetailCacheOwner, traces)
	report := runSpecSlingJob(env, job)
	report.Notices = append(report.Notices, traces.notices()...)
	return report
}

// specSlingEnv is the env a `gt spec sling` child runs its job with, and where
// the child becomes a reader of the tick's design/notes cache: cacheOwner is
// the daemon whose tick wrote the file, so the duplicate check reads it instead
// of showing every pool bead itself (gt-nebs9). An empty owner is a child no
// daemon started, which reads no cache.
func specSlingEnv(townRoot, cacheOwner string, traces *specSlingTraces) specDispatchEnv {
	specDetailCache.Store(specSlingDetailCacheFor(townRoot, cacheOwner))
	return specDispatchEnv{
		Annotate: func(id, key, text string) error {
			return annotateSpecOnce(beads.New(resolveBeadDirFromTownRoot(townRoot, id)), id, key, text)
		},
		AddLabel: func(id, label string) error { return poolBeadLabelAdd(townRoot, id, label) },
		Sling: func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
			trace := session.NewSandboxTrace(time.Now)
			name, err := slingSpec(townRoot, c, resume, seat, nil, trace, newRigDockedMemo(nil))
			traces.add(specSlingTraceLine(c.Spec.ID, seat.Agent, trace, err))
			return name, err
		},
		Sleep: time.Sleep,
		Now:   time.Now,
		PendingInstall: func(now time.Time) (*installpending.Marker, error) {
			return installpending.StoreFor(townRoot).Pending(now)
		},
		Drift: imagedrift.StoreFor(townRoot),
	}
}

// specWriteSlingCensus writes the rows census hands to path, the per-run file
// the daemon named, for the daemon to fold into its census line. A sling run by
// hand has no path and writes nothing (gt-3y319).
func specWriteSlingCensus(path string, census func() []beads.CensusRow) error {
	if path == "" {
		return nil
	}
	return beads.WriteCensusSink(path, census())
}

func printSpecSlingReport(cmd *cobra.Command, report specDispatchReport) error {
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}
