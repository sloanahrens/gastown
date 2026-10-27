package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/deepreview"
	"github.com/steveyegge/gastown/internal/landings"
)

// scheduledDispatchShadow is the entry's dispatch value that runs the
// deep-review plan on the daemon: it plans from the host's landings ledger,
// appends the plan to the run bead's notes and closes the bead. It slings
// nothing, and the run bead carries no formula: label, so a spec dispatcher
// cannot pick it up either (gt-pyxj0).
const scheduledDispatchShadow = "shadow"

// The shadow run's vars. Only the landings plan is ported from
// scripts/deep-review.sh, so mode must say landings; landings_rig names the rig
// whose ledger to read, defaulting to the entry's own rig.
const (
	shadowModeVar        = "mode"
	shadowModeLandings   = "landings"
	shadowLandingsRigVar = "landings_rig"
)

// shadowNotesBeads bounds how many run beads' notes a shadow run reads the
// ledger's cursor from. The cursor is the newest one, and a run is written and
// closed within one tick, so the newest handful always holds it; the bound
// keeps a year of hourly runs from being read into one string.
const shadowNotesBeads = 32

// shadowCloseReason is what a finished shadow run records. The plan is in the
// notes, so the reason carries none of it: the notes are the one place the
// result is written.
const shadowCloseReason = "deep-review shadow run: the plan is in this bead's notes; nothing was slung"

// shadowNoteFailureReason retires a run bead whose plan could not be written.
// It carries the sling-failure marker so decideScheduledSling reads the bead as
// a run that never happened and the next tick retries, instead of reading it as
// a completed run that suppresses the interval.
const shadowNoteFailureReason = scheduledSlingFailureMarker + ": the deep-review shadow run could not write its plan to this bead's notes"

// shadowLedgerPath is the landings file a shadow run reads:
// <town>/.runtime/landings/<rig>.jsonl, where landings_rig overrides the
// entry's rig. landings.Path refuses a rig name that could leave the directory,
// and the error names the var so a bad config says which key to fix.
func shadowLedgerPath(townRoot string, e ScheduledSlingEntry) (string, error) {
	rig := e.Rig
	if v, ok := e.Vars[shadowLandingsRigVar]; ok {
		rig = v
	}
	path, err := landings.Path(townRoot, rig)
	if err != nil {
		return "", fmt.Errorf("scheduled_slings[%s]: vars.%s %q: %w", e.Name, shadowLandingsRigVar, rig, err)
	}
	return path, nil
}

// readLandingsLedger returns the ledger's bytes. A missing or unreadable file
// is an error, never an empty ledger: the plan would call an absent file a
// quiet day, which is the one verdict a review must not give.
func readLandingsLedger(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading landings ledger: %w", err)
	}
	return data, nil
}

// shadowNotes joins the notes of the newest shadowNotesBeads run beads, oldest
// first, so a cursor line's place in the join is its age. ParseCursor finds the
// newest cursor by timestamp, so the join is for reading, not for ordering.
func shadowNotes(runs []scheduledBead) string {
	recent := make([]scheduledBead, len(runs))
	copy(recent, runs)
	sort.SliceStable(recent, func(i, j int) bool {
		return recent[i].CreatedAt.Before(recent[j].CreatedAt)
	})
	if len(recent) > shadowNotesBeads {
		recent = recent[len(recent)-shadowNotesBeads:]
	}
	var b strings.Builder
	for _, run := range recent {
		if run.Notes == "" {
			continue
		}
		b.WriteString(run.Notes)
		// Notes need not end in a newline, and one line glued to the next
		// would stop being a cursor line.
		b.WriteByte('\n')
	}
	return b.String()
}

// shadowPlanNote renders a plan the way scripts/deep-review.sh plan prints it:
// one key=value line per field, then one JSON object per reviewed landing in
// the landings.jsonl shape.
func shadowPlanNote(mode string, p deepreview.Plan) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s\n", mode)
	fmt.Fprintf(&b, "quiet=%t\n", p.Quiet)
	fmt.Fprintf(&b, "landings=%d\n", len(p.Landings))
	fmt.Fprintf(&b, "records=%d\n", p.Records)
	fmt.Fprintf(&b, "unparsed=%d\n", p.Unparsed)
	fmt.Fprintf(&b, "reviewed_until_old=%s\n", p.Since)
	fmt.Fprintf(&b, "reviewed_until_new=%s\n", p.ReviewedUntil)
	for _, rec := range p.Landings {
		line, err := json.Marshal(rec)
		if err != nil {
			return "", fmt.Errorf("rendering landing %s: %w", rec.Bead, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// shadowPlan builds a run's plan from the host's ledger and the entry's vars.
// It reads the rotation-slice plan's var only to refuse it: that plan is not
// ported, and a run that planned the landings under a slice mode would report a
// shape the shell never produced.
func (d *Daemon) shadowPlan(e ScheduledSlingEntry, runs []scheduledBead, now time.Time) (string, deepreview.Plan, error) {
	mode := e.Vars[shadowModeVar]
	if mode != shadowModeLandings {
		return "", deepreview.Plan{}, fmt.Errorf("scheduled_slings[%s]: vars.%s %q is not ported; only %q (the risky-landings plan) exists, the rotation-slice plan is still scripts/deep-review.sh",
			e.Name, shadowModeVar, mode, shadowModeLandings)
	}
	path, err := shadowLedgerPath(d.config.TownRoot, e)
	if err != nil {
		return "", deepreview.Plan{}, err
	}
	ledger, err := readLandingsLedger(path)
	if err != nil {
		return "", deepreview.Plan{}, fmt.Errorf("scheduled_slings[%s]: %w", e.Name, err)
	}
	plan, err := deepreview.BuildPlan(shadowNotes(runs), ledger, now)
	if err != nil {
		return "", deepreview.Plan{}, fmt.Errorf("scheduled_slings[%s]: the landings ledger %s cannot be trusted: %w", e.Name, path, err)
	}
	return mode, plan, nil
}

// runShadowDeepReview is one shadow run: plan the deep review on the host,
// write the plan to the run bead's notes, and stop.
//
// The plan is built before the bead exists, so a ledger the plan cannot trust
// costs no bead. The note is the job's only content write and this bead is the
// only bead it writes: the create carries the entry's identity label and a
// fixed description, the close carries a fixed reason, and both say nothing
// about the plan. Nothing is slung.
//
// A daemon that dies between the create and the note leaves an open bead with
// no plan. The patrol reads it as skip-open until it outlives the sling budget,
// then retires it as a failed run (retireStalledScheduledRuns), so the plan is
// written on a later tick; the plan is idempotent, and a duplicate costs a note
// because nothing was slung.
func (d *Daemon) runShadowDeepReview(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry, runs []scheduledBead, title string, now time.Time) error {
	mode, plan, err := d.shadowPlan(e, runs, now)
	if err != nil {
		return err
	}
	note, err := shadowPlanNote(mode, plan)
	if err != nil {
		return err
	}
	// Planning spent part of the entry's budget on a ledger read and a bead
	// list; the create and the note write start a full one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.scheduledSlingBudget())
	defer cancel()
	id, err := r.createBead(ctx, e.Rig, scheduledRunBead{
		Title:       title,
		Labels:      []string{scheduledSlingLabel(e)},
		Description: shadowRunDescription(e),
		Priority:    scheduledSlingPriority(e),
	})
	if err != nil {
		return err
	}
	if err := r.appendNote(ctx, e.Rig, id, note); err != nil {
		// The bead exists but its plan does not, so it must not read as a
		// completed run: retire it with the failure marker and count the tick
		// as a failure, so the next tick runs the plan again.
		if cerr := d.closeShadowRun(ctx, r, e, id, shadowNoteFailureReason); cerr != nil {
			d.logger.Printf("scheduled_slings: %s: closing shadow run %s after its note failed: %v", e.Name, id, cerr)
		}
		return fmt.Errorf("deep-review shadow run bead %s could not record its plan: %w", id, err)
	}
	if err := d.closeShadowRun(ctx, r, e, id, shadowCloseReason); err != nil {
		// A plan written into a bead that stays open reads as a run in flight
		// until the stale-run recovery retires it, which costs the interval
		// one wait but not the plan.
		return fmt.Errorf("deep-review shadow run bead %s recorded its plan and could not be closed: %w", id, err)
	}
	d.logger.Printf("scheduled_slings: %s: shadow run %s planned %d landing(s) into the bead's notes; nothing was slung", e.Name, id, len(plan.Landings))
	return nil
}

// shadowRunDescription is the run bead's description: what the bead is, with
// nothing about the plan, which lives in the notes.
func shadowRunDescription(e ScheduledSlingEntry) string {
	return fmt.Sprintf("Shadow run of the deep review's %s plan for rig %s, created by the daemon's scheduled_slings patrol. The plan is in this bead's notes. Nothing is slung and no agent runs; label %s is the run's identity.",
		shadowModeLandings, e.Rig, scheduledSlingLabel(e))
}

// closeShadowRun closes a shadow run bead on its own short budget. The sling
// budget can be spent by the time a failure has to be retired, and a close on an
// expired context never runs, which would leave the bead open past its failure.
func (d *Daemon) closeShadowRun(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry, id, reason string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledSlingLookupTimeout)
	defer cancel()
	return r.closeBead(cctx, e.Rig, id, reason)
}
