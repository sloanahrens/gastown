package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
)

// holdStaleNow is the tick every collection in these tests judges against, so
// no test reads the wall clock.
var holdStaleNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// closedBlocker is a blocking dependency that has closed; openBlocker has not.
var (
	closedBlocker = beads.IssueDep{ID: "gt-dep1", Status: "closed", DependencyType: "blocks"}
	openBlocker   = beads.IssueDep{ID: "gt-dep2", Status: "open", DependencyType: "blocks"}
)

// heldBead is an open bead created age before holdStaleNow, wearing labels.
func heldBead(id string, age time.Duration, labels []string, deps ...beads.IssueDep) *beads.Issue {
	return &beads.Issue{
		ID:           id,
		Status:       "open",
		CreatedAt:    holdStaleNow.Add(-age).Format(time.RFC3339),
		Labels:       labels,
		Dependencies: deps,
	}
}

// holdStaleStore serves the fixture's hold reads from issues: a list returns
// the beads carrying the label, and a show returns the bead by id.
func holdStaleStore(f *attentionFixture, issues ...*beads.Issue) {
	f.src.holdRigs = func() []string { return []string{attentionRig} }
	f.src.holdBeads = func(_ context.Context, _ string, label string) ([]*beads.Issue, error) {
		var out []*beads.Issue
		for _, is := range issues {
			if holdHasLabel(is, label) {
				out = append(out, is)
			}
		}
		return out, nil
	}
	f.src.holdBead = func(_ context.Context, _ string, id string) (*beads.Issue, error) {
		for _, is := range issues {
			if is.ID == id {
				return is, nil
			}
		}
		return nil, fmt.Errorf("no bead %s", id)
	}
}

func holdHasLabel(is *beads.Issue, label string) bool {
	for _, have := range is.Labels {
		if have == label {
			return true
		}
	}
	return false
}

// holdStaleItems runs the hold-stale collector once.
func holdStaleItems(t *testing.T, f *attentionFixture) []attention.Item {
	t.Helper()
	c := collectorFor(t, f.src, attention.KindHoldStale)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	return items
}

// A hold raises one item at one day with every dependency closed, and none
// before that. The summary names the bead, its labels, its age and the rule.
func TestHoldStaleOneDayWithDependenciesClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		age       time.Duration
		deps      []beads.IssueDep
		wantItems int
	}{
		{name: "23 hours, dependency closed: none", age: 23 * time.Hour, deps: []beads.IssueDep{closedBlocker}, wantItems: 0},
		{name: "25 hours, dependency closed: one", age: 25 * time.Hour, deps: []beads.IssueDep{closedBlocker}, wantItems: 1},
		{name: "25 hours, no dependencies: one", age: 25 * time.Hour, wantItems: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, holdStaleNow)
			holdStaleStore(f, heldBead("gt-abc12", tc.age, []string{"overseer-only"}, tc.deps...))

			items := holdStaleItems(t, f)
			if len(items) != tc.wantItems {
				t.Fatalf("items = %+v, want %d", items, tc.wantItems)
			}
			if tc.wantItems == 0 {
				return
			}
			it := items[0]
			if it.Key != "hold-stale:gt-abc12" || it.Kind != attention.KindHoldStale ||
				it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.Bead != "gt-abc12" {
				t.Errorf("item = %+v, want a low hold-stale item on gt-abc12", it)
			}
			for _, want := range []string{"gt-abc12", "overseer-only", "25h0m", "over one day, every dependency closed"} {
				if !strings.Contains(it.Summary, want) {
					t.Errorf("summary %q does not name %q", it.Summary, want)
				}
			}
		})
	}
}

// A hold with an open dependency raises nothing before three days, and one
// high item after three days whatever its dependencies. Past three days with
// every dependency closed it is still one high item, not a low one beside it.
func TestHoldStaleOpenDependencyWaitsForThreeDays(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		age       time.Duration
		deps      []beads.IssueDep
		wantItems int
		wantSev   attention.Severity
	}{
		{name: "25 hours, dependency open: none", age: 25 * time.Hour, deps: []beads.IssueDep{openBlocker}, wantItems: 0},
		{name: "71 hours, dependency open: none", age: 71 * time.Hour, deps: []beads.IssueDep{openBlocker}, wantItems: 0},
		{name: "73 hours, dependency open: one high", age: 73 * time.Hour, deps: []beads.IssueDep{openBlocker}, wantItems: 1, wantSev: attention.SeverityHigh},
		{name: "73 hours, dependency closed: one high", age: 73 * time.Hour, deps: []beads.IssueDep{closedBlocker}, wantItems: 1, wantSev: attention.SeverityHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, holdStaleNow)
			holdStaleStore(f, heldBead("gt-abc12", tc.age, []string{"needs-overseer"}, tc.deps...))

			items := holdStaleItems(t, f)
			if len(items) != tc.wantItems {
				t.Fatalf("items = %+v, want %d", items, tc.wantItems)
			}
			if tc.wantItems == 1 && items[0].Severity != tc.wantSev {
				t.Errorf("severity = %s, want %s", items[0].Severity, tc.wantSev)
			}
			if tc.wantItems == 1 && !strings.Contains(items[0].Summary, "over three days") {
				t.Errorf("summary %q does not name the three-day rule", items[0].Summary)
			}
		})
	}
}

// Each hold label raises the item, so a bead wearing any one of them is held.
func TestHoldStaleEveryHoldLabelRaises(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"overseer-only", "needs-overseer", "gt:needs-overseer", "held"} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, holdStaleNow)
			holdStaleStore(f, heldBead("gt-abc12", 25*time.Hour, []string{label}))

			items := holdStaleItems(t, f)
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one for label %q", items, label)
			}
		})
	}
}

// A bead wearing two hold labels raises one item, and its summary names both.
func TestHoldStaleOneBeadOneItemAcrossLabels(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, holdStaleNow)
	holdStaleStore(f, heldBead("gt-abc12", 25*time.Hour, []string{"overseer-only", "held"}))

	items := holdStaleItems(t, f)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	if !strings.Contains(items[0].Summary, "overseer-only, held") {
		t.Errorf("summary %q does not name both labels", items[0].Summary)
	}
}

// A closed bead raises nothing, however old its hold.
func TestHoldStaleDropsClosedBeads(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, holdStaleNow)
	closed := heldBead("gt-abc12", 73*time.Hour, []string{"overseer-only"})
	closed.Status = "closed"
	holdStaleStore(f, closed)

	if items := holdStaleItems(t, f); len(items) != 0 {
		t.Errorf("items = %+v for a closed bead, want none", items)
	}
}

// The item keeps one key over ticks, so a bead held for several ticks raises one
// item. Removing its labels clears it, and so does closing it.
func TestHoldStaleClearsWhenReleasedOrClosed(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, holdStaleNow)
	bead := heldBead("gt-abc12", 25*time.Hour, []string{"overseer-only"})
	holdStaleStore(f, bead)

	first := f.tick(t, holdStaleNow)
	second := f.tick(t, holdStaleNow.Add(5*time.Minute))
	if n := countHoldStale(first); n != 1 {
		t.Fatalf("first tick raised %d hold-stale items, want 1", n)
	}
	if n := countHoldStale(second); n != 1 {
		t.Fatalf("second tick raised %d hold-stale items, want 1", n)
	}

	bead.Labels = nil
	released := f.tick(t, holdStaleNow.Add(10*time.Minute))
	if n := countHoldStale(released); n != 0 {
		t.Errorf("after the label is removed, %d hold-stale items remain, want none", n)
	}

	bead.Labels = []string{"overseer-only"}
	bead.Status = "closed"
	closed := f.tick(t, holdStaleNow.Add(15*time.Minute))
	if n := countHoldStale(closed); n != 0 {
		t.Errorf("after the bead closes, %d hold-stale items remain, want none", n)
	}
}

func countHoldStale(state attention.State) int {
	n := 0
	for _, it := range state.Items {
		if it.Kind == attention.KindHoldStale {
			n++
		}
	}
	return n
}

// A failed label list keeps the previous tick's hold-stale item rather than
// clearing it as an empty queue, and the collector reports the error.
func TestHoldStaleFailedReadKeepsThePreviousItem(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, holdStaleNow)
	holdStaleStore(f, heldBead("gt-abc12", 25*time.Hour, []string{"overseer-only"}))
	if n := countHoldStale(f.tick(t, holdStaleNow)); n != 1 {
		t.Fatalf("first tick raised %d hold-stale items, want 1", n)
	}

	f.src.holdBeads = func(context.Context, string, string) ([]*beads.Issue, error) {
		return nil, errors.New("bd list timed out")
	}
	after := f.tick(t, holdStaleNow.Add(5*time.Minute))
	if n := countHoldStale(after); n != 1 {
		t.Errorf("after a failed list, %d hold-stale items remain, want the previous one kept", n)
	}

	c := collectorFor(t, f.src, attention.KindHoldStale)
	items, err := c.collect(context.Background())
	if err == nil || items != nil {
		t.Errorf("collect over a failed list = %+v, %v; want no items and the list error", items, err)
	}
}

// A failed dependency read, a created time that does not parse, and an unshowable
// bead each fail the collection: none passes as a clean queue.
func TestHoldStaleRefusesUnreadableInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *attentionFixture)
	}{
		{
			name: "dependency read fails",
			setup: func(f *attentionFixture) {
				holdStaleStore(f, heldBead("gt-abc12", 25*time.Hour, []string{"overseer-only"}))
				f.src.holdBead = func(context.Context, string, string) (*beads.Issue, error) {
					return nil, errors.New("bd show timed out")
				}
			},
		},
		{
			name: "created time does not parse",
			setup: func(f *attentionFixture) {
				bead := heldBead("gt-abc12", 25*time.Hour, []string{"overseer-only"})
				bead.CreatedAt = "yesterday"
				holdStaleStore(f, bead)
			},
		},
		{
			name: "bead listed but not shown",
			setup: func(f *attentionFixture) {
				holdStaleStore(f, heldBead("gt-abc12", 25*time.Hour, []string{"overseer-only"}))
				f.src.holdBead = func(context.Context, string, string) (*beads.Issue, error) {
					return nil, nil
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, holdStaleNow)
			tc.setup(f)
			c := collectorFor(t, f.src, attention.KindHoldStale)
			items, err := c.collect(context.Background())
			if err == nil {
				t.Fatalf("collector read the input as clean: items = %+v", items)
			}
			if items != nil {
				t.Errorf("items = %+v beside an unreadable input, want none", items)
			}
		})
	}
}

// The collector owns only its own items, so a failed collection keeps the
// previous tick's hold-stale item and no other kind's.
func TestHoldStaleOwnsOnlyItsOwnItems(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, holdStaleNow)
	c := collectorFor(t, f.src, attention.KindHoldStale)
	hold := attention.Item{Key: "hold-stale:gt-abc12", Kind: attention.KindHoldStale, Severity: attention.SeverityLow}
	other := attention.Item{Key: "config-parse-failed", Kind: attention.KindConfigParseFailed, Severity: attention.SeverityHigh}
	prev := attention.State{Items: []attention.Item{hold, other}}

	got := c.kept(prev)
	if len(got) != 1 || got[0].Key != hold.Key {
		t.Errorf("kept(prev) = %+v, want only the hold-stale item", got)
	}
}
