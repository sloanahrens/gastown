package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/rules"
)

// beadRef names one bead by its rig and id. The fake reader keys by it, so a
// test's expectations do not depend on how commentKey spells a key.
type beadRef struct{ rig, bead string }

// prefetchLine is one logged rules line for bead in rig, submitted at doneAt by
// polecat. Its answers are the ones the report labels from.
func prefetchLine(t *testing.T, rig, bead, polecat string, doneAt time.Time) []byte {
	t.Helper()
	noul := 0.9
	raw, err := json.Marshal(rules.Line{
		TS:      doneAt.Add(time.Minute),
		Rig:     rig,
		Bead:    bead,
		Polecat: polecat,
		DoneAt:  doneAt,
		Model:   "jev-test",
		Answers: map[string]rules.Answer{
			rules.QBrokeStandingRule: {Noul: &noul},
			rules.QRuleKind:          {Choice: rules.KindMainPush, Probabilities: map[string]float64{rules.KindMainPush: 1}},
		},
		QuestionVersion: rules.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeCommentReader serves comments by bead and counts the reads of each. A
// bead in fail returns its error, and a bead not in comments has none. It is
// safe for the pool's workers to share, since they only read its maps.
type fakeCommentReader struct {
	comments map[beadRef][]beads.Comment
	fail     map[beadRef]error

	mu    sync.Mutex
	reads map[beadRef]int
}

func newFakeCommentReader() *fakeCommentReader {
	return &fakeCommentReader{
		comments: map[beadRef][]beads.Comment{},
		fail:     map[beadRef]error{},
		reads:    map[beadRef]int{},
	}
}

func (f *fakeCommentReader) read(rig, bead string) ([]beads.Comment, error) {
	ref := beadRef{rig, bead}
	f.mu.Lock()
	f.reads[ref]++
	f.mu.Unlock()
	if err := f.fail[ref]; err != nil {
		return nil, err
	}
	return f.comments[ref], nil
}

// readsByBead copies the read count of each bead.
func (f *fakeCommentReader) readsByBead() map[beadRef]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[beadRef]int, len(f.reads))
	for ref, n := range f.reads {
		out[ref] = n
	}
	return out
}

// newPrefetchAdapter is the rules adapter for town, its clock at now, and the
// comments read by read. Label reads the town's refusal ledger, so town is a
// temporary directory.
func newPrefetchAdapter(town string, now time.Time, read func(rig, bead string) ([]beads.Comment, error)) *rulesAdapter {
	return &rulesAdapter{
		town:     town,
		comments: read,
		lines:    map[string]rules.Line{},
		now:      func() time.Time { return now },
	}
}

// prefetchFixtureDoneAt is the submission time of the fixtures' first line, and
// prefetchFixtureNow is the clock the adapters read: two hours after it, so a
// line submitted at prefetchFixtureDoneAt has a closed window.
var prefetchFixtureDoneAt = rulesDoneAt

func prefetchFixtureNow() time.Time { return prefetchFixtureDoneAt.Add(2 * time.Hour) }

// writeRulesLog writes lines to the rules log of town, where Build reads them.
func writeRulesLog(t *testing.T, town string, lines ...[]byte) {
	t.Helper()
	path := rules.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	for _, l := range lines {
		buf = append(append(buf, l...), '\n')
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// serialRules hides Prefetch, so Build labels each item one at a time, as it
// did before Prefetch. Context stays, so the two reports compare whole.
type serialRules struct {
	Adapter
	contextual Contextual
}

func (s serialRules) Context() []string { return s.contextual.Context() }

// TestRulesPrefetchMatchesSerialLabels runs the same log through Build once
// with Prefetch and once without. The reports must be identical, labels,
// rules and the sessions count included, and the reader must have been asked
// about the same beads, each once. A bead shared by two lines and a line still
// inside its window are both in the fixture.
func TestRulesPrefetchMatchesSerialLabels(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d0 := prefetchFixtureDoneAt
	writeRulesLog(t, town,
		prefetchLine(t, "gastown", "gt-a", "furiosa", d0),
		prefetchLine(t, "gastown", "gt-a", "furiosa", d0.Add(time.Hour)),
		prefetchLine(t, "gastown", "gt-b", "", d0),
		prefetchLine(t, "gastown", "gt-fresh", "furiosa", prefetchFixtureNow().Add(-5*time.Minute)),
	)
	// A stand-down by gt-a's polecat two minutes after the first submission
	// labels the first line, and sits outside the second line's window.
	standDown := reworkStandDownPrefix + ": head unchanged since the rejection"
	comments := map[beadRef][]beads.Comment{
		{"gastown", "gt-a"}: {{
			Author:    "gastown/polecats/furiosa",
			Text:      standDown,
			CreatedAt: d0.Add(2 * time.Minute).Format(time.RFC3339Nano),
		}},
	}

	run := func(prefetch bool) (string, map[beadRef]int) {
		reader := newFakeCommentReader()
		reader.comments = comments
		a := newPrefetchAdapter(town, prefetchFixtureNow(), reader.read)
		var adapter Adapter = a
		if !prefetch {
			adapter = serialRules{Adapter: a, contextual: a}
		}
		return reportJSON(t, adapter), reader.readsByBead()
	}
	withJSON, withReads := run(true)
	serialJSON, serialReads := run(false)

	if withJSON != serialJSON {
		t.Errorf("report with Prefetch differs from the serial report\nwith:   %s\nserial: %s", withJSON, serialJSON)
	}
	if !reflect.DeepEqual(withReads, serialReads) {
		t.Errorf("reads with Prefetch = %v, serially = %v", withReads, serialReads)
	}
	want := map[beadRef]int{{"gastown", "gt-a"}: 1, {"gastown", "gt-b"}: 1}
	if !reflect.DeepEqual(withReads, want) {
		t.Errorf("reads = %v, want gt-a and gt-b once each and none for the fresh line %v", withReads, want)
	}
}

// TestRulesPrefetchReadsASharedBeadOnce checks a bead two lines share is read
// once, when Prefetch fills the cache and Label reads from it.
func TestRulesPrefetchReadsASharedBeadOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d0 := prefetchFixtureDoneAt
	writeRulesLog(t, town,
		prefetchLine(t, "gastown", "gt-a", "furiosa", d0),
		prefetchLine(t, "gastown", "gt-a", "furiosa", d0.Add(time.Hour)),
	)
	reader := newFakeCommentReader()
	a := newPrefetchAdapter(town, prefetchFixtureNow(), reader.read)

	reportJSON(t, a)

	if got := reader.readsByBead()[beadRef{"gastown", "gt-a"}]; got != 1 {
		t.Errorf("gt-a was read %d times, want 1", got)
	}
	if len(a.commentCache) != 1 {
		t.Errorf("cache holds %d beads, want 1", len(a.commentCache))
	}
}

// TestRulesPrefetchKeepsRigsApart checks two rigs' beads of one id are two
// reads, so one rig's comments never answer for another's.
func TestRulesPrefetchKeepsRigsApart(t *testing.T) {
	t.Parallel()
	d0 := prefetchFixtureDoneAt
	reader := newFakeCommentReader()
	a := newPrefetchAdapter(t.TempDir(), prefetchFixtureNow(), reader.read)
	items := parseRulesLines(t, a,
		prefetchLine(t, "gastown", "gt-a", "furiosa", d0),
		// Its own submission time: an item is named by bead and time, not rig.
		prefetchLine(t, "otherrig", "gt-a", "furiosa", d0.Add(time.Minute)),
	)

	a.Prefetch(context.Background(), items)

	want := map[beadRef]int{{"gastown", "gt-a"}: 1, {"otherrig", "gt-a"}: 1}
	if got := reader.readsByBead(); !reflect.DeepEqual(got, want) {
		t.Errorf("reads = %v, want one per rig %v", got, want)
	}
}

// TestRulesPrefetchFailureLeavesTheLabelAsItWas checks that a bead whose read
// fails, and one the store does not hold, are labeled exactly as Label labels
// them with no Prefetch, while the other beads are still labeled. Prefetch
// costs time on a failure, never a label.
func TestRulesPrefetchFailureLeavesTheLabelAsItWas(t *testing.T) {
	t.Parallel()
	d0 := prefetchFixtureDoneAt
	lines := [][]byte{
		prefetchLine(t, "gastown", "gt-ok", "furiosa", d0),
		prefetchLine(t, "gastown", "gt-err", "furiosa", d0),
		prefetchLine(t, "gastown", "gt-missing", "furiosa", d0),
	}
	errRef := beadRef{"gastown", "gt-err"}
	run := func(prefetch bool) (map[string]labelOutcome, *fakeCommentReader) {
		reader := newFakeCommentReader()
		reader.fail[errRef] = errors.New("bd comments: boom")
		reader.fail[beadRef{"gastown", "gt-missing"}] = beads.ErrNotFound
		a := newPrefetchAdapter(t.TempDir(), prefetchFixtureNow(), reader.read)
		return labelOneByOne(t, a, prefetch, lines), reader
	}
	withLabels, withReader := run(true)
	serialLabels, serialReader := run(false)

	for _, bead := range []string{"gt-ok", "gt-err", "gt-missing"} {
		got, want := withLabels[bead], serialLabels[bead]
		if !reflect.DeepEqual(got.lab, want.lab) || errString(got.err) != errString(want.err) {
			t.Errorf("%s with Prefetch = (%+v, %v), serially (%+v, %v)", bead, got.lab, got.err, want.lab, want.err)
		}
	}
	if err := serialLabels["gt-err"].err; err == nil {
		t.Errorf("gt-err serially: no error, want the read's error")
	}
	if got := serialLabels["gt-missing"]; got.lab.Labeled || got.err != nil {
		t.Errorf("gt-missing serially = (%+v, %v), want unlabeled with no error", got.lab, got.err)
	}
	if got := withLabels["gt-ok"].lab; !got.Labeled {
		t.Errorf("gt-ok with Prefetch is unlabeled, want labeled despite the other failures")
	}
	// The failed read is not cached, so Label reads that bead on its own after
	// the pool's read failed.
	if got := withReader.readsByBead()[errRef]; got != 2 {
		t.Errorf("gt-err with Prefetch was read %d times, want 2: the pool's failed read, then Label's", got)
	}
	if got := serialReader.readsByBead()[errRef]; got != 1 {
		t.Errorf("gt-err serially was read %d times, want 1", got)
	}
}

// TestRulesPrefetchWarnsForAFailedRead pins the alarm the work item asks for: a
// read that fails inside the pool is named with its bead and cause, and a read
// that succeeds says nothing. The warning goes through the adapter's warn seam,
// so the test reads it without touching os.Stderr.
func TestRulesPrefetchWarnsForAFailedRead(t *testing.T) {
	t.Parallel()
	d0 := prefetchFixtureDoneAt
	reader := newFakeCommentReader()
	reader.fail[beadRef{"gastown", "gt-err"}] = errors.New("bd comments: boom")
	a := newPrefetchAdapter(t.TempDir(), prefetchFixtureNow(), reader.read)
	var warnings []string
	a.warn = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	items := parseRulesLines(t, a,
		prefetchLine(t, "gastown", "gt-ok", "furiosa", d0),
		prefetchLine(t, "gastown", "gt-err", "furiosa", d0))

	a.Prefetch(context.Background(), items)

	want := []string{"prefetching the comments of gastown/gt-err: bd comments: boom"}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q, want %q: one for the failed read and none for gt-ok", warnings, want)
	}
}

// TestRulesPrefetchWorkerCap checks the pool runs exactly rulesPrefetchWorkers
// reads at once, and never more. Each read holds until the test releases it.
// The count is taken at quiescence: synctest.Wait returns once every goroutine
// in the bubble is blocked, so the pool has started every read it will start
// and every worker is waiting on work it cannot take. No sleep, no timing.
func TestRulesPrefetchWorkerCap(t *testing.T) {
	t.Parallel()
	if rulesPrefetchWorkers != 4 {
		t.Fatalf("rulesPrefetchWorkers = %d, want 4", rulesPrefetchWorkers)
	}
	synctest.Test(t, func(t *testing.T) {
		// More beads than the cap, so the pool's workers each take more than one.
		const n = 10
		h := newHoldingReader()
		defer h.releaseAll()
		a := newPrefetchAdapter(t.TempDir(), prefetchFixtureNow(), h.read)
		var lines [][]byte
		for i := range n {
			lines = append(lines, prefetchLine(t, "gastown", fmt.Sprintf("gt-%02d", i), "furiosa", prefetchFixtureDoneAt))
		}
		items := parseRulesLines(t, a, lines...)

		done := make(chan struct{})
		go func() {
			defer close(done)
			a.Prefetch(context.Background(), items)
		}()

		// With remaining reads still to finish, min(cap, remaining) of them are
		// in flight at quiescence. Releasing one lets the pool start the next.
		for remaining := n; remaining > 0; remaining-- {
			synctest.Wait()
			if got, want := h.inFlight(), min(rulesPrefetchWorkers, remaining); got != want {
				t.Fatalf("%d reads in flight with %d to finish, want %d", got, remaining, want)
			}
			h.release <- struct{}{}
		}
		<-done
		if peak := h.peakInFlight(); peak != rulesPrefetchWorkers {
			t.Errorf("peak reads in flight = %d, want %d", peak, rulesPrefetchWorkers)
		}
		if got := h.readCount(); got != n {
			t.Errorf("reads = %d, want %d, one per bead", got, n)
		}
	})
}

// TestRulesPrefetchCommentKeyNeedsSlashFreeBead checks the guard commentKey
// relies on: Parse rejects a bead id with a slash, so a key's bead is the part
// after its last slash.
func TestRulesPrefetchCommentKeyNeedsSlashFreeBead(t *testing.T) {
	t.Parallel()
	a := newPrefetchAdapter(t.TempDir(), prefetchFixtureNow(), nil)
	raw := prefetchLine(t, "gastown", "gt/a", "furiosa", prefetchFixtureDoneAt)
	if _, err := a.Parse(raw); err == nil {
		t.Fatal("Parse accepted a bead id with a slash, so commentKey could collide")
	}
}

// labelOutcome is what Label returned for one item.
type labelOutcome struct {
	lab Labeling
	err error
}

// labelOneByOne parses each line and labels it, after Prefetch when prefetch is
// set. It returns each outcome by bead, the way Build reads them.
func labelOneByOne(t *testing.T, a *rulesAdapter, prefetch bool, lines [][]byte) map[string]labelOutcome {
	t.Helper()
	items := parseRulesLines(t, a, lines...)
	if prefetch {
		a.Prefetch(context.Background(), items)
	}
	out := map[string]labelOutcome{}
	for _, it := range items {
		lab, err := a.Label(context.Background(), it)
		out[a.lines[it.ID].Bead] = labelOutcome{lab: lab, err: err}
	}
	return out
}

// parseRulesLines parses lines into items with a, failing the test on a bad line.
func parseRulesLines(t *testing.T, a *rulesAdapter, lines ...[]byte) []Item {
	t.Helper()
	items := make([]Item, 0, len(lines))
	for _, raw := range lines {
		it, err := a.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	return items
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// holdingReader is a comment reader whose reads hold until released. It counts
// the reads in flight and the most at once. Its results are canned: no comments
// and no error.
type holdingReader struct {
	release chan struct{}
	once    sync.Once

	mu        sync.Mutex
	flying    int
	peak      int
	readTotal int
}

func newHoldingReader() *holdingReader {
	return &holdingReader{release: make(chan struct{})}
}

func (h *holdingReader) read(_, _ string) ([]beads.Comment, error) {
	h.mu.Lock()
	h.flying++
	h.readTotal++
	h.peak = max(h.peak, h.flying)
	h.mu.Unlock()

	<-h.release

	h.mu.Lock()
	h.flying--
	h.mu.Unlock()
	return nil, nil
}

// releaseAll frees every held read, and any read after it, so the pool can
// finish when a test stops part way.
func (h *holdingReader) releaseAll() {
	h.once.Do(func() { close(h.release) })
}

func (h *holdingReader) inFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flying
}

func (h *holdingReader) peakInFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peak
}

func (h *holdingReader) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readTotal
}
