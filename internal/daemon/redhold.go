package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// redHoldSweepCycles is the consecutive red sweep cycles on one rig that hold dispatch.
const redHoldSweepCycles = 2

// redHoldBehind is the number of landings not in the installed build that holds
// dispatch: the deployed daemon has drifted from main.
const redHoldBehind = 100

// redHoldGreenStale is how long a swept rig may go without a fully green cycle
// before dispatch is held.
const redHoldGreenStale = 3 * time.Hour

// redHoldSweep is one rig's sweep history, and whether its last result was unread.
type redHoldSweep struct {
	Rig       string
	RedCycles int
	NoResult  bool
}

// redHoldInputs is what the triggers decide from.
type redHoldInputs struct {
	Sweeps []redHoldSweep
	// Waiting is the landings on main that are not in the installed build.
	Waiting int
	// OldestGreenAt is the oldest LastGreenAt across the swept rigs; the zero
	// time means unknown and triggers nothing.
	OldestGreenAt time.Time
	// UpdateOutcome is the last update run's outcome; "", "ok" and "running"
	// are healthy and anything else holds as roll-failed.
	UpdateOutcome string
	// UpdateTarget is the commit the update run rolled to.
	UpdateTarget string
	// ImagesRetries is the failed images builds recorded for one green sha.
	ImagesRetries int
	// PairStuck is the green shas whose image pair never became ready.
	PairStuck []string
}

// redHoldSweepOf converts one rig's sweep record into the evaluator's input; ok
// is false for an unknown record, which the caller leaves out to keep its prior
// state. A RED tier is no-result when the record names tierSweepRunnerNoResult
// as its cause or as its failing name.
func redHoldSweepOf(rig string, st tierSweepState, err error) (redHoldSweep, bool) {
	if err != nil {
		return redHoldSweep{}, false
	}
	s := redHoldSweep{Rig: rig, RedCycles: st.RedCycles}
	for _, tier := range st.Tiers {
		if tier.Verdict != tierSweepRed {
			continue
		}
		if tier.Cause == tierSweepRunnerNoResult || tierSweepNoRunResult(tier) {
			s.NoResult = true
			break
		}
	}
	return s, true
}

// redHoldSources is where redHoldInputsOf reads the rigs' sweep records: the rig
// names in read order, the reader for one name, and the clock for a new Since.
type redHoldSources struct {
	Rigs []string
	Read func(rig string) (tierSweepState, error)
	Now  func() time.Time // nil means time.Now
}

// redHoldInputsOf converts every rig's sweep record into the evaluator's inputs.
// A rig whose record could not be read is left out of the inputs, keeping its
// prior state, and named in the second value in Rigs order so the caller can log
// it.
func redHoldInputsOf(src redHoldSources) (redHoldInputs, []string) {
	var in redHoldInputs
	var unknown []string
	for _, rig := range src.Rigs {
		st, err := src.Read(rig)
		s, ok := redHoldSweepOf(rig, st, err)
		if !ok {
			unknown = append(unknown, rig)
			continue
		}
		in.Sweeps = append(in.Sweeps, s)
	}
	return in, unknown
}

// evalRedHold returns the reasons that hold dispatch: one per kind, sorted by
// kind, each keeping prev's Since when the kind already held and taking now when
// new. A sweep under redHoldSweepCycles contributes nothing; at or above it is
// sweep-no-result when unread, sweep-red otherwise, with Detail naming every rig.
// Waiting at or above redHoldBehind is behind-main; an OldestGreenAt older than
// redHoldGreenStale is green-stale, and the zero time is unknown and adds nothing.
// An UpdateOutcome other than "", "ok" or "running" is roll-failed; ImagesRetries
// at or above tierSweepImagesRetryMax is images-failed; any PairStuck sha is
// pair-stuck.
func evalRedHold(in redHoldInputs, now time.Time, prev dispatch.RedHoldRecord) dispatch.RedHoldRecord {
	details := map[string][]string{}
	for _, s := range in.Sweeps {
		if s.RedCycles < redHoldSweepCycles {
			continue
		}
		kind := dispatch.RedHoldSweepRed
		if s.NoResult {
			kind = dispatch.RedHoldSweepNoResult
		}
		details[kind] = append(details[kind], fmt.Sprintf("%s: %d red cycles", s.Rig, s.RedCycles))
	}
	if in.Waiting >= redHoldBehind {
		details[dispatch.RedHoldBehindMain] = []string{fmt.Sprintf("%d landings behind main", in.Waiting)}
	}
	if age := now.Sub(in.OldestGreenAt); !in.OldestGreenAt.IsZero() && age > redHoldGreenStale {
		details[dispatch.RedHoldGreenStale] = []string{fmt.Sprintf("no green sweep for %dh", int(age.Hours()))}
	}
	if o := in.UpdateOutcome; o != "" && o != "ok" && o != "running" {
		details[dispatch.RedHoldRollFailed] = []string{fmt.Sprintf("update %s at %s", o, redHoldShort(in.UpdateTarget))}
	}
	if in.ImagesRetries >= tierSweepImagesRetryMax {
		details[dispatch.RedHoldImagesFailed] = []string{fmt.Sprintf("images build failed %d times", in.ImagesRetries)}
	}
	if len(in.PairStuck) > 0 {
		shas := make([]string, len(in.PairStuck))
		for i, sha := range in.PairStuck {
			shas[i] = redHoldShort(sha)
		}
		details[dispatch.RedHoldPairStuck] = []string{"stuck pair: " + strings.Join(shas, ", ")}
	}
	var reasons []dispatch.RedHoldReason
	for _, kind := range slices.Sorted(maps.Keys(details)) {
		since := now
		for _, r := range prev.Reasons {
			if r.Kind == kind {
				since = r.Since
				break
			}
		}
		reasons = append(reasons, dispatch.RedHoldReason{Kind: kind, Since: since, Detail: strings.Join(details[kind], ", ")})
	}
	return dispatch.RedHoldRecord{Reasons: reasons}
}

// redHoldShort returns the first 12 characters of a commit id, or the whole id
// when it is shorter.
func redHoldShort(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// runRedHold evaluates the sweep sources and publishes the red hold, writing the
// record and logging the change once. An unreadable sweep record leaves the file
// untouched, so it never clears a hold, and is logged once per rig until that
// rig reads again; unreadLog holds those rigs across heartbeats and is non-nil.
func runRedHold(townRoot string, src redHoldSources, unreadLog map[string]bool, logf func(format string, args ...any)) error {
	in, unknown := redHoldInputsOf(src)
	prev, err := dispatch.ReadRedHold(townRoot)
	if err != nil {
		return fmt.Errorf("read red hold: %w", err)
	}
	// Forget the rigs that read again, so a later outage logs anew.
	maps.DeleteFunc(unreadLog, func(rig string, _ bool) bool { return !slices.Contains(unknown, rig) })
	if len(unknown) > 0 {
		for _, rig := range unknown {
			if !unreadLog[rig] {
				unreadLog[rig] = true
				logf("red hold: sweep record unreadable for %s; keeping the previous state", rig)
			}
		}
		return nil
	}
	now := src.Now
	if now == nil {
		now = time.Now
	}
	next := evalRedHold(in, now(), prev)
	line := redHoldChangeLine(prev, next)
	if line == "" {
		return nil
	}
	if err := dispatch.WriteRedHold(townRoot, next); err != nil {
		return fmt.Errorf("write red hold: %w", err)
	}
	logf("%s", line)
	return nil
}

// redHoldChangeLine says what the daemon logs when the red hold changes: "" when
// the kind set is unchanged, 'red hold: set: <sorted kinds>' when next holds a
// different set, and 'red hold: cleared' when none remain. Since and Detail do
// not count, so a reason that merely refreshed them logs nothing.
func redHoldChangeLine(prev, next dispatch.RedHoldRecord) string {
	prevKinds, nextKinds := redHoldKinds(prev), redHoldKinds(next)
	if slices.Equal(prevKinds, nextKinds) {
		return ""
	}
	if len(nextKinds) == 0 {
		return "red hold: cleared"
	}
	return "red hold: set: " + strings.Join(nextKinds, ", ")
}

// redHoldKinds returns the record's distinct kinds, sorted.
func redHoldKinds(rec dispatch.RedHoldRecord) []string {
	seen := make(map[string]bool, len(rec.Reasons))
	for _, r := range rec.Reasons {
		seen[r.Kind] = true
	}
	return slices.Sorted(maps.Keys(seen))
}
