package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// specAheadIssue is a board spec as the estimate-ahead pass reads the bead: the
// sections the estimate hashes and the fields the pick decides on.
func specAheadIssue(s specdispatch.Spec) beads.Issue {
	return beads.Issue{
		ID: s.ID, Title: s.Title, Type: s.Type, Status: s.Status, Assignee: s.Assignee,
		Priority: s.Priority, CreatedAt: s.CreatedAt, Labels: append([]string(nil), s.Labels...),
		Description: s.Description, AcceptanceCriteria: s.Acceptance,
	}
}

// specAheadSeed is a store holding the beads a tick's board read, as the
// estimate writes them.
func specAheadSeed(t *testing.T, specs ...specdispatch.Spec) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New()
	for _, s := range specs {
		db.Seed(specAheadIssue(s))
	}
	return db
}

func specAheadLabelsOn(t *testing.T, db beads.Client, id string) []string {
	t.Helper()
	issue, err := db.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return issue.Labels
}

func specAheadCommentsOn(t *testing.T, db beads.Client, id string) []string {
	t.Helper()
	cs, err := db.Comments(id)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range cs {
		texts = append(texts, c.Text)
	}
	return texts
}

// aheadNotices returns the lines the estimate-ahead pass wrote.
func aheadNotices(notices []string) []string {
	var out []string
	for _, n := range notices {
		if strings.Contains(n, "story points estimate ahead") {
			out = append(out, n)
		}
	}
	return out
}

// aheadEventLog records the estimates and slings a tick made, in order. The
// slings run on their own goroutines, so every append takes the lock.
type aheadEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *aheadEventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *aheadEventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *aheadEventLog) position(e string) int {
	for i, got := range l.all() {
		if got == e {
			return i
		}
	}
	return -1
}

// TestSpecDispatchStoryPointsEstimateAheadWritesBothAfterTheSlings: with the
// shadow setting on, the tick estimates two shaped beads it did not sling, after
// the slings ran, and writes the labels and note gt spec estimate writes.
func TestSpecDispatchStoryPointsEstimateAheadWritesBothAfterTheSlings(t *testing.T) {
	t.Parallel()
	a := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	b := cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z")
	c := cleanSpec("gt-c", 1, "2026-09-29T12:00:00Z")
	f := newFakeSpecTown(a, b, c)
	env := f.env()
	// One sling this tick; the other two wait and are the pass's.
	env.PerTick = 1
	db := specAheadSeed(t, a, b, c)
	writeEstimate := specStoryPointEstimateFor(
		func(string) specEstimateStore { return db },
		func(string) specEstimateRunner { return specEstimateRunnerFor(specEstimateOK()) })

	var log aheadEventLog
	env.StoryPointShadow = true
	env.StoryPointEstimate = func(ctx context.Context, id string) (specEstimateOutcome, error) {
		log.add("estimate:" + id)
		return writeEstimate(ctx, id)
	}
	sling := env.Sling
	env.Sling = func(cand specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
		log.add("sling:" + cand.Spec.ID)
		return sling(cand, resume, seat)
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want only gt-a", got)
	}
	for _, id := range []string{"gt-b", "gt-c"} {
		labels := specAheadLabelsOn(t, db, id)
		for _, want := range []string{"sp:5", "sp-rung:shape-jev", "sp-minutes:6.3"} {
			if !slices.Contains(labels, want) {
				t.Errorf("%s labels = %v, want %s", id, labels, want)
			}
		}
		comments := specAheadCommentsOn(t, db, id)
		if len(comments) != 1 || !strings.HasPrefix(comments[0], specEstimateNotePrefix) {
			t.Errorf("%s comments = %v, want one estimate note", id, comments)
		}
	}
	if n := aheadNotices(r.Notices); len(n) != 0 {
		t.Errorf("ahead notices %q on clean estimates, want none", n)
	}
	slingAt := log.position("sling:gt-a")
	for _, id := range []string{"gt-b", "gt-c"} {
		at := log.position("estimate:" + id)
		if at < 0 {
			t.Fatalf("events %v: %s was never estimated", log.all(), id)
		}
		if at < slingAt {
			t.Errorf("events %v: %s was estimated before the sling decisions", log.all(), id)
		}
	}
}

// TestSpecDispatchStoryPointsEstimateAheadFailureLogsOneLineAndKeepsSlings: an
// ahead estimate that fails is one logged line, and the sling decisions are
// unchanged.
func TestSpecDispatchStoryPointsEstimateAheadFailureLogsOneLineAndKeepsSlings(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z"))
	env := f.env()
	env.PerTick = 1
	env.StoryPointShadow = true
	env.StoryPointEstimate = func(_ context.Context, id string) (specEstimateOutcome, error) {
		if id == "gt-b" {
			return specEstimateOutcome{}, errors.New("embedder refused")
		}
		return specEstimateOutcome{Action: "estimated", Rung: "all", Minutes: 5, Points: 3}, nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q after a failed ahead estimate, want only gt-a", got)
	}
	n := aheadNotices(r.Notices)
	if len(n) != 1 {
		t.Fatalf("ahead notices = %q, want exactly one line", n)
	}
	if !strings.Contains(n[0], "gt-b") || !strings.Contains(n[0], "embedder refused") {
		t.Errorf("notice %q does not name the bead and the error", n[0])
	}
}

// TestSpecDispatchStoryPointsEstimateAheadTimeoutLogsOneLineAndKeepsSlings: an
// ahead estimate that runs past the shadow timeout is one logged line, and the
// sling decisions are unchanged.
func TestSpecDispatchStoryPointsEstimateAheadTimeoutLogsOneLineAndKeepsSlings(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z"))
	env := f.env()
	env.PerTick = 1
	env.StoryPointShadow = true
	env.After = firedTimer
	env.StoryPointEstimate = func(ctx context.Context, _ string) (specEstimateOutcome, error) {
		<-ctx.Done()
		return specEstimateOutcome{}, ctx.Err()
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q after a timed-out ahead estimate, want only gt-a", got)
	}
	n := aheadNotices(r.Notices)
	if len(n) != 1 {
		t.Fatalf("ahead notices = %q, want exactly one line", n)
	}
	if !strings.Contains(n[0], "gt-b") || !strings.Contains(n[0], "no estimate within") {
		t.Errorf("notice %q does not name the bead and the timeout", n[0])
	}
}

// TestSpecDispatchStoryPointsEstimateAheadOffDoesNothing: with the shadow
// setting off the pass reads and estimates nothing, and the tick slings as it
// did.
func TestSpecDispatchStoryPointsEstimateAheadOffDoesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z"))
	env := f.env()
	env.PerTick = 1
	env.StoryPointShadow = false
	var estimated []string
	env.StoryPointEstimate = func(_ context.Context, id string) (specEstimateOutcome, error) {
		estimated = append(estimated, id)
		return specEstimateOutcome{Action: "estimated"}, nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want only gt-a", got)
	}
	if len(f.aheadReads) != 0 {
		t.Errorf("the pass read %v with the setting off, want none", f.aheadReads)
	}
	if len(estimated) != 0 {
		t.Errorf("estimated %v with the setting off, want none", estimated)
	}
	if n := aheadNotices(r.Notices); len(n) != 0 {
		t.Errorf("ahead notices %q with the setting off, want none", n)
	}
}

// TestSpecDispatchStoryPointsEstimateAheadSkipsACurrentEstimate: a bead whose
// latest note carries its current hash is left alone, and its labels are not
// rewritten.
func TestSpecDispatchStoryPointsEstimateAheadSkipsACurrentEstimate(t *testing.T) {
	t.Parallel()
	a := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	b := cleanSpec("gt-b", 1, "2026-09-29T11:00:00Z")
	b.Labels = []string{"sp:3", "sp-rung:all", "sp-minutes:5"}
	issue := specAheadIssue(b)
	hash := estimate.SectionHash(&issue)
	f := newFakeSpecTown(a, b)
	f.aheadComments[b.ID] = []beads.Comment{{
		Text: fmt.Sprintf("%s sp:3 rung:all hash:%s\nminutes: 5.0", specEstimateNotePrefix, hash),
	}}
	env := f.env()
	env.PerTick = 1
	db := specAheadSeed(t, a, b)
	var estimated []string
	writeEstimate := specStoryPointEstimateFor(
		func(string) specEstimateStore { return db },
		func(string) specEstimateRunner { return specEstimateRunnerFor(specEstimateOK()) })
	env.StoryPointShadow = true
	env.StoryPointEstimate = func(ctx context.Context, id string) (specEstimateOutcome, error) {
		estimated = append(estimated, id)
		return writeEstimate(ctx, id)
	}
	runSpecDispatchCycle(env)

	if slices.Contains(estimated, "gt-b") {
		t.Errorf("estimated %v; gt-b's estimate is current and must be left alone", estimated)
	}
	got := append([]string(nil), specAheadLabelsOn(t, db, "gt-b")...)
	slices.Sort(got)
	if want := []string{"sp-minutes:5", "sp-rung:all", "sp:3"}; !slices.Equal(got, want) {
		t.Errorf("gt-b labels = %v, want its own sp labels unchanged", got)
	}
}

// TestSpecEstimateAheadRead: the pass's reader returns the bead and its
// comments, serves a bead the ledger already holds without a second show, and
// returns a failed show or comments read unchanged rather than as an empty
// bead.
func TestSpecEstimateAheadRead(t *testing.T) {
	t.Parallel()
	showErr := errors.New("bd show refused")
	commentsErr := errors.New("bd comments refused")
	issue := &beads.Issue{ID: "gt-a", Status: string(beads.StatusOpen)}
	want := []beads.Comment{{Text: specEstimateNotePrefix + " sp:3 rung:all hash:x"}}
	cases := []struct {
		name             string
		ledgerHas        bool
		showErr          error
		commentsErr      error
		wantIssue        *beads.Issue
		wantComments     []beads.Comment
		wantErr          error
		wantShows        int
		wantCommentReads int
	}{
		{name: "reads the bead and its comments", wantIssue: issue, wantComments: want, wantShows: 1, wantCommentReads: 1},
		{name: "a bead the ledger holds is not shown again", ledgerHas: true, wantIssue: issue, wantComments: want, wantShows: 0, wantCommentReads: 1},
		{name: "a failed show is returned", showErr: showErr, wantErr: showErr, wantShows: 1, wantCommentReads: 0},
		{name: "a failed comments read is returned", commentsErr: commentsErr, wantErr: commentsErr, wantShows: 1, wantCommentReads: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := newSpecTickReads()
			if tc.ledgerHas {
				reads.shows["gt-a"] = issue
			}
			shows, commentReads := 0, 0
			read := specEstimateAheadRead(reads,
				func(string) (*beads.Issue, error) {
					shows++
					return issue, tc.showErr
				},
				func(string) ([]beads.Comment, error) {
					commentReads++
					return want, tc.commentsErr
				})
			gotIssue, gotComments, err := read("gt-a")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if gotIssue != nil || gotComments != nil {
					t.Errorf("read returned (%v, %v) with an error, want (nil, nil)", gotIssue, gotComments)
				}
			} else if gotIssue != issue || !slices.Equal(gotComments, tc.wantComments) {
				t.Errorf("read returned (%v, %v), want (%v, %v)", gotIssue, gotComments, issue, tc.wantComments)
			}
			if shows != tc.wantShows {
				t.Errorf("showed %d times, want %d", shows, tc.wantShows)
			}
			if commentReads != tc.wantCommentReads {
				t.Errorf("read comments %d times, want %d", commentReads, tc.wantCommentReads)
			}
		})
	}
}

// TestSpecEstimateAheadPassRefusals: the pass does nothing in a dry run, says
// so when no reader is wired, and logs a bead it cannot read rather than
// reading it as nothing to estimate.
func TestSpecEstimateAheadPassRefusals(t *testing.T) {
	t.Parallel()
	readErr := errors.New("bd show refused")
	cases := []struct {
		name      string
		env       specDispatchEnv
		wantReads int
		wantLine  string
	}{
		{
			name:     "a dry run reads nothing",
			env:      specDispatchEnv{StoryPointShadow: true, DryRun: true},
			wantLine: "",
		},
		{
			name:     "no reader is wired",
			env:      specDispatchEnv{StoryPointShadow: true},
			wantLine: "no bead reader is wired",
		},
		{
			name: "a read that fails is one line",
			env: specDispatchEnv{StoryPointShadow: true, EstimateAheadRead: func(string) (*beads.Issue, []beads.Comment, error) {
				return nil, nil, readErr
			}},
			wantReads: 1,
			wantLine:  "cannot read: bd show refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := tc.env
			reads := 0
			if env.EstimateAheadRead != nil {
				read := env.EstimateAheadRead
				env.EstimateAheadRead = func(id string) (*beads.Issue, []beads.Comment, error) {
					reads++
					return read(id)
				}
			}
			lines := specEstimateAheadPass(env, []string{"gt-a"}, nil)
			if reads != tc.wantReads {
				t.Errorf("read %d bead(s), want %d", reads, tc.wantReads)
			}
			switch {
			case tc.wantLine == "":
				if len(lines) != 0 {
					t.Errorf("lines = %q, want none", lines)
				}
			default:
				if len(lines) != 1 || !strings.Contains(lines[0], tc.wantLine) {
					t.Errorf("lines = %q, want one holding %q", lines, tc.wantLine)
				}
			}
		})
	}
}
