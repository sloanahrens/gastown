// Package scorecard reads a Jev battery's JSONL log through a per-battery
// adapter and reports, for each question, how Jev's answers agree with the
// labels the adapter finds. It only reads the log and the adapter's sources.
package scorecard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
)

// sample is one labeled answer to one question.
type sample struct {
	value  float64
	chosen bool // a choice's answer picked its positive option
	label  bool
}

// Build reads the adapter's log, keeps the items stamped at or after since (a
// zero since keeps all), and reports each question. A line that does not parse
// is skipped and counted; a missing log is an empty report.
func Build(ctx context.Context, a Adapter, since time.Time) (Report, error) {
	items, skipped, err := readItems(a.LogPath(), a.Parse, since)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Battery: a.Battery(), Items: len(items), LinesSkipped: skipped}
	if !since.IsZero() {
		rep.Since = &since
	}

	labels := make([]map[QuestionID]bool, len(items))
	for i, it := range items {
		m, labeled, err := a.Label(ctx, it)
		if err != nil {
			return Report{}, fmt.Errorf("labeling item %s: %w", it.ID, err)
		}
		if labeled {
			labels[i] = m
		}
	}

	kinds := questionKinds(items)
	ids := make([]QuestionID, 0, len(kinds))
	for id := range kinds {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		rep.Questions = append(rep.Questions, scoreQuestion(id, kinds[id], items, labels))
	}
	return rep, nil
}

func readItems(path string, parse func([]byte) (Item, error), since time.Time) ([]Item, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("opening the log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var items []Item
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		it, err := parse(raw)
		if err != nil {
			skipped++
			continue
		}
		if !since.IsZero() && it.Time.Before(since) {
			continue
		}
		items = append(items, it)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading the log: %w", err)
	}
	return items, skipped, nil
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

func scoreQuestion(id QuestionID, kind string, items []Item, labels []map[QuestionID]bool) QuestionReport {
	qr := QuestionReport{ID: string(id), Kind: kind}
	var samples []sample
	for i, it := range items {
		ans, ok := it.Answers[id]
		if !ok {
			continue
		}
		value, chosen, ok := usable(ans, kind)
		if !ok {
			continue
		}
		qr.Logged++
		label, ok := labels[i][id]
		if !ok {
			continue
		}
		qr.Labeled++
		samples = append(samples, sample{value: value, chosen: chosen, label: label})
		if label {
			qr.Positives++
		} else {
			qr.Negatives++
		}
	}

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
	case KindScore:
		qr.MeanScorePositive, qr.MeanScoreNegative = meanByLabel(samples)
	}
	qr.Verdict = verdict(qr.Labeled, best, base)
	return qr
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

func binOf(v float64) int {
	return max(0, min(int(v*nBins), nBins-1))
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

// verdict is earning when the best F1 beats the base rate by the margin on at
// least minLabeled lines, not-yet below that, and no-signal otherwise.
func verdict(labeled int, bestF1, baseRate float64) string {
	switch {
	case labeled < minLabeled:
		return VerdictNotYet
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
