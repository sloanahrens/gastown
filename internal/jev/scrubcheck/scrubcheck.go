// Package scrubcheck reads the shadow scrub check log and reports, per caller
// and question, what scrubbing the state costs in signal: how far the answers
// move when the state is scrubbed, set against how far the same question moves
// when it is asked again with the same state.
package scrubcheck

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steveyegge/gastown/internal/jev/logtriage"
	"github.com/steveyegge/gastown/internal/jev/noise"
)

// MinPairs is how many measured pairs a question needs before its verdict
// counts. Fewer leave its crossing share too thin to judge.
const MinPairs = 30

// FreeMargin is how far a scrub's crossing share may sit above the repeat
// baseline and still count as free. The operator reviews it.
const FreeMargin = 0.02

// CostlyMargin is how far above the repeat baseline a scrub's crossing share
// must sit to count as costly; between the two margins the verdict is
// borderline. The operator reviews it.
const CostlyMargin = 0.05

// Verdicts a question can get.
const (
	VerdictFree       = "free"
	VerdictBorderline = "borderline"
	VerdictCostly     = "costly"
	VerdictTooFew     = "too few"
	VerdictNoBaseline = "no baseline"
)

// outcomeChecked is the outcome of a sampled state whose check ran. Only these
// lines hold answers that can be measured.
const outcomeChecked = "checked"

// Line is one line of scrubcheck.jsonl, in the shape the shadow check writes.
// The file holds no state text, only answers and rule counts.
type Line struct {
	TS          time.Time `json:"ts"`
	Caller      string    `json:"caller"`
	QuestionIDs []string  `json:"question_ids"`
	Outcome     string    `json:"outcome"`
	Original    *Answers  `json:"original_answers,omitempty"`
	// Unchanged is true when the scrub replaced nothing, so no second call ran.
	Unchanged bool     `json:"unchanged,omitempty"`
	Scrubbed  *Answers `json:"scrubbed_answers,omitempty"`
	// Replacements counts how many times each scrubbing rule replaced text.
	Replacements map[string]int `json:"replacements,omitempty"`
}

// Answers is one answer set of a check line.
type Answers struct {
	Choice map[string]Choice  `json:"choice,omitempty"`
	Noul   map[string]float64 `json:"noul,omitempty"`
	Score  map[string]Score   `json:"score,omitempty"`
}

// Choice is a choice answer: the option picked and every option's probability.
type Choice struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Score is a score answer: the level index picked.
type Score struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

// ErrNoLog is what Read returns when no check log exists yet, so no sampled
// request has been checked.
var ErrNoLog = errors.New("no scrub check log")

// Read loads the check log at path. It returns ErrNoLog when no log exists yet.
// A line that does not decode fails the read: a measurement that skips lines
// reads as cleaner than the traffic it measured.
func Read(path string) ([]Line, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoLog
	}
	if err != nil {
		return nil, fmt.Errorf("opening the scrub check log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var lines []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("reading the scrub check log: line %d does not decode as a check line", n)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the scrub check log: %w", err)
	}
	return lines, nil
}

// Baseline reads the repeat noise and returns, per caller and question, the
// share the repeats move: the share of items whose answers cross the cut for a
// noul or score, and the share whose pick changed for a choice. The repeat log
// is logtriage's, so the baseline belongs to logtriage's caller name. A town
// with no repeat log gets an empty map.
func Baseline(townRoot string, cut float64, since time.Time) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	logs, err := noise.Read(logtriage.LogPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("reading the repeat baseline: %w", err)
	}
	if !logs.RepeatLogFound {
		return out, nil
	}
	rep, err := noise.Compute(logs.Originals, logs.Repeats, noise.Options{Since: since, Cut: cut})
	if err != nil {
		return nil, fmt.Errorf("reading the repeat baseline: %w", err)
	}
	shares := map[string]float64{}
	for _, r := range rep.Rows {
		share := r.CrossShare
		if r.Kind == noise.KindChoice {
			share = r.Changed
		}
		if share != nil {
			shares[r.Question] = *share
		}
	}
	out[logtriage.Caller] = shares
	return out, nil
}

// Options narrows and reads a report.
type Options struct {
	// Since counts only lines at or after it. The zero time keeps all.
	Since time.Time
	// Cut is the threshold a noul or score answer is split at.
	Cut float64
	// Caller counts only that caller's lines. Empty keeps all callers.
	Caller string
}

// Row is one caller's question across the states it was measured on.
type Row struct {
	Caller   string `json:"caller"`
	Question string `json:"question"`
	Kind     string `json:"kind"`
	// Pairs is how many states were measured with both answer sets.
	Pairs int `json:"pairs"`
	// Unchanged is how many sampled states the scrub left unchanged, so no
	// second call ran for them.
	Unchanged int `json:"unchanged"`
	// Missing is how many states asked the question, whose scrubbed call did
	// not answer it with the same kind: they are not in Pairs.
	Missing int `json:"missing"`
	// MeanChange and MaxChange are the mean and largest absolute change across
	// pairs: of a noul's or score's value, or of the largest option probability
	// for a choice. Nil without pairs.
	MeanChange *float64 `json:"mean_change,omitempty"`
	MaxChange  *float64 `json:"max_change,omitempty"`
	// Crossing is the share of pairs whose answer crosses the cut for a noul or
	// score, and whose picked option changed for a choice. Nil without pairs.
	Crossing *float64 `json:"crossing_share,omitempty"`
	// Baseline is the repeat share for the same caller and question. Nil when
	// the repeat data does not have it.
	Baseline *float64 `json:"baseline,omitempty"`
	// Rules counts the replacements of the states measured for this question,
	// by rule name.
	Rules   map[string]int `json:"rules,omitempty"`
	Verdict string         `json:"verdict"`
}

// Report is the scrub check's signal cost, one row per caller and question.
type Report struct {
	Cut  float64 `json:"cut"`
	Rows []Row   `json:"rows"`
}

// key names one caller's question.
type key struct {
	caller, question string
}

// measure gathers one caller's question across its lines.
type measure struct {
	kind      string
	unchanged int
	missing   int
	changes   []float64
	crossings int
	rules     map[string]int
}

// Compute measures every checked line of the log. baseline maps each caller to
// the repeat share per question, as Baseline reads it. A question answered as
// two kinds across the lines is refused: its numbers would mix two measures.
func Compute(lines []Line, opts Options, baseline map[string]map[string]float64) (Report, error) {
	rep := Report{Cut: opts.Cut, Rows: []Row{}}
	accums := map[key]*measure{}
	for _, l := range lines {
		if l.Outcome != outcomeChecked || l.Original == nil {
			continue
		}
		if !opts.Since.IsZero() && l.TS.Before(opts.Since) {
			continue
		}
		if opts.Caller != "" && l.Caller != opts.Caller {
			continue
		}
		for _, q := range questionsOf(l.Original) {
			kind := kindOf(l.Original, q)
			k := key{l.Caller, q}
			m := accums[k]
			if m == nil {
				m = &measure{kind: kind}
				accums[k] = m
			}
			if m.kind != kind {
				return Report{}, fmt.Errorf("question %s was answered as %s and as %s", q, m.kind, kind)
			}
			switch {
			case l.Unchanged:
				m.unchanged++
			case l.Scrubbed != nil && kindOf(l.Scrubbed, q) == kind:
				m.pair(l.Original, l.Scrubbed, q, opts.Cut)
				for rule, n := range l.Replacements {
					if m.rules == nil {
						m.rules = map[string]int{}
					}
					m.rules[rule] += n
				}
			default:
				m.missing++
			}
		}
	}

	keys := make([]key, 0, len(accums))
	for k := range accums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].caller != keys[j].caller {
			return keys[i].caller < keys[j].caller
		}
		return keys[i].question < keys[j].question
	})
	for _, k := range keys {
		var base *float64
		if share, ok := baseline[k.caller][k.question]; ok {
			base = &share
		}
		rep.Rows = append(rep.Rows, accums[k].row(k, base))
	}
	return rep, nil
}

// pair measures one state's original and scrubbed answer to question q.
func (m *measure) pair(orig, scrubbed *Answers, q string, cut float64) {
	var change float64
	var crosses bool
	switch m.kind {
	case noise.KindNoul:
		a, b := orig.Noul[q], scrubbed.Noul[q]
		change, crosses = math.Abs(a-b), (a < cut) != (b < cut)
	case noise.KindScore:
		a, b := orig.Score[q].Score, scrubbed.Score[q].Score
		change, crosses = math.Abs(a-b), (a < cut) != (b < cut)
	case noise.KindChoice:
		a, b := orig.Choice[q], scrubbed.Choice[q]
		change, crosses = maxProbGap(a.Probabilities, b.Probabilities), a.Choice != b.Choice
	}
	m.changes = append(m.changes, change)
	if crosses {
		m.crossings++
	}
}

// row turns the gathered readings into the question's row and its verdict.
func (m *measure) row(k key, base *float64) Row {
	r := Row{Caller: k.caller, Question: k.question, Kind: m.kind, Unchanged: m.unchanged, Missing: m.missing, Pairs: len(m.changes), Rules: m.rules, Baseline: base}
	var share float64
	if r.Pairs > 0 {
		var sum, maxChange float64
		for _, c := range m.changes {
			sum += c
			maxChange = math.Max(maxChange, c)
		}
		mean := sum / float64(r.Pairs)
		share = float64(m.crossings) / float64(r.Pairs)
		r.MeanChange, r.MaxChange, r.Crossing = &mean, &maxChange, &share
	}
	r.Verdict = verdict(r.Pairs, r.Missing, share, base)
	return r
}

// verdict judges a scrub's crossing share against the repeat baseline. Too few
// pairs comes first: no baseline can make a thin measure count. A scrub that
// left more answers missing than it measured is too few as well: the pairs that
// came back are not most of the question, so their share does not stand for it.
// The excess is rounded so a share exactly the margin above the baseline sits on
// the margin, not past it, whatever the float arithmetic of the two shares.
func verdict(pairs, missing int, crossing float64, baseline *float64) string {
	switch {
	case pairs < MinPairs, missing > pairs:
		return VerdictTooFew
	case baseline == nil:
		return VerdictNoBaseline
	}
	excess := math.Round((crossing-*baseline)*1e9) / 1e9
	switch {
	case excess <= FreeMargin:
		return VerdictFree
	case excess <= CostlyMargin:
		return VerdictBorderline
	}
	return VerdictCostly
}

// questionsOf lists the question ids an answer set holds, sorted.
func questionsOf(a *Answers) []string {
	seen := map[string]bool{}
	for q := range a.Choice {
		seen[q] = true
	}
	for q := range a.Noul {
		seen[q] = true
	}
	for q := range a.Score {
		seen[q] = true
	}
	out := make([]string, 0, len(seen))
	for q := range seen {
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

// kindOf names the kind of question q's answer holds, or "" when it holds none.
func kindOf(a *Answers, q string) string {
	switch {
	case hasKey(a.Noul, q):
		return noise.KindNoul
	case hasKey(a.Score, q):
		return noise.KindScore
	case hasKey(a.Choice, q):
		return noise.KindChoice
	}
	return ""
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

// maxProbGap is the largest absolute difference between two option
// probability maps, an option missing from one side counting as zero.
func maxProbGap(a, b map[string]float64) float64 {
	var gap float64
	for opt, p := range a {
		gap = math.Max(gap, math.Abs(p-b[opt]))
	}
	for opt, p := range b {
		if _, ok := a[opt]; !ok {
			gap = math.Max(gap, math.Abs(p))
		}
	}
	return gap
}

// WriteText prints the report as an aligned table, one row per caller and
// question.
func WriteText(w io.Writer, rep Report) error {
	if _, err := fmt.Fprintf(w, "scrub check, cut %.2f, margins: free within %s, costly above %s, at least %d pairs\n",
		rep.Cut, pts(FreeMargin), pts(CostlyMargin), MinPairs); err != nil {
		return err
	}
	if len(rep.Rows) == 0 {
		_, err := fmt.Fprintln(w, "no checked request has both answer sets to count")
		return err
	}
	// The tabwriter buffers every cell until Flush, so its writes cannot fail;
	// a write error to w comes back from Flush.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CALLER\tQUESTION\tKIND\tPAIRS\tUNCHANGED\tMISSING\tMEAN CHANGE\tMAX CHANGE\tCROSSING\tBASELINE\tVERDICT\tRULES")
	for _, r := range rep.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Caller, r.Question, r.Kind, r.Pairs, r.Unchanged, r.Missing,
			num(r.MeanChange), num(r.MaxChange), pct(r.Crossing), pct(r.Baseline), r.Verdict, rules(r.Rules))
	}
	return tw.Flush()
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

func pts(v float64) string {
	return fmt.Sprintf("%.0f points", 100*v)
}

// rules prints the rule counts as name:count pairs in name order.
func rules(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s:%d", name, m[name]))
	}
	return strings.Join(parts, " ")
}
