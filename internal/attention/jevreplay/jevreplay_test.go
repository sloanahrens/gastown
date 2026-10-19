package jevreplay

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/jev"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// life is one cleared item life that started at t0+start minutes and lasted
// 10 minutes. acked marks an ack the queue files still held at the clear.
func life(key string, kind attention.Kind, summary string, start int, acked bool) attention.Record {
	first := t0.Add(time.Duration(start) * time.Minute)
	cleared := first.Add(10 * time.Minute)
	r := attention.Record{
		Key: key, Kind: kind, Severity: attention.SeverityLow, Summary: summary,
		FirstSeen: first, LastSeen: cleared, ClearedAt: &cleared,
	}
	if acked {
		r.Acked, r.AckedAt = true, &first
	}
	return r
}

// fixture is eight lives, each named by its summary, with the verdict the
// stub gives it and the proxy label each tier reads:
//
//	sum-a  acked in queue      jev needs-you .90  TP in both tiers
//	sum-b  no ack              jev noise     .85  TN in both
//	sum-c  acked in history    jev noise     .90  FN in tier 2, TN in tier 1
//	sum-d  no ack              jev needs-you .75  FP in both
//	sum-e  escalation, acked   no call            TP in both
//	sum-f  no ack              jev auto      .50  forced low-confidence, FP
//	sum-g  standing, no ack    jev noise     .95  no proxy label
//	sum-h  no ack              call fails         forced no-answer, FP
func fixture() (attention.Snapshot, []attention.AckRecord) {
	g := attention.Record{
		Key: "g", Kind: attention.KindBDSlow, Severity: attention.SeverityLow, Summary: "sum-g",
		FirstSeen: t0.Add(70 * time.Minute), LastSeen: t0.Add(80 * time.Minute),
	}
	snap := attention.Snapshot{Items: []attention.Record{
		life("a", attention.KindRedMain, "sum-a", 0, true),
		life("b", attention.KindBDSlow, "sum-b", 10, false),
		life("c", attention.KindPolecatStall, "sum-c", 20, false),
		life("d", attention.KindBDSlow, "sum-d", 30, false),
		life("e", attention.KindEscalation, "sum-e", 40, true),
		life("f", attention.KindBDSlow, "sum-f", 50, false),
		g,
		life("h", attention.KindBDSlow, "sum-h", 60, false),
	}}
	history := []attention.AckRecord{
		{TS: t0.Add(25 * time.Minute), Key: "c", Kind: attention.KindPolecatStall},
		// An ack of d from before the life, and one of b after it cleared:
		// neither belongs to the life.
		{TS: t0.Add(5 * time.Minute), Key: "d", Kind: attention.KindBDSlow},
		{TS: t0.Add(2 * time.Hour), Key: "b", Kind: attention.KindBDSlow},
	}
	return snap, history
}

type reply struct {
	choice string
	conf   float64
	err    error
}

// stub answers by the summary found in the question's state, with 100 input
// and 20 output tokens per answer, and records every call.
type stub struct {
	mu      sync.Mutex
	replies map[string]reply
	calls   []string
	// first429 makes the first call of each summary in it fail with a 429.
	first429 map[string]time.Duration
	seen     map[string]int
}

func (s *stub) Choice(_ context.Context, state any, _ string, criteria map[string]string) (*jev.Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := state.(string)
	for sum, r := range s.replies {
		if !strings.Contains(text, `"`+sum+`"`) {
			continue
		}
		s.calls = append(s.calls, sum)
		if d, ok := s.first429[sum]; ok && s.seen[sum] == 0 {
			s.seen[sum]++
			return nil, &jev.RateLimitError{RetryAfter: d}
		}
		if r.err != nil {
			return nil, r.err
		}
		probs := map[string]float64{}
		for k := range criteria {
			probs[k] = (1 - r.conf) / 2
		}
		probs[r.choice] = r.conf
		return &jev.Answer{Choice: r.choice, Probabilities: probs, Confidence: r.conf,
			Usage: jev.Usage{InputTokens: 100, OutputTokens: 20}}, nil
	}
	return nil, errors.New("stub: no reply for state")
}

func newStub() *stub {
	return &stub{
		replies: map[string]reply{
			"sum-a": {"needs-you", 0.90, nil},
			"sum-b": {"noise", 0.85, nil},
			"sum-c": {"noise", 0.90, nil},
			"sum-d": {"needs-you", 0.75, nil},
			"sum-f": {"auto-fixable", 0.50, nil},
			"sum-g": {"noise", 0.95, nil},
			"sum-h": {"", 0, jev.ErrTimeout},
		},
		first429: map[string]time.Duration{},
		seen:     map[string]int{},
	}
}

func opts() Options {
	// 120 tokens a call at $1000 per billion is $0.00012 a call.
	return Options{Floor: 0.7, MaxSpendUSD: 5, USDPerBillionTokens: 1000}
}

func ptr(v float64) *float64 { return &v }

func TestRunFixtureNumbers(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	st := newStub()
	rep, err := Run(context.Background(), st, snap, history, opts())
	require.NoError(t, err)

	assert.Equal(t, 8, rep.Items)
	assert.Equal(t, 8, rep.Reached)
	assert.Equal(t, StopNone, rep.Stopped)
	assert.Equal(t, 7, rep.Asked, "every item but the escalation costs a call")
	assert.Equal(t, 1, rep.Failed)
	assert.Equal(t, 1, rep.SkippedByKind)
	assert.NotContains(t, st.calls, "sum-e", "the escalation's kind decides it with no call")
	assert.Equal(t, 3, rep.AckHistory)

	assert.Equal(t, Forced{Total: 3, EscalationKind: 1, LowConfidence: 1, NoAnswer: 1}, rep.Forced)

	t2 := rep.Tier2
	assert.Equal(t, 7, t2.Labeled, "the standing, unacked item has no label")
	assert.Equal(t, 3, t2.Needed, "a, c (history) and e")
	assert.Equal(t, Confusion{TP: 2, FP: 3, FN: 1, TN: 1}, t2.Confusion)
	assert.InDelta(t, 0.4, *t2.Precision, 1e-9)
	assert.InDelta(t, 2.0/3.0, *t2.Recall, 1e-9)

	t1 := rep.Tier1
	assert.Equal(t, 7, t1.Labeled)
	assert.Equal(t, 2, t1.Needed, "tier 1 cannot see the history ack of c")
	assert.Equal(t, Confusion{TP: 2, FP: 3, FN: 0, TN: 2}, t1.Confusion)
	assert.InDelta(t, 0.4, *t1.Precision, 1e-9)
	assert.InDelta(t, 1.0, *t1.Recall, 1e-9)

	// Calibration (tier 2): Jev's own choice against the proxy, for the five
	// labeled items Jev answered. The forced and unanswered ones carry none.
	require.Len(t, t2.Calibration, 5)
	bins := t2.Calibration
	assert.Equal(t, 0, bins[0].N)
	assert.Equal(t, 0, bins[1].N)
	assert.Equal(t, Bin{Lo: 0.4, Hi: 0.6, N: 1, MeanConfidence: 0.5, Accuracy: 1}, bins[2])
	assert.Equal(t, Bin{Lo: 0.6, Hi: 0.8, N: 1, MeanConfidence: 0.75, Accuracy: 0}, bins[3])
	assert.Equal(t, 3, bins[4].N)
	assert.InDelta(t, (0.9+0.85+0.9)/3, bins[4].MeanConfidence, 1e-9)
	assert.InDelta(t, 2.0/3.0, bins[4].Accuracy, 1e-9, "c is wrong against the history proxy")
	assert.InDelta(t, 1.0, t1.Calibration[4].Accuracy, 1e-9, "against tier 1 c is right")

	// Six answers carry usage: all seven calls but the timeout.
	assert.Equal(t, 6, rep.Spend.Calls)
	assert.Equal(t, 600, rep.Spend.InputTokens)
	assert.Equal(t, 120, rep.Spend.OutputTokens)
	assert.InDelta(t, 720*1000/1e9, rep.Spend.USD, 1e-12)
	assert.Equal(t, 5.0, rep.Spend.CapUSD)
}

func TestRunSpendCapStopsAndSaysSo(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	o := opts()
	// $1.20 a call, cap $3: two calls fit, and a third at $1.20 would pass it.
	o.USDPerBillionTokens = 1e7
	o.MaxSpendUSD = 3
	st := newStub()
	rep, err := Run(context.Background(), st, snap, history, o)
	require.NoError(t, err)

	assert.Equal(t, StopSpendCap, rep.Stopped)
	assert.Equal(t, 2, rep.Asked)
	assert.Equal(t, []string{"sum-a", "sum-b"}, st.calls)
	assert.Equal(t, 2, rep.Reached)
	assert.Equal(t, 8, rep.Items)
	assert.InDelta(t, 2.4, rep.Spend.USD, 1e-9)
	assert.LessOrEqual(t, rep.Spend.USD, o.MaxSpendUSD)
	assert.Equal(t, 2, rep.Tier2.Labeled, "only the items reached are measured")

	var buf bytes.Buffer
	rep.Write(&buf)
	assert.Contains(t, buf.String(), "STOPPED at the spend cap after 2 of 8 items")
	assert.Contains(t, buf.String(), "Spend: $2.4000 of $3.00 cap")
}

func TestRunRejectsOptionsThatLeaveTheCapOpen(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	for name, mutate := range map[string]func(*Options){
		"zero cap":       func(o *Options) { o.MaxSpendUSD = 0 },
		"negative cap":   func(o *Options) { o.MaxSpendUSD = -1 },
		"NaN cap":        func(o *Options) { o.MaxSpendUSD = math.NaN() },
		"infinite cap":   func(o *Options) { o.MaxSpendUSD = math.Inf(1) },
		"zero price":     func(o *Options) { o.USDPerBillionTokens = 0 },
		"negative price": func(o *Options) { o.USDPerBillionTokens = -5 },
		"NaN price":      func(o *Options) { o.USDPerBillionTokens = math.NaN() },
		"infinite price": func(o *Options) { o.USDPerBillionTokens = math.Inf(1) },
	} {
		o := opts()
		mutate(&o)
		st := newStub()
		_, err := Run(context.Background(), st, snap, history, o)
		assert.Error(t, err, name)
		assert.Error(t, o.Validate(), name)
		assert.Empty(t, st.calls, "%s: no call is made", name)
	}
	assert.NoError(t, opts().Validate())
}

// noUsage answers like the stub but reports no tokens.
type noUsage struct{ *stub }

func (n noUsage) Choice(ctx context.Context, state any, ins string, crit map[string]string) (*jev.Answer, error) {
	ans, err := n.stub.Choice(ctx, state, ins, crit)
	if ans != nil {
		ans.Usage = jev.Usage{}
	}
	return ans, err
}

func TestRunStopsWhenAnAnswerReportsNoUsage(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	st := newStub()
	rep, err := Run(context.Background(), noUsage{st}, snap, history, opts())
	require.NoError(t, err)

	assert.Equal(t, StopNoUsage, rep.Stopped)
	assert.Equal(t, 1, rep.Asked, "the run does not carry on unpriced")
	assert.Equal(t, 0, rep.Reached)
	assert.Zero(t, rep.Spend.USD)

	var buf bytes.Buffer
	rep.Write(&buf)
	assert.Contains(t, buf.String(), "STOPPED after 0 of 8 items: Jev answered with no token usage")
}

func TestRunRateLimitWaitsOnceThenRetries(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	st := newStub()
	st.first429["sum-a"] = 10 * time.Minute // beyond the cap on a wait
	var waits []time.Duration
	o := opts()
	o.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	rep, err := Run(context.Background(), st, snap, history, o)
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{time.Minute}, waits)
	assert.Equal(t, 1, rep.Forced.NoAnswer, "only sum-h is unanswered: the retry of sum-a succeeded")
	assert.Equal(t, 6, rep.Spend.Calls)
}

func TestRunRateLimitTwiceIsNoAnswer(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	st := newStub()
	st.replies["sum-a"] = reply{err: &jev.RateLimitError{}}
	o := opts()
	o.Sleep = func(context.Context, time.Duration) error { return nil }
	rep, err := Run(context.Background(), st, snap, history, o)
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Forced.NoAnswer)
	assert.Equal(t, 2, rep.Failed)
}

func TestRunUnauthorizedStopsAtOnce(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	st := newStub()
	st.replies["sum-b"] = reply{err: jev.ErrUnauthorized}
	rep, err := Run(context.Background(), st, snap, history, opts())
	require.ErrorIs(t, err, jev.ErrUnauthorized)
	assert.Equal(t, StopUnauthorized, rep.Stopped)
	assert.Equal(t, []string{"sum-a", "sum-b"}, st.calls)
	assert.Equal(t, 1, rep.Reached)
}

func TestRunInterrupted(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := newStub()
	rep, err := Run(ctx, st, snap, history, opts())
	require.NoError(t, err)
	assert.Equal(t, StopInterrupted, rep.Stopped)
	assert.Empty(t, st.calls)
}

func TestRunEmptyHistoryHasNoRatios(t *testing.T) {
	t.Parallel()
	rep, err := Run(context.Background(), newStub(), attention.Snapshot{}, nil, opts())
	require.NoError(t, err)
	assert.Nil(t, rep.Tier2.Precision)
	assert.Nil(t, rep.Tier2.Recall)
	var buf bytes.Buffer
	rep.Write(&buf)
	assert.Contains(t, buf.String(), "precision: n/a")
}

func TestAckedInLife(t *testing.T) {
	t.Parallel()
	r := life("k", attention.KindBDSlow, "s", 0, false) // t0 to t0+10m
	standing := r
	standing.ClearedAt = nil
	ack := func(key string, min int) []attention.AckRecord {
		return []attention.AckRecord{{TS: t0.Add(time.Duration(min) * time.Minute), Key: key}}
	}
	tests := []struct {
		name string
		r    attention.Record
		h    []attention.AckRecord
		want bool
	}{
		{"ack during the life", r, ack("k", 5), true},
		{"ack at the raise", r, ack("k", 0), true},
		{"ack at the clear", r, ack("k", 10), true},
		{"ack before the life", r, ack("k", -1), false},
		{"ack after the clear", r, ack("k", 11), false},
		{"another key", r, ack("other", 5), false},
		{"standing item, later ack", standing, ack("k", 600), true},
		{"no history", r, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ackedInLife(tt.r, tt.h))
		})
	}
}

func TestProxyLabels(t *testing.T) {
	t.Parallel()
	cleared := life("k", attention.KindBDSlow, "s", 0, false)
	standing := cleared
	standing.ClearedAt = nil
	ackedStanding := standing
	ackedStanding.Acked = true
	assert.Equal(t, proxyAuto, proxyTier1(cleared))
	assert.Equal(t, proxyNone, proxyTier1(standing))
	assert.Equal(t, proxyNeedsYou, proxyTier1(ackedStanding), "an acked standing item is labeled already")
	assert.Equal(t, proxyNeedsYou, proxyTier2(cleared, []attention.AckRecord{{TS: t0.Add(time.Minute), Key: "k"}}))

	// A disposition labels the item when the ack carries one.
	at := func(min int, d attention.Disposition) []attention.AckRecord {
		return []attention.AckRecord{{TS: t0.Add(time.Duration(min) * time.Minute), Key: "k", Disposition: d}}
	}
	assert.Equal(t, proxyNeedsYou, proxyTier2(cleared, at(1, attention.DispositionNeededHuman)))
	assert.Equal(t, proxyAuto, proxyTier2(cleared, at(1, attention.DispositionFixedByOverseer)))
	assert.Equal(t, proxyAuto, proxyTier2(cleared, at(1, attention.DispositionNoise)))
	// A disposition on an ack outside the life still does not label it.
	assert.Equal(t, proxyAuto, proxyTier2(cleared, at(11, attention.DispositionNeededHuman)))
	// An old line with no disposition keeps the old reading.
	assert.Equal(t, proxyNeedsYou, proxyTier2(cleared, at(1, "")))
	// The disposition wins over an ack acks.json still holds.
	assert.Equal(t, proxyAuto, proxyTier2(ackedStanding, at(1, attention.DispositionNoise)))

	// A later disposition-bearing ack supersedes an earlier one in the life,
	// whichever order the history arrives in.
	two := func(t1, t2 int, d1, d2 attention.Disposition) []attention.AckRecord {
		return []attention.AckRecord{
			{TS: t0.Add(time.Duration(t1) * time.Minute), Key: "k", Disposition: d1},
			{TS: t0.Add(time.Duration(t2) * time.Minute), Key: "k", Disposition: d2},
		}
	}
	assert.Equal(t, proxyAuto, proxyTier2(cleared, two(1, 5, attention.DispositionNeededHuman, attention.DispositionFixedByOverseer)))
	assert.Equal(t, proxyAuto, proxyTier2(cleared, two(1, 5, attention.DispositionNeededHuman, attention.DispositionNoise)))
	assert.Equal(t, proxyNeedsYou, proxyTier2(cleared, two(1, 5, attention.DispositionNoise, attention.DispositionNeededHuman)))
	paired := two(1, 5, attention.DispositionNeededHuman, attention.DispositionNoise)
	assert.Equal(t, proxyAuto, proxyTier2(cleared, []attention.AckRecord{paired[1], paired[0]}), "order in the slice does not decide")
	// A later disposition after the clear belongs to no life and changes nothing.
	assert.Equal(t, proxyNeedsYou, proxyTier2(cleared, two(1, 11, attention.DispositionNeededHuman, attention.DispositionNoise)))
}

func TestReportSaysTheLabelsAreAProxy(t *testing.T) {
	t.Parallel()
	snap, history := fixture()
	rep, err := Run(context.Background(), newStub(), snap, history, opts())
	require.NoError(t, err)
	var buf bytes.Buffer
	rep.Write(&buf)
	out := buf.String()
	for _, want := range []string{
		"proxy, not a judgment", "precision: 40.0%", "recall:    66.7%", "recall:    100.0%",
		"calibration", "Forced to needs-you by a fail-safe rule: 3", "Spend: $0.0007 of $5.00 cap",
		"tier 2: acks-history.jsonl", "tier 1:",
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "STOPPED")
}
