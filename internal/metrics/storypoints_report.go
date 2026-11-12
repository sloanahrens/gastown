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
	// pairs are the landed beads with both an estimate and a measured time.
	pairs []spPair
	// unmeasured counts the landed beads with an estimate and no measured time.
	unmeasured int
	// rungNone counts the landed beads whose estimate made no rung.
	rungNone int
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
		pair := spPair{seat: row["seat_class"], rung: row["sp_rung"], est: points, actual: measured}
		if m := row["sp_minutes"]; m != "" {
			minutes, err := strconv.ParseFloat(m, 64)
			if err != nil || math.IsNaN(minutes) || minutes <= 0 {
				return spLandings{}, fmt.Errorf("metrics: sp_minutes %q is not a positive number", m)
			}
			pair.minutes, pair.hasMinutes = minutes, true
		}
		out.pairs = append(out.pairs, pair)
	}
	return out, nil
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
// shadowMinLandings landed beads with an estimate and a measured time it writes
// that count and nothing else. Otherwise it writes the rank tables per seat class
// and per rung, the unmeasured and rung-none counts, and the trigger table when
// every landed bead carries its estimated minutes.
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
	writeTriggers(&b, land.pairs)
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
// It is written only when every landed bead carries its estimated minutes; a
// table over some beads would read as the whole.
func writeTriggers(b *strings.Builder, pairs []spPair) {
	est := make([]float64, 0, len(pairs))
	actual := make([]int, 0, len(pairs))
	for _, p := range pairs {
		if !p.hasMinutes {
			fmt.Fprintf(b, "split trigger: not computed, %d of %d landings carry no estimated minutes\n",
				countWithoutMinutes(pairs), len(pairs))
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
