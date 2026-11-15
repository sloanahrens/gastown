package daemon

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/metrics"
)

// driftTick is the time the story point drift cases tick at.
var driftTick = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// driftReading is a read of the measure with one reading and one weights name.
func driftReading(rho float64, measurable bool, weights string) metrics.StoryPointsDriftRead {
	return metrics.StoryPointsDriftRead{
		Dir:     "/town/.runtime/metrics/2026-11-14",
		Weights: weights,
		Drift: metrics.StoryPointsDrift{
			N:       100,
			Current: metrics.StoryPointsDriftReading{N: 100, Rho: rho, Measurable: measurable},
		},
	}
}

// driftRig is the fixture with the measure answering read.
func driftRig(t *testing.T, read func(time.Time) (metrics.StoryPointsDriftRead, error)) *attentionFixture {
	t.Helper()
	f := newAttentionFixture(t, driftTick)
	f.src.storyPointsDrift = read
	return f
}

// storyPointEvents is the events for one key.
func storyPointEvents(t *testing.T, f *attentionFixture, key string) []attention.Event {
	t.Helper()
	var out []attention.Event
	for _, e := range f.events(t) {
		if e.Key == key {
			out = append(out, e)
		}
	}
	return out
}

// TestStoryPointsDriftAttentionRaisesOneItemNamingTheValueAndWeights: a reading
// of 0.42 raises one item, and the line the queue writes names the value, the
// weights the estimator runs and the extract it was read from.
func TestStoryPointsDriftAttentionRaisesOneItemNamingTheValueAndWeights(t *testing.T) {
	t.Parallel()
	f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
		return driftReading(0.42, true, "v3"), nil
	})

	items := f.collect(t, f.src.collectStoryPointsDrift)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	got := items[0]
	if got.Key != storyPointsDriftKey || got.Kind != attention.KindStoryPointsDrift {
		t.Errorf("item = %+v, want key %q and kind %q", got, storyPointsDriftKey, attention.KindStoryPointsDrift)
	}
	if got.Severity != attention.SeverityMedium {
		t.Errorf("severity = %q, want %q", got.Severity, attention.SeverityMedium)
	}
	for _, want := range []string{"0.42", "v3", "2026-11-14"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary %q does not name %q", got.Summary, want)
		}
	}

	st := f.tick(t, driftTick)
	if _, ok := attention.Find(st, storyPointsDriftKey); !ok {
		t.Fatalf("the drift item did not reach the queue: %+v", st.Items)
	}
	raised := storyPointEvents(t, f, storyPointsDriftKey)
	if len(raised) != 1 || raised[0].State != attention.EventNew {
		t.Fatalf("events = %+v, want one raised line", raised)
	}
	if raised[0].Rule != attention.RuleStoryPointsDrift {
		t.Errorf("rule = %q, want %q", raised[0].Rule, attention.RuleStoryPointsDrift)
	}
	if raised[0].Handled != attention.HandledOperatorAction || raised[0].Wake {
		t.Errorf("flags = %+v, want operator_action and no wake", raised[0])
	}
}

// TestStoryPointsDriftAttentionKeepsOneItemWhileItStaysUnderTheBar: the second
// reading under the bar is the same condition, so it adds no item and no line,
// and the item keeps the age it was raised with.
func TestStoryPointsDriftAttentionKeepsOneItemWhileItStaysUnderTheBar(t *testing.T) {
	t.Parallel()
	f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
		return driftReading(0.42, true, "v1"), nil
	})

	first := f.tick(t, driftTick)
	raisedAt := itemByKey(t, first, storyPointsDriftKey).FirstSeen

	second := f.tickFrom(t, first, driftTick.Add(time.Minute))
	if n := countKind(second.Items, attention.KindStoryPointsDrift); n != 1 {
		t.Fatalf("items of the drift kind = %d, want 1", n)
	}
	if got := itemByKey(t, second, storyPointsDriftKey).FirstSeen; !got.Equal(raisedAt) {
		t.Errorf("first_seen = %v, want the raised %v", got, raisedAt)
	}
	if raised := storyPointEvents(t, f, storyPointsDriftKey); len(raised) != 1 {
		t.Errorf("events = %+v, want the one raised line and no second", raised)
	}
}

// TestStoryPointsDriftAttentionBarIsTheBoundary: a reading at or over the bar is
// not drift, and one just under it is, so the comparison is read at the bar
// itself and not somewhere nearby.
func TestStoryPointsDriftAttentionBarIsTheBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rho    float64
		raised int
	}{
		{rho: 0.49, raised: 1},
		{rho: 0.5, raised: 0},
		{rho: 0.55, raised: 0},
		{rho: 1, raised: 0},
	}
	for _, tc := range tests {
		f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
			return driftReading(tc.rho, true, "v1"), nil
		})
		if items := f.collect(t, f.src.collectStoryPointsDrift); len(items) != tc.raised {
			t.Errorf("reading %v raised %+v, want %d", tc.rho, items, tc.raised)
		}
	}
}

// TestStoryPointsDriftAttentionClearsAtTheBar: the item raised under the bar is
// dropped by the tick whose reading is back at it.
func TestStoryPointsDriftAttentionClearsAtTheBar(t *testing.T) {
	t.Parallel()
	rho := 0.42
	f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
		return driftReading(rho, true, "v1"), nil
	})
	first := f.tick(t, driftTick)
	itemByKey(t, first, storyPointsDriftKey)

	rho = 0.55
	second := f.tickFrom(t, first, driftTick.Add(time.Minute))
	if _, ok := attention.Find(second, storyPointsDriftKey); ok {
		t.Errorf("the item is still queued after a reading at the bar: %+v", second.Items)
	}
	events := storyPointEvents(t, f, storyPointsDriftKey)
	if len(events) != 2 || events[1].State != attention.EventCleared {
		t.Errorf("events = %+v, want a raised and a cleared line", events)
	}
}

// TestStoryPointsDriftAttentionRaisesNothingForAnUnmeasurableReading: a reading
// nobody could take is not drift, and it does not clear a drift that stands.
func TestStoryPointsDriftAttentionRaisesNothingForAnUnmeasurableReading(t *testing.T) {
	t.Parallel()
	f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
		return driftReading(math.NaN(), false, "v1"), nil
	})
	if items := f.collect(t, f.src.collectStoryPointsDrift); len(items) != 0 {
		t.Errorf("items = %+v, want nothing for an unmeasurable reading", items)
	}

	held := attention.Item{Key: storyPointsDriftKey, Kind: attention.KindStoryPointsDrift, Summary: "held"}
	f.src.prev = attention.State{Items: []attention.Item{held}}
	items := f.collect(t, f.src.collectStoryPointsDrift)
	if len(items) != 1 || items[0].Key != storyPointsDriftKey {
		t.Errorf("items = %+v, want the held drift item kept", items)
	}
}

// TestStoryPointsDriftAttentionRaisesTheUnreadableItemWhenTheReadFails: a read
// that cannot answer raises one item naming the file and the fault, so a town
// whose extract stopped does not look like a town without drift.
func TestStoryPointsDriftAttentionRaisesTheUnreadableItemWhenTheReadFails(t *testing.T) {
	t.Parallel()
	const why = "no landings.csv under /town/.runtime/metrics (run gt metrics extract first)"
	f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
		return metrics.StoryPointsDriftRead{}, errors.New(why)
	})

	items := f.collect(t, f.src.collectStoryPointsDrift)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly one", items)
	}
	got := items[0]
	if got.Key != storyPointsDriftUnreadableKey || got.Kind != attention.KindStoryPointsDrift {
		t.Errorf("item = %+v, want key %q and kind %q", got, storyPointsDriftUnreadableKey, attention.KindStoryPointsDrift)
	}
	if got.Severity != attention.SeverityMedium {
		t.Errorf("severity = %q, want no higher than the drift item's %q", got.Severity, attention.SeverityMedium)
	}
	if !strings.Contains(got.Summary, "no landings.csv") || !strings.Contains(got.Summary, "/town/.runtime/metrics") {
		t.Errorf("summary %q does not name the file and the fault", got.Summary)
	}
}

// TestStoryPointsDriftAttentionRefusesAMeasurableReadingWithNoNumber: a reading
// the measure says is measurable but that carries no number is refused before
// the comparison, so no item ever prints a NaN as the value.
func TestStoryPointsDriftAttentionRefusesAMeasurableReadingWithNoNumber(t *testing.T) {
	t.Parallel()
	for _, rho := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
			return driftReading(rho, true, "v1"), nil
		})
		items := f.collect(t, f.src.collectStoryPointsDrift)
		if len(items) != 1 || items[0].Key != storyPointsDriftUnreadableKey {
			t.Errorf("reading %v raised %+v, want the unreadable item", rho, items)
		}
		if len(items) == 1 && !strings.Contains(items[0].Summary, fmt.Sprint(rho)) {
			t.Errorf("summary %q does not name the reading %v", items[0].Summary, rho)
		}
	}
}

// TestStoryPointsDriftAttentionClearsTheUnreadableItemOnTheNextGoodRead: the
// unreadable item is the measure's failure, so a later readable measure takes it
// off the queue.
func TestStoryPointsDriftAttentionClearsTheUnreadableItemOnTheNextGoodRead(t *testing.T) {
	t.Parallel()
	fail := true
	f := driftRig(t, func(time.Time) (metrics.StoryPointsDriftRead, error) {
		if fail {
			return metrics.StoryPointsDriftRead{}, errors.New("read landings.csv: permission denied")
		}
		return driftReading(0.9, true, "v1"), nil
	})

	first := f.tick(t, driftTick)
	itemByKey(t, first, storyPointsDriftUnreadableKey)

	fail = false
	second := f.tickFrom(t, first, driftTick.Add(time.Minute))
	if _, ok := attention.Find(second, storyPointsDriftUnreadableKey); ok {
		t.Errorf("the unreadable item is still queued after a good read: %+v", second.Items)
	}
	if n := countKind(second.Items, attention.KindStoryPointsDrift); n != 0 {
		t.Errorf("items of the drift kind = %d after a healthy read, want 0", n)
	}
	events := storyPointEvents(t, f, storyPointsDriftUnreadableKey)
	if len(events) != 2 || events[1].State != attention.EventCleared {
		t.Errorf("events = %+v, want a raised and a cleared line", events)
	}
}

// TestStoryPointsDriftAttentionIsWiredAndRegistered: the production sources
// carry the read, the read reaches the town's own extract, and the collector is
// one of the queue's.
func TestStoryPointsDriftAttentionIsWiredAndRegistered(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t)
	s := d.attentionSources(driftTick)
	if s.storyPointsDrift == nil {
		t.Fatal("the production sources carry no drift read")
	}
	read, err := s.storyPointsDrift(driftTick)
	if err == nil {
		t.Fatalf("the read of an empty town answered %+v, want an error naming the extract", read)
	}
	if !strings.Contains(err.Error(), filepath.Join(d.config.TownRoot, ".runtime", "metrics")) {
		t.Errorf("err = %q, want it to name the town's metrics directory", err)
	}
	if c := collectorFor(t, s, attention.KindStoryPointsDrift); c.collect == nil {
		t.Error("the drift collector is registered with no collect")
	}
}

// countKind is the items of one kind.
func countKind(items []attention.Item, kind attention.Kind) int {
	n := 0
	for _, it := range items {
		if it.Kind == kind {
			n++
		}
	}
	return n
}
