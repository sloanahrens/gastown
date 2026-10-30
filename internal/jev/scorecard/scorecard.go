// Package scorecard reads a Jev battery's JSONL log through a per-battery
// adapter and reports, for each question, how Jev's answers agree with the
// labels the adapter finds. It only reads the log and the adapter's sources.
package scorecard

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"time"
)

// Question kinds, taken from which answer field is set.
const (
	KindNoul   = "noul"
	KindChoice = "choice"
	KindScore  = "score"
)

// Verdicts for one question.
const (
	VerdictEarning  = "earning"
	VerdictNotYet   = "not-yet"
	VerdictNoSignal = "no-signal"
)

// The three floors below are judgement calls, not measurements, and they sit
// together so they are easy to change. The acting threshold is a separate,
// later decision (gt-1nxhm).
const (
	// minPositives is a judgement call, vetted 2026-10-29 as the floor for "not worth reading as a signal": finding.any_duplicate read earning on 615 labeled lines with only 3 positives (gt-1nxhm).
	minPositives = 10
	// minNegatives is the same judgement as minPositives, guarding the label a precision needs as its denominator (gt-1nxhm).
	minNegatives = 10
	// minPrecisionAtBest is a judgement call, vetted 2026-10-29 as the floor for "not worth reading as a signal": finding.any_duplicate (0.018) and finding.report_only (0.053) read earning (gt-1nxhm).
	minPrecisionAtBest = 0.3
	// minFlaggedAtBest is the same judgement as minPositives, guarding the count a precision is measured on: finding.report_only read earning on a precision of 1.0 measured over one flagged item, and a precision over fewer than ten labeled items is an anecdote (gt-dx8t2).
	minFlaggedAtBest = 10
)

const (
	// minLabeled is how many labeled lines a question needs before it can
	// earn; fewer is not-yet, whatever the metrics say.
	minLabeled = 30
	// earnMargin is how far the best F1 must beat the label base rate.
	earnMargin = 0.1
	// eps absorbs float error at the margin, where 0.6 - 0.5 is not exactly 0.1.
	eps = 1e-9
	// noulCut is the probability at which a noul counts as a yes.
	noulCut = 0.5
	// nBins is the number of equal-width calibration bands over 0 to 1.
	nBins = 5
	// minMonotoneBand is how many labeled lines a band needs before the
	// monotone flag reads its label rate: a rate over a handful of lines moves
	// by chance, so a fall read there is not one a reader can act on (gt-km2b8).
	minMonotoneBand = 10
)

// sample is one labeled answer to one question.
type sample struct {
	value  float64
	chosen bool // a choice's answer picked its positive option
	label  bool
}

// Reading is the parse-and-label pass over a battery's log: the items the log
// holds and what the adapter labels each one, before any question is scored.
type Reading struct {
	// Items are the log's items stamped at or after the window's start, in the
	// order the log wrote them.
	Items []Item
	// Labelings holds item i's labeling at index i.
	Labelings []Labeling
	// LinesSkipped counts the log lines that did not parse.
	LinesSkipped int
	// LogMissing is set when the battery has no log yet.
	LogMissing bool
}

// Read parses the adapter's log and labels every item, the pass Build scores
// and a caller that needs the answers themselves reads instead.
func Read(ctx context.Context, a Adapter, since time.Time) (Reading, error) {
	items, skipped, found, err := readItems(a.LogPath(), parseLines(a), since)
	if err != nil {
		return Reading{}, err
	}
	if p, ok := a.(Prefetcher); ok {
		p.Prefetch(ctx, items)
	}
	labelings := make([]Labeling, len(items))
	for i, it := range items {
		lab, err := a.Label(ctx, it)
		if err != nil {
			return Reading{}, fmt.Errorf("labeling item %s: %w", it.ID, err)
		}
		labelings[i] = lab
	}
	return Reading{Items: items, Labelings: labelings, LinesSkipped: skipped, LogMissing: !found}, nil
}

// Build reads the adapter's log, keeps the items stamped at or after since (a
// zero since keeps all), and reports each question. A line that does not parse
// is skipped and counted; a missing log is an empty report that says so.
func Build(ctx context.Context, a Adapter, since time.Time) (Report, error) {
	read, err := Read(ctx, a, since)
	if err != nil {
		return Report{}, err
	}
	items, labelings := read.Items, read.Labelings
	rep := Report{Battery: a.Battery(), Items: len(items), LinesSkipped: read.LinesSkipped, LogMissing: read.LogMissing, Questions: []QuestionReport{}}
	if !since.IsZero() {
		rep.Since = &since
	}

	labels := make([]map[QuestionID]bool, len(items))
	sources := make([]string, len(items))
	rep.Lines = make([]LineReport, 0, len(items))
	for i, it := range items {
		lab := labelings[i]
		if lab.Labeled {
			labels[i] = lab.Labels
			sources[i] = lab.Source
			rep.Labeled++
		}
		rep.Lines = append(rep.Lines, lineReport(it, lab))
	}

	if c, ok := a.(Contextual); ok {
		rep.Context = c.Context()
	}

	kinds := questionKinds(items)
	ids := make([]QuestionID, 0, len(kinds))
	for id := range kinds {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		rep.Questions = append(rep.Questions, scoreQuestion(id, kinds[id], items, labels, sources))
	}
	return rep, nil
}

// parseLines reads a line into its items: an Expander's ParseLines, or the one
// item its Parse returns.
func parseLines(a Adapter) func([]byte) ([]Item, error) {
	if e, ok := a.(Expander); ok {
		return e.ParseLines
	}
	return func(raw []byte) ([]Item, error) {
		it, err := a.Parse(raw)
		if err != nil {
			return nil, err
		}
		return []Item{it}, nil
	}
}

// readItems returns the items in the log at path. found is false when there is
// no log at all, which is not an error.
func readItems(path string, parse func([]byte) ([]Item, error), since time.Time) (items []Item, skipped int, found bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("opening the log: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		its, err := parse(raw)
		if err != nil {
			skipped++
			continue
		}
		for _, it := range its {
			if !since.IsZero() && it.Time.Before(since) {
				continue
			}
			items = append(items, it)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("reading the log: %w", err)
	}
	return items, skipped, true, nil
}

// questionKinds maps each question to the kind of its first usable answer. A
// question with no answer of any kind is left out.
func questionKinds(items []Item) map[QuestionID]string {
	kinds := map[QuestionID]string{}
	for _, it := range items {
		for id, ans := range it.Answers {
			if _, seen := kinds[id]; seen {
				continue
			}
			if k := kindOf(ans); k != "" {
				kinds[id] = k
			}
		}
	}
	return kinds
}

func kindOf(ans Answer) string {
	switch {
	case ans.Noul != nil:
		return KindNoul
	case ans.Score != nil:
		return KindScore
	case ans.Choice != "" || ans.Positive != "":
		return KindChoice
	}
	return ""
}

// usable reads the value a question of kind scores an answer by: a noul's
// probability, a score's level, or a choice's probability for its positive
// option. chosen is whether a choice picked that option.
func usable(ans Answer, kind string) (value float64, chosen, ok bool) {
	switch kind {
	case KindNoul:
		if ans.Noul != nil {
			return *ans.Noul, false, true
		}
	case KindScore:
		if ans.Score != nil {
			return *ans.Score, false, true
		}
	case KindChoice:
		if ans.Positive == "" {
			return 0, false, false
		}
		p, ok := ans.Probabilities[ans.Positive]
		if !ok {
			return 0, false, false
		}
		return p, ans.Choice == ans.Positive, true
	}
	return 0, false, false
}

func scoreQuestion(id QuestionID, kind string, items []Item, labels []map[QuestionID]bool, sources []string) QuestionReport {
	qr := QuestionReport{ID: string(id), Kind: kind}
	var samples []sample
	contextLines := map[string]int{}
	sourceCounts := map[string]SourceCount{}
	for i, it := range items {
		label, labeled := labels[i][id]
		ans, answered := it.Answers[id]
		value, chosen, ok := usable(ans, kind)
		if !answered || !ok {
			// A label with nothing to score it against is a disagreement
			// between the log and the adapter, so it is counted.
			if labeled {
				qr.Unmatched++
			}
			continue
		}
		qr.Logged++
		if c := it.Context[id]; c != "" {
			contextLines[c]++
		}
		if !labeled {
			continue
		}
		qr.Labeled++
		samples = append(samples, sample{value: value, chosen: chosen, label: label})
		if label {
			qr.Positives++
		} else {
			qr.Negatives++
		}
		if src := sources[i]; src != "" {
			c := sourceCounts[src]
			c.Source = src
			c.Labeled++
			if label {
				c.Positives++
			}
			sourceCounts[src] = c
		}
	}
	qr.Sources = sourceCountList(sourceCounts)

	var base, best float64
	if qr.Labeled > 0 {
		base = float64(qr.Positives) / float64(qr.Labeled)
		qr.BaseRate = &base
	}
	if thr, f1, ok := bestThreshold(samples); ok {
		qr.BestThreshold = &thr
		qr.BestF1 = &f1
		best = f1
	}

	switch kind {
	case KindNoul, KindChoice:
		qr.Precision, qr.Recall = cutMetrics(kind, samples)
		qr.Calibration = calibration(samples)
		qr.ECE, qr.Brier, qr.CalibrationMonotone = calibrationMetrics(qr.Calibration, samples)
		if qr.BestThreshold != nil {
			qr.PrecisionAtBest, qr.FlaggedAtBest = precisionAtCut(samples, *qr.BestThreshold)
		}
	case KindScore:
		qr.MeanScorePositive, qr.MeanScoreNegative = meanByLabel(samples)
	}
	qr.Verdict = verdict(kind, qr.Positives, qr.Negatives, qr.Labeled, best, base, qr.PrecisionAtBest, qr.FlaggedAtBest)
	qr.Context = contextCounts(contextLines)
	return qr
}

// sourceCountList lists the label sources in source order, or nil when no
// labeled line named one.
func sourceCountList(counts map[string]SourceCount) []SourceCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]SourceCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b SourceCount) int { return cmp.Compare(a.Source, b.Source) })
	return out
}

// contextCounts lists the context values in value order, or nil when no line
// carried one.
func contextCounts(lines map[string]int) []ContextCount {
	if len(lines) == 0 {
		return nil
	}
	out := make([]ContextCount, 0, len(lines))
	for v, n := range lines {
		out = append(out, ContextCount{Value: v, Lines: n})
	}
	slices.SortFunc(out, func(a, b ContextCount) int { return cmp.Compare(a.Value, b.Value) })
	return out
}

// bestThreshold sweeps every distinct value as a cut, predicting a yes at or
// above it, and returns the cut with the best F1. Values are walked from the
// top down with a strict comparison, so a tie keeps the higher cut.
func bestThreshold(samples []sample) (thr, f1 float64, ok bool) {
	if len(samples) == 0 {
		return 0, 0, false
	}
	byValue := slices.Clone(samples)
	slices.SortFunc(byValue, func(a, b sample) int {
		switch {
		case a.value > b.value:
			return -1
		case a.value < b.value:
			return 1
		}
		return 0
	})
	positives := 0
	for _, s := range samples {
		if s.label {
			positives++
		}
	}

	best := -1.0
	tp, fp := 0, 0
	for i := 0; i < len(byValue); {
		v := byValue[i].value
		for ; i < len(byValue) && byValue[i].value == v; i++ {
			if byValue[i].label {
				tp++
			} else {
				fp++
			}
		}
		if f := f1Score(tp, fp, positives-tp); f > best {
			best, thr = f, v
		}
	}
	return thr, best, true
}

func f1Score(tp, fp, fn int) float64 {
	if 2*tp+fp+fn == 0 {
		return 0
	}
	return float64(2*tp) / float64(2*tp+fp+fn)
}

// cutMetrics scores a noul at 0.5 and a choice at the option it chose.
func cutMetrics(kind string, samples []sample) (precision, recall *float64) {
	var tp, fp, fn int
	for _, s := range samples {
		predicted := s.chosen
		if kind == KindNoul {
			predicted = s.value >= noulCut
		}
		switch {
		case predicted && s.label:
			tp++
		case predicted:
			fp++
		case s.label:
			fn++
		}
	}
	return ratio(tp, tp+fp), ratio(tp, tp+fn)
}

// precisionAtCut is the precision of a yes at or above thr, the same prediction
// bestThreshold's sweep makes, over the same samples cutMetrics scores, and
// flagged is how many labeled items that cut predicts yes. Precision is nil
// when the cut predicts no yes, since precision has no value then.
func precisionAtCut(samples []sample, thr float64) (precision *float64, flagged int) {
	var tp, fp int
	for _, s := range samples {
		if s.value < thr {
			continue
		}
		if s.label {
			tp++
		} else {
			fp++
		}
	}
	return ratio(tp, tp+fp), tp + fp
}

// calibration bins the labeled values into equal bands. Mean value is over the
// band's labeled lines, so the label rate is read against the same lines.
func calibration(samples []sample) []Bin {
	type acc struct {
		n, positives int
		sum          float64
	}
	var bins [nBins]acc
	for _, s := range samples {
		b := &bins[binOf(s.value)]
		b.n++
		b.sum += s.value
		if s.label {
			b.positives++
		}
	}
	out := make([]Bin, nBins)
	for i, b := range bins {
		out[i] = Bin{Lo: float64(i) / nBins, Hi: float64(i+1) / nBins, N: b.n}
		if b.n > 0 {
			out[i].MeanValue = ptr(b.sum / float64(b.n))
			out[i].LabelRate = ptr(float64(b.positives) / float64(b.n))
		}
	}
	return out
}

// calibrationMetrics reads the bins calibration returned for the same samples.
// ece is the labeled-weighted mean gap between a band's mean value and its
// label rate over every populated band; brier is the mean squared gap between
// each value and its label. monotone is true when the label rate never falls
// from one band of at least minMonotoneBand labeled lines to the next higher
// one, and nil when no band is that large, so a thin log reads no flag at all
// rather than a rising rate nobody measured. ece and brier are nil when no line
// is labeled.
func calibrationMetrics(bins []Bin, samples []sample) (ece, brier *float64, monotone *bool) {
	if len(samples) == 0 {
		return nil, nil, nil
	}
	var squared float64
	for _, s := range samples {
		y := 0.0
		if s.label {
			y = 1
		}
		squared += (s.value - y) * (s.value - y)
	}
	var gap float64
	for _, b := range bins {
		if b.N == 0 {
			continue
		}
		gap += float64(b.N) / float64(len(samples)) * math.Abs(*b.MeanValue-*b.LabelRate)
	}

	up := true
	var prev float64
	seen := false
	for _, b := range bins {
		if b.N < minMonotoneBand {
			continue
		}
		if seen && *b.LabelRate < prev {
			up = false
			break
		}
		prev, seen = *b.LabelRate, true
	}
	if !seen {
		return ptr(gap), ptr(squared / float64(len(samples))), nil
	}
	return ptr(gap), ptr(squared / float64(len(samples))), boolPtr(up)
}

func binOf(v float64) int {
	return max(0, min(int(v*nBins), nBins-1))
}

// Band names the calibration band a scored value falls in, such as "0.2-0.4":
// the bands the calibration reports, each [Lo, Hi).
func Band(v float64) string {
	i := binOf(v)
	return fmt.Sprintf("%.1f-%.1f", float64(i)/nBins, float64(i+1)/nBins)
}

func meanByLabel(samples []sample) (positive, negative *float64) {
	var pSum, nSum float64
	var pN, nN int
	for _, s := range samples {
		if s.label {
			pSum += s.value
			pN++
		} else {
			nSum += s.value
			nN++
		}
	}
	if pN > 0 {
		positive = ptr(pSum / float64(pN))
	}
	if nN > 0 {
		negative = ptr(nSum / float64(nN))
	}
	return positive, negative
}

// verdict holds earning back to a question whose labels are plentiful enough
// to act on: too few positives or negatives is not-yet whatever the metrics
// say, and a noul or choice whose best cut flags too few labeled items to
// measure a precision on is not-yet too, since that precision is an anecdote.
// A noul or choice whose precision at the best cut is under the floor or
// cannot be read is no-signal, since a yes it answers cannot be trusted. A
// score kind has no precision, so only the count floors apply to it.
func verdict(kind string, positives, negatives, labeled int, bestF1, baseRate float64, precisionAtBest *float64, flaggedAtBest int) string {
	switch {
	case positives < minPositives || negatives < minNegatives:
		return VerdictNotYet
	case kind != KindScore && precisionAtBest != nil && flaggedAtBest < minFlaggedAtBest:
		return VerdictNotYet
	case labeled < minLabeled:
		return VerdictNotYet
	case kind != KindScore && (precisionAtBest == nil || *precisionAtBest < minPrecisionAtBest-eps):
		return VerdictNoSignal
	case bestF1-baseRate >= earnMargin-eps:
		return VerdictEarning
	}
	return VerdictNoSignal
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	return ptr(float64(num) / float64(den))
}

func ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
