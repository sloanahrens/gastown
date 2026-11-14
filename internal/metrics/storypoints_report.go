package metrics

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// The story-points shadow report (gt-5d9k2f SP15). It reads the landings table
// the extract wrote and says how well the estimate ranks landed beads against
// their measured points, per seat class and per rung, and what a split trigger
// would have flagged. Below shadowMinLandings it prints the count and stops.

// shadowMinLandings is how many landed beads carry both an estimate and a
// measured time before the report prints its tables.
const shadowMinLandings = 60

// bootstrapResamples is how many resamples each interval is the percentile of.
const bootstrapResamples = 400

// bootstrapSeed fixes the resamples, so two runs over one table print one
// interval.
const bootstrapSeed = 1

// longOverPoints is the points above which a landed bead ran long: the 21-point
// bucket's top, 27.5 minutes.
const longOverPoints = 21

// ErrNoSpread reports a rank correlation over a series with no spread: every
// value tied, so no ordering is measured.
var ErrNoSpread = errors.New("metrics: no spread in one of the series, so no rank correlation")

// Spearman is the rank correlation of x and y: the Pearson correlation of their
// ranks, with tied values sharing the average of their ranks. A perfect ranking
// is 1 and a reversed one is -1.
func Spearman(x, y []float64) (float64, error) {
	if len(x) != len(y) {
		return 0, fmt.Errorf("metrics: spearman over %d and %d values", len(x), len(y))
	}
	if len(x) < 2 {
		return 0, fmt.Errorf("metrics: spearman needs at least 2 pairs, have %d", len(x))
	}
	return rankCorrelation(averageRanks(x), averageRanks(y))
}

// averageRanks is each value's 1-based rank, tied values sharing the mean of
// the ranks they span.
func averageRanks(v []float64) []float64 {
	order := make([]int, len(v))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(v[a], v[b]) })
	ranks := make([]float64, len(v))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && v[order[j+1]] == v[order[i]] {
			j++
		}
		mean := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			ranks[order[k]] = mean
		}
		i = j + 1
	}
	return ranks
}

// rankCorrelation is the Pearson correlation of two rank series. A series with
// no spread has no correlation, so it is ErrNoSpread rather than a number.
func rankCorrelation(x, y []float64) (float64, error) {
	mx, my := sampleMean(x), sampleMean(y)
	var cov, vx, vy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx == 0 || vy == 0 {
		return 0, ErrNoSpread
	}
	return math.Max(-1, math.Min(1, cov/math.Sqrt(vx*vy))), nil
}

func sampleMean(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

// BootstrapSpearman is the 95% percentile interval of the rank correlation of x
// and y over resamples of their pairs, drawn with replacement. The generator is
// seeded, so the same pairs and seed give the same interval. A resample with no
// spread has no correlation and is skipped; when every resample is skipped there
// is no interval and the error is ErrNoSpread.
func BootstrapSpearman(x, y []float64, resamples int, seed uint64) (lo, hi float64, err error) {
	if len(x) != len(y) {
		return 0, 0, fmt.Errorf("metrics: bootstrap over %d and %d values", len(x), len(y))
	}
	if len(x) < 2 {
		return 0, 0, fmt.Errorf("metrics: bootstrap needs at least 2 pairs, have %d", len(x))
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	bx, by := make([]float64, len(x)), make([]float64, len(y))
	rhos := make([]float64, 0, resamples)
	for range resamples {
		for i := range bx {
			j := rng.IntN(len(x))
			bx[i], by[i] = x[j], y[j]
		}
		rho, err := Spearman(bx, by)
		if err != nil {
			continue
		}
		rhos = append(rhos, rho)
	}
	if len(rhos) == 0 {
		return 0, 0, ErrNoSpread
	}
	slices.Sort(rhos)
	return percentile(rhos, 0.025), percentile(rhos, 0.975), nil
}

// percentile is the value at quantile p of sorted values, by linear
// interpolation between the two nearest ranks.
func percentile(sorted []float64, p float64) float64 {
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// TriggerRow is one candidate split trigger: the estimated minutes at or above
// which a bead is flagged, the beads that flags, how many of them ran long
// (precision) and how many of the long beads it caught (recall).
type TriggerRow struct {
	Minutes float64
	Flagged int
	Long    int
	Caught  int
}

// Precision is the share of flagged beads that ran long, false when nothing was
// flagged.
func (r TriggerRow) Precision() (float64, bool) {
	return share(r.Caught, r.Flagged)
}

// Recall is the share of long beads that were flagged, false when no bead ran
// long.
func (r TriggerRow) Recall() (float64, bool) {
	return share(r.Caught, r.Long)
}

// TriggerTable computes each trigger's row over the estimated minutes and the
// measured points of the same beads. A bead is flagged when its estimate is at
// least the trigger, and ran long when its measured points are over 21.
func TriggerTable(estMinutes []float64, actual []int, triggers []float64) ([]TriggerRow, error) {
	if len(estMinutes) != len(actual) {
		return nil, fmt.Errorf("metrics: trigger table over %d estimates and %d measures", len(estMinutes), len(actual))
	}
	rows := make([]TriggerRow, 0, len(triggers))
	for _, t := range triggers {
		row := TriggerRow{Minutes: t}
		for i, est := range estMinutes {
			long := actual[i] > longOverPoints
			if long {
				row.Long++
			}
			if est >= t {
				row.Flagged++
				if long {
					row.Caught++
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// spPair is one landed bead the report reads: its estimated and measured points,
// the seat class and rung it carries, and its estimated minutes when the table
// has them.
type spPair struct {
	seat, rung  string
	est, actual int
	minutes     float64
	hasMinutes  bool
}

// spLandings is the landings table read for the report.
type spLandings struct {
	// pairs are the scored landed beads: an estimate, a measured time, and an
	// estimate written before the bead's first dispatch.
	pairs []spPair
	// unmeasured counts the landed beads with an estimate and no measured time.
	unmeasured int
	// rungNone counts the landed beads whose estimate made no rung.
	rungNone int
	// afterDispatch counts the landed beads with an estimate and a measured time
	// that are not scored: the estimate was written at or after the first
	// dispatch, or one of the two times is missing or unreadable.
	afterDispatch int
	// minutesRows counts the landed rows whose sp_minutes cell is set.
	minutesRows int
}

// readSPLandings reads the table's story-point cells. A cell that does not parse
// is an error: the table is then not the contract, and a bead cannot be counted
// into a tier it was not read into.
func readSPLandings(rows []Row) (spLandings, error) {
	var out spLandings
	for _, row := range rows {
		if row["sp_rung"] == rungNone {
			out.rungNone++
		}
		minutes, hasMinutes, err := parseMinutesCell(row["sp_minutes"])
		if err != nil {
			return spLandings{}, err
		}
		if hasMinutes {
			out.minutesRows++
		}
		est := row["sp"]
		if est == "" {
			continue
		}
		points, err := parseSPCell("sp", est)
		if err != nil {
			return spLandings{}, err
		}
		actual := row["sp_actual"]
		if actual == "" {
			out.unmeasured++
			continue
		}
		measured, err := parseSPCell("sp_actual", actual)
		if err != nil {
			return spLandings{}, err
		}
		if !estimatedBeforeDispatch(row) {
			out.afterDispatch++
			continue
		}
		out.pairs = append(out.pairs, spPair{
			seat: row["seat_class"], rung: row["sp_rung"], est: points, actual: measured,
			minutes: minutes, hasMinutes: hasMinutes,
		})
	}
	return out, nil
}

// estimatedBeforeDispatch reports whether a landed row's estimate was written
// before the bead's first dispatch. Both times must read: an estimate with no
// readable write time, or a bead with no readable dispatch, is not known to be
// before its dispatch, so it is not scored.
func estimatedBeforeDispatch(row Row) bool {
	written, err := ParseTS(row["sp_estimated_at"])
	if err != nil {
		return false
	}
	dispatched, err := ParseTS(row["dispatched_at"])
	if err != nil {
		return false
	}
	return written.Before(dispatched)
}

// parseMinutesCell reads a sp_minutes cell: "" is no minutes, and a value that is
// not a positive number is an error, since the trigger table over it would count
// a bead wrongly.
func parseMinutesCell(cell string) (minutes float64, ok bool, err error) {
	if cell == "" {
		return 0, false, nil
	}
	minutes, err = strconv.ParseFloat(cell, 64)
	if err != nil || math.IsNaN(minutes) || minutes <= 0 {
		return 0, false, fmt.Errorf("metrics: sp_minutes %q is not a positive number", cell)
	}
	return minutes, true, nil
}

// parseSPCell reads a points cell and refuses a value that is not a point on the
// scale.
func parseSPCell(column, cell string) (int, error) {
	points, err := strconv.Atoi(cell)
	if err != nil || !storypoints.Valid(points) {
		return 0, fmt.Errorf("metrics: %s %q is not a story point", column, cell)
	}
	return points, nil
}

// spStats is one group's line in the report.
type spStats struct {
	n           int
	rho, lo, hi float64
	rhoErr      error
	exact       float64
	exactOK     bool
	withinOne   float64
	withinOK    bool
}

// statsOf measures a group of pairs: the rank correlation with its bootstrap
// interval, and the exact and within-one-bucket shares.
func statsOf(pairs []spPair) spStats {
	est := make([]float64, len(pairs))
	actual := make([]float64, len(pairs))
	var exact, within int
	for i, p := range pairs {
		est[i], actual[i] = float64(p.est), float64(p.actual)
		if p.est == p.actual {
			exact++
		}
		if bucketGap(p.est, p.actual) <= 1 {
			within++
		}
	}
	s := spStats{n: len(pairs)}
	s.exact, s.exactOK = share(exact, len(pairs))
	s.withinOne, s.withinOK = share(within, len(pairs))
	s.rho, s.rhoErr = Spearman(est, actual)
	if s.rhoErr == nil {
		s.lo, s.hi, s.rhoErr = BootstrapSpearman(est, actual, bootstrapResamples, bootstrapSeed)
	}
	return s
}

// bucketGap is how many buckets apart two points are on the scale.
func bucketGap(a, b int) int {
	ia, ib := bucketIndex(a), bucketIndex(b)
	if ia > ib {
		return ia - ib
	}
	return ib - ia
}

func bucketIndex(points int) int {
	for i, e := range storypoints.Edges() {
		if e.Points == points {
			return i
		}
	}
	return -1
}

// share is n over d as a fraction, false when d is zero.
func share(n, d int) (float64, bool) {
	if d == 0 {
		return 0, false
	}
	return float64(n) / float64(d), true
}

// WriteStoryPointsReport writes the shadow report for the landings rows. Below
// shadowMinLandings scored landed beads (an estimate, a measured time, and an
// estimate written before dispatch) it writes that count and nothing else.
// Otherwise it writes the rank tables per seat class and per rung, the unmeasured,
// rung-none and after-dispatch counts, and the trigger section: the trigger table
// when the scored beads carry their estimated minutes, else the line that none is
// recorded yet.
func WriteStoryPointsReport(w io.Writer, rows []Row) error {
	land, err := readSPLandings(rows)
	if err != nil {
		return err
	}
	if len(land.pairs) < shadowMinLandings {
		_, err := fmt.Fprintf(w, "story points: %d estimated landings with a measured time; the report needs %d\n",
			len(land.pairs), shadowMinLandings)
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "story points shadow report: %d estimated landings with a measured time\n", len(land.pairs))
	b.WriteString("rank correlation of estimate and measure, bootstrap 95% interval; bar 0.4\n")
	writeGroups(&b, "per seat class", land.pairs, func(p spPair) string { return orNone(p.seat, "(no seat class)") })
	writeGroups(&b, "per rung", land.pairs, func(p spPair) string { return orNone(p.rung, "(no rung)") })
	fmt.Fprintf(&b, "estimated but unmeasured: %d; rung none: %d\n", land.unmeasured, land.rungNone)
	fmt.Fprintf(&b, "estimates written after dispatch: %d\n", land.afterDispatch)
	writeTriggers(&b, land)
	_, err = io.WriteString(w, b.String())
	return err
}

// writeGroups writes one table, one line per group key, in key order.
func writeGroups(b *strings.Builder, title string, pairs []spPair, key func(spPair) string) {
	groups := map[string][]spPair{}
	for _, p := range pairs {
		groups[key(p)] = append(groups[key(p)], p)
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)
	fmt.Fprintf(b, "%s\n", title)
	for _, name := range names {
		s := statsOf(groups[name])
		fmt.Fprintf(b, "  %-18s n=%-4d %s  exact %s  within one %s\n",
			name, s.n, s.rhoText(), fmtShare(s.exact, s.exactOK), fmtShare(s.withinOne, s.withinOK))
	}
}

// rhoText is the rank correlation with its interval, or why it is undefined.
func (s spStats) rhoText() string {
	if s.rhoErr != nil {
		return fmt.Sprintf("rank correlation n/a (%v)", s.rhoErr)
	}
	return fmt.Sprintf("rank correlation %.2f [%.2f, %.2f]", s.rho, s.lo, s.hi)
}

// writeTriggers writes the split trigger table at the minutes the design names.
// When no landed row carries its estimated minutes it says so in the section, so
// the section is never silently absent. It is written from the scored beads only
// when every one of them carries its minutes; a table over some beads would read
// as the whole.
func writeTriggers(b *strings.Builder, land spLandings) {
	if land.minutesRows == 0 {
		b.WriteString("split trigger\n  no estimated minutes recorded yet\n")
		return
	}
	est := make([]float64, 0, len(land.pairs))
	actual := make([]int, 0, len(land.pairs))
	for _, p := range land.pairs {
		if !p.hasMinutes {
			fmt.Fprintf(b, "split trigger: not computed, %d of %d landings carry no estimated minutes\n",
				countWithoutMinutes(land.pairs), len(land.pairs))
			return
		}
		est = append(est, p.minutes)
		actual = append(actual, p.actual)
	}
	rows, err := TriggerTable(est, actual, []float64{11, 13, 15, 17})
	if err != nil {
		fmt.Fprintf(b, "split trigger: not computed: %v\n", err)
		return
	}
	b.WriteString("split trigger (estimated minutes at least; long is over 21 points)\n")
	for _, r := range rows {
		p, pok := r.Precision()
		rc, rok := r.Recall()
		fmt.Fprintf(b, "  %4.0f min  flagged %-4d precision %s  recall %s\n",
			r.Minutes, r.Flagged, fmtShare(p, pok), fmtShare(rc, rok))
	}
}

func countWithoutMinutes(pairs []spPair) int {
	n := 0
	for _, p := range pairs {
		if !p.hasMinutes {
			n++
		}
	}
	return n
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

// fmtShare is a fraction as a two-decimal number, "n/a" when it is undefined.
func fmtShare(v float64, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", v)
}

// The story-points drift measure, for the weekly report. It reads the landings
// table for the most recent estimated landings, correlates the a priori
// estimate with the measured polecat time, and takes a second reading of the
// same landings scored with the previous adopted weights. It is the measurement
// only: the previous weights' score of a landing is a value the caller supplies.

// storyPointsDriftWindow is how many of the most recent landings a drift reading
// is taken over.
const storyPointsDriftWindow = 100

// storyPointsDriftMin is the fewest landings a drift reading is read from. Under
// it the reading is unmeasurable, and no number is reported.
const storyPointsDriftMin = 30

// StoryPointsDriftReading is one rank correlation of an estimate series against
// the measured polecat time, over the drift window.
type StoryPointsDriftReading struct {
	// N is the landings in the window the reading covers.
	N int
	// Rho is the rank correlation, NaN when Measurable is false, so a caller that
	// ignores Measurable cannot print a zero as a result.
	Rho float64
	// Measurable is false when the window is under storyPointsDriftMin or a
	// series has no spread.
	Measurable bool
	// Why says why the reading is unmeasurable, empty when it is measurable.
	Why string
}

// StoryPointsDriftPrevious scores one landing in minutes under the previous
// adopted weights. ok is false when those weights cannot score the landing.
type StoryPointsDriftPrevious func(row Row) (minutes float64, ok bool)

// StoryPointsDrift is the pair of readings the weekly report takes over one
// window of landings: the a priori estimate against the measured polecat time,
// and the same landings scored with the previous adopted weights.
type StoryPointsDrift struct {
	// N is the landings in the window: the most recent of the landings that carry
	// both an a priori estimate and a measured polecat time, at most
	// storyPointsDriftWindow.
	N int
	// Current is the a priori estimate against the measured time.
	Current StoryPointsDriftReading
	// Previous is the same N landings scored with the previous adopted weights.
	Previous StoryPointsDriftReading
}

// driftLanding is one landing the drift window can hold: the row itself, its a
// priori estimate and measured time in minutes, and when it landed.
type driftLanding struct {
	row      Row
	est      float64
	measured float64
	landed   time.Time
}

// StoryPointsDriftOver takes the drift readings over the landings rows. A
// landing is in the window when an estimate was written before its first
// dispatch, sp_minutes holds it, and polecat_secs holds a measured time that is
// not a lower bound; the window is the storyPointsDriftWindow most recent such
// landings by landed_at. A cell that does not parse is an error: the table is
// then not the contract, and a landing cannot be read into a window it was not
// read into. previous scores each landing in the window for the second reading;
// nil, a landing it cannot score, or a score that is not a finite positive
// number of minutes makes the second reading unmeasurable or an error, never a
// number over a smaller sample.
func StoryPointsDriftOver(rows []Row, previous StoryPointsDriftPrevious) (StoryPointsDrift, error) {
	window, err := driftWindow(rows)
	if err != nil {
		return StoryPointsDrift{}, err
	}
	est := make([]float64, len(window))
	measured := make([]float64, len(window))
	for i, l := range window {
		est[i], measured[i] = l.est, l.measured
	}
	out := StoryPointsDrift{N: len(window)}
	out.Current = rankReading(est, measured)
	out.Previous, err = previousReading(window, measured, previous)
	if err != nil {
		return StoryPointsDrift{}, err
	}
	return out, nil
}

// driftWindow reads the eligible landings, keeps the most recent
// storyPointsDriftWindow by landed_at, and returns them newest first. Ties in
// landed_at keep the order the rows came in.
func driftWindow(rows []Row) ([]driftLanding, error) {
	var eligible []driftLanding
	for _, row := range rows {
		l, ok, err := driftLandingOf(row)
		if err != nil {
			return nil, err
		}
		if ok {
			eligible = append(eligible, l)
		}
	}
	slices.SortStableFunc(eligible, func(a, b driftLanding) int { return b.landed.Compare(a.landed) })
	if len(eligible) > storyPointsDriftWindow {
		eligible = eligible[:storyPointsDriftWindow]
	}
	return eligible, nil
}

// driftLandingOf reads one landing the drift window can hold, or false when the
// row is not one: it has no estimate minutes, no measured time, a lower-bound
// time, or an estimate written at or after its first dispatch.
func driftLandingOf(row Row) (driftLanding, bool, error) {
	est, hasEstimate, err := parseMinutesCell(row["sp_minutes"])
	if err != nil {
		return driftLanding{}, false, fmt.Errorf("story points drift: %w", err)
	}
	if !hasEstimate {
		return driftLanding{}, false, nil
	}
	measured, hasMeasure, err := measuredMinutesCell(row["polecat_secs"], row["polecat_lower_bound"])
	if err != nil {
		return driftLanding{}, false, fmt.Errorf("story points drift: %w", err)
	}
	if !hasMeasure {
		return driftLanding{}, false, nil
	}
	before, err := driftEstimatedBeforeDispatch(row)
	if err != nil {
		return driftLanding{}, false, err
	}
	if !before {
		return driftLanding{}, false, nil
	}
	landed, err := ParseTS(row["landed_at"])
	if err != nil {
		return driftLanding{}, false, fmt.Errorf("story points drift: landed_at: %w", err)
	}
	return driftLanding{row: row, est: est, measured: measured, landed: landed}, true, nil
}

// driftEstimatedBeforeDispatch reports whether a landing's estimate was written
// before its first dispatch. A missing cell is a landing the table does not
// place on that clock, so it is not in the window; a cell that is present and
// does not parse is an error, since the table is then not the contract and the
// landing cannot be read as one that was not written first.
func driftEstimatedBeforeDispatch(row Row) (bool, error) {
	written, ok, err := driftStamp(row["sp_estimated_at"], "sp_estimated_at")
	if err != nil || !ok {
		return false, err
	}
	dispatched, ok, err := driftStamp(row["dispatched_at"], "dispatched_at")
	if err != nil || !ok {
		return false, err
	}
	return written.Before(dispatched), nil
}

// driftStamp reads one of the two stamps the drift window compares. An empty
// cell is no stamp, and a stamp that does not parse is an error.
func driftStamp(cell, column string) (time.Time, bool, error) {
	if cell == "" {
		return time.Time{}, false, nil
	}
	at, err := ParseTS(cell)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("story points drift: %s: %w", column, err)
	}
	return at, true, nil
}

// measuredMinutesCell reads a landing's measured polecat time in minutes. An
// empty, zero or negative cell is no time (the writer omits a zero), and a
// lower-bound time is not a measurement, so both read as absent. A cell that is
// not a finite number is an error.
func measuredMinutesCell(secs, lowerBound string) (float64, bool, error) {
	if secs == "" {
		return 0, false, nil
	}
	seconds, err := strconv.ParseFloat(secs, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, false, fmt.Errorf("metrics: polecat_secs %q is not a number", secs)
	}
	if seconds <= 0 || lowerBound == "1" {
		return 0, false, nil
	}
	return seconds / 60, true, nil
}

// rankReading is the rank correlation of the estimate and measured series: a
// window under storyPointsDriftMin, or a series with no spread, is unmeasurable
// rather than a number.
func rankReading(est, measured []float64) StoryPointsDriftReading {
	r := StoryPointsDriftReading{N: len(est), Rho: math.NaN()}
	if r.N < storyPointsDriftMin {
		r.Why = driftTooFew(r.N)
		return r
	}
	rho, err := Spearman(est, measured)
	if err != nil {
		r.Why = err.Error()
		return r
	}
	r.Rho, r.Measurable = rho, true
	return r
}

// driftTooFew is why a window of n landings is not read.
func driftTooFew(n int) string {
	return fmt.Sprintf("%d landings with both an estimate and a measured time, need %d", n, storyPointsDriftMin)
}

// previousReading scores the window with the previous adopted weights and
// correlates those minutes with the same measured series. It reads the window
// in full or not at all: a landing the previous weights cannot score makes the
// reading unmeasurable, and a score that is not a finite positive number is an
// error.
func previousReading(window []driftLanding, measured []float64, previous StoryPointsDriftPrevious) (StoryPointsDriftReading, error) {
	r := StoryPointsDriftReading{N: len(window), Rho: math.NaN()}
	if r.N < storyPointsDriftMin {
		r.Why = driftTooFew(r.N)
		return r, nil
	}
	if previous == nil {
		r.Why = "no previous-weights score"
		return r, nil
	}
	scores := make([]float64, 0, r.N)
	unscored := 0
	for _, l := range window {
		minutes, ok := previous(l.row)
		if !ok {
			unscored++
			continue
		}
		if !(minutes > 0) || math.IsInf(minutes, 0) || math.IsNaN(minutes) {
			return StoryPointsDriftReading{}, fmt.Errorf("story points drift: previous weights scored bead %q %v minutes", l.row["bead"], minutes)
		}
		scores = append(scores, minutes)
	}
	if unscored > 0 {
		r.Why = fmt.Sprintf("the previous weights cannot score %d of %d landings", unscored, r.N)
		return r, nil
	}
	return rankReading(scores, measured), nil
}
