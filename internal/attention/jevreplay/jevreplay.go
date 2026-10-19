// Package jevreplay replays the attention queue's retained history through
// the Jev triage and measures it against a proxy for what a human did.
//
// The proxy is the only ground truth the queue has: an item a human acked
// needed them, and an item that cleared without an ack settled on its own.
// It is a proxy, not a judgment: a human may ack an item they could have
// ignored, and may let a real problem clear unseen. The report says so.
//
// The package reads a Snapshot and the ack history the caller loaded and
// writes nothing; the caller owns the Jev client, the files and the cap's
// price list.
package jevreplay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/attention/jevtriage"
	"github.com/steveyegge/gastown/internal/jev"
)

// Classifier asks one Choice question. *jev.Client is one.
type Classifier interface {
	Choice(ctx context.Context, state any, instructions string, criteria map[string]string) (*jev.Answer, error)
}

// maxRetryWait caps how long a 429 may make the run wait before its one retry.
const maxRetryWait = time.Minute

// Options are the knobs of one replay.
type Options struct {
	// Floor is the confidence below which an answer is forced to needs-you.
	Floor float64
	// MaxSpendUSD stops the run once the spend reaches it, or would reach it
	// on a call as dear as the dearest so far. It must be finite and above
	// zero.
	MaxSpendUSD float64
	// USDPerBillionTokens prices input and output tokens alike. It must be
	// finite and above zero: at zero every call costs nothing and the cap
	// never trips.
	USDPerBillionTokens float64
	// Sleep waits d for a rate limit; nil uses a timer that ctx can cut short.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Validate refuses options under which the spend cap could not hold: a cap
// or a price that is zero, negative, infinite or NaN.
func (o Options) Validate() error {
	if !positiveFinite(o.MaxSpendUSD) {
		return fmt.Errorf("jevreplay: the spend cap must be finite and above zero, not %v", o.MaxSpendUSD)
	}
	if !positiveFinite(o.USDPerBillionTokens) {
		return fmt.Errorf("jevreplay: the price per billion tokens must be finite and above zero, not %v", o.USDPerBillionTokens)
	}
	return nil
}

func positiveFinite(f float64) bool { return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f) }

// Stop says why a run ended before every item was reached.
type Stop string

const (
	// StopNone: every item was reached.
	StopNone Stop = ""
	// StopSpendCap: the spend reached MaxSpendUSD.
	StopSpendCap Stop = "spend-cap"
	// StopInterrupted: the context ended.
	StopInterrupted Stop = "interrupted"
	// StopUnauthorized: Jev refused the key, so no later call could succeed.
	StopUnauthorized Stop = "unauthorized"
	// StopNoUsage: a call answered with no token usage, so its cost is
	// unknown and the cap can no longer be enforced.
	StopNoUsage Stop = "no-usage"
)

// Confusion counts the needs-you decisions against the proxy.
type Confusion struct {
	// TP: Jev's label was needs-you and the proxy says a human was needed.
	TP int `json:"tp"`
	// FP: needs-you where the item cleared on its own.
	FP int `json:"fp"`
	// FN: not needs-you where a human acked.
	FN int `json:"fn"`
	// TN: not needs-you where the item cleared on its own.
	TN int `json:"tn"`
}

// Bin is one confidence band of the calibration table.
type Bin struct {
	// Lo is the inclusive lower edge; Hi the exclusive upper edge, inclusive
	// for the last band.
	Lo, Hi float64
	// N is the labeled items whose confidence fell in the band.
	N int
	// MeanConfidence is the mean stated confidence of those items.
	MeanConfidence float64
	// Accuracy is the share of them where Jev's own choice, needs-you or not,
	// matched the proxy.
	Accuracy float64
}

// Tier is the measure against one way of labeling the items.
type Tier struct {
	// Name says how the proxy was read.
	Name string
	// Labeled is the items with a proxy label and a verdict; standing items
	// that nobody acked have no label yet and are left out.
	Labeled int
	// Needed is how many of them the proxy says needed a human.
	Needed int
	Confusion
	// Precision and Recall are nil when their denominator is zero.
	Precision *float64
	Recall    *float64
	// Calibration is stated confidence against proxy accuracy, by band.
	Calibration []Bin
}

// Forced counts the verdicts a fail-safe rule moved to needs-you.
type Forced struct {
	// Total is the sum of the three reasons.
	Total          int
	EscalationKind int
	LowConfidence  int
	NoAnswer       int
}

// Spend is what the run cost against its cap.
type Spend struct {
	// Calls is the calls that returned usage.
	Calls        int
	InputTokens  int
	OutputTokens int
	// USD is the priced tokens. Calls that failed report no usage and add
	// nothing.
	USD float64
	// CapUSD and USDPerBillionTokens are the options the run priced with.
	CapUSD              float64
	USDPerBillionTokens float64
}

// Report is the outcome of one replay.
type Report struct {
	// Items is the item lives in the history; Reached is how many the run got
	// to before it ended, and Stopped says why it ended early.
	Items   int
	Reached int
	Stopped Stop
	// Asked is the Jev calls made, Failed those that returned no answer, and
	// SkippedByKind the items decided by their kind with no call.
	Asked         int
	Failed        int
	SkippedByKind int
	// AckHistory is the ack records the proxy could use.
	AckHistory int
	// Tier1 labels by the acks the queue files still hold; Tier2 adds the ack
	// history, which outlives the item. The gap is what tier 1 misses.
	Tier1 Tier
	Tier2 Tier
	Forced
	Spend Spend
}

// Run replays snap's item lives, oldest first, through c. history is the ack
// history the proxy adds to the acks the snapshot holds. It returns the
// report so far with an error only for a refused key.
func Run(ctx context.Context, c Classifier, snap attention.Snapshot, history []attention.AckRecord, opts Options) (Report, error) {
	if err := opts.Validate(); err != nil {
		return Report{}, err
	}
	r := &run{c: c, opts: opts}
	rep := Report{
		Items:      len(snap.Items),
		AckHistory: len(history),
		Spend:      Spend{CapUSD: opts.MaxSpendUSD, USDPerBillionTokens: opts.USDPerBillionTokens},
	}
	t1, t2 := newTally(), newTally()
	var runErr error

	for _, rec := range snap.Items {
		if err := ctx.Err(); err != nil {
			rep.Stopped = StopInterrupted
			break
		}
		it := itemOf(rec)
		var v jevtriage.Verdict
		if jevtriage.ForcedNeedsYou(it) {
			v = jevtriage.Resolve(it, nil, opts.Floor)
			rep.SkippedByKind++
		} else {
			if r.capReached(rep.Spend.USD) {
				rep.Stopped = StopSpendCap
				break
			}
			ans, err := r.ask(ctx, it)
			rep.Asked++
			if err != nil {
				rep.Failed++
				if errors.Is(err, jev.ErrUnauthorized) {
					rep.Stopped, runErr = StopUnauthorized, err
					break
				}
				if ctx.Err() != nil {
					rep.Stopped = StopInterrupted
					break
				}
			} else if ans.Usage.InputTokens+ans.Usage.OutputTokens <= 0 {
				// An answer that reports no usage costs nothing on paper, so
				// carrying on would let the spend sit under the cap forever.
				rep.Stopped = StopNoUsage
				break
			} else {
				addUsage(&rep.Spend, ans.Usage, opts.USDPerBillionTokens)
				r.noteCall(ans.Usage)
			}
			v = jevtriage.Resolve(it, toAnswer(ans), opts.Floor)
		}
		rep.Reached++
		switch v.Reason {
		case jevtriage.ReasonEscalationKind:
			rep.Forced.EscalationKind++
		case jevtriage.ReasonLowConfidence:
			rep.Forced.LowConfidence++
		case jevtriage.ReasonNoAnswer:
			rep.Forced.NoAnswer++
		}
		if v.Reason != jevtriage.ReasonNone {
			rep.Forced.Total++
		}
		t1.add(proxyTier1(rec), v)
		t2.add(proxyTier2(rec, history), v)
	}
	rep.Tier1 = t1.tier("tier 1: acks the queue files still hold")
	rep.Tier2 = t2.tier("tier 2: acks-history.jsonl")
	return rep, runErr
}

// run holds what the calls learn as it goes.
type run struct {
	c    Classifier
	opts Options
	// dearest is the highest cost of one call so far, the stand-in for the
	// next call's cost when checking the cap.
	dearest float64
}

func (r *run) capReached(spent float64) bool {
	return spent >= r.opts.MaxSpendUSD || spent+r.dearest > r.opts.MaxSpendUSD
}

func (r *run) noteCall(u jev.Usage) {
	if c := priceOf(u, r.opts.USDPerBillionTokens); c > r.dearest {
		r.dearest = c
	}
}

// ask puts the item's question, and on a 429 waits what the server asked, up
// to a minute, and asks once more.
func (r *run) ask(ctx context.Context, it attention.Item) (*jev.Answer, error) {
	q := jevtriage.BuildQuestion(it)
	ans, err := r.c.Choice(ctx, q.State, q.Instructions, q.Criteria)
	var rl *jev.RateLimitError
	if !errors.As(err, &rl) {
		return ans, err
	}
	wait := min(rl.RetryAfter, maxRetryWait)
	sleep := r.opts.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	if sleep(ctx, wait) != nil {
		return nil, err
	}
	return r.c.Choice(ctx, q.State, q.Instructions, q.Criteria)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func toAnswer(a *jev.Answer) *jevtriage.Answer {
	if a == nil {
		return nil
	}
	return &jevtriage.Answer{Choice: a.Choice, Probabilities: a.Probabilities, Confidence: a.Confidence}
}

func itemOf(r attention.Record) attention.Item {
	return attention.Item{
		Key: r.Key, Kind: r.Kind, Severity: r.Severity, Rig: r.Rig, Bead: r.Bead,
		SHA: r.SHA, Summary: r.Summary, FirstSeen: r.FirstSeen, LastSeen: r.LastSeen,
	}
}

func priceOf(u jev.Usage, usdPerBillion float64) float64 {
	return float64(u.InputTokens+u.OutputTokens) * usdPerBillion / 1e9
}

func addUsage(s *Spend, u jev.Usage, usdPerBillion float64) {
	s.Calls++
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	s.USD += priceOf(u, usdPerBillion)
}
