package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/lock"
)

// The daily per-rig bd snapshot records the tracker's counts, ages and shape
// once per local day, before the Dolt history flatten retires each day's
// commits: `bd history` stops at a flatten, this file does not (gt-8tcuo).
//
// A line records the local day that ended at the run's midnight, so its
// created and closed counts cover a whole day; the first run after midnight
// writes it, and that day's later runs write nothing for a rig it holds.
//
// A rig whose listing fails is a gap line, never zeros, and the run fails so
// the hour is not read as a complete day. A gap does not finish the rig's day:
// a later run that day retries it and appends the counts line, which is the
// line a reader takes for a (date, rig) pair since the file is append-only.
//
// Dependency chain depth would need the edge list, which `bd list` does not
// give; the line carries the per-bead dependency_count histogram and nothing
// about depth.
//
// The age and shaped blocks also carry a work-only twin over the beads that
// are not identity beads (type rig, or label gt:agent or handoff). Rig and
// gt:agent beads stay open for the life of the town, and a handoff bead is a
// note, not work, so a raw block can name one of these as the oldest open bead
// or count it as unshaped; the twin is what says whether any work is stuck
// (gt-9iel9).

// bdDailyCallTimeout bounds one rig's bd listing: the town's largest rig lists
// in a few seconds, and the loader retries once, so a store hung on a dead Dolt
// server costs its rig a gap line, not the 15-minute town-metrics hour.
const bdDailyCallTimeout = 20 * time.Second

// BdDailyPath is the file the daily snapshot appends to under town.
func BdDailyPath(town string) string {
	return filepath.Join(town, ".runtime", "metrics", "bd-daily.jsonl")
}

// BdDailyLockPath is the lock a daily run holds over its read and appends.
func BdDailyLockPath(town string) string {
	return filepath.Join(town, ".runtime", "metrics", "bd-daily.lock")
}

// BdDailyLine is one rig's tracker state on Date, the local day that ended at
// the run's midnight; CreatedToday and ClosedToday count that day.
type BdDailyLine struct {
	Date             string         `json:"date"`
	Rig              string         `json:"rig"`
	Total            int            `json:"total"`
	ByStatus         map[string]int `json:"by_status"`
	ByPriority       map[string]int `json:"by_priority"`
	ByType           map[string]int `json:"by_type"`
	ByLabel          map[string]int `json:"by_label"`
	DependencyCount  map[string]int `json:"dependency_count"`
	OpenAgeDays      BdDailyAges    `json:"open_age_days"`
	OpenAgeDaysWork  BdDailyAges    `json:"open_age_days_work"`
	CreatedToday     int            `json:"created_today"`
	ClosedToday      int            `json:"closed_today"`
	ShapedOpen       int            `json:"shaped_open"`
	UnshapedOpen     int            `json:"unshaped_open"`
	ShapedOpenWork   int            `json:"shaped_open_work"`
	UnshapedOpenWork int            `json:"unshaped_open_work"`
}

// BdDailyAges is the age in days of a set of open beads: the rig's beads in
// open_age_days, its work beads in open_age_days_work. A set with no bead
// carries null percentiles and no oldest_id, never zeros.
type BdDailyAges struct {
	P50      *float64 `json:"p50"`
	P90      *float64 `json:"p90"`
	Max      *float64 `json:"max"`
	OldestID string   `json:"oldest_id"`
}

// BdDailyGap is one rig whose line could not be counted. It carries no counts,
// so a reader cannot mistake it for a rig that holds no beads.
type BdDailyGap struct {
	Date string `json:"date"`
	Rig  string `json:"rig"`
	Gap  string `json:"gap"`
}

// BdDailyResult is what one run recorded: the day and file, the rigs it wrote,
// the rigs the file already held, the rigs it could only record as gaps, and
// whether another run held the lock. Date is empty only when the run refused
// before it read the clock, and then the returned error is non-nil.
type BdDailyResult struct {
	Date     string
	Path     string
	Written  []string
	Recorded []string
	Gaps     []string
	Skipped  bool
}

// BdDailyGapsError reports the rigs a run could only record as gaps. Their gap
// lines are in the file, but the day is incomplete and the run is not a
// success.
type BdDailyGapsError struct {
	Rigs []string
}

func (e *BdDailyGapsError) Error() string {
	return fmt.Sprintf("%d rig(s) could not be counted: %s", len(e.Rigs), strings.Join(e.Rigs, ", "))
}

// RunBdDaily appends the line for the local day that ended at midnight for
// every registered rig, skipping a rig the file already holds for that day and
// retrying a rig it holds only a gap for. A run that cannot take the day's
// lock reports Skipped and writes nothing, since the holder is writing the
// same day.
func RunBdDaily(ctx context.Context, env Env) (BdDailyResult, error) {
	res := BdDailyResult{Path: BdDailyPath(env.Town)}
	switch {
	case env.Town == "":
		return res, errors.New("no town")
	case env.ReadFile == nil:
		return res, errors.New("no file reader")
	case env.RunDir == nil:
		return res, errors.New("no bd runner")
	}
	now := env.now()
	res.Date = bdDailyDay(now)
	rigs, err := queueRigs(env)
	if err != nil {
		return res, err
	}
	lockPath := BdDailyLockPath(env.Town)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return res, fmt.Errorf("creating %s: %w", filepath.Dir(lockPath), err)
	}
	unlock, held, err := lock.FlockTryAcquire(lockPath)
	if err != nil {
		return res, fmt.Errorf("locking %s: %w", lockPath, err)
	}
	if !held {
		res.Skipped = true
		return res, nil
	}
	defer unlock()
	recorded, gapped, err := bdDailyRecorded(env.ReadFile, res.Path, res.Date)
	if err != nil {
		return res, err
	}
	run := bdDailyRun(ctx, env.RunDir)
	for _, rig := range rigs {
		if recorded[rig.name] {
			res.Recorded = append(res.Recorded, rig.name)
			continue
		}
		line, gap := bdDailyRig(run, env.Town, rig.name, now)
		if gap != "" {
			res.Gaps = append(res.Gaps, rig.name)
			if gapped[rig.name] {
				// The day already holds this rig's gap; another adds nothing.
				continue
			}
		}
		if err := battery.Append(res.Path, line); err != nil {
			return res, fmt.Errorf("appending %s: %w", rig.name, err)
		}
		if gap == "" {
			res.Written = append(res.Written, rig.name)
		}
	}
	if len(res.Gaps) > 0 {
		return res, &BdDailyGapsError{Rigs: res.Gaps}
	}
	return res, nil
}

// bdDailyDay is the local day a run records: the day that ended at the run's
// midnight. The rows then hold a whole day of created_at and closed_at, where
// a line stamped with the new day would count only the minutes since midnight.
func bdDailyDay(now time.Time) string {
	return now.AddDate(0, 0, -1).Format(time.DateOnly)
}

// bdDailyRig is the record for one rig: its line, or the gap that replaced it.
// A failed listing is a gap by the loader's own record; a listing whose rows
// cannot be counted is a gap too, so the file never holds a count that is
// partly untrue.
func bdDailyRig(run BdRun, town, rig string, now time.Time) (any, string) {
	date := bdDailyDay(now)
	if !bdDailyRigName(rig) {
		reason := "the rig name is not a directory name"
		return BdDailyGap{Date: date, Rig: rig, Gap: reason}, reason
	}
	dir := filepath.Join(town, rig)
	dropped := &Dropped{}
	rows := LoadBeads(run, []string{dir}, dropped)
	if reason, failed := bdDailyListingFailure(dropped, dir); failed {
		return BdDailyGap{Date: date, Rig: rig, Gap: reason}, reason
	}
	line, err := bdDailyAggregate(rig, rows, now)
	if err != nil {
		return BdDailyGap{Date: date, Rig: rig, Gap: err.Error()}, err.Error()
	}
	return line, ""
}

// bdDailyRigName reports whether a registry rig name is one directory name: the
// registry's names are directories under the town, so a name that leaves the
// town never reaches a bd directory.
func bdDailyRigName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, 0) && filepath.Base(name) == name
}

// bdDailyListingFailure names the reason bd list failed in dir, if it did.
func bdDailyListingFailure(dropped *Dropped, dir string) (string, bool) {
	source := "bd:" + dir
	for _, failed := range dropped.Failed {
		if failed != source {
			continue
		}
		for _, item := range dropped.Items {
			if item.Source == source {
				return item.Reason, true
			}
		}
		return "bd list failed", true
	}
	return "", false
}

// bdDailyRecorded reads the day's lines: the rigs whose counts line the file
// holds on day, and the rigs it holds only a gap for, which a later run
// retries. A missing file is the first run of the day; any other read failure
// is an error. A malformed line is an error too, except a final fragment with
// no newline after it, which a crashed append left and battery.Append
// separates rather than treats as a record.
func bdDailyRecorded(readFile func(string) ([]byte, error), path, day string) (recorded, gapped map[string]bool, err error) {
	recorded, gapped = map[string]bool{}, map[string]bool{}
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return recorded, gapped, nil
		}
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Date string  `json:"date"`
			Rig  string  `json:"rig"`
			Gap  *string `json:"gap"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			if i == len(lines)-1 && !strings.HasSuffix(string(data), "\n") {
				continue
			}
			return nil, nil, fmt.Errorf("reading %s: line %d: %w", path, i+1, err)
		}
		if rec.Date != day || rec.Rig == "" {
			continue
		}
		if rec.Gap == nil {
			recorded[rec.Rig] = true
		} else {
			gapped[rec.Rig] = true
		}
	}
	return recorded, gapped, nil
}

// bdDailyRun is the bd seam over the snapshot's directory runner.
func bdDailyRun(ctx context.Context, runDir RunnerDir) BdRun {
	return func(dir string, args []string) ([]byte, bool) {
		callCtx, cancel := context.WithTimeout(ctx, bdDailyCallTimeout)
		defer cancel()
		out, err := runDir(callCtx, dir, append([]string{"bd"}, args...))
		return out, err == nil
	}
}

// bdDailyAggregate counts one rig's listing as the line for now's day. Only
// status "open" is open: a bead that is in_progress, hooked, blocked or
// deferred is not counted in the ages or the shaped totals. The work-only
// twin blocks take the same open beads minus the identity beads
// bdDailyWorkBead rejects. A label used on fewer than 3 of the rig's beads is
// summed into "other", so the labels that name one bead do not each cost a key.
func bdDailyAggregate(rig string, rows []BeadRow, now time.Time) (BdDailyLine, error) {
	day := bdDailyDay(now)
	line := BdDailyLine{
		Date:            day,
		Rig:             rig,
		Total:           len(rows),
		ByStatus:        map[string]int{},
		ByPriority:      map[string]int{},
		ByType:          map[string]int{},
		ByLabel:         map[string]int{},
		DependencyCount: map[string]int{"0": 0, "1": 0, "2": 0, "3-5": 0, "6+": 0},
	}
	labelUses := map[string]int{}
	var open, openWork []bdDailyBeadAge
	for _, b := range rows {
		status := b.Status
		if status == "" {
			status = "unknown"
		}
		line.ByStatus[status]++
		priority := "none"
		if b.Priority != nil {
			priority = strconv.Itoa(*b.Priority)
		}
		line.ByPriority[priority]++
		issueType := b.Type
		if issueType == "" {
			issueType = "unknown"
		}
		line.ByType[issueType]++
		seen := make(map[string]bool, len(b.Labels))
		for _, label := range b.Labels {
			if seen[label] {
				continue
			}
			seen[label] = true
			labelUses[label]++
		}
		if b.DependencyCount < 0 {
			return BdDailyLine{}, fmt.Errorf("bead %s: dependency_count %d is negative", b.ID, b.DependencyCount)
		}
		line.DependencyCount[bdDailyBucket(b.DependencyCount)]++

		created, err := bdDailyTime(b.CreatedAt)
		if err != nil {
			return BdDailyLine{}, fmt.Errorf("bead %s: created_at: %w", b.ID, err)
		}
		if bdDailySameDay(created, day, now.Location()) {
			line.CreatedToday++
		}
		if b.ClosedAt != nil {
			closed, err := bdDailyTime(*b.ClosedAt)
			if err != nil {
				return BdDailyLine{}, fmt.Errorf("bead %s: closed_at: %w", b.ID, err)
			}
			if bdDailySameDay(closed, day, now.Location()) {
				line.ClosedToday++
			}
		}
		if status != "open" {
			continue
		}
		shaped := b.Shaped
		if shaped {
			line.ShapedOpen++
		} else {
			line.UnshapedOpen++
		}
		days := now.Sub(created).Hours() / 24
		if days < 0 {
			// A bead stamped later than the clock is 0 days old, not negative.
			days = 0
		}
		age := bdDailyBeadAge{days: days, id: b.ID}
		open = append(open, age)
		if !bdDailyWorkBead(b) {
			continue
		}
		if shaped {
			line.ShapedOpenWork++
		} else {
			line.UnshapedOpenWork++
		}
		openWork = append(openWork, age)
	}

	other := 0
	for label, n := range labelUses {
		if n < 3 {
			other += n
			continue
		}
		line.ByLabel[label] = n
	}
	if other > 0 {
		line.ByLabel["other"] += other
	}

	line.OpenAgeDays = bdDailyAgesOf(open)
	line.OpenAgeDaysWork = bdDailyAgesOf(openWork)
	return line, nil
}

// bdDailyBeadAge is one open bead's age, which the age blocks are read from.
type bdDailyBeadAge struct {
	days float64
	id   string
}

// bdDailyWorkBead reports whether a bead is work. Rig and gt:agent beads are
// identity beads that stay open for the life of the town. A handoff bead is a
// note that gets closed, not work. The beads whose type is rig or whose labels
// hold gt:agent or handoff are left out, since counting them says nothing about
// stuck work.
func bdDailyWorkBead(b BeadRow) bool {
	if b.Type == "rig" {
		return false
	}
	for _, label := range b.Labels {
		if label == "gt:agent" || label == "handoff" {
			return false
		}
	}
	return true
}

// bdDailyAgesOf is the age block of the given open beads, or the null block of
// a set that holds none.
func bdDailyAgesOf(open []bdDailyBeadAge) BdDailyAges {
	if len(open) == 0 {
		return BdDailyAges{}
	}
	// Equal ages are ordered by id so the oldest_id is the same on every run.
	sort.Slice(open, func(i, j int) bool {
		if open[i].days != open[j].days {
			return open[i].days < open[j].days
		}
		return open[i].id > open[j].id
	})
	ages := make([]float64, len(open))
	for i, a := range open {
		ages[i] = a.days
	}
	p50 := bdDailyRound(bdDailyPercentile(ages, 50))
	p90 := bdDailyRound(bdDailyPercentile(ages, 90))
	max := bdDailyRound(ages[len(ages)-1])
	return BdDailyAges{P50: &p50, P90: &p90, Max: &max, OldestID: open[len(open)-1].id}
}

// bdDailyPercentile is the nearest-rank percentile of ascending values: the
// ceil(p/100*n)-th value, which the pth percent of the data does not exceed.
func bdDailyPercentile(asc []float64, p float64) float64 {
	rank := int(math.Ceil(p/100*float64(len(asc)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(asc) {
		rank = len(asc) - 1
	}
	return asc[rank]
}

// bdDailyRound keeps one decimal of a day count, so a bead less than a day old
// is not indistinguishable from one created at midnight.
func bdDailyRound(days float64) float64 {
	return math.Round(days*10) / 10
}

// bdDailyTime parses a bd timestamp. A value it cannot parse is an error: a
// bead's age and day would otherwise be silently counted as zero.
func bdDailyTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// bdDailySameDay reports whether t falls on day in loc, the zone the run
// stamps its dates with.
func bdDailySameDay(t time.Time, day string, loc *time.Location) bool {
	return t.In(loc).Format(time.DateOnly) == day
}

// bdDailyBucket names the dependency-count histogram bucket a count falls in.
func bdDailyBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n == 1:
		return "1"
	case n == 2:
		return "2"
	case n <= 5:
		return "3-5"
	default:
		return "6+"
	}
}
