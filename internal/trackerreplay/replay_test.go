package trackerreplay

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// Unit tier: a scripted feed and a recording runner, no process and no clock.

// fakeSource serves events in pages of pageSize, after the seq asked for, and
// reports firstSeq as the oldest seq it still holds.
type fakeSource struct {
	events   []api.Event
	pageSize int
	firstSeq int64
}

func (f *fakeSource) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	var out []api.Event
	for _, e := range f.events {
		if e.Seq > after && (f.pageSize == 0 || len(out) < f.pageSize) {
			out = append(out, e)
		}
	}
	page := api.EventsPage{Events: out, FirstSeq: f.firstSeq}
	if len(out) > 0 {
		page.NextAfter = out[len(out)-1].Seq
	}
	return page, nil
}

// recorder runs nothing: it keeps each argv it was asked to apply and fails
// the call at failAt (1-based), if set.
type recorder struct {
	calls  [][]string
	failAt int
	err    error
}

func (r *recorder) apply(argv []string) error {
	r.calls = append(r.calls, argv)
	if r.failAt == len(r.calls) {
		return r.err
	}
	return nil
}

func str(s string) *string { return &s }

func TestCommandMapsEachReplayableEventToItsBdCall(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ev   api.Event
		want []string
	}{
		"created with an actor": {
			ev:   api.Event{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("Add x"), Actor: "town/crew/x"},
			want: []string{"create", "--id=gt-1", "--title=Add x", "--actor=town/crew/x"},
		},
		"created without an actor": {
			ev:   api.Event{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("Add x")},
			want: []string{"create", "--id=gt-1", "--title=Add x"},
		},
		"status changed": {
			ev:   api.Event{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: str("in_progress")},
			want: []string{"update", "gt-1", "--status=in_progress"},
		},
		"reopened": {
			ev:   api.Event{Seq: 3, Op: "reopened", IssueID: "gt-1", NewValue: str("open")},
			want: []string{"update", "gt-1", "--status=open"},
		},
		"label added": {
			ev:   api.Event{Seq: 4, Op: "label_added", IssueID: "gt-1", NewValue: str("gt:agent")},
			want: []string{"update", "gt-1", "--add-label=gt:agent"},
		},
		"label removed": {
			ev:   api.Event{Seq: 5, Op: "label_removed", IssueID: "gt-1", OldValue: str("gt:agent")},
			want: []string{"update", "gt-1", "--remove-label=gt:agent"},
		},
		"closed with a reason": {
			ev:   api.Event{Seq: 6, Op: "closed", IssueID: "gt-1", Comment: str("done")},
			want: []string{"close", "gt-1", "--reason=done"},
		},
		"closed without a reason": {
			ev:   api.Event{Seq: 7, Op: "closed", IssueID: "gt-1"},
			want: []string{"close", "gt-1"},
		},
		"deleted": {
			ev:   api.Event{Seq: 8, Op: "deleted", IssueID: "gt-1"},
			want: []string{"delete", "gt-1", "--force"},
		},
		"dependency added": {
			ev:   api.Event{Seq: 9, Op: "dependency_added", IssueID: "gt-1", NewValue: str("gt-2"), Comment: str("blocks")},
			want: []string{"dep", "add", "gt-1", "gt-2", "--type=blocks"},
		},
		"dependency removed": {
			ev:   api.Event{Seq: 10, Op: "dependency_removed", IssueID: "gt-1", OldValue: str("gt-2"), Comment: str("blocks")},
			want: []string{"dep", "remove", "gt-1", "gt-2"},
		},
		"title updated": {
			ev:   api.Event{Seq: 11, Op: "updated", IssueID: "gt-1", NewValue: str("New title"), Comment: str("title")},
			want: []string{"update", "gt-1", "--title=New title"},
		},
		"priority updated": {
			ev:   api.Event{Seq: 12, Op: "updated", IssueID: "gt-1", NewValue: str("1"), Comment: str("priority")},
			want: []string{"update", "gt-1", "--priority=1"},
		},
		"assignee cleared": {
			ev:   api.Event{Seq: 13, Op: "updated", IssueID: "gt-1", NewValue: str(""), Comment: str("assignee")},
			want: []string{"update", "gt-1", "--assignee="},
		},
	}
	for name, tc := range cases {
		got, err := Command(tc.ev)
		if err != nil {
			t.Errorf("%s: Command = %v, want %v", name, err, tc.want)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Command = %q, want %q", name, got, tc.want)
		}
	}
}

func TestCommandRefusesWhatTheFeedCannotReplayAndNamesKindAndSeq(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ev   api.Event
		kind string
	}{
		"comment text is not in the feed": {
			ev:   api.Event{Seq: 20, Op: "commented", IssueID: "gt-1"},
			kind: "commented",
		},
		"description update has no new value": {
			ev:   api.Event{Seq: 21, Op: "updated", IssueID: "gt-1", Comment: str("description")},
			kind: "updated/description",
		},
		"notes update has no new value": {
			ev:   api.Event{Seq: 22, Op: "updated", IssueID: "gt-1", Comment: str("notes")},
			kind: "updated/notes",
		},
		"priority outside 0-4": {
			ev:   api.Event{Seq: 23, Op: "updated", IssueID: "gt-1", NewValue: str("9"), Comment: str("priority")},
			kind: "updated/priority",
		},
		"unknown event kind": {
			ev:   api.Event{Seq: 24, Op: "frobnicated", IssueID: "gt-1"},
			kind: "frobnicated",
		},
		"dependency without its type": {
			ev:   api.Event{Seq: 25, Op: "dependency_added", IssueID: "gt-1", NewValue: str("gt-2")},
			kind: "dependency_added",
		},
		"creation without a title": {
			ev:   api.Event{Seq: 26, Op: "created", IssueID: "gt-1"},
			kind: "created",
		},
		"status change without the new status": {
			ev:   api.Event{Seq: 27, Op: "status_changed", IssueID: "gt-1"},
			kind: "status_changed",
		},
		"label added without its name": {
			ev:   api.Event{Seq: 28, Op: "label_added", IssueID: "gt-1"},
			kind: "label_added",
		},
		"label removed without its name": {
			ev:   api.Event{Seq: 29, Op: "label_removed", IssueID: "gt-1"},
			kind: "label_removed",
		},
		"dependency added without its target": {
			ev:   api.Event{Seq: 30, Op: "dependency_added", IssueID: "gt-1", Comment: str("blocks")},
			kind: "dependency_added",
		},
		"dependency removed without its target": {
			ev:   api.Event{Seq: 31, Op: "dependency_removed", IssueID: "gt-1"},
			kind: "dependency_removed",
		},
		"issue id that bd would read as a flag": {
			ev:   api.Event{Seq: 32, Op: "deleted", IssueID: "--force"},
			kind: "deleted",
		},
		"issue id holding a space": {
			ev:   api.Event{Seq: 33, Op: "status_changed", IssueID: "gt 1", NewValue: str("open")},
			kind: "status_changed",
		},
		"dependency target that bd would read as a flag": {
			ev:   api.Event{Seq: 34, Op: "dependency_added", IssueID: "gt-1", NewValue: str("-x"), Comment: str("blocks")},
			kind: "dependency_added",
		},
	}
	for name, tc := range cases {
		argv, err := Command(tc.ev)
		var u *UnreplayableError
		if !errors.As(err, &u) {
			t.Errorf("%s: Command = %q, %v; want *UnreplayableError", name, argv, err)
			continue
		}
		if u.Seq != tc.ev.Seq || u.Kind != tc.kind {
			t.Errorf("%s: refused seq %d kind %q, want seq %d kind %q", name, u.Seq, u.Kind, tc.ev.Seq, tc.kind)
		}
	}
}

// failingSource is a feed that cannot be read.
type failingSource struct{ err error }

func (f failingSource) Events(_ context.Context, _ int64, _ int) (api.EventsPage, error) {
	return api.EventsPage{}, f.err
}

func TestReplayReturnsAFeedReadFailureWithoutRunningAnything(t *testing.T) {
	t.Parallel()
	readErr := errors.New("tracker unreachable")
	run := &recorder{}
	res, err := Replay(context.Background(), failingSource{err: readErr}, run.apply, 3, false, nil)
	if !errors.Is(err, readErr) || res != (Result{Applied: 3}) || len(run.calls) != 0 {
		t.Errorf("Replay over an unreadable feed = %+v, %v with %d calls; want the read error, Applied 3 and no calls", res, err, len(run.calls))
	}
}

func TestReplayDryRunPrintsEachBdCallAndRunsNone(t *testing.T) {
	t.Parallel()
	src := &fakeSource{pageSize: 1, events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("Title with space")},
		{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: str("in_progress")},
	}}
	var out bytes.Buffer
	res, err := Replay(context.Background(), src, nil, 0, true, &out)
	if err != nil {
		t.Fatalf("Replay dry run: %v", err)
	}
	want := "bd create --id=gt-1 \"--title=Title with space\"\nbd update gt-1 --status=in_progress\n"
	if out.String() != want {
		t.Errorf("dry run printed %q, want %q", out.String(), want)
	}
	if res != (Result{Applied: 2, Commands: 2}) {
		t.Errorf("dry run result = %+v, want Applied 2 Commands 2", res)
	}
}

func TestReplayStopsAtTheFirstFailingCommandAndKeepsItsErrorOutOfTheMessage(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("a")},
		{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: str("open")},
		{Seq: 3, Op: "closed", IssueID: "gt-1"},
	}}
	run := &recorder{failAt: 2, err: errors.New("database locked: token sk-secret-value")}
	res, err := Replay(context.Background(), src, run.apply, 0, false, nil)

	var ae *ApplyError
	if !errors.As(err, &ae) || ae.Seq != 2 || ae.Verb != "update" {
		t.Fatalf("Replay error = %v, want *ApplyError at seq 2 verb update", err)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("error message carries the runner's output: %q", err.Error())
	}
	if res != (Result{Applied: 1, Commands: 1}) {
		t.Errorf("result = %+v, want Applied 1 (the last seq that ran) Commands 1", res)
	}
	if len(run.calls) != 2 {
		t.Errorf("runner was called %d times, want 2: the close after the failure must not run", len(run.calls))
	}
}

func TestReplayRefusesAnUnknownKindAfterRunningTheCommandsBeforeIt(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("a")},
		{Seq: 2, Op: "frobnicated", IssueID: "gt-1"},
		{Seq: 3, Op: "created", IssueID: "gt-2", NewValue: str("b")},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, false, nil)

	var u *UnreplayableError
	if !errors.As(err, &u) || u.Seq != 2 || u.Kind != "frobnicated" {
		t.Fatalf("Replay error = %v, want unreplayable frobnicated at seq 2", err)
	}
	if res.Applied != 1 || len(run.calls) != 1 {
		t.Errorf("result %+v with %d calls, want seq 1 applied and nothing after the refusal", res, len(run.calls))
	}
}

func TestReplayResumesAfterSinceAndAppliesTheRest(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "deleted", IssueID: "gt-0"},
		{Seq: 2, Op: "deleted", IssueID: "gt-1"},
		{Seq: 3, Op: "deleted", IssueID: "gt-2"},
		{Seq: 4, Op: "deleted", IssueID: "gt-3"},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 2, false, nil)
	if err != nil {
		t.Fatalf("Replay from 2: %v", err)
	}
	want := [][]string{{"delete", "gt-2", "--force"}, {"delete", "gt-3", "--force"}}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("applied %q, want %q: a rerun from the last applied seq starts after it", run.calls, want)
	}
	if res.Applied != 4 {
		t.Errorf("Applied = %d, want 4", res.Applied)
	}
}

func TestReplayRefusesAFeedPrunedPastSince(t *testing.T) {
	t.Parallel()
	src := &fakeSource{firstSeq: 5, events: []api.Event{
		{Seq: 5, Op: "deleted", IssueID: "gt-5"},
	}}
	run := &recorder{}
	_, err := Replay(context.Background(), src, run.apply, 2, false, nil)
	var tr *TruncatedError
	if !errors.As(err, &tr) || tr.Since != 2 || tr.Floor != 5 {
		t.Fatalf("Replay from 2 with oldest retained 5 = %v, want *TruncatedError{Since:2, Floor:5}", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("runner was called %d times on a pruned feed, want 0", len(run.calls))
	}
	if _, err := Replay(context.Background(), src, run.apply, 4, false, nil); err != nil {
		t.Errorf("Replay from 4 with oldest retained 5 = %v, want no error: nothing is missing", err)
	}
}

// pageSource serves the pages it was given in order, whatever cursor it is
// asked for, so a test can script a feed that changes shape between reads.
type pageSource struct {
	pages []api.EventsPage
	asked []int64
}

func (p *pageSource) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	p.asked = append(p.asked, after)
	if len(p.asked) > len(p.pages) {
		return api.EventsPage{}, nil
	}
	return p.pages[len(p.asked)-1], nil
}

func TestReplayRefusesAPrunedFeedWhoseFirstPageIsEmpty(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{{FirstSeq: 5}}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 2, false, nil)
	var tr *TruncatedError
	if !errors.As(err, &tr) || tr.Since != 2 || tr.Floor != 5 {
		t.Fatalf("Replay from 2 over an empty page with oldest retained 5 = %v, want *TruncatedError{Since:2, Floor:5}", err)
	}
	if len(run.calls) != 0 || res != (Result{Applied: 2}) {
		t.Errorf("result %+v with %d calls, want nothing applied: events 3 and 4 were pruned", res, len(run.calls))
	}
}

func TestReplayRefusesAFeedPrunedWhileItReads(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{{Seq: 2, Op: "deleted", IssueID: "gt-2"}}, FirstSeq: 1, NextAfter: 2},
		{FirstSeq: 6, NextAfter: 2},
	}}
	run := &recorder{}
	_, err := Replay(context.Background(), src, run.apply, 0, false, nil)
	var tr *TruncatedError
	if !errors.As(err, &tr) || tr.Since != 2 || tr.Floor != 6 {
		t.Fatalf("Replay whose feed pruned past its cursor = %v, want *TruncatedError{Since:2, Floor:6}", err)
	}
	if len(run.calls) != 1 {
		t.Errorf("runner was called %d times, want 1: seq 2 ran before the gap was seen", len(run.calls))
	}
}

func TestReplayRefusesAnEmptyPageThatSaysMoreEventsFollow(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{{Seq: 1, Op: "deleted", IssueID: "gt-1"}}, FirstSeq: 1, NextAfter: 1},
		{NextAfter: 9, FirstSeq: 1},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, false, nil)
	if err == nil || !strings.Contains(err.Error(), "empty page") || !strings.Contains(err.Error(), "9") {
		t.Fatalf("Replay over an empty page that points at seq 9 = %v, want a refusal naming it", err)
	}
	if res != (Result{Applied: 1, Commands: 1}) {
		t.Errorf("result = %+v, want seq 1 applied and the feed not read as ended", res)
	}
	if len(src.asked) != 2 || src.asked[1] != 1 {
		t.Errorf("pages were asked for after %v, want the second after 1: the cursor follows the last event applied", src.asked)
	}
}

// endlessSource hands out one new event per call, forever.
type endlessSource struct{}

func (endlessSource) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	next := after + 1
	return api.EventsPage{Events: []api.Event{{Seq: next, Op: "deleted", IssueID: "gt-x"}}, NextAfter: next}, nil
}

func TestReplayStopsAFeedThatNeverEnds(t *testing.T) {
	t.Parallel()
	run := &recorder{}
	res, err := Replay(context.Background(), endlessSource{}, run.apply, 0, false, nil)
	if err == nil || !strings.Contains(err.Error(), "did not end") {
		t.Fatalf("Replay over an endless feed = %v, want a refusal naming the page bound", err)
	}
	if res.Applied != maxPages || len(run.calls) != maxPages {
		t.Errorf("result %+v with %d calls, want the run stopped after %d events", res, len(run.calls), maxPages)
	}
}

func TestReplayDryRunQuotesAValueTheShellWouldRewrite(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("line one\nline two")},
		{Seq: 2, Op: "created", IssueID: "gt-2", NewValue: str("café")},
	}}
	var out bytes.Buffer
	if _, err := Replay(context.Background(), src, nil, 0, true, &out); err != nil {
		t.Fatalf("Replay dry run: %v", err)
	}
	want := "bd create --id=gt-1 \"--title=line one\\nline two\"\nbd create --id=gt-2 \"--title=café\"\n"
	if out.String() != want {
		t.Errorf("dry run printed %q, want %q", out.String(), want)
	}
}

func TestReplayReadsEveryPageOfTheFeed(t *testing.T) {
	t.Parallel()
	var evs []api.Event
	for i := int64(1); i <= 5; i++ {
		evs = append(evs, api.Event{Seq: i, Op: "deleted", IssueID: "gt-x"})
	}
	src := &fakeSource{pageSize: 2, events: evs}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, false, nil)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res != (Result{Applied: 5, Commands: 5}) || len(run.calls) != 5 {
		t.Errorf("result %+v with %d calls, want all 5 events applied across pages", res, len(run.calls))
	}
}

// unorderedSource hands out a seq that does not move the feed forward. It
// ends the feed after a few pages, so a replay that stops checking ordering
// fails the test instead of looping on seq 2 forever.
type unorderedSource struct{ calls int }

func (u *unorderedSource) Events(_ context.Context, _ int64, _ int) (api.EventsPage, error) {
	u.calls++
	switch {
	case u.calls == 1:
		return api.EventsPage{Events: []api.Event{{Seq: 3, Op: "deleted", IssueID: "gt-3"}}}, nil
	case u.calls <= 3:
		return api.EventsPage{Events: []api.Event{{Seq: 2, Op: "deleted", IssueID: "gt-2"}}}, nil
	default:
		return api.EventsPage{}, nil
	}
}

func TestReplayRefusesAFeedThatGoesBackwards(t *testing.T) {
	t.Parallel()
	run := &recorder{}
	_, err := Replay(context.Background(), &unorderedSource{}, run.apply, 0, false, nil)
	if err == nil || !strings.Contains(err.Error(), "not ascending") {
		t.Fatalf("Replay over a backwards feed = %v, want a not-ascending refusal", err)
	}
	if len(run.calls) != 1 {
		t.Errorf("runner was called %d times, want 1: only the ordered event runs", len(run.calls))
	}
}
