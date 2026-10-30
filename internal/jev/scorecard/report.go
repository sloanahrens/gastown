package scorecard

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Report is what one scorecard run prints, as text or as JSON.
type Report struct {
	Battery string `json:"battery"`
	// Since is the window's start; nil means all time.
	Since *time.Time `json:"since,omitempty"`
	Items int        `json:"items"`
	// Labeled is how many items a rule labeled: the count of Lines whose
	// Labeled is set, kept apart so a report without its Lines still has it. It
	// is always printed, 0 included: a reader that ignores unknown keys is
	// unaffected, and one that wants the count never has to guess an absent key.
	Labeled      int `json:"labeled"`
	LinesSkipped int `json:"lines_skipped"`
	// LogMissing is set when the battery has no log yet. The report is then
	// empty, which is the normal state before a battery's first line.
	LogMissing bool `json:"log_missing,omitempty"`
	// Context holds the lines a Contextual adapter reported, such as the
	// finding beads its FAIL labels named. They follow the item counts in the
	// text report.
	Context   []string         `json:"context,omitempty"`
	Questions []QuestionReport `json:"questions"`
	// Lines is each parsed item's labeling and the rule that set it. The text
	// report leaves it out; the JSON report names the rule for every line.
	Lines []LineReport `json:"lines,omitempty"`
	// LinesOmitted is set when the report leaves Lines out on purpose, as for a
	// Bulky battery; --lines prints them.
	LinesOmitted bool `json:"lines_omitted,omitempty"`
}

// LineReport is one parsed item's labeling: whether a rule labeled it, which
// rule, where the label came from, and the labels it set.
type LineReport struct {
	ID      string          `json:"id"`
	Labeled bool            `json:"labeled"`
	Rule    string          `json:"rule,omitempty"`
	Source  string          `json:"source,omitempty"`
	Labels  map[string]bool `json:"labels,omitempty"`
}

func lineReport(it Item, lab Labeling) LineReport {
	lr := LineReport{ID: it.ID, Labeled: lab.Labeled, Rule: lab.Rule, Source: lab.Source}
	if lab.Labeled {
		lr.Labels = make(map[string]bool, len(lab.Labels))
		for id, v := range lab.Labels {
			lr.Labels[string(id)] = v
		}
	}
	return lr
}

// QuestionReport is the scorecard for one question. A nil metric is one that
// has no value on these lines, such as precision with nothing predicted yes.
type QuestionReport struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Logged  int    `json:"logged"`
	Labeled int    `json:"labeled"`
	// Unmatched counts the labeled lines with no usable answer to the
	// question, so they could not be scored. It is printed only when set.
	Unmatched int `json:"unmatched,omitempty"`
	// Positives and Negatives count the labeled lines by their label.
	Positives int      `json:"positives"`
	Negatives int      `json:"negatives"`
	BaseRate  *float64 `json:"base_rate"`
	// Precision and Recall are set for noul and choice questions only.
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`
	// BestThreshold and BestF1 are the cut with the best F1 on the labeled lines.
	BestThreshold *float64 `json:"best_threshold"`
	BestF1        *float64 `json:"best_f1"`
	// PrecisionAtBest is the precision at BestThreshold: of the labeled lines
	// that cut predicts yes, how many are positive. It is set for noul and
	// choice questions only, since a score has no precision, and is omitted
	// when the cut predicts no yes.
	PrecisionAtBest *float64 `json:"precision_at_best,omitempty"`
	// FlaggedAtBest is how many labeled lines BestThreshold predicts yes: the
	// count PrecisionAtBest was measured on. It is set for noul and choice
	// questions only, beside PrecisionAtBest.
	FlaggedAtBest int `json:"flagged_at_best,omitempty"`
	// Calibration is set for noul and choice questions only.
	Calibration []Bin `json:"calibration,omitempty"`
	// ECE, Brier and CalibrationMonotone summarise the calibration table for
	// noul and choice questions: the labeled-weighted gap between value and
	// label rate, the mean squared gap between value and label, and whether
	// the label rate never falls across readable bands. They are absent for a
	// score question and for a question with no labeled lines.
	ECE                 *float64 `json:"ece,omitempty"`
	Brier               *float64 `json:"brier,omitempty"`
	CalibrationMonotone *bool    `json:"calibration_monotone,omitempty"`
	// MeanScorePositive and MeanScoreNegative are set for score questions only.
	MeanScorePositive *float64 `json:"mean_score_positive,omitempty"`
	MeanScoreNegative *float64 `json:"mean_score_negative,omitempty"`
	// Context counts the Item.Context values on the question's logged lines.
	// It is printed beside the question and never scored.
	Context []ContextCount `json:"context,omitempty"`
	// Sources counts the question's labeled lines by the source of their
	// label, in source order, so a review from one source is never read as
	// another's. It is printed only when set.
	Sources []SourceCount `json:"sources,omitempty"`
	Verdict string        `json:"verdict"`
}

// ContextCount is how many of a question's logged lines carry one context value.
type ContextCount struct {
	Value string `json:"value"`
	Lines int    `json:"lines"`
}

// SourceCount is how many of a question's labeled lines came from one source,
// and how many of those carried a true label.
type SourceCount struct {
	Source    string `json:"source"`
	Labeled   int    `json:"labeled"`
	Positives int    `json:"positives"`
}

// Bin is one calibration band: the labeled lines whose value falls in [Lo, Hi).
type Bin struct {
	Lo        float64  `json:"lo"`
	Hi        float64  `json:"hi"`
	N         int      `json:"n"`
	MeanValue *float64 `json:"mean_value"`
	LabelRate *float64 `json:"label_rate"`
}

// WriteText prints the report as the text scorecard, one block per question.
func WriteText(w io.Writer, rep Report) error {
	ew := &errWriter{w: w}
	window := "all time"
	if rep.Since != nil {
		window = "since " + rep.Since.Format(time.RFC3339)
	}
	ew.printf("Scorecard for battery %s, %s\n", rep.Battery, window)
	ew.printf("Items: %d parsed, %d line(s) skipped\n", rep.Items, rep.LinesSkipped)
	if rep.LogMissing {
		ew.printf("No log yet: the battery has written no lines.\n")
	}
	for _, line := range rep.Context {
		ew.printf("%s\n", line)
	}
	if len(rep.Questions) == 0 {
		ew.printf("\nNo question has a usable answer yet.\n")
	}
	for _, q := range rep.Questions {
		ew.printf("\n%s (%s)\n", q.ID, q.Kind)
		ew.printf("  logged %d, labeled %d, positives %d, negatives %d, base rate %s\n",
			q.Logged, q.Labeled, q.Positives, q.Negatives, pct(q.BaseRate))
		if q.Unmatched > 0 {
			ew.printf("  labeled with no usable answer, not scored: %d\n", q.Unmatched)
		}
		if len(q.Sources) > 0 {
			ew.printf("  labels by source: %s\n", sourceText(q.Sources))
		}
		switch q.Kind {
		case KindNoul:
			ew.printf("  at 0.5: precision %s, recall %s\n", pct(q.Precision), pct(q.Recall))
		case KindChoice:
			ew.printf("  at the chosen option: precision %s, recall %s\n", pct(q.Precision), pct(q.Recall))
		case KindScore:
			ew.printf("  mean score: positives %s, negatives %s\n", num(q.MeanScorePositive), num(q.MeanScoreNegative))
		}
		if len(q.Context) > 0 {
			ew.printf("  context, not scored: %s\n", contextText(q.Context))
		}
		ew.printf("  best F1 %s at threshold %s\n", num(q.BestF1), num(q.BestThreshold))
		if q.PrecisionAtBest != nil {
			ew.printf("  precision at the best F1 cut: %s over %d flagged\n", pct(q.PrecisionAtBest), q.FlaggedAtBest)
		}
		if len(q.Calibration) > 0 {
			ew.printf("  calibration (band, labeled n, mean value, label rate):\n")
			for _, b := range q.Calibration {
				ew.printf("    %.1f-%.1f  %5d  %s  %s\n", b.Lo, b.Hi, b.N, num(b.MeanValue), num(b.LabelRate))
			}
			if q.CalibrationMonotone != nil {
				ew.printf("  calibration error %.2f, brier %.2f, monotone: %s\n", *q.ECE, *q.Brier, yesNo(*q.CalibrationMonotone))
			}
		}
		ew.printf("  verdict: %s\n", q.Verdict)
	}
	return ew.err
}

// errWriter keeps the first write error, so the report is written in straight
// lines and checked once at the end.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err == nil {
		_, e.err = fmt.Fprintf(e.w, format, args...)
	}
}

// contextText lists the context values as "alpha 3 lines, beta 1 line".
func contextText(counts []ContextCount) string {
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		unit := "lines"
		if c.Lines == 1 {
			unit = "line"
		}
		parts = append(parts, fmt.Sprintf("%s %d %s", c.Value, c.Lines, unit))
	}
	return strings.Join(parts, ", ")
}

// sourceText lists the label sources as "overseer 2 labels, 1 positive;
// polecat 3 labels, 2 positives".
func sourceText(counts []SourceCount) string {
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		labels, positives := "labels", "positives"
		if c.Labeled == 1 {
			labels = "label"
		}
		if c.Positives == 1 {
			positives = "positive"
		}
		parts = append(parts, fmt.Sprintf("%s %d %s, %d %s", c.Source, c.Labeled, labels, c.Positives, positives))
	}
	return strings.Join(parts, "; ")
}

func pct(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}

func num(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", *v)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
