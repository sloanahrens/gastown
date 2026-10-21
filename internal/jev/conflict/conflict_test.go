package conflict

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// fakeAsker answers every pair with the same probabilities. fail, when set,
// decides per state whether the ask fails.
type fakeAsker struct {
	states []string
	fail   func(state string) error
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	f.states = append(f.states, state.(string))
	if f.fail != nil {
		if err := f.fail(state.(string)); err != nil {
			return nil, err
		}
	}
	if len(qs) != 3 {
		return nil, fmt.Errorf("asked %d questions, want 3", len(qs))
	}
	return &jev.Answers{
		Model: "jev-test-1",
		Noul:  map[string]float64{qSameFiles: 0.75, qSameFunction: 0.25},
		Score: map[string]jev.ScoreAnswer{qRisk: {Score: 2, Confidence: 0.5}},
	}, nil
}

// fakeLister lists the beads it was given, whatever the time.
type fakeLister struct {
	all []*beads.Issue
}

func (f fakeLister) Placed(time.Time) ([]*beads.Issue, error) { return f.all, nil }

// placed makes a bead placed at minute minute. A zero closedMinute leaves it
// held; a positive one makes it closed at that minute.
func placed(id string, minute int, closedMinute int) *beads.Issue {
	b := &beads.Issue{
		ID:    id,
		Title: "Title of " + id,
		Description: "attached_molecule: gt-wisp-" + id + "\n" +
			"attached_formula: mol-polecat-work\n" +
			"attached_at: " + stampAt(minute) + "\n\n" +
			"## Goal\n\nGOAL-OF-" + id + "\n\n## Constraints\n\nCONSTRAINTS-OF-" + id + "\n",
		Status: "in_progress",
	}
	if closedMinute > 0 {
		b.Status = "closed"
		b.ClosedAt = stampAt(closedMinute)
	}
	return b
}

func stampAt(minute int) string {
	return t0.Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
}

// newPass wires a pass over fakes, with its files under a temporary town and
// its cursor set to cursorAt.
func newPass(t *testing.T, all []*beads.Issue, asker Asker, cursorAt time.Time) *Pass {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".runtime", "jev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := battery.WriteCursor(CursorPath(town), battery.Cursor{Time: cursorAt}); err != nil {
		t.Fatal(err)
	}
	return &Pass{
		Asker:      asker,
		Lister:     fakeLister{all: all},
		LogPath:    LogPath(town),
		CursorPath: CursorPath(town),
		PairsPath:  PairsPath(town),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return t0.Add(time.Hour) },
	}
}

// readLog returns the lines the pass appended, in order.
func readLog(t *testing.T, path string) []Line {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []Line
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("parsing log line %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// Three beads in flight: a and b are held, and c is placed after both and
// closed later. The pass asks one pair for each of the three, and asks none of
// them again on the next pass over the same placements.
func TestPassAsksEachPairOnce(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{}
	all := []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0), placed("gt-c", 30, 40)}
	p := newPass(t, all, asker, t0)

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 3 {
		t.Errorf("Asked = %d, want 3 (b with a, c with a, c with b)", sum.Asked)
	}
	got := readLog(t, p.LogPath)
	if len(got) != 3 {
		t.Fatalf("log has %d lines, want 3", len(got))
	}
	// Each placement pairs with the in-flight beads most recent first.
	want := [][2]string{{"gt-b", "gt-a"}, {"gt-c", "gt-b"}, {"gt-c", "gt-a"}}
	for i, l := range got {
		if l.Bead != want[i][0] || l.Other != want[i][1] {
			t.Errorf("line %d pairs %s with %s, want %s with %s", i, l.Bead, l.Other, want[i][0], want[i][1])
		}
		if l.QuestionVersion != Version || l.Model != "jev-test-1" {
			t.Errorf("line %d = version %d model %q", i, l.QuestionVersion, l.Model)
		}
	}
	if len(asker.states) != 3 {
		t.Errorf("asked %d pairs, want 3", len(asker.states))
	}

	// Rewind the cursor so the same placements come round again: every pair is
	// already done, so nothing is asked and nothing is logged.
	if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	sum, err = p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || len(asker.states) != 3 {
		t.Errorf("second pass asked %d (total %d), want none new", sum.Asked, len(asker.states))
	}
	if n := len(readLog(t, p.LogPath)); n != 3 {
		t.Errorf("log has %d lines after the second pass, want 3", n)
	}
}

// A bead closed before another was placed was not in flight then, so the two
// are not paired.
func TestPassPairsOnlyInFlightBeads(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{}
	// gt-early is closed at minute 8, before gt-late is placed at minute 20.
	all := []*beads.Issue{placed("gt-early", 5, 8), placed("gt-late", 20, 0)}
	p := newPass(t, all, asker, t0)

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 {
		t.Errorf("Asked = %d, want 0: gt-early was closed before gt-late was placed", sum.Asked)
	}
}

// Of more than five beads in flight, a placement is paired with the five most
// recent ones only.
func TestPassPairsAtMostFiveInFlight(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{}
	var all []*beads.Issue
	for i := 1; i <= 7; i++ {
		all = append(all, placed(fmt.Sprintf("gt-%d", i), i*10, 0))
	}
	p := newPass(t, all, asker, t0)

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Bead k is paired with the min(k-1, 5) placements before it.
	if want := 0 + 1 + 2 + 3 + 4 + 5 + 5; sum.Asked != want {
		t.Errorf("Asked = %d, want %d", sum.Asked, want)
	}
	for _, l := range readLog(t, p.LogPath) {
		if l.Bead == "gt-7" && l.Other == "gt-1" {
			t.Errorf("gt-7 was paired with gt-1, the sixth most recent placement before it")
		}
	}
}

// The first pass over a town with no cursor only sets one, and asks nothing.
func TestPassFirstRunAsksNothing(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{}
	p := newPass(t, []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0)}, asker, t0)
	if err := os.Remove(p.CursorPath); err != nil {
		t.Fatal(err)
	}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Started || sum.Asked != 0 || len(asker.states) != 0 {
		t.Errorf("first run = %+v, asked %d; want Started and nothing asked", sum, len(asker.states))
	}
}

// The day's cap stops the pass before the next pair, and the pairs it did not
// reach are asked on a later pass.
func TestPassStopsAtTheCap(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{}
	all := []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0), placed("gt-c", 30, 0)}
	p := newPass(t, all, asker, t0)
	calls := 0
	p.Allows = func() (bool, error) {
		calls++
		return calls <= 2, nil
	}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Asked != 2 {
		t.Errorf("summary = %+v, want the cap reached after 2 pairs", sum)
	}
	if n := len(readLog(t, p.LogPath)); n != 2 {
		t.Errorf("log has %d lines, want 2", n)
	}
}

// A pair Jev refuses for its own text is tried again on the next pass and given
// up after maxAttempts failures. The cursor stays before the placement until
// then.
func TestPassRetriesABeadFaultThenGivesUp(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{fail: func(state string) error {
		if strings.Contains(state, "Title of gt-b") {
			return &jev.StatusError{Status: http.StatusBadRequest}
		}
		return nil
	}}
	all := []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0)}
	p := newPass(t, all, asker, t0)

	for pass := 1; pass < maxAttempts; pass++ {
		sum, err := p.Run(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if sum.Failed != 1 || sum.GivenUp != 0 || sum.Asked != 0 {
			t.Errorf("pass %d = %+v, want one failure and nothing given up", pass, sum)
		}
		cur, _, err := battery.ReadCursor(p.CursorPath)
		if err != nil {
			t.Fatal(err)
		}
		if cur.ID != "gt-a" {
			t.Errorf("pass %d left the cursor at %+v; it must stop before gt-b, which holds the failed pair", pass, cur)
		}
	}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.GivenUp != 1 {
		t.Errorf("final pass = %+v, want the pair given up", sum)
	}
	if n := len(readLog(t, p.LogPath)); n != 0 {
		t.Errorf("a given-up pair must log no line, got %d", n)
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != "gt-b" {
		t.Errorf("cursor = %+v, want it moved past gt-b once the pair is given up", cur)
	}
}

// A pair that fails for the beads' own text holds the cursor before its
// placement, but it does not block the pairs the same pass can still ask.
func TestPassBeadFaultDoesNotBlockLaterPlacements(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{fail: func(state string) error {
		if strings.Contains(state, "Title of gt-b") {
			return &jev.StatusError{Status: http.StatusBadRequest}
		}
		return nil
	}}
	all := []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0), placed("gt-c", 30, 0)}
	p := newPass(t, all, asker, t0)

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// gt-b's two pairs fail; gt-c+gt-a does not, and is asked in this same pass.
	if sum.Asked != 1 || sum.Failed != 2 {
		t.Errorf("summary = %+v, want 1 asked and 2 failed", sum)
	}
	got := readLog(t, p.LogPath)
	if len(got) != 1 || got[0].Bead != "gt-c" || got[0].Other != "gt-a" {
		t.Errorf("log = %+v, want the one gt-c+gt-a line", got)
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != "gt-a" {
		t.Errorf("cursor = %+v; it must hold before gt-b, whose pairs are retried", cur)
	}
}

// An outage stops the pass with an error, and costs the pair no attempt, so it
// is asked in full once Jev is back.
func TestPassOutageStopsWithoutAnAttempt(t *testing.T) {
	t.Parallel()
	down := true
	asker := &fakeAsker{fail: func(string) error {
		if down {
			return &jev.StatusError{Status: http.StatusServiceUnavailable}
		}
		return nil
	}}
	p := newPass(t, []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0)}, asker, t0)

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("an outage must fail the pass")
	}
	st, err := readPairs(p.PairsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Attempts) != 0 || len(st.Done) != 0 {
		t.Errorf("state after an outage = %+v, want no attempts and nothing done", st)
	}

	down = false
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 {
		t.Errorf("after the outage Asked = %d, want the pair asked once", sum.Asked)
	}
}

// The log line holds bead IDs, stamps and numbers. The title and the spec text
// go to Jev and never reach the log.
func TestPassLogHoldsNoBeadText(t *testing.T) {
	t.Parallel()
	p := newPass(t, []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0)}, &fakeAsker{}, t0)
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Title of", "GOAL-OF", "CONSTRAINTS-OF"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("log leaks %q: %s", leak, raw)
		}
	}
}

// The state Jev sees carries each bead's goal and constraints, quoted.
func TestBuildStateQuotesEachBead(t *testing.T) {
	t.Parallel()
	state := buildState(placed("gt-a", 10, 0), placed("gt-b", 20, 0))
	for _, want := range []string{
		`first_title: "Title of gt-a"`,
		`first_goal: "GOAL-OF-gt-a"`,
		`second_constraints: "CONSTRAINTS-OF-gt-b"`,
	} {
		if !strings.Contains(state, want) {
			t.Errorf("state lacks %s:\n%s", want, state)
		}
	}
}
