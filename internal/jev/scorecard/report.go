package scorecard

import (
	"fmt"
	"io"
	"time"
)

// Report is what one scorecard run prints, as text or as JSON.
type Report struct {
	Battery string `json:"battery"`
	// Since is the window's start; nil means all time.
	Since        *time.Time       `json:"since,omitempty"`
	Items        int              `json:"items"`
	LinesSkipped int              `json:"lines_skipped"`
	Questions    []QuestionReport `json:"questions"`
}

// QuestionReport is the scorecard for one question. A nil metric is one that
// has no value on these lines, such as precision with nothing predicted yes.
type QuestionReport struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Logged  int    `json:"logged"`
	Labeled int    `json:"labeled"`
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
	// Calibration is set for noul and choice questions only.
	Calibration []Bin `json:"calibration,omitempty"`
	// MeanScorePositive and MeanScoreNegative are set for score questions only.
	MeanScorePositive *float64 `json:"mean_score_positive,omitempty"`
	MeanScoreNegative *float64 `json:"mean_score_negative,omitempty"`
	Verdict           string   `json:"verdict"`
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
	if len(rep.Questions) == 0 {
		ew.printf("\nNo question has a usable answer yet.\n")
	}
	for _, q := range rep.Questions {
		ew.printf("\n%s (%s)\n", q.ID, q.Kind)
		ew.printf("  logged %d, labeled %d, positives %d, negatives %d, base rate %s\n",
			q.Logged, q.Labeled, q.Positives, q.Negatives, pct(q.BaseRate))
		switch q.Kind {
		case KindNoul:
			ew.printf("  at 0.5: precision %s, recall %s\n", pct(q.Precision), pct(q.Recall))
		case KindChoice:
			ew.printf("  at the chosen option: precision %s, recall %s\n", pct(q.Precision), pct(q.Recall))
		case KindScore:
			ew.printf("  mean score: positives %s, negatives %s\n", num(q.MeanScorePositive), num(q.MeanScoreNegative))
		}
		ew.printf("  best F1 %s at threshold %s\n", num(q.BestF1), num(q.BestThreshold))
		if len(q.Calibration) > 0 {
			ew.printf("  calibration (band, labeled n, mean value, label rate):\n")
			for _, b := range q.Calibration {
				ew.printf("    %.1f-%.1f  %5d  %s  %s\n", b.Lo, b.Hi, b.N, num(b.MeanValue), num(b.LabelRate))
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
