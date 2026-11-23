package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
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
	// WaitingUnread marks the count unread: behind-main keeps its previous
	// reason and every other trigger still reads.
	WaitingUnread bool
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
	// MainRed is the rigs whose main is red; the caller reads it.
	MainRed []string
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

// redHoldWaitingKey keys unreadLog for the waiting count; no rig directory name can be it.
const redHoldWaitingKey = "\x00waiting"

// redHoldUpdateKey keys unreadLog for the update outcome; no rig directory name can be it.
const redHoldUpdateKey = "\x00update"

// redHoldMainRedKey keys unreadLog for the main-red read; no rig directory name can be it.
const redHoldMainRedKey = "\x00mainred"

// redHoldSources is where redHoldInputsOf reads the rigs' sweep records: the rig
// names in read order, the reader for one name, the waiter for the count of
// landings not in the installed build, the reader for the update run's outcome
// and target commit, the stuck pairs, the main-red reader, and the clock. A nil
// Waiting, Update, PairStuck or MainRed contributes nothing.
type redHoldSources struct {
	Rigs      []string
	Read      func(rig string) (tierSweepState, error)
	Waiting   func() (int, error)                        // nil means no count
	Update    func() (outcome, target string, err error) // nil means no outcome
	PairStuck func() []string                            // nil means no pairs
	MainRed   func(rig string) (bool, error)             // nil means no main-red read
	Now       func() time.Time                           // nil means time.Now
}

// redHoldInputsOf converts every rig's sweep record into the evaluator's inputs.
// A rig whose record could not be read is left out of the inputs, keeping its
// prior state, and named in the second value in Rigs order so the caller can log
// it. OldestGreenAt is the oldest LastGreenAt across the known rigs, zero when any
// has none; ImagesRetries is the largest count across the known rigs.
func redHoldInputsOf(src redHoldSources) (redHoldInputs, []string) {
	var in redHoldInputs
	var unknown []string
	var oldest time.Time
	noGreen := false
	for _, rig := range src.Rigs {
		st, err := src.Read(rig)
		s, ok := redHoldSweepOf(rig, st, err)
		if !ok {
			unknown = append(unknown, rig)
			continue
		}
		in.Sweeps = append(in.Sweeps, s)
		// A count recorded for another tip reads as zero retries.
		retries := 0
		if st.LastGreenSHA != "" && st.ImagesRetrySHA == st.LastGreenSHA {
			retries = st.ImagesRetries
		}
		in.ImagesRetries = max(in.ImagesRetries, retries)
		if st.LastGreenAt.IsZero() {
			oldest, noGreen = time.Time{}, true
		} else if !noGreen && (oldest.IsZero() || st.LastGreenAt.Before(oldest)) {
			oldest = st.LastGreenAt
		}
	}
	in.OldestGreenAt = oldest
	return in, unknown
}

// evalRedHold returns the reasons that hold dispatch: one per kind, sorted by
// kind, each keeping prev's Since when the kind already held and taking now when
// new. A sweep under redHoldSweepCycles contributes nothing; at or above it is
// sweep-no-result when unread, sweep-red otherwise, with Detail naming every rig.
// Waiting at or above redHoldBehind is behind-main; an unread count keeps prev's
// behind-main reason. An OldestGreenAt older than redHoldGreenStale is
// green-stale, and the zero time is unknown and adds nothing.
// An UpdateOutcome other than "", "ok" or "running" is roll-failed; ImagesRetries
// at or above tierSweepImagesRetryMax is images-failed; any PairStuck sha is
// pair-stuck; any MainRed rig is main-red, with Detail naming every rig.
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
	if in.WaitingUnread {
		for _, r := range prev.Reasons {
			if r.Kind == dispatch.RedHoldBehindMain {
				details[r.Kind] = []string{r.Detail}
			}
		}
	} else if in.Waiting >= redHoldBehind {
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
	if len(in.MainRed) > 0 {
		details[dispatch.RedHoldMainRed] = []string{"main red: " + strings.Join(in.MainRed, ", ")}
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

// checkRedHold is the heartbeat's red hold step: it evaluates every swept rig's
// record and the roll sources, and publishes daemon/red-hold from them.
func (d *Daemon) checkRedHold() {
	if d.redHoldUnread == nil {
		d.redHoldUnread = map[string]bool{}
	}
	src := redHoldSources{
		Rigs:      tierSweepRigs(d.patrol(), d.getKnownRigs()),
		Read:      func(rig string) (tierSweepState, error) { return readTierSweepState(d.config.TownRoot, rig) },
		Waiting:   d.redHoldWaiting,
		Update:    d.redHoldUpdate,
		PairStuck: d.redHoldPairStuck,
		MainRed:   d.redHoldMainRed,
		Now:       d.clk().Now,
	}
	if err := runRedHold(d.config.TownRoot, src, d.redHoldUnread, d.logger.Printf); err != nil {
		d.logger.Printf("red hold step: %v", err)
	}
}

// redHoldWaiting counts the landings waiting to deploy, as the update patrol
// reads them: the town's gt source checkout and the installed build's commit.
// A town with no checkout is an error, not zero: without the checkout nothing
// proves a landing is not in the installed build. The step keeps the previous
// behind-main reason on an error and logs that the count went unread.
func (d *Daemon) redHoldWaiting() (int, error) {
	repoRoot, err := version.GetRepoRootForTown(d.config.TownRoot)
	if err != nil {
		return 0, fmt.Errorf("read the town's gt source checkout: %w", err)
	}
	info, ok := d.rebuildGTStale(repoRoot)
	if !ok {
		return 0, errors.New("the installed build's staleness could not be read")
	}
	return d.updateLandingsWaiting(repoRoot, info.BinaryCommit)
}

// redHoldUpdate reads the last update run's outcome and the commit it set out to
// install; no state is "", "" and no error, an unreadable one is an error.
func (d *Daemon) redHoldUpdate() (string, string, error) {
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	st, err := store.Read()
	if errors.Is(err, update.ErrNoState) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return string(st.Outcome), st.Target.SHA, nil
}

// redHoldPairStuck is the commits whose image pair never became ready, as the
// pair check last raised them.
func (d *Daemon) redHoldPairStuck() []string {
	var shas []string
	for _, it := range d.pairReady.observed() {
		if it.Kind == attention.KindDeployPairStuck {
			shas = append(shas, it.SHA)
		}
	}
	return shas
}

// redHoldMainRed reports whether one rig's main is red: its last run differs
// from the last green, or a revert is in flight. A missing file is the zero
// state (not red); an unreadable one is the error, which keeps the previous hold.
func (d *Daemon) redHoldMainRed(rig string) (bool, error) {
	st, err := fileMainState{path: RedMainStatePath(d.config.TownRoot, rig)}.Load()
	if err != nil {
		return false, err
	}
	return (st.LastRun != "" && st.LastRun != st.LastGreen) || st.Revert != nil, nil
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
// record and logging the change once. An unreadable sweep record, update state,
// main-red read or lift request leaves the file untouched, so it never clears a
// hold; an unread waiting count keeps only its behind-main reason and every
// other trigger still reads. Each outage is logged once until it reads again;
// unreadLog holds those across heartbeats and is non-nil. A lift request lifts
// the hold and is recorded when the evaluation is clear.
func runRedHold(townRoot string, src redHoldSources, unreadLog map[string]bool, logf func(format string, args ...any)) error {
	in, unknown := redHoldInputsOf(src)
	prev, err := dispatch.ReadRedHold(townRoot)
	if err != nil {
		return fmt.Errorf("read red hold: %w", err)
	}
	// Forget the rigs that read again, so a later outage logs anew; the waiting
	// count, the update state and the main-red read are forgotten below.
	maps.DeleteFunc(unreadLog, func(key string, _ bool) bool {
		return key != redHoldWaitingKey && key != redHoldUpdateKey && key != redHoldMainRedKey && !slices.Contains(unknown, key)
	})
	if len(unknown) > 0 {
		for _, rig := range unknown {
			if !unreadLog[rig] {
				unreadLog[rig] = true
				logf("red hold: sweep record unreadable for %s; keeping the previous state", rig)
			}
		}
		return nil
	}
	if src.Waiting != nil {
		waiting, err := src.Waiting()
		if err != nil {
			in.WaitingUnread = true
			if !unreadLog[redHoldWaitingKey] {
				unreadLog[redHoldWaitingKey] = true
				logf("red hold: waiting landings unreadable; keeping the previous state")
			}
		} else {
			in.Waiting = waiting
		}
	}
	if !in.WaitingUnread {
		delete(unreadLog, redHoldWaitingKey)
	}
	if src.Update != nil {
		outcome, target, err := src.Update()
		if err != nil {
			if !unreadLog[redHoldUpdateKey] {
				unreadLog[redHoldUpdateKey] = true
				logf("red hold: update state unreadable; keeping the previous state")
			}
			return nil
		}
		in.UpdateOutcome, in.UpdateTarget = outcome, target
	}
	delete(unreadLog, redHoldUpdateKey)
	if src.PairStuck != nil {
		in.PairStuck = src.PairStuck()
	}
	if src.MainRed != nil {
		for _, rig := range src.Rigs {
			red, err := src.MainRed(rig)
			if err != nil {
				if !unreadLog[redHoldMainRedKey] {
					unreadLog[redHoldMainRedKey] = true
					logf("red hold: main-red state unreadable; keeping the previous state")
				}
				return nil
			}
			if red {
				in.MainRed = append(in.MainRed, rig)
			}
		}
	}
	delete(unreadLog, redHoldMainRedKey)
	now := src.Now
	if now == nil {
		now = time.Now
	}
	at := now()
	next := evalRedHold(in, at, prev)
	req, requested, err := dispatch.ReadLift(townRoot)
	if err != nil {
		return fmt.Errorf("read lift request: %w", err)
	}
	if requested && len(next.Reasons) == 0 {
		return liftRedHold(townRoot, req, at, logf)
	}
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

// liftRedHold applies a granted request: it removes the hold file, appends one
// ledger line, logs the lift and clears the request. The first failure stops the
// sequence with the request in place, so the next heartbeat repeats the lift.
func liftRedHold(townRoot string, req dispatch.LiftRequest, at time.Time, logf func(format string, args ...any)) error {
	step, err := "write red hold", dispatch.WriteRedHold(townRoot, dispatch.RedHoldRecord{})
	if err == nil {
		step, err = "record red hold lift", appendRedHoldLift(townRoot, at, req.Reason, req.By)
	}
	if err == nil {
		logf("red hold: lifted by %s: %s", req.By, req.Reason)
		step, err = "clear lift request", dispatch.ClearLift(townRoot)
	}
	if err != nil {
		return fmt.Errorf("lift red hold: %s: %w", step, err)
	}
	return nil
}

// appendRedHoldLift appends one lift line to the town's ledger at
// <town>/.runtime/red-hold/ledger.jsonl, creating the file and its directory.
// The one O_APPEND write of one line cannot interleave with a concurrent append.
func appendRedHoldLift(townRoot string, at time.Time, reason, by string) error {
	path := filepath.Join(townRoot, ".runtime", "red-hold", "ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	werr := json.NewEncoder(f).Encode(map[string]any{"at": at, "reason": reason, "by": by, "lifted": true})
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
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
