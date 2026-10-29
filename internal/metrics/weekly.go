package metrics

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
)

// The bead-side weekly quantities: labels, dependency density, the handoff
// ratio, handoff size, mode shift and the weekly row that joins them.

// WeeklyColumns is the header weekly.csv carries, in the order its columns are
// written. Change it and WeeklyMetrics together, or the extract writes a column
// no row fills.
var WeeklyColumns = []string{
	"week", "rig", "thin", "landings", "unreadable", "size_median", "size_p90", "file_entropy_mean",
	"package_entropy_mean", "attempts_mean", "rejected_share", "first_try_share", "manual_rescue_count",
	"h_labels", "h_rigs", "h_types", "handoff_bytes_total", "handoff_bytes_median", "sessions_median",
	"rho_numerator", "rho_denominator", "rho", "dependency_density", "rejections", "conflict_gate_share",
	"unknown_share", "mode_shift", "om_threshold", "dominant_model",
}

// shiftColumns are the columns ModeShift measures between weeks, read in sorted
// order.
var shiftColumns = []string{"size_median", "file_entropy_mean", "rejected_share"}

// Bead is a bead record the weekly metrics read: the loader's JSON object, not
// a CSV row. An absent timestamp is "".
type Bead struct {
	ID              string   `json:"id"`
	Rig             string   `json:"rig"`
	CreatedAt       string   `json:"created_at"`
	ClosedAt        string   `json:"closed_at"`
	Type            string   `json:"type"`
	Labels          []string `json:"labels"`
	DependencyCount int      `json:"dependency_count"`
}

// WeeklyMetrics is one row of weekly.csv: a (week, rig) group's figures, with a
// rig of "ALL" for the town's row. A nil pointer marks a figure
// the week could not supply; the counts are ints because weekly.csv writes them
// without a decimal point.
type WeeklyMetrics struct {
	Week string
	Rig  string
	Thin bool

	Landings           int
	Unreadable         int
	SizeMedian         *float64
	SizeP90            *float64
	FileEntropyMean    *float64
	PackageEntropyMean *float64
	AttemptsMean       *float64
	RejectedShare      *float64
	FirstTryShare      *float64
	ManualRescueCount  int

	HLabels *float64
	HRigs   *float64
	HTypes  *float64

	HandoffBytesTotal  int
	HandoffBytesMedian *float64
	SessionsMedian     *float64

	RhoNumerator   int
	RhoDenominator int
	Rho            *float64

	DependencyDensity *float64

	Rejections        int
	ConflictGateShare *float64
	UnknownShare      *float64

	ModeShift     *float64
	OMThreshold   *float64
	DominantModel *string
}

// Cell returns the cell weekly.csv carries for column: "" for None, a count
// without a decimal point, a bool as True or False, and every float through
// FormatFloat, so a float typed as 2.0 is still 2.0. The shared
// median is a float64, so an odd count's integer median is 20.0 here, where the
// weekly.csv cell holds 20. The second result is false when column is not one
// that weekly.csv carries.
func (r WeeklyMetrics) Cell(column string) (string, bool) {
	switch column {
	case "week":
		return r.Week, true
	case "rig":
		return r.Rig, true
	case "thin":
		if r.Thin {
			return "True", true
		}
		return "False", true
	case "dominant_model":
		if r.DominantModel == nil {
			return "", true
		}
		return *r.DominantModel, true
	case "landings":
		return strconv.Itoa(r.Landings), true
	case "unreadable":
		return strconv.Itoa(r.Unreadable), true
	case "manual_rescue_count":
		return strconv.Itoa(r.ManualRescueCount), true
	case "handoff_bytes_total":
		return strconv.Itoa(r.HandoffBytesTotal), true
	case "rho_numerator":
		return strconv.Itoa(r.RhoNumerator), true
	case "rho_denominator":
		return strconv.Itoa(r.RhoDenominator), true
	case "rejections":
		return strconv.Itoa(r.Rejections), true
	}

	var v *float64
	switch column {
	case "size_median":
		v = r.SizeMedian
	case "size_p90":
		v = r.SizeP90
	case "file_entropy_mean":
		v = r.FileEntropyMean
	case "package_entropy_mean":
		v = r.PackageEntropyMean
	case "attempts_mean":
		v = r.AttemptsMean
	case "rejected_share":
		v = r.RejectedShare
	case "first_try_share":
		v = r.FirstTryShare
	case "h_labels":
		v = r.HLabels
	case "h_rigs":
		v = r.HRigs
	case "h_types":
		v = r.HTypes
	case "handoff_bytes_median":
		v = r.HandoffBytesMedian
	case "sessions_median":
		v = r.SessionsMedian
	case "rho":
		v = r.Rho
	case "dependency_density":
		v = r.DependencyDensity
	case "conflict_gate_share":
		v = r.ConflictGateShare
	case "unknown_share":
		v = r.UnknownShare
	case "mode_shift":
		v = r.ModeShift
	case "om_threshold":
		v = r.OMThreshold
	default:
		return "", false
	}
	if v == nil {
		return "", true
	}
	return FormatFloat(*v), true
}

// EntropyFigures is LabelEntropy's answer: three separate figures, never
// blended, in bits.
type EntropyFigures struct {
	HLabels float64
	HRigs   float64
	HTypes  float64
}

// counts is how many times each item appears, one entry
// per distinct item in first-appearance order.
func counts(items []string) []float64 {
	seen := make(map[string]int, len(items))
	order := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; !ok {
			order = append(order, item)
		}
		seen[item]++
	}
	out := make([]float64, len(order))
	for i, item := range order {
		out[i] = float64(seen[item])
	}
	return out
}

// LabelEntropy is the entropy of the beads' labels, rigs and types. A label in
// rare (fewer than three uses overall) folds into "other".
func LabelEntropy(beads []Bead, rare map[string]bool) EntropyFigures {
	var labels, rigs, types []string
	for _, b := range beads {
		for _, label := range b.Labels {
			if rare[label] {
				label = "other"
			}
			labels = append(labels, label)
		}
		rigs = append(rigs, b.Rig)
		types = append(types, b.Type)
	}
	return EntropyFigures{
		HLabels: Entropy(counts(labels)),
		HRigs:   Entropy(counts(rigs)),
		HTypes:  Entropy(counts(types)),
	}
}

// DependencyDensity is the mean dependency count of the closed beads, nil when
// none are closed.
func DependencyDensity(closed []Bead) *float64 {
	if len(closed) == 0 {
		return nil
	}
	sum := 0
	for _, b := range closed {
		sum += b.DependencyCount
	}
	v := float64(sum) / float64(len(closed))
	return &v
}

// RhoFigure is Rho's answer. Both parts are published beside the ratio, so a
// small denominator is visible; a nil Rho means no daemon
// landing.
type RhoFigure struct {
	Numerator   int
	Denominator int
	Rho         *float64
}

// Rho is (non-daemon landings + handoff and crew beads closed) / daemon
// landings.
func Rho(landings []Row, handoffClosed int) RhoFigure {
	daemon := 0
	for _, l := range landings {
		if l["route"] == "daemon" {
			daemon++
		}
	}
	f := RhoFigure{Numerator: len(landings) - daemon + handoffClosed, Denominator: daemon}
	if daemon != 0 {
		v := float64(f.Numerator) / float64(f.Denominator)
		f.Rho = &v
	}
	return f
}

// HandoffSizeFigure is HandoffSize's answer; a nil pointer means the id is
// absent from that map.
type HandoffSizeFigure struct {
	HandoffBytesTotal  int
	HandoffBytesMedian *float64
	SessionsMedian     *float64
}

// HandoffSize is the total and median handoff bytes and the median session
// count over the closed handoff beads both maps know.
func HandoffSize(closedHandoffIDs []string, handoff, sessions map[string]int) HandoffSizeFigure {
	var sizes, sess []float64
	for _, id := range closedHandoffIDs {
		if v, ok := handoff[id]; ok {
			sizes = append(sizes, float64(v))
		}
		if v, ok := sessions[id]; ok {
			sess = append(sess, float64(v))
		}
	}
	f := HandoffSizeFigure{}
	for _, v := range sizes {
		f.HandoffBytesTotal += int(v)
	}
	if len(sizes) > 0 {
		sort.Float64s(sizes)
		f.HandoffBytesMedian = median(sizes)
	}
	if len(sess) > 0 {
		sort.Float64s(sess)
		f.SessionsMedian = median(sess)
	}
	return f
}

// mode is the most common value, ties to the smallest, nil
// when there are no values. Callers leave out a value the row does not carry.
func mode[T cmp.Ordered](values []T) *T {
	if len(values) == 0 {
		return nil
	}
	freq := make(map[T]int, len(values))
	for _, v := range values {
		freq[v]++
	}
	var best T
	bestCount := -1
	for v, n := range freq {
		if n > bestCount || (n == bestCount && v < best) {
			best, bestCount = v, n
		}
	}
	return &best
}

// ModeShift is the distance between consecutive weeks in z-scored columns: the
// root of the summed squared changes, each divided by its column's standard
// deviation. The scale comes from non-thin weeks only, so a noisy thin week
// cannot inflate it. An incomplete or non-consecutive week (when weeks is
// given) is nil, as is the first week; an empty series is one nil.
//
// A column a week has no value for is absent from its map, or nil; the two
// read the same.
func ModeShift(series []map[string]*float64, thin []bool, weeks []string) ([]*float64, error) {
	if len(thin) != len(series) {
		return nil, fmt.Errorf("mode_shift: %d weeks in series, %d thin flags", len(series), len(thin))
	}
	if weeks != nil && len(weeks) != len(series) {
		return nil, fmt.Errorf("mode_shift: %d weeks in series, %d week dates", len(series), len(weeks))
	}

	colSet := map[string]bool{}
	for _, row := range series {
		for c := range row {
			colSet[c] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		cols = append(cols, c)
	}
	sort.Strings(cols)

	sd := make(map[string]float64, len(cols))
	for _, c := range cols {
		var vals []float64
		for i, row := range series {
			if thin[i] {
				continue
			}
			if v := row[c]; v != nil {
				vals = append(vals, *v)
			}
		}
		s := 0.0
		if len(vals) > 1 {
			s = pstdev(vals)
		}
		if s == 0 {
			s = 1
		}
		sd[c] = s
	}

	out := make([]*float64, len(series))
	if len(out) == 0 {
		out = []*float64{nil}
	}
	for i := 1; i < len(series); i++ {
		a, b := series[i-1], series[i]
		gap := false
		if weeks != nil {
			d, err := days(weeks[i-1], weeks[i])
			if err != nil {
				return nil, err
			}
			gap = d != 7
		}
		if gap || thin[i] || thin[i-1] {
			continue
		}
		missing := false
		for _, c := range cols {
			if a[c] == nil || b[c] == nil {
				missing = true
				break
			}
		}
		if missing {
			continue
		}
		sum := 0.0
		for _, c := range cols {
			d := (*b[c] - *a[c]) / sd[c]
			sum += d * d
		}
		v := math.Sqrt(sum)
		out[i] = &v
	}
	return out, nil
}

// emptyLandingSummary is the summary of a week known only from
// beads has no landings, so it is thin and every size and share is nil.
func emptyLandingSummary() LandingSummary {
	return LandingSummary{Thin: true}
}

// landingMode is the mode of a landing column: the most common non-empty cell,
// parsed as a number because values compare by type, so a tie goes to
// the smallest number rather than the smallest text.
func landingMode(rows []Row, column string) (*float64, error) {
	values := make([]float64, 0, len(rows))
	for i, r := range rows {
		v, err := optionalFloat(r, column)
		if err != nil {
			return nil, fmt.Errorf("landings row %d: %w", i+2, err)
		}
		if v != nil {
			values = append(values, *v)
		}
	}
	return mode(values), nil
}

// landingName is _mode over a landing column's non-empty cells.
func landingName(rows []Row, column string) *string {
	var values []string
	for _, r := range rows {
		if v := r[column]; v != "" {
			values = append(values, v)
		}
	}
	return mode(values)
}

// BuildWeekly joins the landing summaries, the beads created and closed, the
// handoff sizes and the rejection kinds into one row per (week, rig) and
// (week, "ALL"), sorted by rig then week. A week known only from beads still
// gets a row, and that row is thin with no landings. Bead rows carry a rig name
// (not a prefix), like landings do.
func BuildWeekly(landings []Row, beads []Bead, handoff, sessions map[string]int,
	rejectionKindsByWeek map[WeekRig][]string, rareLabels map[string]bool) ([]WeeklyMetrics, error) {
	byKey, err := WeeklyLandingMetrics(landings)
	if err != nil {
		return nil, err
	}

	landRows := make(map[WeekRig][]Row, len(landings)*2)
	for _, l := range landings {
		for _, rig := range []string{l["rig"], "ALL"} {
			key := WeekRig{Week: l["week"], Rig: rig}
			landRows[key] = append(landRows[key], l)
		}
	}

	created := make(map[WeekRig][]Bead)
	closed := make(map[WeekRig][]Bead)
	for _, b := range beads {
		for _, rig := range []string{b.Rig, "ALL"} {
			if b.CreatedAt != "" {
				week, err := WeekOf(b.CreatedAt)
				if err != nil {
					return nil, fmt.Errorf("bead %q created_at: %w", b.ID, err)
				}
				key := WeekRig{Week: week, Rig: rig}
				created[key] = append(created[key], b)
			}
			if b.ClosedAt != "" {
				week, err := WeekOf(b.ClosedAt)
				if err != nil {
					return nil, fmt.Errorf("bead %q closed_at: %w", b.ID, err)
				}
				key := WeekRig{Week: week, Rig: rig}
				closed[key] = append(closed[key], b)
			}
		}
	}

	keys := make([]WeekRig, 0, len(byKey)+len(created)+len(closed))
	seen := make(map[WeekRig]bool, cap(keys))
	addKey := func(key WeekRig) {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for key := range byKey {
		addKey(key)
	}
	for key := range created {
		addKey(key)
	}
	for key := range closed {
		addKey(key)
	}

	rows := make([]WeeklyMetrics, 0, len(keys))
	for _, key := range keys {
		s, hasLandings := byKey[key]
		if !hasLandings {
			s = emptyLandingSummary()
		}
		ls := landRows[key]
		made, done := created[key], closed[key]
		var handoffDone []Bead
		for _, b := range done {
			if slices.Contains(b.Labels, "handoff") || slices.Contains(b.Labels, "crew") {
				handoffDone = append(handoffDone, b)
			}
		}

		row := WeeklyMetrics{
			Week:               key.Week,
			Rig:                key.Rig,
			Thin:               s.Thin,
			Landings:           s.Landings,
			Unreadable:         s.Unreadable,
			SizeMedian:         s.SizeMedian,
			SizeP90:            s.SizeP90,
			FileEntropyMean:    s.FileEntropyMean,
			PackageEntropyMean: s.PackageEntropyMean,
			AttemptsMean:       s.AttemptsMean,
			ManualRescueCount:  s.ManualRescueCount,
		}
		if hasLandings {
			row.RejectedShare = &s.RejectedShare
			row.FirstTryShare = &s.FirstTryShare
		}

		if len(made) > 0 {
			figures := LabelEntropy(made, rareLabels)
			row.HLabels, row.HRigs, row.HTypes = &figures.HLabels, &figures.HRigs, &figures.HTypes
		}

		var handoffIDs []string
		for _, b := range handoffDone {
			if slices.Contains(b.Labels, "handoff") {
				handoffIDs = append(handoffIDs, b.ID)
			}
		}
		size := HandoffSize(handoffIDs, handoff, sessions)
		row.HandoffBytesTotal = size.HandoffBytesTotal
		row.HandoffBytesMedian = size.HandoffBytesMedian
		row.SessionsMedian = size.SessionsMedian

		r := Rho(ls, len(handoffDone))
		row.RhoNumerator, row.RhoDenominator, row.Rho = r.Numerator, r.Denominator, r.Rho
		row.DependencyDensity = DependencyDensity(done)

		nc := NonConservative(rejectionKindsByWeek[key])
		row.Rejections, row.ConflictGateShare, row.UnknownShare = nc.Rejections, nc.ConflictGateShare, nc.UnknownShare

		row.OMThreshold, err = landingMode(ls, "om_threshold")
		if err != nil {
			return nil, err
		}
		row.DominantModel = landingName(ls, "dominant_model")
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Rig != rows[j].Rig {
			return rows[i].Rig < rows[j].Rig
		}
		return rows[i].Week < rows[j].Week
	})

	// Mode shift reads one rig's weeks in the sorted order, so each rig's rows
	// are contiguous; the town's "ALL" rows are a rig like any other.
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].Rig == rows[start].Rig {
			end++
		}
		mine := rows[start:end]
		series := make([]map[string]*float64, len(mine))
		thin := make([]bool, len(mine))
		weeks := make([]string, len(mine))
		for i, row := range mine {
			series[i] = make(map[string]*float64, len(shiftColumns))
			for _, c := range shiftColumns {
				series[i][c] = row.shiftColumn(c)
			}
			thin[i] = row.Thin
			weeks[i] = row.Week
		}
		shifts, err := ModeShift(series, thin, weeks)
		if err != nil {
			return nil, err
		}
		for i, s := range shifts {
			mine[i].ModeShift = s
		}
		start = end
	}
	return rows, nil
}

// shiftColumn is the value ModeShift reads for one of its three columns.
func (r WeeklyMetrics) shiftColumn(column string) *float64 {
	switch column {
	case "size_median":
		return r.SizeMedian
	case "file_entropy_mean":
		return r.FileEntropyMean
	case "rejected_share":
		return r.RejectedShare
	}
	return nil
}
