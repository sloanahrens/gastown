package scorecard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// warnLines returns a warn seam that appends each line it is given, formatted,
// to lines. A test reads the warnings through it instead of swapping os.Stderr.
func warnLines(lines *[]string) func(format string, args ...any) {
	return func(format string, args ...any) {
		*lines = append(*lines, fmt.Sprintf(format, args...))
	}
}

// goneShowBeads answers Show one id at a time and fails any batch that names a
// bead the store no longer holds with beads.ErrNotFound, as bd fails a show
// whose line holds a missing id.
type goneShowBeads struct {
	issues  map[string]*beads.Issue
	gone    map[string]bool
	shows   []string
	batches [][]string
}

func (g *goneShowBeads) Show(id string) (*beads.Issue, error) {
	g.shows = append(g.shows, id)
	if issue, ok := g.issues[id]; ok {
		return issue, nil
	}
	return nil, fmt.Errorf("%w: %s", beads.ErrNotFound, id)
}

func (g *goneShowBeads) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	g.batches = append(g.batches, append([]string(nil), ids...))
	for _, id := range ids {
		if g.gone[id] {
			return nil, fmt.Errorf("bd show: %w", beads.ErrNotFound)
		}
	}
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := g.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

// goneListBeads answers Show one id at a time, and ListByIDs with
// beads.ErrNotFound for any query naming a bead the store no longer holds: the
// failure showAll re-reads one id at a time. It has no batch read, so showAll
// is driven down its lister path.
type goneListBeads struct {
	issues map[string]*beads.Issue
	gone   map[string]bool
	lists  [][]string
}

func (g *goneListBeads) Show(id string) (*beads.Issue, error) {
	if issue, ok := g.issues[id]; ok {
		return issue, nil
	}
	return nil, fmt.Errorf("%w: %s", beads.ErrNotFound, id)
}

func (g *goneListBeads) ListByIDs(ids []string) (map[string]*beads.Issue, error) {
	g.lists = append(g.lists, append([]string(nil), ids...))
	for _, id := range ids {
		if g.gone[id] {
			return nil, fmt.Errorf("bd list: %w", beads.ErrNotFound)
		}
	}
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := g.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

// warnShowErr says the count on one line, with the rig when it is given one,
// and hands the errors a caller still has to warn about back to it; a count of
// zero and an error holding no count are the caller's whole.
func TestWarnShowErr(t *testing.T) {
	t.Parallel()
	realErr := errors.New("dolt down")
	zero := &GoneError{N: 0}
	wrapped := fmt.Errorf("reading the batch: %w", &GoneError{N: 1})
	cases := []struct {
		name    string
		subject string
		err     error
		lines   []string
		// want is returned as it is.
		want error
		// keeps names an error the result must still hold.
		keeps error
	}{
		{name: "no error", err: nil},
		{name: "an error with no count", err: realErr, want: realErr},
		{name: "a count of zero", err: zero, want: zero},
		{name: "the count alone", err: &GoneError{N: 3}, lines: []string{"3 beads gone"}},
		{name: "the count of a rig", subject: "gastown", err: &GoneError{N: 3}, lines: []string{"3 beads gone in gastown"}},
		{name: "the count joined with a real error", err: errors.Join(&GoneError{N: 2}, realErr),
			lines: []string{"2 beads gone"}, keeps: realErr},
		{name: "a count wrapped in another error", err: wrapped, lines: []string{"1 beads gone"}, want: wrapped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var lines []string
			got := warnShowErr(warnLines(&lines), c.subject, c.err)
			if !reflect.DeepEqual(lines, c.lines) {
				t.Fatalf("warned %q, want %q", lines, c.lines)
			}
			switch {
			case c.want != nil:
				if got != c.want {
					t.Errorf("warnShowErr = %v, want %v as it is", got, c.want)
				}
			case c.keeps != nil:
				if !errors.Is(got, c.keeps) {
					t.Errorf("warnShowErr = %v, want the real error kept", got)
				}
				var gone *GoneError
				if errors.As(got, &gone) {
					t.Errorf("warnShowErr = %v, want the count taken out", got)
				}
			default:
				if got != nil {
					t.Errorf("warnShowErr = %v, want nil", got)
				}
			}
		})
	}
}

// attentionItemLines names one bead per item, as the battery lines do.
func attentionItemLines(a *attentionAdapter, beads ...string) []Item {
	items := make([]Item, len(beads))
	for i, bead := range beads {
		id := fmt.Sprintf("i%d", i)
		items[i] = Item{ID: id}
		a.lines[id] = attentionLine(bead)
	}
	return items
}

// Three beads the store no longer holds are said once through the batch read,
// however many there are, and the ones it does hold are cached: gt-mejw9j.
func TestAttentionPrefetchSaysGoneBeadsOnce(t *testing.T) {
	t.Parallel()
	store := &goneShowBeads{
		issues: map[string]*beads.Issue{"gt-a": {ID: "gt-a"}, "gt-b": {ID: "gt-b"}},
		gone:   map[string]bool{"gt-x": true, "gt-y": true, "gt-z": true},
	}
	a := newAttentionTestAdapter(t.TempDir(), store)
	var warnings []string
	a.warn = warnLines(&warnings)
	items := attentionItemLines(a, "gt-a", "gt-x", "gt-b", "gt-y", "gt-z")

	a.Prefetch(context.Background(), items)

	if !reflect.DeepEqual(warnings, []string{"3 beads gone"}) {
		t.Fatalf("warned %q, want one line %q", warnings, "3 beads gone")
	}
	if len(store.batches) == 0 {
		t.Errorf("Prefetch made no batch call, want the batcher path driven")
	}
	if len(a.beadCache) != 2 || a.beadCache["gt-a"] == nil || a.beadCache["gt-b"] == nil {
		t.Errorf("cached %v, want the 2 beads the store holds", a.beadCache)
	}
	for _, bead := range []string{"gt-a", "gt-b"} {
		if issue, err := a.bead(bead); err != nil || issue == nil {
			t.Errorf("bead(%s) = %v, %v; want the cached bead", bead, issue, err)
		}
	}
	if len(store.shows) != 0 {
		t.Errorf("read %v one at a time after the batch, want the cache", store.shows)
	}
}

// The lister path counts the gone beads too: the query that names one fails as
// bd list fails, the single reads keep what the store holds, and the count is
// said once: gt-mejw9j.
func TestAttentionPrefetchListerSaysGoneBeadsOnce(t *testing.T) {
	t.Parallel()
	store := &goneListBeads{
		issues: map[string]*beads.Issue{"gt-a": {ID: "gt-a"}, "gt-b": {ID: "gt-b"}},
		gone:   map[string]bool{"gt-x": true, "gt-y": true, "gt-z": true},
	}
	a := newAttentionTestAdapter(t.TempDir(), store)
	var warnings []string
	a.warn = warnLines(&warnings)
	items := attentionItemLines(a, "gt-a", "gt-x", "gt-b", "gt-y", "gt-z")

	a.Prefetch(context.Background(), items)

	if !reflect.DeepEqual(warnings, []string{"3 beads gone"}) {
		t.Fatalf("warned %q, want one line %q", warnings, "3 beads gone")
	}
	if len(store.lists) == 0 {
		t.Errorf("Prefetch made no list call, want the lister path driven")
	}
	if len(a.beadCache) != 2 || a.beadCache["gt-a"] == nil || a.beadCache["gt-b"] == nil {
		t.Errorf("cached %v, want the 2 beads the store holds", a.beadCache)
	}
}

// A batch that fails for another reason still warns, and says nothing about
// beads gone.
func TestAttentionPrefetchWarnsOnARealBatchFailure(t *testing.T) {
	t.Parallel()
	store := &batchBeads{issues: map[string]*beads.Issue{"gt-a": {ID: "gt-a"}}, batchErr: errors.New("bd down")}
	a := newAttentionTestAdapter(t.TempDir(), store)
	var warnings []string
	a.warn = warnLines(&warnings)
	items := attentionItemLines(a, "gt-a")

	a.Prefetch(context.Background(), items)

	want := []string{"reading the attention beads in a batch: bd down"}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warned %q, want %q", warnings, want)
	}
}

// A bead gone from the store leaves the attention report exactly as the
// one-at-a-time read made it: the count is a warning line, not a label.
func TestAttentionPrefetchGoneBeadsLeaveTheReportAsItWas(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := attentionCases()
	writeAttentionTown(t, town, cases)
	gone := map[string]bool{}
	for _, c := range cases {
		if c.bead != "" && c.issue == nil {
			gone[c.bead] = true
		}
	}
	if !gone["hq-17"] {
		t.Fatalf("the cases no longer name a bead the store does not hold")
	}

	want := reportJSON(t, withoutPrefetch{newAttentionTestAdapter(town, &goneShowBeads{issues: beadsOf(cases).issues, gone: gone})})
	store := &goneShowBeads{issues: beadsOf(cases).issues, gone: gone}
	a := newAttentionTestAdapter(town, store)
	a.warn = func(string, ...any) {}
	got := reportJSON(t, a)

	if got != want {
		t.Errorf("the batched report differs from the per-item report\n got: %s\nwant: %s", got, want)
	}
	if len(store.batches) == 0 {
		t.Errorf("Prefetch made no batch call, want the read the gone bead comes from")
	}
}

// The finding adapter says the count once too, and caches what it read.
func TestFindingPrefetchSaysGoneBeadsOnce(t *testing.T) {
	t.Parallel()
	store := &goneShowBeads{
		issues: map[string]*beads.Issue{"gt-f1": {ID: "gt-f1", Status: "open"}},
		gone:   map[string]bool{"gt-f2": true},
	}
	f := NewFinding(t.TempDir(), store, &countingDiffs{lines: map[string]int{}}).(*finding)
	var warnings []string
	f.warn = warnLines(&warnings)

	f.Prefetch(context.Background(), []Item{{ID: "gt-f1"}, {ID: "gt-f2"}})

	if !reflect.DeepEqual(warnings, []string{"1 beads gone"}) {
		t.Fatalf("warned %q, want one line %q", warnings, "1 beads gone")
	}
}

// The dispatch adapter says the count once too, and keeps the bead it read.
func TestDispatchPrefetchSaysGoneBeadsOnce(t *testing.T) {
	t.Parallel()
	store := &goneShowBeads{
		issues: map[string]*beads.Issue{"gt-d1": {ID: "gt-d1"}},
		gone:   map[string]bool{"gt-d2": true},
	}
	a := newDispatchAdapter(t.TempDir(), func() (beadReader, error) { return store, nil })
	var warnings []string
	a.warn = warnLines(&warnings)

	a.Prefetch(context.Background(), []Item{{ID: "gt-d1"}, {ID: "gt-d2"}})

	if !reflect.DeepEqual(warnings, []string{"1 beads gone"}) {
		t.Fatalf("warned %q, want one line %q", warnings, "1 beads gone")
	}
	if a.shown["gt-d1"] == nil {
		t.Errorf("shown = %v, want the bead the store holds", a.shown)
	}
}

// The town notes' batch says the count once, names the rig it is of, and keeps
// the bead the store holds; the rigs that read through it warn only about what
// else failed.
func TestTownNotesShowAllSaysGoneBeadsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(_ context.Context, argv []string) ([]byte, []byte, error) {
		switch argv[0] {
		case "list":
			return []byte(`[{"id":"gt-a","status":"open"}]`), nil, nil
		case "show":
			for _, arg := range argv {
				if arg == "gt-gone" {
					return []byte(`{"error":{"kind":"not_found"}}`), []byte("issue not found"), errors.New("exit status 1")
				}
			}
			return []byte(`[{"id":"gt-a","status":"open"}]`), nil, nil
		}
		return nil, nil, fmt.Errorf("unexpected bd call %v", argv)
	}
	tn := &townNotes{town: dir, stores: map[string]*beads.Beads{
		"gastown": beads.New(dir, beads.WithRunner(run)),
	}}
	var warnings []string
	tn.warn = warnLines(&warnings)

	got, gotErr := tn.ShowAll("gastown", []string{"gt-a", "gt-gone"})

	if !reflect.DeepEqual(warnings, []string{"1 beads gone in gastown"}) {
		t.Fatalf("warned %q, want one line %q", warnings, "1 beads gone in gastown")
	}
	if gotErr != nil {
		t.Errorf("ShowAll error = %v, want the count taken out", gotErr)
	}
	if len(got) != 1 || got["gt-a"] == nil {
		t.Errorf("ShowAll = %v, want the bead the store holds", got)
	}
}
