package daemon

// The om_lessons patrol (gt-vdy9do): rework-program lever 3, the lessons loop
// that runs without a bird (docs/design/rework-program.md). daemon.patrols.
// om_lessons.interval names the period between two passes, and each pass calls
// one run seam that builds the week's proposals (internal/omlessons).
//
// The seam reads the review records of the window ending at the pass's clock,
// and the numbered rules of the town's directives, and returns one proposal per
// rejection class. Each proposal with enough findings is then filed as one
// overseer-only bead through the town's beads client (gt-pawdfa), at most once
// per class and ISO week (gt-ygtazd).
//
// The ticker is a check cadence, not a run cadence: an in-process ticker resets
// its countdown on every daemon start, so a ticker set to the 168h interval
// would never fire in a town that restarts oftener, and the week would be
// re-run from zero (gt-ima2). Due-ness is decided from the persisted last-run
// record instead, so a restart reads what the town already had and skips it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

const (
	// omLessonsPatrol is the patrol's name in daemon.patrols and in
	// daemon.container_off.
	omLessonsPatrol = "om_lessons"

	// omLessonsTickTimeout bounds a whole pass, so a run that reads a large
	// review record frees the patrol's single-flight guard at a known point.
	omLessonsTickTimeout = 10 * time.Minute

	// omLessonsJoinWait bounds how long Run waits for a pass still running
	// after the ticker stopped. A pass left past it finishes its own bounded
	// read, and the next start needs nothing it wrote.
	omLessonsJoinWait = 10 * time.Second
)

// omLessonsInterval returns the period between two passes, from the config the
// loader already judged: a value it refused never reaches the daemon.
func omLessonsInterval(cfg *DaemonPatrolConfig) (time.Duration, error) {
	var c *agentconfig.OmLessonsConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.OmLessons
	}
	return c.IntervalDuration()
}

// omLessonsRunFunc is one pass: the proposals built at time at, or the error
// that ended it. The daemon calls it once per tick, and it must return when ctx
// is done, so the tick's bound frees the patrol's single-flight guard.
type omLessonsRunFunc func(ctx context.Context, at time.Time) ([]omlessons.Proposal, error)

// omLessonsSources is one pass's inputs: how long its window is, the town's
// review records, and the numbered rules its directives carry. The readers are
// functions so a test supplies a fake record source.
type omLessonsSources struct {
	// Days is the window's length in calendar days; zero means
	// omlessons.DefaultDays.
	Days int
	// Records reads the review records at or after since, the prior window's
	// first day.
	Records func(since time.Time) ([]land.ReviewRecord, error)
	// Rules returns the numbered rule headings the directives carry, as their
	// decimal text.
	Rules func() (map[string]bool, error)
}

// omLessonsPass is the run seam over named sources: the production pass reads
// the town's own records and directives, and a test reads a fake source.
func omLessonsPass(sources omLessonsSources) omLessonsRunFunc {
	return func(ctx context.Context, at time.Time) ([]omlessons.Proposal, error) {
		return omLessonsRunFrom(ctx, at, sources)
	}
}

// omLessonsRunFrom builds one proposal for every rejection class from the
// review records of the window ending at at. Both reads finish before any
// proposal is built, and an input it cannot read is an error rather than a week
// with no findings.
func omLessonsRunFrom(ctx context.Context, at time.Time, sources omLessonsSources) ([]omlessons.Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("om_lessons: the pass was canceled before it read anything: %w", err)
	}
	if sources.Records == nil || sources.Rules == nil {
		return nil, errors.New("om_lessons: the pass has no sources: it needs a review-record reader and a directive-rule reader")
	}
	days := sources.Days
	if days == 0 {
		days = omlessons.DefaultDays
	}
	span, err := omlessons.Windows(at, time.Time{}, days)
	if err != nil {
		return nil, fmt.Errorf("om_lessons: %w", err)
	}
	records, err := sources.Records(span.Prior.Start)
	if err != nil {
		return nil, fmt.Errorf("om_lessons: reading the review records: %w", err)
	}
	rules, err := sources.Rules()
	if err != nil {
		return nil, fmt.Errorf("om_lessons: reading the directives: %w", err)
	}
	if len(rules) == 0 {
		return nil, errors.New("om_lessons: the directives hold no numbered rules, so every class would read as a gap")
	}
	return omlessons.Proposals(omlessons.Cluster(records, span), rules), nil
}

// omLessonsTownSources is the pass's production inputs over one town: its
// review log and the numbered rules of the directive files in its directives
// directory.
func omLessonsTownSources(townRoot string) (omLessonsSources, error) {
	if strings.TrimSpace(townRoot) == "" {
		return omLessonsSources{}, errors.New("om_lessons: no town root is named, so the pass has no records to read")
	}
	return omLessonsSources{
		Days: omlessons.DefaultDays,
		Records: func(since time.Time) ([]land.ReviewRecord, error) {
			return omLessonsTownRecords(townRoot, since)
		},
		Rules: func() (map[string]bool, error) {
			return omLessonsDirectiveRules(agentconfig.CentralDirectiveDir(townRoot))
		},
	}, nil
}

// omLessonsTownRecords reads the review records of the window that starts at
// since.
func omLessonsTownRecords(townRoot string, since time.Time) ([]land.ReviewRecord, error) {
	file := land.TownReviewsFile(townRoot)
	if _, err := os.Stat(file.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("the town's review log %s does not exist, so the week cannot be read", file.Path)
		}
		return nil, fmt.Errorf("the town's review log %s: %w", file.Path, err)
	}
	return file.Since(since)
}

// omLessonsRuleHeading matches a numbered rule's heading in a directive, such
// as "## 1. Failure is not success (fail-open)". Rule numbers run 1 to 99.
var omLessonsRuleHeading = regexp.MustCompile(`^## ([1-9][0-9]?)\. `)

// omLessonsDirectiveFiles are the directive files whose numbered rules count as
// coverage: the polecat rules and the shared rules.
var omLessonsDirectiveFiles = []string{"polecat.md", "_common.md"}

// omLessonsDirectiveRules returns the rule numbers the directive files in dir
// carry, as their decimal text. A file that cannot be read is an error, and so
// is a set with no numbered rule at all, since that would read every class as a
// gap.
func omLessonsDirectiveRules(dir string) (map[string]bool, error) {
	rules := map[string]bool{}
	for _, name := range omLessonsDirectiveFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the directive: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if m := omLessonsRuleHeading.FindStringSubmatch(line); m != nil {
				rules[m[1]] = true
			}
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%s holds no numbered rules: every class would read as a gap", dir)
	}
	return rules, nil
}

// startOmLessonsTicker starts the patrol's own ticker when the patrol is
// active, and returns a nil channel and stop func when it is not: a town that
// names no key starts nothing. A configured interval the patrol cannot schedule
// starts no ticker either, and says so once, rather than running on a cadence
// the town never asked for.
//
// The ticker fires on shortPatrolCheckTick of the run interval, and each tick
// asks whether the week is due; see startOmLessonsTick.
func (d *Daemon) startOmLessonsTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive(omLessonsPatrol) {
		return nil, nil
	}
	every, err := omLessonsInterval(d.patrol())
	if err != nil {
		d.logger.Printf("om_lessons patrol not started: %v", err)
		return nil, nil
	}
	newTicker := d.newOmLessonsTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	check := shortPatrolCheckTick(every)
	ticks, stop := newTicker(check)
	d.logger.Printf("om_lessons ticker started (check every %v, run every %v)", check, every)
	return ticks, stop
}

// omLessonsRunMemo holds the last completion time each daemon itself recorded,
// so a check in this process does not re-run the week after a save that could
// not create the record (gt-4nqej). It is keyed by the daemon, not the town
// root: a freshly built patrol over the same town must read what the previous
// process persisted, not inherit a completion it never observed.
//
// A daemon's entry is dropped by stopOmLessonsPatrol, which Run defers once a
// ticker exists. A tick driven without Run leaves its entry until the process
// ends, which only a test does.
var omLessonsRunMemo struct {
	mu   sync.Mutex
	seen map[*Daemon]*patrolLastRunMemory
}

// omLessonsLastRun returns this daemon's in-memory completion time, creating it
// on first use.
func (d *Daemon) omLessonsLastRun() *patrolLastRunMemory {
	omLessonsRunMemo.mu.Lock()
	defer omLessonsRunMemo.mu.Unlock()
	if omLessonsRunMemo.seen == nil {
		omLessonsRunMemo.seen = map[*Daemon]*patrolLastRunMemory{}
	}
	mem, ok := omLessonsRunMemo.seen[d]
	if !ok {
		mem = &patrolLastRunMemory{}
		omLessonsRunMemo.seen[d] = mem
	}
	return mem
}

// forgetOmLessonsLastRun drops this daemon's in-memory completion time.
func (d *Daemon) forgetOmLessonsLastRun() {
	omLessonsRunMemo.mu.Lock()
	defer omLessonsRunMemo.mu.Unlock()
	delete(omLessonsRunMemo.seen, d)
}

// startOmLessonsTick runs one pass on its own goroutine when the week's record
// says it is due: a pass reads the week's review records, which does not belong
// on the main select loop. A tick that arrives before the record says a week has
// passed is skipped, and one that arrives while the previous pass still runs is
// skipped and logged, since two passes at once would propose the same window
// twice.
func (d *Daemon) startOmLessonsTick() {
	if d.config == nil || !d.isPatrolActive(omLessonsPatrol) {
		return
	}
	every, err := omLessonsInterval(d.patrol())
	if err != nil {
		d.logger.Printf("om_lessons: not checking: %v", err)
		return
	}
	dec := evaluatePatrolDue(d.config.TownRoot, omLessonsPatrol, d.omLessonsLastRun().last(), d.clk().Now(), every)
	if !dec.due {
		d.logger.Printf("om_lessons: not due — %s", dec.note)
		return
	}
	if !d.omLessonsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("om_lessons: previous pass still running, skipping")
		return
	}
	if dec.warn != "" {
		d.logger.Printf("om_lessons: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("om_lessons: due — %s", dec.note)
	}
	d.omLessonsCycles.Add(1)
	go func() {
		defer d.omLessonsCycles.Done()
		defer d.omLessonsRunning.Store(false)
		d.triggerOmLessons()
	}()
}

// triggerOmLessons runs one pass over the town's own run seam and logs what it
// built, or the error that ended it. The pass records its start time whatever
// the outcome, the "attempted" convention checkpoint_dog and jsonl_git_backup
// follow: a broken pass costs one line per week, not one per check.
func (d *Daemon) triggerOmLessons() {
	if d.config == nil || !d.isPatrolActive(omLessonsPatrol) {
		return
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, omLessonsTickTimeout)
	defer cancel()
	at := d.clk().Now()
	defer d.recordPatrolRun(d.omLessonsLastRun(), omLessonsPatrol, at)
	proposals, err := d.runOmLessonsTick(ctx, at)
	if err != nil {
		d.logger.Printf("om_lessons: %v", err)
		return
	}
	d.logger.Printf("om_lessons: the pass built %d proposal(s)", len(proposals))
}

// runOmLessonsTick runs one pass: it calls the run seam once, then files the
// proposals it built. A daemon built without a run seam runs the pass over the
// town's own review records and directives.
func (d *Daemon) runOmLessonsTick(ctx context.Context, at time.Time) ([]omlessons.Proposal, error) {
	run := d.omLessonsRun
	if run == nil {
		if d.config == nil {
			return nil, errors.New("om_lessons: the daemon names no town, so the pass has no records to read")
		}
		sources, err := omLessonsTownSources(d.config.TownRoot)
		if err != nil {
			return nil, err
		}
		run = omLessonsPass(sources)
	}
	proposals, err := run(ctx, at)
	if err != nil {
		return proposals, err
	}
	return proposals, d.fileOmLessonsProposals(at, omlessons.DefaultDays, proposals)
}

// omLessonsDayLayout formats the window's first and last days, as the
// `gt land om-lessons` report does.
const omLessonsDayLayout = "2006-01-02"

// omLessonsFiledFileName is the record of which proposal classes the current
// ISO week already filed. It lives in the town's daemon directory beside the
// patrol last-run record, so a restarted daemon reads what the town filed
// before it.
const omLessonsFiledFileName = "om_lessons_filed.json"

// omLessonsFiledPath returns the path of the town's filed-classes record.
func omLessonsFiledPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", omLessonsFiledFileName)
}

// omLessonsFiledRecord is the on-disk shape: the ISO week the classes belong
// to and the classes filed in it. A record of another week reads as nothing
// filed, so the file holds one week at a time.
type omLessonsFiledRecord struct {
	Week    string   `json:"week"`
	Classes []string `json:"classes"`
}

// omLessonsISOWeek names the ISO week at falls in, such as "2026-W47". The ISO
// year is used rather than the calendar year, so the days after New Year that
// still belong to the previous week count with it.
func omLessonsISOWeek(at time.Time) string {
	year, week := at.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// loadOmLessonsFiled returns the classes the record says week already filed. A
// missing record is an empty set, the first pass of the town's life. A record
// that cannot be read or parsed is an error rather than an empty set: an
// unreadable record might have filed every class, and reading it as none would
// file them all a second time.
func loadOmLessonsFiled(townRoot, week string) (map[string]bool, error) {
	data, err := os.ReadFile(omLessonsFiledPath(townRoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", omLessonsFiledPath(townRoot), err)
	}
	var record omLessonsFiledRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", omLessonsFiledFileName, err)
	}
	filed := map[string]bool{}
	if record.Week != week {
		return filed, nil
	}
	for _, id := range record.Classes {
		filed[id] = true
	}
	return filed, nil
}

// saveOmLessonsFiled records the classes filed in week, replacing the week the
// record held before.
func saveOmLessonsFiled(townRoot, week string, filed map[string]bool) error {
	classes := make([]string, 0, len(filed))
	for id := range filed {
		classes = append(classes, id)
	}
	sort.Strings(classes)
	path := omLessonsFiledPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, omLessonsFiledRecord{Week: week, Classes: classes})
}

// fileOmLessonsProposals files one overseer-only bead for each proposal with
// enough findings to draft an edit, at most once per class and ISO week. A
// too-few class has no draft and files nothing. The counts named in each bead
// are read over a window of days ending on at's day, the window the pass read.
//
// A class the week already filed is skipped and named in the log, so a second
// pass in the week — a restart, a shorter configured interval, a rerun — files
// nothing twice. The record is written as each class is filed, so a pass that
// ends between two creates leaves the classes it already filed recorded. A
// class whose create fails is left out of the record, so the proposals after
// it still file and the next pass of the week tries it again; every failure is
// returned together, so a pass that lost a bead never reads as clean.
func (d *Daemon) fileOmLessonsProposals(at time.Time, days int, proposals []omlessons.Proposal) error {
	var drafts []omlessons.Proposal
	for _, p := range proposals {
		if !p.TooFew {
			drafts = append(drafts, p)
		}
	}
	if len(drafts) == 0 {
		return nil
	}
	if d.config == nil {
		return errors.New("om_lessons: the daemon names no town, so the proposals have no directory to name")
	}
	townRoot := strings.TrimSpace(d.config.TownRoot)
	if townRoot == "" {
		return errors.New("om_lessons: the daemon names no town root, so the filed classes have no record to live in")
	}
	span, err := omlessons.Windows(at, time.Time{}, days)
	if err != nil {
		return fmt.Errorf("om_lessons: %w", err)
	}
	window := omLessonsWindowLine(span)
	directives := agentconfig.CentralDirectiveDir(townRoot)
	week := omLessonsISOWeek(at)
	filed, err := loadOmLessonsFiled(townRoot, week)
	if err != nil {
		return fmt.Errorf("om_lessons: reading the classes filed in %s: %w", week, err)
	}
	writer := d.noticeBeads()
	var failures []error
	for _, p := range drafts {
		if filed[p.ID] {
			d.logger.Printf("om_lessons: %s was already filed in %s, skipping", p.ID, week)
			continue
		}
		_, err := writer.Create(beads.CreateOptions{
			Title:       fmt.Sprintf("om lessons: %s (%s)", p.Name, p.ID),
			Labels:      []string{dispatch.OverseerOnlyLabel},
			Priority:    2,
			Description: omLessonsProposalBody(p, window, directives),
			Actor:       daemonActor,
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("om_lessons: filing the proposal for %s: %w", p.ID, err))
			continue
		}
		filed[p.ID] = true
		// Record the class with its bead, not once after the loop: a pass
		// that dies before the loop ends leaves the classes it already filed
		// recorded, and a write that fails here is retried by the next class's
		// write, which carries the accumulated set.
		if err := saveOmLessonsFiled(townRoot, week, filed); err != nil {
			failures = append(failures, fmt.Errorf("om_lessons: recording the classes filed in %s: %w", week, err))
		}
	}
	return errors.Join(failures...)
}

// omLessonsWindowLine names the two windows a proposal's counts are read over.
func omLessonsWindowLine(span omlessons.Span) string {
	first := func(b omlessons.Bounds) string { return b.Start.Format(omLessonsDayLayout) }
	last := func(b omlessons.Bounds) string { return b.End.AddDate(0, 0, -1).Format(omLessonsDayLayout) }
	return fmt.Sprintf("om lessons: %s to %s (%d days), against %s to %s",
		first(span.Current), last(span.Current), span.Days, first(span.Prior), last(span.Prior))
}

// omLessonsProposalBody is the text of the bead a proposal files: the class, its
// counts, its three example beads and the directive edit the overseer writes.
// It reads as writeLandOmLessonsProposals in internal/cmd does, so the bead and
// the `gt land om-lessons --propose` draft say the same thing.
func omLessonsProposalBody(p omlessons.Proposal, window, directives string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", window)
	fmt.Fprintf(&b, "Class: %s (%s)\n", p.Name, p.ID)
	fmt.Fprintf(&b, "Coverage: %s\n", p.Coverage)
	fmt.Fprintf(&b, "Counts: %d finding(s) on %d bead(s), %d round(s) burned; prior window %d finding(s) on %d bead(s), %d round(s)\n",
		p.Current.Findings, p.Current.Beads, p.Current.Rounds,
		p.Prior.Findings, p.Prior.Beads, p.Prior.Rounds)
	fmt.Fprintf(&b, "Examples: %s\n", strings.Join(p.Examples, ", "))
	fmt.Fprintln(&b, "\nGoal: a directive edit that stops om raising this class.")
	fmt.Fprintf(&b, "Constraints: the edit goes to the directive files in %s; the edit text is the overseer's to write. A rule that changes what polecats may do needs the operator's yes.\n", directives)
	fmt.Fprintln(&b, "Out of scope: om's bar or threshold; turning anything on.")
	fmt.Fprintln(&b, "Gate: go test ./internal/cmd -run TestLandOmLessons -count=1")
	fmt.Fprintln(&b, "Size: one directive edit, one landing.")
	fmt.Fprintln(&b, "Acceptance:")
	fmt.Fprintln(&b, "- [ ] the rule for this class says what the case is and what to do instead")
	fmt.Fprintln(&b, "- [ ] the next weekly pass counts fewer findings of this class than this window")
	return b.String()
}

// stopOmLessonsPatrol stops the patrol's ticker and joins the pass it may have
// running, bounded, so Run's return leaves no pass reading the town after the
// daemon is gone.
func (d *Daemon) stopOmLessonsPatrol(stop func()) {
	defer d.forgetOmLessonsLastRun()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.omLessonsCycles.Wait()
		close(done)
	}()
	bound := d.omLessonsJoinGrace
	if bound <= 0 {
		bound = omLessonsJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("om_lessons: a pass is still running %s after the ticker stopped; Run leaves without it", bound)
	}
}
