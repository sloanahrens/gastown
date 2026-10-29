package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The landing-side weekly metrics: size, rework and rejections per (week, rig)
// and per week across the town.

// WeekRig is the group a landing belongs to; a rig of "ALL" is the town's row.
type WeekRig struct {
	Week string
	Rig  string
}

// LandingSummary is one group's figures; a nil pointer is the Python's None, a
// figure no row could supply.
type LandingSummary struct {
	Landings           int
	Unreadable         int
	SizeMedian         *float64
	SizeP90            *float64
	FileEntropyMean    *float64
	PackageEntropyMean *float64
	AttemptsMean       *float64
	RejectedShare      float64
	FirstTryShare      float64
	ManualRescueCount  int
	Thin               bool
}

// Percentile is the linearly interpolated p-quantile of ascending values; it
// needs at least one value, as the Python indexed into an empty list.
func Percentile(sortedValues []float64, p float64) float64 {
	if len(sortedValues) == 1 {
		return sortedValues[0]
	}
	k := float64(len(sortedValues)-1) * p
	lo := int(k)
	hi := lo + 1
	if last := len(sortedValues) - 1; hi > last {
		hi = last
	}
	return sortedValues[lo] + (sortedValues[hi]-sortedValues[lo])*(k-float64(lo))
}

// parsedLanding is the cells of one CSV row these metrics read, parsed once so
// a row feeding both its rig's group and the town's is not parsed twice.
type parsedLanding struct {
	week, rig                                    string
	linesTotal, fileEntropy, packageEntropy      *float64
	attempts, rejected, manualRescue, unreadable int
}

// parseLanding reads the cells an unreadable landing leaves empty as the
// Python's None, and refuses a cell that is neither empty nor a number: a
// silent zero would publish a figure the file did not hold.
func parseLanding(r Row) (parsedLanding, error) {
	l := parsedLanding{week: r["week"], rig: r["rig"]}
	var err error
	if l.linesTotal, err = optionalFloat(r, "lines_total"); err != nil {
		return parsedLanding{}, err
	}
	if l.fileEntropy, err = optionalFloat(r, "file_entropy"); err != nil {
		return parsedLanding{}, err
	}
	if l.packageEntropy, err = optionalFloat(r, "package_entropy"); err != nil {
		return parsedLanding{}, err
	}
	if l.attempts, err = rowInt(r, "attempts"); err != nil {
		return parsedLanding{}, err
	}
	if l.rejected, err = rowInt(r, "rejected_before_landing"); err != nil {
		return parsedLanding{}, err
	}
	if l.manualRescue, err = rowInt(r, "manual_rescue"); err != nil {
		return parsedLanding{}, err
	}
	l.unreadable, err = rowInt(r, "unreadable")
	return l, err
}

// optionalFloat reads a cell, returning nil where the cell is empty.
func optionalFloat(r Row, column string) (*float64, error) {
	cell := r[column]
	if cell == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(cell, 64)
	if err != nil {
		return nil, cellError(r, column, cell)
	}
	return &v, nil
}

// rowInt reads a cell that must be an integer, as the Python's int() did.
func rowInt(r Row, column string) (int, error) {
	cell := r[column]
	v, err := strconv.Atoi(cell)
	if err != nil {
		return 0, cellError(r, column, cell)
	}
	return v, nil
}

func cellError(r Row, column, cell string) error {
	return fmt.Errorf("landing %q: %s %q is not a number", r["bead"], column, cell)
}

// mean is the arithmetic mean of xs, or nil when it has no values.
func mean(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	m := sum / float64(len(xs))
	return &m
}

// median is statistics.median: the middle value, or the mean of the two middle
// ones; it needs at least one value, as the Python raised for an empty list.
func median(sortedValues []float64) *float64 {
	n := len(sortedValues)
	var v float64
	if n%2 == 1 {
		v = sortedValues[n/2]
	} else {
		v = (sortedValues[n/2-1] + sortedValues[n/2]) / 2
	}
	return &v
}

// summarize is metrics.py's private group summary, spelled with the linter's US
// spelling. It needs at least one row: rejections are a share of the group.
func summarize(rows []parsedLanding) LandingSummary {
	var sizes, fileEntropy, packageEntropy []float64
	rejected := 0
	s := LandingSummary{}
	for _, r := range rows {
		if r.linesTotal != nil {
			sizes = append(sizes, *r.linesTotal)
		}
		if r.fileEntropy != nil {
			fileEntropy = append(fileEntropy, *r.fileEntropy)
		}
		if r.packageEntropy != nil {
			packageEntropy = append(packageEntropy, *r.packageEntropy)
		}
		rejected += r.rejected
		s.Unreadable += r.unreadable
		s.ManualRescueCount += r.manualRescue
	}
	sort.Float64s(sizes)
	n := float64(len(rows))
	s.Landings = len(rows)
	s.Thin = len(rows) < 10
	s.RejectedShare = float64(rejected) / n
	s.FirstTryShare = 1 - float64(rejected)/n
	if len(sizes) > 0 {
		s.SizeMedian = median(sizes)
		p90 := Percentile(sizes, 0.9)
		s.SizeP90 = &p90
	}
	s.FileEntropyMean = mean(fileEntropy)
	s.PackageEntropyMean = mean(packageEntropy)
	attempts := make([]float64, len(rows))
	for i, r := range rows {
		attempts[i] = float64(r.attempts)
	}
	s.AttemptsMean = mean(attempts)
	return s
}

// WeeklyLandingMetrics summarizes landings per (week, rig) and per week across
// the town, under a rig of "ALL"; a row's empty diff cells are an unreadable
// commit, not a zero.
func WeeklyLandingMetrics(landings []Row) (map[WeekRig]LandingSummary, error) {
	groups := make(map[WeekRig][]parsedLanding)
	for i, row := range landings {
		l, err := parseLanding(row)
		if err != nil {
			return nil, fmt.Errorf("landings row %d: %w", i+2, err)
		}
		rig := WeekRig{Week: l.week, Rig: l.rig}
		all := WeekRig{Week: l.week, Rig: "ALL"}
		groups[rig] = append(groups[rig], l)
		groups[all] = append(groups[all], l)
	}
	out := make(map[WeekRig]LandingSummary, len(groups))
	for key, rows := range groups {
		out[key] = summarize(rows)
	}
	return out, nil
}

// rejectionKinds are the words a landing worker's MERGE REJECTION may carry.
var rejectionKinds = map[string]bool{
	"conflict": true, "gate": true, "review": true, "policy": true, "race": true,
}

// ClassifyRejection is the kind word the landing worker writes in
// "MERGE REJECTION (attempt N): <kind>"; anything else is "unknown".
func ClassifyRejection(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	if rejectionKinds[k] {
		return k
	}
	return "unknown"
}

// NonConservativeCount is NonConservative's answer; a nil pointer is the
// Python's None, a share no rejection could supply.
type NonConservativeCount struct {
	Rejections        int
	ConflictGateShare *float64
	UnknownShare      *float64
}

// NonConservative counts the rejections and the shares that were a conflict or
// a gate, and that were unknown.
func NonConservative(kinds []string) NonConservativeCount {
	if len(kinds) == 0 {
		return NonConservativeCount{}
	}
	conflictGate, unknown := 0, 0
	for _, k := range kinds {
		if k == "conflict" || k == "gate" {
			conflictGate++
		}
		if k == "unknown" {
			unknown++
		}
	}
	n := float64(len(kinds))
	conflictGateShare := float64(conflictGate) / n
	unknownShare := float64(unknown) / n
	return NonConservativeCount{
		Rejections:        len(kinds),
		ConflictGateShare: &conflictGateShare,
		UnknownShare:      &unknownShare,
	}
}
