// Package noise measures how far Jev's answers move when a sampled group is
// asked again with the same state. It pairs each repeat in a battery's repeat
// log with the original answer it repeats and reports the spread per question.
package noise

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

// logtriageName is the battery whose log the repeat log sits beside. Only it
// writes repeats.
const logtriageName = "logtriage"

// MinRepeats is how many repeats an item needs to count. Fewer leave its spread
// too thin to read, so the item is skipped and counted.
const MinRepeats = 3

// DefaultCut is the cut a noul or score answer splits at when --cut is not set.
// It is the midpoint of a noul's probability, so the default reads as a yes or
// no line.
const DefaultCut = 0.5

// Question kinds, read from which field of an answer is set.
const (
	KindNoul   = "noul"
	KindScore  = "score"
	KindChoice = "choice"
)

// Repeat is one line of a battery's repeat log in the shape this package reads:
// the answers a repeat of a sampled group gave, with the key and question
// version that pair it to its original.
type Repeat struct {
	TS              time.Time
	Key             string
	QuestionVersion int
	Answers         map[string]logtriage.Answer
}

// Options narrows and reads a report.
type Options struct {
	// Since counts only items whose original is at or after it. The zero time keeps all.
	Since time.Time
	// Cut is the threshold a noul or score answer is split at.
	Cut float64
}

// Row is one question's noise across the counted items.
type Row struct {
	Question string `json:"question"`
	Kind     string `json:"kind"`
	// Items is how many counted items answered the question.
	Items int `json:"items"`
	// MeanSD and MaxSD are the mean and largest sample standard deviation of a
	// noul or score across an item's original and repeat answers. Nil for a choice.
	MeanSD *float64 `json:"mean_sd,omitempty"`
	MaxSD  *float64 `json:"max_sd,omitempty"`
	// CrossShare is the share of items whose answers fall on both sides of the
	// cut. Nil for a choice.
	CrossShare *float64 `json:"cross_share,omitempty"`
	// Width90 is the widest answer range among the middle 90 percent of items,
	// so a band that wide around the cut covers their answers. Nil for a choice.
	Width90 *float64 `json:"width90,omitempty"`
	// Changed is the share of items where some repeat picked another option than
	// the original. Nil for a noul or score.
	Changed *float64 `json:"changed,omitempty"`
}

// Report is the noise of one battery's repeat log.
type Report struct {
	Battery string  `json:"battery"`
	Cut     float64 `json:"cut"`
	// Skipped counts sampled items with no original answer or fewer than
	// MinRepeats repeats.
	Skipped int   `json:"skipped_items"`
	Rows    []Row `json:"rows"`
	// Unpaired counts repeats older than every original of their key and question
	// version, so nothing measures them.
	Unpaired int `json:"unpaired_repeats"`
}

// Logs is what Read loads for one battery log.
type Logs struct {
	Originals []logtriage.Line
	Repeats   []Repeat
	// RepeatLogFound is false when no repeat log exists beside the battery log,
	// so no group has been asked again yet.
	RepeatLogFound bool
}

// LogPath is the battery log that Read takes for the named battery. Only
// logtriage writes repeats, so it is the only battery with a log to read.
func LogPath(name, townRoot string) (string, error) {
	if name == logtriageName {
		return logtriage.LogPath(townRoot), nil
	}
	return "", fmt.Errorf("battery %q writes no repeat log; the battery with repeats is %s", name, logtriageName)
}

// Read loads the battery log at logPath and the repeat log beside it.
func Read(logPath string) (Logs, error) {
	var out Logs
	if err := battery.ReadRecent(logPath, math.MaxInt, &out.Originals); err != nil {
		return Logs{}, fmt.Errorf("reading the battery log: %w", err)
	}
	reask := battery.ReaskPath(logPath)
	_, err := os.Stat(reask)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case err != nil:
		return Logs{}, fmt.Errorf("checking the repeat log: %w", err)
	}
	out.RepeatLogFound = true
	var lines []battery.ReaskLine
	if err := battery.ReadRecent(reask, math.MaxInt, &lines); err != nil {
		return Logs{}, fmt.Errorf("reading the repeat log: %w", err)
	}
	for _, l := range lines {
		r, err := fromReask(l)
		if err != nil {
			return Logs{}, err
		}
		out.Repeats = append(out.Repeats, r)
	}
	return out, nil
}

// fromReask converts one repeat line into the shape Compute reads. Its answers
// arrive as untyped JSON, so they are decoded again into the battery's answers.
func fromReask(l battery.ReaskLine) (Repeat, error) {
	data, err := json.Marshal(l.Answers)
	if err != nil {
		return Repeat{}, fmt.Errorf("encoding a repeat's answers: %w", err)
	}
	var answers map[string]logtriage.Answer
	if err := json.Unmarshal(data, &answers); err != nil {
		return Repeat{}, fmt.Errorf("decoding a repeat's answers: %w", err)
	}
	return Repeat{TS: l.TS, Key: l.Key, QuestionVersion: l.QuestionVersion, Answers: answers}, nil
}

// item is one sampled original and the repeats paired with it.
type item struct {
	orig    logtriage.Line
	repeats []Repeat
}

// group names the repeats' pairing: a pattern key under one question set.
type group struct {
	key     string
	version int
}

// Compute reports the noise of the originals and their repeats. A repeat pairs
// with the latest original of its key and question version at or before its
// time, so a repeat never pairs across keys or question sets and a later pass's
// sample never pairs with an earlier pass's state.
func Compute(originals []logtriage.Line, repeats []Repeat, opts Options) (Report, error) {
	rep := Report{Cut: opts.Cut, Rows: []Row{}}

	byGroup := map[group][]int{}
	for i, l := range originals {
		g := group{l.Key, l.QuestionVersion}
		byGroup[g] = append(byGroup[g], i)
	}
	for _, idx := range byGroup {
		sort.SliceStable(idx, func(a, b int) bool { return originals[idx[a]].TS.Before(originals[idx[b]].TS) })
	}

	repeatsOf := map[int][]Repeat{}
	for _, r := range repeats {
		idx := byGroup[group{r.Key, r.QuestionVersion}]
		n := sort.Search(len(idx), func(i int) bool { return originals[idx[i]].TS.After(r.TS) })
		if n == 0 {
			// No original of this key before the repeat: nothing to measure it against.
			rep.Unpaired++
			continue
		}
		repeatsOf[idx[n-1]] = append(repeatsOf[idx[n-1]], r)
	}

	var items []item
	for i, l := range originals {
		reps := repeatsOf[i]
		if len(reps) == 0 {
			continue
		}
		if !opts.Since.IsZero() && l.TS.Before(opts.Since) {
			continue
		}
		if len(l.Answers) == 0 || len(reps) < MinRepeats {
			rep.Skipped++
			continue
		}
		items = append(items, item{orig: l, repeats: reps})
	}

	kinds := map[string]string{}
	for _, it := range items {
		for _, qid := range sortedKeys(it.orig.Answers) {
			k := kindOf(it.orig.Answers[qid])
			if k == "" {
				continue
			}
			if prev, ok := kinds[qid]; ok && prev != k {
				return Report{}, fmt.Errorf("question %s was answered as %s and as %s", qid, prev, k)
			}
			kinds[qid] = k
		}
	}

	accums := map[string]*accum{}
	for _, it := range items {
		for _, qid := range sortedKeys(it.orig.Answers) {
			ans := it.orig.Answers[qid]
			switch kindOf(ans) {
			case KindNoul, KindScore:
				vals := []float64{numberOf(ans, kindOf(ans))}
				for _, r := range it.repeats {
					if ra, ok := r.Answers[qid]; ok && kindOf(ra) == kindOf(ans) {
						vals = append(vals, numberOf(ra, kindOf(ans)))
					}
				}
				if len(vals) < 2 {
					continue
				}
				accumFor(accums, qid, kindOf(ans)).addNumbers(vals, opts.Cut)
			case KindChoice:
				changed, seen := false, 0
				for _, r := range it.repeats {
					if ra, ok := r.Answers[qid]; ok && ra.Choice != "" {
						seen++
						if ra.Choice != ans.Choice {
							changed = true
						}
					}
				}
				if seen == 0 {
					continue
				}
				accumFor(accums, qid, KindChoice).addChoice(changed)
			}
		}
	}

	for _, qid := range sortedKeys(accums) {
		rep.Rows = append(rep.Rows, accums[qid].row(qid))
	}
	return rep, nil
}

// accum gathers one question's per-item readings.
type accum struct {
	kind    string
	items   int
	sds     []float64
	spans   []float64
	crosses int
	changed int
}

func accumFor(m map[string]*accum, qid, kind string) *accum {
	a := m[qid]
	if a == nil {
		a = &accum{kind: kind}
		m[qid] = a
	}
	return a
}

// addNumbers counts one noul or score item from its original and repeat values.
func (a *accum) addNumbers(vals []float64, cut float64) {
	mn, mx := vals[0], vals[0]
	for _, v := range vals[1:] {
		mn, mx = math.Min(mn, v), math.Max(mx, v)
	}
	a.items++
	a.sds = append(a.sds, sampleSD(vals))
	a.spans = append(a.spans, mx-mn)
	if mn < cut && mx >= cut {
		a.crosses++
	}
}

// addChoice counts one choice item, and whether its picked option changed.
func (a *accum) addChoice(changed bool) {
	a.items++
	if changed {
		a.changed++
	}
}

// row turns the gathered readings into the question's row.
func (a *accum) row(qid string) Row {
	r := Row{Question: qid, Kind: a.kind, Items: a.items}
	if a.kind == KindChoice {
		share := float64(a.changed) / float64(a.items)
		r.Changed = &share
		return r
	}
	var sum float64
	var maxSD float64
	for _, sd := range a.sds {
		sum += sd
		maxSD = math.Max(maxSD, sd)
	}
	mean := sum / float64(a.items)
	cross := float64(a.crosses) / float64(a.items)
	width := nearestRank(a.spans, 0.95)
	r.MeanSD, r.MaxSD, r.CrossShare, r.Width90 = &mean, &maxSD, &cross, &width
	return r
}

// kindOf names the question kind an answer holds, or "" when it holds none.
func kindOf(a logtriage.Answer) string {
	switch {
	case a.Noul != nil:
		return KindNoul
	case a.Score != nil:
		return KindScore
	case a.Choice != "":
		return KindChoice
	}
	return ""
}

// numberOf reads a noul's probability or a score's level index.
func numberOf(a logtriage.Answer, kind string) float64 {
	if kind == KindNoul {
		return *a.Noul
	}
	return *a.Score
}

// sampleSD is the sample standard deviation of vals, zero when they all agree so
// identical answers report exactly zero rather than a rounding residue.
func sampleSD(vals []float64) float64 {
	mn, mx := vals[0], vals[0]
	for _, v := range vals[1:] {
		mn, mx = math.Min(mn, v), math.Max(mx, v)
	}
	if mn == mx {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var ss float64
	for _, v := range vals {
		ss += (v - mean) * (v - mean)
	}
	return math.Sqrt(ss / float64(len(vals)-1))
}

// nearestRank is the value at quantile p of vals by the nearest-rank method.
func nearestRank(vals []float64, p float64) float64 {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(rank, 0)]
}

// sortedKeys returns a map's keys in order, so a report is the same on every run.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// WriteText prints the report as one small table per question.
func WriteText(w io.Writer, rep Report) error {
	if _, err := fmt.Fprintf(w, "battery %s, cut %.2f, %d sampled item(s) skipped (no original answer or fewer than %d repeats)\n",
		rep.Battery, rep.Cut, rep.Skipped, MinRepeats); err != nil {
		return err
	}
	if rep.Unpaired > 0 {
		if _, err := fmt.Fprintf(w, "%d repeat(s) with no earlier original were not paired\n", rep.Unpaired); err != nil {
			return err
		}
	}
	if len(rep.Rows) == 0 {
		_, err := fmt.Fprintln(w, "no sampled item has an original answer and enough repeats to count")
		return err
	}
	for _, r := range rep.Rows {
		if _, err := fmt.Fprintf(w, "\n%s (%s)\n", r.Question, r.Kind); err != nil {
			return err
		}
		lines := [][2]string{{"items", fmt.Sprint(r.Items)}}
		if r.Kind == KindChoice {
			lines = append(lines, [2]string{"changed share", pct(r.Changed)})
		} else {
			lines = append(lines,
				[2]string{"mean sd", num(r.MeanSD)},
				[2]string{"largest sd", num(r.MaxSD)},
				[2]string{"cross share", pct(r.CrossShare)},
				[2]string{"middle 90% span", num(r.Width90)},
			)
		}
		for _, l := range lines {
			if _, err := fmt.Fprintf(w, "  %-16s %s\n", l[0], l[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func num(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.4f", *v)
}

func pct(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100**v)
}
