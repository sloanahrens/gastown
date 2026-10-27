package daemon

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
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
	// shadowSliceFilesVar and shadowSliceBytesVar bound the landings one run
	// reviews, the way the shell's --slice and --slice-bytes do. Unset, they
	// are deepreview's defaults.
	shadowSliceFilesVar = "slice_files"
	shadowSliceBytesVar = "slice_bytes"
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

// shadowSlice is the bounded set of landings one run reviews, and the cursor
// the next run starts from.
type shadowSlice struct {
	// Lines are the reviewed landings as the note carries them, one
	// landings.jsonl object each, in plan order.
	Lines []string
	// Skipped counts the plan's landings the bound left for the next run.
	Skipped int
	// Until is the next cursor: the last reviewed landing's landed_at, or the
	// plan's window start when the run reviewed none.
	Until deepreview.Timestamp
}

// shadowSliceBounds returns the run's row and byte bounds: deepreview's
// defaults, overridden by the entry's slice_files and slice_bytes vars.
//
// A bound that does not parse or is not positive is refused here rather than
// defaulted: a run that silently reviewed everything, or nothing, under a typo
// would move the cursor on a mistake.
func shadowSliceBounds(e ScheduledSlingEntry) (maxRows, maxBytes int, err error) {
	if maxRows, err = shadowSliceBound(e, shadowSliceFilesVar, deepreview.DefaultSliceFiles); err != nil {
		return 0, 0, err
	}
	maxBytes, err = shadowSliceBound(e, shadowSliceBytesVar, deepreview.DefaultSliceBytes)
	return maxRows, maxBytes, err
}

// shadowSliceBound is one bound: the var's value, or def when the entry sets
// none.
func shadowSliceBound(e ScheduledSlingEntry, key string, def int) (int, error) {
	raw, ok := e.Vars[key]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	// Atoi answers ErrRange with the largest int, so the error is refused
	// rather than read: a bound past the range is a typo, not a wide slice.
	if err != nil || n < 1 {
		return 0, fmt.Errorf("scheduled_slings[%s]: vars.%s %q must be a positive whole number", e.Name, key, raw)
	}
	return n, nil
}

// boundLandings takes the landings one run reviews: whole groups of landings
// that share one landed_at, in plan order, while they fit the row and byte
// bounds.
//
// The cut is by group, not by row, because the next run's window starts
// strictly after the cursor's instant: a landing left behind at that instant
// would never be reviewed. deepreview.BuildSlice cuts row by row, so it cannot
// make this cut. A group that would cross the bound is left whole for the next
// run, and the head group is taken whole even past the bound, since a run that
// dropped it would review nothing and never move the cursor.
func boundLandings(landings []deepreview.Record, maxRows, maxBytes int, render func(deepreview.Record) (string, error)) ([]string, error) {
	var (
		lines []string
		size  int
	)
	for i := 0; i < len(landings); {
		end := i + 1
		for end < len(landings) && landings[end].LandedAt.Time.Equal(landings[i].LandedAt.Time) {
			end++
		}
		group := make([]string, 0, end-i)
		groupBytes := 0
		for _, rec := range landings[i:end] {
			line, err := render(rec)
			if err != nil {
				return nil, fmt.Errorf("rendering landing %s: %w", rec.Bead, err)
			}
			group = append(group, line)
			groupBytes += len(line)
		}
		if len(lines) > 0 && (len(lines)+len(group) > maxRows || size+groupBytes > maxBytes) {
			break
		}
		lines = append(lines, group...)
		size += groupBytes
		i = end
	}
	return lines, nil
}

// shadowSliceOf cuts the plan's landings at the run's bounds and places the
// cursor on the last one reviewed.
func (d *Daemon) shadowSliceOf(plan deepreview.Plan, maxRows, maxBytes int) (shadowSlice, error) {
	lines, err := boundLandings(plan.Landings, maxRows, maxBytes, func(rec deepreview.Record) (string, error) {
		line, err := d.seams.encodeShadowLanding(rec)
		return string(line), err
	})
	if err != nil {
		return shadowSlice{}, err
	}
	s := shadowSlice{Lines: lines, Skipped: len(plan.Landings) - len(lines), Until: plan.Since}
	if len(lines) > 0 {
		s.Until = plan.Landings[len(lines)-1].LandedAt
	}
	return s, nil
}

// shadowPlanNote renders a run's note: the plan's key=value lines as
// scripts/deep-review.sh plan prints them, the reviewed landings in the
// landings.jsonl shape, and the cursor line last.
//
// landings= and reviewed_until_new= describe what this run reviewed, not what
// the ledger held, so a bounded run reports its own slice and skipped= counts
// the landings it left for the next run.
//
// A run that reviewed nothing writes no cursor line, as the shell writes none
// on a quiet day: the line would pin the default window at its start and a
// quiet week would put a week of landings in one run's window.
//
// The cursor line is last and in the same append as the rows, so a note read
// back can never carry a cursor past rows that did not land; the bead is
// created first (see runShadowDeepReview), which leaves a crash between the two
// re-reading the same landings instead of skipping them.
func shadowPlanNote(mode string, plan deepreview.Plan, s shadowSlice) string {
	lines := []string{
		"mode=" + mode,
		fmt.Sprintf("quiet=%t", plan.Quiet),
		fmt.Sprintf("landings=%d", len(s.Lines)),
		fmt.Sprintf("skipped=%d", s.Skipped),
		fmt.Sprintf("records=%d", plan.Records),
		fmt.Sprintf("unparsed=%d", plan.Unparsed),
		"reviewed_until_old=" + plan.Since.String(),
		"reviewed_until_new=" + s.Until.String(),
	}
	lines = append(lines, s.Lines...)
	if len(s.Lines) > 0 {
		lines = append(lines, "cursor: "+s.Until.String())
	}
	return strings.Join(lines, "\n")
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
// write the slice it reviews and the cursor it moves to into the run bead's
// notes, and stop.
//
// The plan and the note are built before the bead exists, so a ledger the plan
// cannot trust, a bound that does not parse and a landing the encoder cannot
// render all cost no bead. The note is the job's only content write and this
// bead is the only bead it writes: the create carries the entry's identity
// label and a fixed description, the close carries a fixed reason, and both say
// nothing about the plan. Nothing is slung.
//
// The cursor lives only in the bead's notes, and the bead is created before the
// notes are appended, so a daemon that dies between the two moves no cursor:
// the next run reads the same window and reviews the same landings again. The
// plan is idempotent and a duplicate costs a note because nothing was slung,
// where the other order would skip a run's landings for good.
func (d *Daemon) runShadowDeepReview(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry, runs []scheduledBead, title string, now time.Time) error {
	maxRows, maxBytes, err := shadowSliceBounds(e)
	if err != nil {
		return err
	}
	mode, plan, err := d.shadowPlan(e, runs, now)
	if err != nil {
		return err
	}
	s, err := d.shadowSliceOf(plan, maxRows, maxBytes)
	if err != nil {
		return err
	}
	note := shadowPlanNote(mode, plan, s)
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
	if len(s.Lines) == 0 {
		// A run that reviewed nothing writes no cursor line (see
		// shadowPlanNote), so the next run reads the same window. The log
		// must not claim the move the note did not make (gt-80jmz).
		d.logger.Printf("scheduled_slings: %s: shadow run %s reviewed no landings and moved no cursor; nothing was slung", e.Name, id)
		return nil
	}
	d.logger.Printf("scheduled_slings: %s: shadow run %s reviewed %d landing(s) with %d left for the next run, and moved the cursor to %s; nothing was slung", e.Name, id, len(s.Lines), s.Skipped, s.Until)
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
