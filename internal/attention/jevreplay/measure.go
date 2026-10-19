package jevreplay

import (
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/attention/jevtriage"
)

// proxy is the ground truth for one item life.
type proxy int

const (
	// proxyNone: the item still stands and nobody acked it, so nothing says
	// yet what it needed.
	proxyNone proxy = iota
	// proxyNeedsYou: a human acked it.
	proxyNeedsYou
	// proxyAuto: it cleared with no ack.
	proxyAuto
)

// proxyTier1 labels from what the queue files still hold: an ack in
// state.json, or in acks.json up to the clear. attention.Reconcile drops an
// ack from acks.json when its item clears, so a cleared item reads as auto
// here unless state.json recorded the ack first.
func proxyTier1(r attention.Record) proxy {
	return label(r.Acked, r)
}

// proxyTier2 adds the ack history: an ack of the item's key made during its
// life, from the raise to the clear (to now while it stands). An ack that
// carries a disposition labels the item by it, so fixed-by-overseer and noise
// read as not needing a human; a line written before dispositions existed
// keeps the old reading, that an ack means a human was needed.
func proxyTier2(r attention.Record, history []attention.AckRecord) proxy {
	if p, ok := dispositionInLife(r, history); ok {
		return p
	}
	return label(r.Acked || ackedInLife(r, history), r)
}

func label(acked bool, r attention.Record) proxy {
	switch {
	case acked:
		return proxyNeedsYou
	case r.ClearedAt != nil:
		return proxyAuto
	}
	return proxyNone
}

// dispositionInLife labels by the first ack in the item's life that carries a
// disposition. The two dispositions that are not needed-human share proxyAuto,
// the "no human was needed" side of the proxy.
func dispositionInLife(r attention.Record, history []attention.AckRecord) (proxy, bool) {
	for _, a := range history {
		if a.Key != r.Key || a.Disposition == "" || !inLife(a.TS, r) {
			continue
		}
		if a.Disposition.NeedsHuman() {
			return proxyNeedsYou, true
		}
		return proxyAuto, true
	}
	return proxyNone, false
}

func ackedInLife(r attention.Record, history []attention.AckRecord) bool {
	for _, a := range history {
		if a.Key == r.Key && inLife(a.TS, r) {
			return true
		}
	}
	return false
}

// inLife reports whether at falls in the item's life: from the raise to the
// clear, or to now while the item stands.
func inLife(at time.Time, r attention.Record) bool {
	if at.Before(r.FirstSeen) {
		return false
	}
	return r.ClearedAt == nil || !at.After(*r.ClearedAt)
}

// nBins is the number of equal confidence bands: wide enough to fill on a
// history of a few thousand items.
const nBins = 5

// tally accumulates one tier.
type tally struct {
	labeled, needed int
	c               Confusion
	bins            [nBins]binSum
}

type binSum struct {
	n       int
	confSum float64
	correct int
}

func newTally() *tally { return &tally{} }

// add counts one verdict against its proxy label. Calibration uses Jev's own
// choice rather than the final label, because a forced label says nothing
// about how well Jev's confidence is calibrated; an item with no usable
// answer has no confidence and joins the confusion matrix only.
func (t *tally) add(p proxy, v jevtriage.Verdict) {
	if p == proxyNone {
		return
	}
	needed := p == proxyNeedsYou
	predicted := v.Label == jevtriage.NeedsYou
	t.labeled++
	if needed {
		t.needed++
	}
	switch {
	case predicted && needed:
		t.c.TP++
	case predicted:
		t.c.FP++
	case needed:
		t.c.FN++
	default:
		t.c.TN++
	}
	if v.JevChoice == "" {
		return
	}
	b := &t.bins[binOf(v.Confidence)]
	b.n++
	b.confSum += v.Confidence
	if (v.JevChoice == string(jevtriage.NeedsYou)) == needed {
		b.correct++
	}
}

func binOf(conf float64) int {
	i := int(conf * nBins)
	return max(0, min(i, nBins-1))
}

func (t *tally) tier(name string) Tier {
	out := Tier{Name: name, Labeled: t.labeled, Needed: t.needed, Confusion: t.c}
	out.Precision = ratio(t.c.TP, t.c.TP+t.c.FP)
	out.Recall = ratio(t.c.TP, t.c.TP+t.c.FN)
	for i, b := range t.bins {
		bin := Bin{Lo: float64(i) / nBins, Hi: float64(i+1) / nBins, N: b.n}
		if b.n > 0 {
			bin.MeanConfidence = b.confSum / float64(b.n)
			bin.Accuracy = float64(b.correct) / float64(b.n)
		}
		out.Calibration = append(out.Calibration, bin)
	}
	return out
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}

// Write prints the report as text.
func (r Report) Write(w io.Writer) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }
	p("Jev replay of the attention queue history\n")
	p("The labels are a proxy, not a judgment: an acked item is labeled by its recorded\n")
	p("disposition, fixed-by-overseer and noise counting as not needing a human, and an ack with\n")
	p("none (from before dispositions existed) counting as needed-human. An item that cleared\n")
	p("with no ack counts as settled on its own. A human may ack what they could have ignored\n")
	p("and may let a real problem clear unseen.\n\n")

	p("Items: %d in the history, %d reached\n", r.Items, r.Reached)
	switch r.Stopped {
	case StopSpendCap:
		p("STOPPED at the spend cap after %d of %d items; the numbers below cover only those.\n", r.Reached, r.Items)
	case StopInterrupted:
		p("STOPPED by interrupt after %d of %d items; the numbers below cover only those.\n", r.Reached, r.Items)
	case StopNoUsage:
		p("STOPPED after %d of %d items: Jev answered with no token usage, so the spend cannot be held to the cap.\n", r.Reached, r.Items)
	case StopUnauthorized:
		p("STOPPED: Jev refused the API key after %d of %d items.\n", r.Reached, r.Items)
	}
	p("Jev calls: %d asked, %d failed; %d items decided by kind with no call\n", r.Asked, r.Failed, r.SkippedByKind)
	p("Ack history: %d records\n\n", r.AckHistory)

	r.Tier2.write(w, "")
	r.Tier1.write(w, "tier 1 holds only the acks acks.json still had when the item cleared; the gap to tier 2 is what it misses")

	p("Forced to needs-you by a fail-safe rule: %d (escalation kind %d, low confidence %d, no answer %d)\n\n",
		r.Forced.Total, r.Forced.EscalationKind, r.Forced.LowConfidence, r.Forced.NoAnswer)

	p("Spend: $%.4f of $%.2f cap (%d calls, %d input tokens, %d output tokens, at $%g per billion tokens)\n",
		r.Spend.USD, r.Spend.CapUSD, r.Spend.Calls, r.Spend.InputTokens, r.Spend.OutputTokens, r.Spend.USDPerBillionTokens)
}

func (t Tier) write(w io.Writer, note string) {
	fmt.Fprintf(w, "%s\n", t.Name)
	if note != "" {
		fmt.Fprintf(w, "  (%s)\n", note)
	}
	fmt.Fprintf(w, "  labeled items: %d (%d needed a human by the proxy)\n", t.Labeled, t.Needed)
	fmt.Fprintf(w, "  needs-you vs proxy: TP %d, FP %d, FN %d, TN %d\n", t.TP, t.FP, t.FN, t.TN)
	fmt.Fprintf(w, "  precision: %s\n", pct(t.Precision))
	fmt.Fprintf(w, "  recall:    %s\n", pct(t.Recall))
	fmt.Fprintf(w, "  calibration (confidence band, items, mean confidence, accuracy):\n")
	for _, b := range t.Calibration {
		if b.N == 0 {
			fmt.Fprintf(w, "    %.1f-%.1f  %5d  -       -\n", b.Lo, b.Hi, 0)
			continue
		}
		fmt.Fprintf(w, "    %.1f-%.1f  %5d  %.3f   %.3f\n", b.Lo, b.Hi, b.N, b.MeanConfidence, b.Accuracy)
	}
	fmt.Fprintln(w)
}

func pct(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}
