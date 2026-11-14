package omclass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// fakeAsker answers every question with choice and records each state it was
// handed. noChoice answers with no choice; err, when set, is returned instead.
type fakeAsker struct {
	choice   string
	noChoice bool
	err      error
	states   []state
}

func (f *fakeAsker) Ask(_ context.Context, s any, _ map[string]jev.Question) (*jev.Answers, error) {
	f.states = append(f.states, s.(state))
	if f.err != nil {
		return nil, f.err
	}
	if f.noChoice {
		return &jev.Answers{Model: "jev-test", Choice: map[string]jev.ChoiceAnswer{}}, nil
	}
	return &jev.Answers{
		Model: "jev-test",
		Choice: map[string]jev.ChoiceAnswer{
			QRuleClass: {Choice: f.choice, Probabilities: map[string]float64{f.choice: 1}},
		},
	}, nil
}

// fakeReviews lists the records a test hands the pass, whatever time it names.
type fakeReviews struct {
	recs []land.ReviewRecord
	err  error
}

func (f fakeReviews) Since(time.Time) ([]land.ReviewRecord, error) { return f.recs, f.err }

// newPass returns a pass over a temp log and cursor. A non-zero cursorAt is
// written as the cursor, so the first Run asks about what came after it.
func newPass(t *testing.T, asker Asker, reviews Reviews, cursorAt time.Time) *Pass {
	t.Helper()
	dir := t.TempDir()
	p := &Pass{
		Asker:      asker,
		Reviews:    reviews,
		LogPath:    filepath.Join(dir, "omclass.jsonl"),
		CursorPath: filepath.Join(dir, "omclass.cursor.json"),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return t0.Add(time.Hour) },
	}
	if !cursorAt.IsZero() {
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: cursorAt}); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// review is a verdict record carrying findings, written at at.
func review(bead string, attempt int, at time.Time, findings ...land.ReviewFindingText) land.ReviewRecord {
	return land.ReviewRecord{
		BeadID: bead, Rig: "gastown", Attempt: attempt, At: at,
		Verdict: land.VerdictRequestChanges, FindingText: findings,
	}
}

// failOpen is a finding the table's keywords class as fail-open.
var failOpen = land.ReviewFindingText{File: "internal/x/x.go", Title: "reads as success on a failed read", Claim: "the error is dropped"}

// unclassified is a finding no keyword of the table matches.
var unclassified = land.ReviewFindingText{File: "internal/y/y.go", Title: "the grid shows blue", Claim: "the color is off"}

func readLog(t *testing.T, path string) []Line {
	t.Helper()
	var lines []Line
	if err := battery.ReadRecent(path, 1000, &lines); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestOMClassQuestionOptionsAreClassNamesPlusNone(t *testing.T) {
	q, ok := Questions()[QRuleClass]
	if !ok {
		t.Fatalf("no %s question", QRuleClass)
	}
	if q.Type != "choice" {
		t.Errorf("type = %q, want choice", q.Type)
	}
	for _, want := range []string{"fail-open", "silent-drop", NoneLabel} {
		if _, ok := q.Options[want]; !ok {
			t.Errorf("options %v lack %q", q.Options, want)
		}
	}
	if q.Options[NoneLabel] == "" {
		t.Error("none option carries no description")
	}
}

func TestOMClassKeywordLabelsAFindingByTheTable(t *testing.T) {
	cases := []struct {
		name string
		f    land.ReviewFindingText
		want string
	}{
		{"matched by title", failOpen, "fail-open"},
		{"unmatched is none", unclassified, NoneLabel},
		{"empty finding is none", land.ReviewFindingText{}, NoneLabel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Keyword(c.f); got != c.want {
				t.Errorf("Keyword = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOMClassRunFirstPassSetsCursorOnly(t *testing.T) {
	asker := &fakeAsker{choice: "fail-open"}
	p := newPass(t, asker, fakeReviews{recs: []land.ReviewRecord{review("gt-a", 1, t0.Add(time.Minute), failOpen)}}, time.Time{})
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Started || sum.Asked != 0 || len(asker.states) != 0 {
		t.Errorf("first pass = %+v with %d asks, want started with none", sum, len(asker.states))
	}
}

func TestOMClassRunAsksEachNewFinding(t *testing.T) {
	asker := &fakeAsker{choice: "fail-open"}
	recs := []land.ReviewRecord{
		review("gt-old", 1, t0.Add(-time.Minute), failOpen),
		review("gt-new", 1, t0.Add(time.Minute), failOpen, unclassified),
		{BeadID: "gt-queue", Rig: "gastown", At: t0.Add(2 * time.Minute), Queue: &land.FollowupQueue{}, FindingText: []land.ReviewFindingText{failOpen}},
	}
	p := newPass(t, asker, fakeReviews{recs: recs}, t0)
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 2 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v, want 2 asked and none skipped", sum)
	}
	lines := readLog(t, p.LogPath)
	if len(lines) != 2 {
		t.Fatalf("log has %d lines, want 2", len(lines))
	}
	if lines[0].Keyword != "fail-open" || lines[1].Keyword != NoneLabel {
		t.Errorf("keyword labels = %q, %q; want fail-open, none", lines[0].Keyword, lines[1].Keyword)
	}
	for _, l := range lines {
		if got := l.Answers[QRuleClass].Choice; got != "fail-open" {
			t.Errorf("answer = %q, want fail-open", got)
		}
	}
	if len(asker.states) != 2 || asker.states[0].Title != failOpen.Title || asker.states[0].File != failOpen.File {
		t.Errorf("state = %+v, want the finding's title and file", asker.states)
	}
}

func TestOMClassRunLogHoldsNoFindingText(t *testing.T) {
	p := newPass(t, &fakeAsker{choice: "fail-open"}, fakeReviews{recs: []land.ReviewRecord{review("gt-new", 1, t0.Add(time.Minute), failOpen)}}, t0)
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), failOpen.Claim) || strings.Contains(string(raw), failOpen.Title) {
		t.Errorf("log carries the finding's text: %s", raw)
	}
}

// TestOMClassRunAsksEveryFindingInOrder takes twelve findings from one review.
// An unpadded index would sort "/10" before "/2" and the cursor would skip them.
func TestOMClassRunAsksEveryFindingInOrder(t *testing.T) {
	var findings []land.ReviewFindingText
	for i := 0; i < 12; i++ {
		findings = append(findings, land.ReviewFindingText{Title: fmt.Sprintf("finding %d", i), Claim: "claim"})
	}
	asker := &fakeAsker{choice: NoneLabel}
	p := newPass(t, asker, fakeReviews{recs: []land.ReviewRecord{review("gt-many", 1, t0.Add(time.Minute), findings...)}}, t0)
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 12 {
		t.Fatalf("asked %d, want 12", sum.Asked)
	}
	for i, l := range readLog(t, p.LogPath) {
		if l.Finding != i {
			t.Errorf("line %d names finding %d", i, l.Finding)
		}
	}
}

func TestOMClassRunCapsAt300(t *testing.T) {
	var findings []land.ReviewFindingText
	for i := 0; i < MaxFindingsPerRun+1; i++ {
		findings = append(findings, land.ReviewFindingText{Title: fmt.Sprintf("finding %d", i), Claim: "claim"})
	}
	asker := &fakeAsker{choice: NoneLabel}
	p := newPass(t, asker, fakeReviews{recs: []land.ReviewRecord{review("gt-big", 1, t0.Add(time.Minute), findings...)}}, t0)

	first, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Asked != MaxFindingsPerRun || !first.Capped {
		t.Fatalf("first run = %+v, want %d asked and capped", first, MaxFindingsPerRun)
	}
	second, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Asked != 1 || second.Capped {
		t.Errorf("second run = %+v, want the one finding the cap held back", second)
	}
	if len(asker.states) != MaxFindingsPerRun+1 {
		t.Errorf("Jev was asked %d times, want %d", len(asker.states), MaxFindingsPerRun+1)
	}
}

// TestOMClassRunRefusals drives each refusal branch. A branch that reads no
// usable answer skips the finding and moves the cursor past it; a branch that
// fails the pass leaves the cursor on the finding it could not take.
func TestOMClassRunRefusals(t *testing.T) {
	recs := []land.ReviewRecord{review("gt-a", 1, t0.Add(time.Minute), failOpen)}
	cases := []struct {
		name           string
		asker          *fakeAsker
		reviews        Reviews
		allows         func() (bool, error)
		recs           []land.ReviewRecord
		wantErr        bool
		wantCapReached bool
		wantCalls      int
		wantAsked      int
		wantSkipped    int
		wantCursorAt   time.Time
	}{
		{
			name:    "reviews unreadable stops the pass",
			asker:   &fakeAsker{choice: "fail-open"},
			reviews: fakeReviews{err: errors.New("disk gone")},
			wantErr: true, wantCursorAt: t0,
		},
		{
			name:    "transport error stops the pass without moving the cursor",
			asker:   &fakeAsker{err: errors.New("jev unreachable")},
			recs:    recs,
			wantErr: true, wantCalls: 1, wantCursorAt: t0,
		},
		{
			name:  "reply with no choice is skipped and the cursor moves past it",
			asker: &fakeAsker{noChoice: true},
			recs:  recs, wantCalls: 1, wantSkipped: 1, wantCursorAt: t0.Add(time.Minute),
		},
		{
			name:        "finding with no title or claim is skipped unasked",
			asker:       &fakeAsker{choice: "fail-open"},
			recs:        []land.ReviewRecord{review("gt-blank", 1, t0.Add(time.Minute), land.ReviewFindingText{File: "a.go"})},
			wantSkipped: 1, wantCursorAt: t0.Add(time.Minute),
		},
		{
			name:           "spend cap reached stops before the ask",
			asker:          &fakeAsker{choice: "fail-open"},
			recs:           recs,
			allows:         func() (bool, error) { return false, nil },
			wantCapReached: true, wantCursorAt: t0,
		},
		{
			name:    "unreadable spend stops the pass",
			asker:   &fakeAsker{choice: "fail-open"},
			recs:    recs,
			allows:  func() (bool, error) { return false, errors.New("ledger corrupt") },
			wantErr: true, wantCursorAt: t0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reviews := c.reviews
			if reviews == nil {
				reviews = fakeReviews{recs: c.recs}
			}
			p := newPass(t, c.asker, reviews, t0)
			if c.allows != nil {
				p.Allows = c.allows
			}
			sum, err := p.Run(context.Background())
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if sum.Asked != c.wantAsked || sum.Skipped != c.wantSkipped {
				t.Errorf("summary = %+v, want asked %d skipped %d", sum, c.wantAsked, c.wantSkipped)
			}
			if sum.CapReached != c.wantCapReached {
				t.Errorf("CapReached = %v, want %v", sum.CapReached, c.wantCapReached)
			}
			if len(c.asker.states) != c.wantCalls {
				t.Errorf("Jev was called %d times, want %d", len(c.asker.states), c.wantCalls)
			}
			cur, _, err := battery.ReadCursor(p.CursorPath)
			if err != nil {
				t.Fatal(err)
			}
			if !cur.Time.Equal(c.wantCursorAt) {
				t.Errorf("cursor at %v, want %v", cur.Time, c.wantCursorAt)
			}
		})
	}
}

func TestOMClassRunBusyWhenAnotherPassHoldsTheLock(t *testing.T) {
	p := newPass(t, &fakeAsker{choice: "fail-open"}, fakeReviews{}, t0)
	release, err := battery.Lock(p.CursorPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := p.Run(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("Run = %v, want ErrBusy", err)
	}
}
